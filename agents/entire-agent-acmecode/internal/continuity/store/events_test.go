package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func at(minute int) time.Time {
	return time.Date(2026, 3, 1, 10, minute, 0, 0, time.UTC)
}

// demoEvents is a realistic cross-agent sequence: OpenClaw starts the task,
// delegates a test sub-agent, checkpoints, is interrupted, and Hermes picks the
// same task up afterwards. That last part is the observable proof of cross-agent
// continuity (plan §44).
func demoEvents() []model.AgentEvent {
	return []model.AgentEvent{
		{Type: model.SessionStarted, Timestamp: at(0), TaskID: testTaskID, SessionID: "sess_main", Agent: model.AgentOpenClaw, Role: model.RoleMain, Summary: "start"},
		{Type: model.ToolUsed, Timestamp: at(1), TaskID: testTaskID, SessionID: "sess_main", Agent: model.AgentOpenClaw, Attrs: map[string]string{"tool_name": "edit"}},
		{Type: model.SubagentStarted, Timestamp: at(2), TaskID: testTaskID, SessionID: "sess_test", ParentSessionID: "sess_main", Agent: model.AgentOpenClaw, Role: model.RoleTest},
		{Type: model.SubagentEnded, Timestamp: at(4), TaskID: testTaskID, SessionID: "sess_test", ParentSessionID: "sess_main", Agent: model.AgentOpenClaw},
		{Type: model.CheckpointCreated, Timestamp: at(5), TaskID: testTaskID, SessionID: "sess_main", Agent: model.AgentOpenClaw, Attrs: map[string]string{
			"checkpoint_id": "cp_001",
			"commit_sha":    "9f1c2b3",
			"label":         "tests green",
		}},
		{Type: model.SessionInterrupted, Timestamp: at(6), TaskID: testTaskID, SessionID: "sess_main", Agent: model.AgentOpenClaw, Summary: "user pressed stop"},
		{Type: model.SessionStarted, Timestamp: at(7), TaskID: testTaskID, SessionID: "sess_hermes", Agent: model.AgentHermes, Role: model.RoleMain},
		{Type: model.TaskResumed, Timestamp: at(8), TaskID: testTaskID, SessionID: "sess_hermes", Agent: model.AgentHermes},
	}
}

func eventLines(t *testing.T, f *FS, taskID string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.Root(), tasksDirName, taskID, eventsFileName))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	var out [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func TestAppendEventIdempotentUnderReplay(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	events := demoEvents()
	for _, ev := range events {
		if err := f.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent %s: %v", ev.Type, err)
		}
	}
	// Replay the same log, reversed, the way a resumed adapter re-scans a
	// transcript it has already reported once.
	for i := len(events) - 1; i >= 0; i-- {
		if err := f.AppendEvent(ctx, events[i]); err != nil {
			t.Fatalf("replay AppendEvent %s: %v", events[i].Type, err)
		}
	}

	if got, want := len(eventLines(t, f, testTaskID)), len(events); got != want {
		t.Errorf("event log has %d lines, want %d: replay duplicated evidence", got, want)
	}
	stored, err := f.Events(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != len(events) {
		t.Fatalf("Events = %d, want %d", len(stored), len(events))
	}
	// Events come back in canonical order regardless of arrival order.
	for i := 1; i < len(stored); i++ {
		if stored[i].Timestamp.Before(stored[i-1].Timestamp) {
			t.Fatalf("Events not chronological at %d: %v before %v", i, stored[i].Timestamp, stored[i-1].Timestamp)
		}
	}
}

func TestAppendEventRejections(t *testing.T) {
	ctx := context.Background()
	valid := demoEvents()[0]

	tests := []struct {
		name         string
		mutate       func(model.AgentEvent) model.AgentEvent
		wantNotFound bool
	}{
		{
			name:         "unknown task",
			mutate:       func(e model.AgentEvent) model.AgentEvent { e.TaskID = "task_never_created"; return e },
			wantNotFound: true,
		},
		{
			name:   "missing session id",
			mutate: func(e model.AgentEvent) model.AgentEvent { e.SessionID = ""; return e },
		},
		{
			name:   "unknown event type",
			mutate: func(e model.AgentEvent) model.AgentEvent { e.Type = "Invented"; return e },
		},
		{
			name:   "zero timestamp",
			mutate: func(e model.AgentEvent) model.AgentEvent { e.Timestamp = time.Time{}; return e },
		},
		{
			// A runtime this build does not integrate with is storable — the
			// agent vocabulary is open, so "chatgpt" is a fine identity. What
			// is still rejected is a value that is not an identifier at all,
			// because agent names reach output and map keys.
			name:   "malformed agent",
			mutate: func(e model.AgentEvent) model.AgentEvent { e.Agent = "Chat GPT!! <b>"; return e },
		},
		{
			name:   "session is its own parent",
			mutate: func(e model.AgentEvent) model.AgentEvent { e.ParentSessionID = e.SessionID; return e },
		},
		{
			name:   "session id escapes the store",
			mutate: func(e model.AgentEvent) model.AgentEvent { e.SessionID = "../../evil"; return e },
		},
		{
			name: "checkpoint id escapes the store",
			mutate: func(e model.AgentEvent) model.AgentEvent {
				e.Type = model.CheckpointCreated
				e.Attrs = map[string]string{"checkpoint_id": "../../evil"}
				return e
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			seedTask(t, f)
			err := f.AppendEvent(ctx, tc.mutate(valid))
			if err == nil {
				t.Fatal("AppendEvent = nil error, want rejection")
			}
			if got := errors.Is(err, model.ErrNotFound); got != tc.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v (err: %v)", got, tc.wantNotFound, err)
			}
			// Nothing may be half-applied: a rejected event leaves no log.
			if _, err := os.Stat(filepath.Join(f.Root(), tasksDirName, testTaskID, eventsFileName)); err == nil {
				t.Error("a rejected event still produced an event log")
			}
		})
	}
}

func TestSessionBelongsToExactlyOneTask(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)
	second := model.Task{ID: "task_second", Repo: testRepo, Branch: "other", RootSessionID: "sess_second_root"}
	if err := f.CreateTask(ctx, second); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	ev := demoEvents()[0]
	if err := f.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	ev.TaskID = second.ID
	err := f.AppendEvent(ctx, ev)
	if err == nil {
		t.Fatal("rebinding a session to a second task = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "already belongs") {
		t.Errorf("error = %v, want it to name the existing owner", err)
	}
	// The original binding is untouched, so the session still resolves home.
	got, err := f.ResolveTask(ctx, ev.SessionID, "", "")
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}
	if got.ID != testTaskID {
		t.Errorf("session resolved to %q, want %q", got.ID, testTaskID)
	}
}

func TestLineageFromEvents(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)
	for _, ev := range demoEvents() {
		if err := f.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent %s: %v", ev.Type, err)
		}
	}

	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("assembled lineage is structurally broken: %v", err)
	}
	if got, want := len(l.Sessions), 3; got != want {
		t.Fatalf("sessions = %d, want %d", got, want)
	}
	if got, want := len(l.Checkpoints), 1; got != want {
		t.Fatalf("checkpoints = %d, want %d", got, want)
	}

	main, ok := l.Session("sess_main")
	if !ok {
		t.Fatal("sess_main missing from lineage")
	}
	if !main.StartedAt.Equal(at(0)) {
		t.Errorf("sess_main StartedAt = %v, want %v", main.StartedAt, at(0))
	}
	if !main.Interrupted {
		t.Error("sess_main should be marked interrupted: its capture is incomplete, not final")
	}
	if main.Active() {
		t.Error("an interrupted session must not read as active")
	}
	if main.Agent != model.AgentOpenClaw || main.Role != model.RoleMain {
		t.Errorf("sess_main agent/role = %q/%q, want openclaw/main", main.Agent, main.Role)
	}

	child, ok := l.Session("sess_test")
	if !ok {
		t.Fatal("sess_test missing from lineage")
	}
	if child.ParentSessionID != "sess_main" {
		t.Errorf("sess_test parent = %q, want sess_main", child.ParentSessionID)
	}
	if !child.EndedAt.Equal(at(4)) {
		t.Errorf("sess_test EndedAt = %v, want %v", child.EndedAt, at(4))
	}
	if kids := l.Children("sess_main"); len(kids) != 1 || kids[0].SessionID != "sess_test" {
		t.Errorf("Children(sess_main) = %v, want one sess_test", kids)
	}

	cp, ok := l.LatestCheckpoint()
	if !ok {
		t.Fatal("no checkpoint in lineage")
	}
	if cp.CheckpointID != "cp_001" || cp.SessionID != "sess_main" {
		t.Errorf("checkpoint = %q from %q, want cp_001 from sess_main", cp.CheckpointID, cp.SessionID)
	}
	if cp.CommitSHA != "9f1c2b3" || cp.Label != "tests green" {
		t.Errorf("checkpoint detail = %q/%q, want the attributes the adapter reported", cp.CommitSHA, cp.Label)
	}
	if !cp.CreatedAt.Equal(at(5)) {
		t.Errorf("checkpoint CreatedAt = %v, want %v", cp.CreatedAt, at(5))
	}

	// Two runtimes on one task identity is the differentiator the lineage exists
	// to make visible (plan §26).
	agents := l.Agents()
	if len(agents) != 2 || agents[0] != model.AgentOpenClaw || agents[1] != model.AgentHermes {
		t.Errorf("Agents() = %v, want [openclaw hermes]", agents)
	}
}

// TestLineageFoldIsOrderIndependent guards the replay path: adapters do not
// promise ordering, so the same set of events must always produce the same
// lineage.
func TestLineageFoldIsOrderIndependent(t *testing.T) {
	ctx := context.Background()

	build := func(t *testing.T, events []model.AgentEvent) model.Lineage {
		t.Helper()
		f := newStore(t)
		seedTask(t, f)
		for _, ev := range events {
			if err := f.AppendEvent(ctx, ev); err != nil {
				t.Fatalf("AppendEvent %s: %v", ev.Type, err)
			}
		}
		l, err := f.Lineage(ctx, testTaskID)
		if err != nil {
			t.Fatalf("Lineage: %v", err)
		}
		return l
	}

	forward := demoEvents()
	reversed := demoEvents()
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}

	a, b := build(t, forward), build(t, reversed)
	if len(a.Sessions) != len(b.Sessions) || len(a.Checkpoints) != len(b.Checkpoints) {
		t.Fatalf("shapes differ: %d/%d sessions, %d/%d checkpoints",
			len(a.Sessions), len(b.Sessions), len(a.Checkpoints), len(b.Checkpoints))
	}
	for i := range a.Sessions {
		if a.Sessions[i] != b.Sessions[i] {
			t.Errorf("session %d differs:\nforward %+v\nreverse %+v", i, a.Sessions[i], b.Sessions[i])
		}
	}
	for i := range a.Checkpoints {
		if a.Checkpoints[i] != b.Checkpoints[i] {
			t.Errorf("checkpoint %d differs:\nforward %+v\nreverse %+v", i, a.Checkpoints[i], b.Checkpoints[i])
		}
	}
}

// TestCheckpointEventWithoutID covers the degradation path: the adapter said a
// checkpoint happened but not which one, so the event is kept as evidence and no
// checkpoint node is invented (plan §33, §48).
func TestCheckpointEventWithoutID(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	ev := model.AgentEvent{
		Type:      model.CheckpointCreated,
		Timestamp: at(3),
		TaskID:    testTaskID,
		SessionID: "sess_main",
		Agent:     model.AgentOpenClaw,
		Summary:   "checkpoint reported without an id",
	}
	if err := f.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, err := f.Events(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Events = %d, want the event kept as evidence", len(events))
	}
	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(l.Checkpoints) != 0 {
		t.Errorf("checkpoints = %d, want 0: a node with no id is a claim with nothing behind it", len(l.Checkpoints))
	}
	if _, ok := l.LatestCheckpoint(); ok {
		t.Error("LatestCheckpoint reported a checkpoint that was never identified")
	}
}

func TestEventsDegradationPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown task", func(t *testing.T) {
		f := newStore(t)
		if _, err := f.Events(ctx, "task_absent"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("Events(absent) error = %v, want model.ErrNotFound", err)
		}
	})

	t.Run("task with no events yet", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		got, err := f.Events(ctx, testTaskID)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("Events = %d, want 0", len(got))
		}
	})

	t.Run("an undecodable line is reported, never skipped", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		ev := demoEvents()[0]
		if err := f.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		path := filepath.Join(f.Root(), tasksDirName, testTaskID, eventsFileName)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := os.WriteFile(path, append(raw, []byte("{not json\n")...), filePerm); err != nil {
			t.Fatalf("write: %v", err)
		}

		_, err = f.Events(ctx, testTaskID)
		if err == nil {
			t.Fatal("Events = nil error, want the damaged log reported")
		}
		if !strings.Contains(err.Error(), "line 2") {
			t.Errorf("error = %v, want it to name the damaged line", err)
		}
		if errors.Is(err, model.ErrNotFound) {
			t.Errorf("corruption reported as absence: %v", err)
		}
		// Appending onto a log that cannot be read would risk duplicating events
		// the store can no longer see, so it refuses too, and refuses before it
		// has written anything.
		next := demoEvents()[2]
		if err := f.AppendEvent(ctx, next); err == nil {
			t.Fatal("AppendEvent onto a damaged log = nil error, want refusal")
		}
		if _, err := os.Stat(filepath.Join(f.Root(), indexDirName, sessionIndexName, next.SessionID)); err == nil {
			t.Errorf("a refused append still indexed session %q", next.SessionID)
		}
	})
}

// TestConcurrentAppends exercises the mutex. Adapters are hooks: several can
// fire at once inside one agent process, and the multi-file writes behind a
// single AppendEvent are only consistent if they do not interleave.
func TestConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	events := demoEvents()
	var wg sync.WaitGroup
	errs := make([]error, len(events)*2)
	for i := 0; i < len(events)*2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Every event is submitted twice, concurrently, which is what a
			// duplicated hook registration looks like in practice.
			errs[i] = f.AppendEvent(ctx, events[i%len(events)])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent AppendEvent %d: %v", i, err)
		}
	}

	got, err := f.Events(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != len(events) {
		t.Errorf("Events = %d, want %d: concurrent replay duplicated evidence", len(got), len(events))
	}
	if lines := len(eventLines(t, f, testTaskID)); lines != len(events) {
		t.Errorf("event log = %d lines, want %d", lines, len(events))
	}
	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(l.Sessions) != 3 || len(l.Checkpoints) != 1 {
		t.Errorf("lineage = %d sessions / %d checkpoints, want 3/1", len(l.Sessions), len(l.Checkpoints))
	}
}

// tiedEvents are three events that tie on every key model.SortEvents compares:
// same timestamp, same session, same type. An adapter that stamps at second
// granularity emits these routinely.
func tiedEvents() []model.AgentEvent {
	base := model.AgentEvent{
		Type: model.ToolUsed, Timestamp: at(1), TaskID: testTaskID,
		SessionID: "sess_main", Agent: model.AgentOpenClaw,
	}
	out := make([]model.AgentEvent, 0, 3)
	for _, s := range []string{"ran the tests", "edited billing.go", "read the plan"} {
		ev := base
		ev.Summary = s
		out = append(out, ev)
	}
	return out
}

// TestEventsOrderIsIndependentOfArrival is the claim Events() makes for itself:
// two agents that saw the same events must read the log the same way, whoever
// wrote first. model.SortEvents is stable and compares only timestamp, session
// and type, so without a total tiebreak the ties come back in arrival order and
// the two agents derive different state from identical evidence.
func TestEventsOrderIsIndependentOfArrival(t *testing.T) {
	ctx := context.Background()

	summaries := func(t *testing.T, evs []model.AgentEvent) []string {
		t.Helper()
		f := newStore(t)
		seedTask(t, f)
		for _, ev := range evs {
			if err := f.AppendEvent(ctx, ev); err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
		}
		got, err := f.Events(ctx, testTaskID)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		out := make([]string, 0, len(got))
		for _, e := range got {
			out = append(out, e.Summary)
		}
		return out
	}

	forward := tiedEvents()
	reversed := []model.AgentEvent{forward[2], forward[1], forward[0]}
	a, b := summaries(t, forward), summaries(t, reversed)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("got %d and %d events, want 3 each", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("Events() depends on arrival order at %d: %q vs %q", i, a[i], b[i])
		}
	}

	// The canonical ordering still governs: ties may not be reordered across
	// events that genuinely differ in timestamp.
	mixed := append(tiedEvents(), demoEvents()[0])
	got := summaries(t, mixed)
	if got[0] != "start" {
		t.Errorf("chronological order lost: first event is %q, want the at(0) event", got[0])
	}
}

// TestUnknownAgentIsRecordedAsUnknown covers the difference between an agent
// that reported it does not know its own runtime and a value that went missing.
// model.AgentEvent.Validate accepts model.AgentUnknown, so the node must carry
// it rather than an empty AgentKind that fails model.AgentKind.Valid.
func TestUnknownAgentIsRecordedAsUnknown(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	anon := model.AgentEvent{
		Type: model.SessionStarted, Timestamp: at(0), TaskID: testTaskID,
		SessionID: "sess_anon", Agent: model.AgentUnknown, Role: model.RoleMain,
	}
	if err := anon.Validate(); err != nil {
		t.Fatalf("the model accepts this event: %v", err)
	}
	if err := f.AppendEvent(ctx, anon); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	got, ok := l.Session("sess_anon")
	if !ok {
		t.Fatal("sess_anon missing from lineage")
	}
	if got.Agent != model.AgentUnknown {
		t.Errorf("Agent = %q, want %q", got.Agent, model.AgentUnknown)
	}
	if !got.Agent.Valid() {
		t.Errorf("stored Agent %q fails model.AgentKind.Valid", got.Agent)
	}

	// "Unknown" is still no information, so a later event that does know upgrades it.
	if err := f.AppendEvent(ctx, model.AgentEvent{
		Type: model.SessionEnded, Timestamp: at(5), TaskID: testTaskID,
		SessionID: "sess_anon", Agent: model.AgentHermes,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	l, err = f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if got, _ = l.Session("sess_anon"); got.Agent != model.AgentHermes {
		t.Errorf("Agent = %q, want the later event that knew to upgrade it to hermes", got.Agent)
	}

	// A checkpoint node folded from an unidentified agent gets the same treatment.
	if err := f.AppendEvent(ctx, model.AgentEvent{
		Type: model.CheckpointCreated, Timestamp: at(6), TaskID: testTaskID,
		SessionID: "sess_anon", Agent: model.AgentUnknown,
		Attrs: map[string]string{"checkpoint_id": "cp_anon"},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	l, err = f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(l.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(l.Checkpoints))
	}
	if !l.Checkpoints[0].Agent.Valid() {
		t.Errorf("checkpoint Agent = %q, which fails model.AgentKind.Valid", l.Checkpoints[0].Agent)
	}
}
