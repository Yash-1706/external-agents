package model

import (
	"testing"
	"time"
)

// The domain contract is the one place every other package trusts without
// checking. These tests pin the rules that the rest of the system relies on
// being true — above all that a claim can never be stronger than its evidence.

func TestConfidenceForNeverExceedsEvidence(t *testing.T) {
	tests := []struct {
		name string
		ev   []Evidence
		want Confidence
	}{
		{"no evidence is never better than unknown", nil, Unknown},
		{"empty slice is unknown", []Evidence{}, Unknown},
		{"a commit is observed", []Evidence{{Kind: EvidenceCommit, Ref: "abc"}}, Observed},
		{"a test result is observed", []Evidence{{Kind: EvidenceTest, Ref: "TestX"}}, Observed},
		{"a checkpoint is observed", []Evidence{{Kind: EvidenceCheckpoint, Ref: "cp_1"}}, Observed},
		{"an agent statement is only inferred", []Evidence{{Kind: EvidencePrompt, Ref: "p"}}, Inferred},
		{"model inference is only inferred", []Evidence{{Kind: EvidenceInference, Ref: "i"}}, Inferred},
		{
			"the strongest evidence wins",
			[]Evidence{{Kind: EvidenceInference, Ref: "i"}, {Kind: EvidenceCommit, Ref: "abc"}},
			Observed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ConfidenceFor(tt.ev); got != tt.want {
				t.Errorf("ConfidenceFor = %q, want %q", got, tt.want)
			}
		})
	}
}

// Plan §31: a repository fact outranks a model inference. The whole merge layer
// depends on this ordering being numerically correct.
func TestEvidenceLevelHierarchy(t *testing.T) {
	order := []struct {
		kind  EvidenceKind
		level EvidenceLevel
	}{
		{EvidenceCommit, LevelRepository},
		{EvidenceFile, LevelRepository},
		{EvidenceTest, LevelRepository},
		{EvidenceGraph, LevelRepository},
		{EvidenceCheckpoint, LevelCheckpoint},
		{EvidenceSession, LevelCheckpoint},
		{EvidenceEvent, LevelStatement},
		{EvidencePrompt, LevelStatement},
		{EvidenceInference, LevelInference},
	}
	for _, o := range order {
		if got := o.kind.Level(); got != o.level {
			t.Errorf("%s level = %d, want %d", o.kind, got, o.level)
		}
	}
	if !(LevelRepository < LevelCheckpoint && LevelCheckpoint < LevelStatement && LevelStatement < LevelInference) {
		t.Fatal("the evidence hierarchy is not ordered strongest-first")
	}
	if _, ok := StrongestLevel(nil); ok {
		t.Error("StrongestLevel reported a level for no evidence")
	}
}

func TestTaskIDIsDeterministic(t *testing.T) {
	a := NewTaskID("owner/repo", "main", "sess_1")
	if b := NewTaskID("owner/repo", "main", "sess_1"); a != b {
		t.Fatalf("the same inputs produced two ids: %q then %q", a, b)
	}
	// Cross-agent lineage depends on the same task resolving identically on a
	// different machine, so each component must actually participate.
	for _, other := range []string{
		NewTaskID("owner/other", "main", "sess_1"),
		NewTaskID("owner/repo", "dev", "sess_1"),
		NewTaskID("owner/repo", "main", "sess_2"),
	} {
		if other == a {
			t.Errorf("a differing component produced the same id %q", a)
		}
	}
	// Field boundaries must not be ambiguous: "ab"+"c" must differ from "a"+"bc".
	if NewTaskID("ab", "c", "s") == NewTaskID("a", "bc", "s") {
		t.Error("task id fields are not separated; concatenation collides")
	}
}

func TestTaskStatusTransitions(t *testing.T) {
	if !CanTransition(StatusPartial, StatusPartial) {
		t.Error("a status must be allowed to stay where it is")
	}
	// The rule that matters: work cannot jump straight from unfinished to done.
	if CanTransition(StatusPartial, StatusComplete) {
		t.Error("partial -> complete is not a declared transition (plan §17)")
	}
	if CanTransition(StatusNew, StatusComplete) {
		t.Error("new -> complete skips the entire lifecycle")
	}
	if !CanTransition(StatusVerified, StatusComplete) {
		t.Error("verified -> complete must be allowed")
	}
	if !CanTransition(StatusBlocked, StatusResumed) {
		t.Error("blocked -> resumed must be allowed")
	}
}

func TestAgentKindValidity(t *testing.T) {
	tests := []struct {
		in   AgentKind
		want bool
	}{
		{AgentOpenClaw, true},
		{AgentHermes, true},
		{AgentHuman, true},
		{AgentUnknown, true},
		// The vocabulary is open: a runtime we have never integrated with is
		// still a runtime the transcript named.
		{"acmecode", true},
		{"cursor", true},
		{"my-agent_2.0", true},
		// It is open, not unbounded. These reach output and map keys.
		{"", false},
		{"-leading", false},
		{"Has Spaces", false},
		{"UPPER", false},
		{"has!punct", false},
		{AgentKind("aaaaaaaaaabbbbbbbbbbccccccccccddddd"), false},
	}
	for _, tt := range tests {
		if got := tt.in.Valid(); got != tt.want {
			t.Errorf("AgentKind(%q).Valid() = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeAgent(t *testing.T) {
	tests := []struct {
		in   string
		want AgentKind
	}{
		{"AcmeCode", AgentKind("acmecode")},
		{"Acme Code", AgentKind("acme-code")},
		{"OpenClaw", AgentOpenClaw},
		{"  Hermes  ", AgentHermes},
		{"", AgentUnknown},
		{"!!!", AgentUnknown},
	}
	for _, tt := range tests {
		if got := NormalizeAgent(tt.in); got != tt.want {
			t.Errorf("NormalizeAgent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// A third-party runtime must render under its own name, or two of them draw
	// identical branches in the lineage tree.
	if got := AgentKind("acmecode").Display(); got != "Acmecode" {
		t.Errorf("Display = %q, want a name rather than Unknown", got)
	}
	if AgentKind("cursor").Display() == AgentKind("aider").Display() {
		t.Error("two distinct runtimes render identically")
	}
}

func TestAgentEventValidate(t *testing.T) {
	base := AgentEvent{
		Type: SessionStarted, Timestamp: time.Unix(1, 0), TaskID: "task_1",
		SessionID: "s1", Agent: AgentOpenClaw,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a well-formed event was rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(AgentEvent) AgentEvent
	}{
		{"unknown type", func(e AgentEvent) AgentEvent { e.Type = "Nope"; return e }},
		{"no session", func(e AgentEvent) AgentEvent { e.SessionID = ""; return e }},
		{"no task", func(e AgentEvent) AgentEvent { e.TaskID = ""; return e }},
		{"no timestamp", func(e AgentEvent) AgentEvent { e.Timestamp = time.Time{}; return e }},
		{"malformed agent", func(e AgentEvent) AgentEvent { e.Agent = "Bad Agent!"; return e }},
		{"own parent", func(e AgentEvent) AgentEvent { e.ParentSessionID = e.SessionID; return e }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.mutate(base).Validate(); err == nil {
				t.Error("Validate accepted an event it should reject")
			}
		})
	}

	// Retained unknown records must remain storable, or the curveball fix would
	// have nowhere to put them.
	unknown := base
	unknown.Type = EventUnknown
	if err := unknown.Validate(); err != nil {
		t.Errorf("a retained unknown event is not storable: %v", err)
	}
}

func TestCaptureOrAndPruneMissing(t *testing.T) {
	observed := Capture{GitAvailable: true, EventsAvailable: true}
	silent := Capture{Missing: []string{MissingGit, MissingCheckpoints}}

	merged := observed.Or(silent)
	if !merged.GitAvailable {
		t.Error("Or dropped an observation the other side simply never made")
	}
	// A gap the other side satisfied must not survive, or the same input is
	// listed as both verified and unknown.
	for _, m := range merged.Missing {
		if m == MissingGit {
			t.Error("a satisfied gap survived the merge")
		}
	}
	if len(merged.Missing) != 1 || merged.Missing[0] != MissingCheckpoints {
		t.Errorf("Missing = %v, want just %q", merged.Missing, MissingCheckpoints)
	}

	// Pruning is idempotent and preserves names this package does not own.
	c := Capture{GitAvailable: true, Missing: []string{MissingGit, "something else", "something else"}}
	p := c.PruneMissing()
	if len(p.Missing) != 1 || p.Missing[0] != "something else" {
		t.Errorf("PruneMissing = %v, want the foreign name kept once", p.Missing)
	}
	if again := p.PruneMissing(); len(again.Missing) != len(p.Missing) {
		t.Error("PruneMissing is not idempotent")
	}
}

func TestStateHashIsStableAndSemantic(t *testing.T) {
	build := func() *EngineeringState {
		s := NewEngineeringState(TaskRef{ID: "t1", Title: "Demo", OriginalIntent: "do the thing"})
		s.Requirements = []Requirement{
			{ID: "R2", Description: "second", Status: ReqUnresolved},
			{ID: "R1", Description: "first", Status: ReqPartial},
		}
		s.Tests = []TestResult{{Name: "TestB", Status: TestFailed}, {Name: "TestA", Status: TestPassed}}
		return s
	}

	a, b := build(), build()
	// Timestamps that vary per run are excluded, or the receipt hash would churn
	// for two identical engineering conclusions.
	a.GeneratedAt = time.Now()
	b.GeneratedAt = time.Now().Add(time.Hour)
	a.Repo.ObservedAt = time.Now()
	if a.Hash() != b.Hash() {
		t.Error("the same conclusion hashed differently across runs")
	}

	// Order must not matter; content must.
	c := build()
	c.Requirements[0], c.Requirements[1] = c.Requirements[1], c.Requirements[0]
	if c.Hash() != a.Hash() {
		t.Error("re-ordering the same requirements changed the hash")
	}
	d := build()
	d.Requirements[0].Status = ReqComplete
	if d.Hash() == a.Hash() {
		t.Error("a changed requirement status did not change the hash")
	}
}

func TestDeriveStatusFromEvidence(t *testing.T) {
	withReqs := func(statuses ...ReqStatus) *EngineeringState {
		s := NewEngineeringState(TaskRef{ID: "t"})
		s.Sessions = []SessionNode{{SessionID: "s1"}}
		for i, st := range statuses {
			s.Requirements = append(s.Requirements, Requirement{ID: string(rune('A' + i)), Status: st})
		}
		return s
	}

	if got := NewEngineeringState(TaskRef{ID: "t"}).DeriveStatus(); got != StatusNew {
		t.Errorf("empty state = %q, want %q", got, StatusNew)
	}
	if got := withReqs(ReqComplete, ReqComplete).DeriveStatus(); got != StatusVerified {
		t.Errorf("all complete = %q, want %q", got, StatusVerified)
	}
	if got := withReqs(ReqComplete, ReqUnresolved).DeriveStatus(); got != StatusPartial {
		t.Errorf("one unresolved = %q, want %q", got, StatusPartial)
	}
	if got := withReqs(ReqComplete, ReqBlocked).DeriveStatus(); got != StatusBlocked {
		t.Errorf("one blocked = %q, want %q", got, StatusBlocked)
	}

	// A failing test caps the status at partial no matter how the requirements
	// look: nothing is verified while a test is red.
	failing := withReqs(ReqComplete, ReqComplete)
	failing.Tests = []TestResult{{Name: "T", Status: TestFailed}}
	if got := failing.DeriveStatus(); got != StatusPartial {
		t.Errorf("all requirements complete with a failing test = %q, want %q", got, StatusPartial)
	}
	// DeriveStatus must never return Complete: completion is a human judgement,
	// not something evidence alone establishes.
	for _, s := range []*EngineeringState{
		withReqs(ReqComplete), withReqs(ReqComplete, ReqComplete), failing,
	} {
		if s.DeriveStatus() == StatusComplete {
			t.Error("DeriveStatus declared a task complete on its own")
		}
	}
}

func TestLineageValidateAndAgents(t *testing.T) {
	l := Lineage{
		Sessions: []SessionNode{
			{SessionID: "a", Agent: AgentOpenClaw, StartedAt: time.Unix(1, 0)},
			{SessionID: "b", ParentSessionID: "a", Agent: AgentHermes, StartedAt: time.Unix(2, 0)},
		},
		Checkpoints: []CheckpointNode{{CheckpointID: "cp1", SessionID: "a", CreatedAt: time.Unix(3, 0)}},
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("a well-formed lineage was rejected: %v", err)
	}
	if got := l.Agents(); len(got) != 2 || got[0] != AgentOpenClaw || got[1] != AgentHermes {
		t.Errorf("Agents = %v, want [openclaw hermes] in first-seen order", got)
	}
	if cp, ok := l.LatestCheckpoint(); !ok || cp.CheckpointID != "cp1" {
		t.Errorf("LatestCheckpoint = %+v, %v", cp, ok)
	}

	orphan := Lineage{Checkpoints: []CheckpointNode{{CheckpointID: "cp1", SessionID: "ghost"}}}
	if err := orphan.Validate(); err == nil {
		t.Error("a checkpoint naming an unknown session was accepted")
	}
	empty := Lineage{}
	if _, ok := empty.LatestCheckpoint(); ok {
		t.Error("an empty lineage reported a checkpoint")
	}
}

func TestDedupeEvidencePreservesOrder(t *testing.T) {
	in := []Evidence{
		{Kind: EvidenceCommit, Ref: "a"},
		{Kind: EvidenceTest, Ref: "T"},
		{Kind: EvidenceCommit, Ref: "a"},
	}
	got := DedupeEvidence(in)
	if len(got) != 2 || got[0].Ref != "a" || got[1].Ref != "T" {
		t.Errorf("DedupeEvidence = %+v, want first-seen order with one duplicate removed", got)
	}
}
