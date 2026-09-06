package derive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

const (
	fixtureTaskID = "task_0123456789abcdef"
	mainSession   = "sess_main"
	subSession    = "sess_sub"
)

// origin anchors every fixture timestamp. Fixtures never read the wall clock,
// so a failing test always means the code changed and never that the day did.
var origin = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

func at(minute int) time.Time { return origin.Add(time.Duration(minute) * time.Minute) }

// clock is a Clock that always answers the same instant. Unlike model.FixedClock
// it does not advance, which lets a test assert that the derivation reads the
// clock exactly once per run.
type clock struct {
	at    time.Time
	calls int
}

func (c *clock) Now() time.Time {
	c.calls++
	return c.at
}

func newClock() *clock { return &clock{at: at(120)} }

// fakeRepo is a scripted model.RepoInspector.
type fakeRepo struct {
	head     model.RepoState
	headErr  error
	files    []model.ChangedFile
	filesErr error
	hashes   map[string]string
	hashErr  error

	baseRefSeen string
	hashedPaths []string
}

func (r *fakeRepo) Head(context.Context) (model.RepoState, error) {
	if r.headErr != nil {
		return model.RepoState{}, r.headErr
	}
	return r.head, nil
}

func (r *fakeRepo) ChangedFiles(_ context.Context, baseRef string) ([]model.ChangedFile, error) {
	r.baseRefSeen = baseRef
	if r.filesErr != nil {
		return nil, r.filesErr
	}
	return append([]model.ChangedFile(nil), r.files...), nil
}

func (r *fakeRepo) FileHash(_ context.Context, path string) (string, error) {
	r.hashedPaths = append(r.hashedPaths, path)
	if r.hashErr != nil {
		return "", r.hashErr
	}
	h, ok := r.hashes[path]
	if !ok {
		return "", model.ErrNotFound
	}
	return h, nil
}

func (r *fakeRepo) Exists(_ context.Context, path string) bool {
	_, ok := r.hashes[path]
	return ok
}

func event(typ model.EventType, minute int, session string) model.AgentEvent {
	return model.AgentEvent{
		Type:      typ,
		Timestamp: at(minute),
		TaskID:    fixtureTaskID,
		SessionID: session,
		Agent:     model.AgentOpenClaw,
		Role:      model.RoleMain,
	}
}

func withAttrs(ev model.AgentEvent, kv ...string) model.AgentEvent {
	for i := 0; i+1 < len(kv); i += 2 {
		ev = ev.WithAttr(kv[i], kv[i+1])
	}
	return ev
}

// fixtureEvents is the reference timeline: an OpenClaw main session that
// checkpoints and is interrupted, a Hermes sub-agent that runs tests, a test
// that fails and is later fixed, and a mid-task constraint (plan §64).
func fixtureEvents() []model.AgentEvent {
	sub := func(typ model.EventType, minute int) model.AgentEvent {
		e := event(typ, minute, subSession)
		e.Agent = model.AgentHermes
		e.Role = model.RoleTest
		e.ParentSessionID = mainSession
		return e
	}
	start := event(model.SessionStarted, 0, mainSession)
	start.PayloadRef = "transcript://sess_main/0"

	return []model.AgentEvent{
		start,
		withAttrs(event(model.ToolUsed, 5, mainSession),
			attrTestName, "TestDuplicateWebhook",
			attrTestStatus, "failed",
			attrTestCommand, "go test ./billing/...",
			attrTestPackage, "billing"),
		withAttrs(event(model.CheckpointCreated, 10, mainSession),
			attrCheckpointID, "ckpt_001",
			attrCheckpointLabel, "before refactor",
			attrCommitSHA, "abc123",
			attrBranch, "feat/idempotency"),
		withAttrs(event(model.ConstraintAdded, 12, mainSession),
			attrConstraintText, "webhooks must be idempotent across retries",
			attrConstraintSource, "product owner"),
		sub(model.SubagentStarted, 15),
		withAttrs(sub(model.TurnEnded, 20),
			attrTestName, "TestDuplicateWebhook",
			attrTestStatus, "passed",
			attrTestCommand, "go test ./billing/...",
			attrTestPackage, "billing"),
		withAttrs(sub(model.TurnEnded, 21),
			attrTestName, "TestRefundReplay",
			attrTestStatus, "failed",
			attrTestCommand, "go test ./billing/...",
			attrTestPackage, "billing"),
		sub(model.SubagentEnded, 25),
		event(model.SessionInterrupted, 30, mainSession),
	}
}

func fixtureRepo() *fakeRepo {
	return &fakeRepo{
		head: model.RepoState{
			Repo:      "github.com/acme/billing",
			Branch:    "feat/idempotency",
			CommitSHA: "abc123",
			Dirty:     true,
		},
		files: []model.ChangedFile{
			{Path: "billing/webhook.go", Status: "M", Insert: 20, Delete: 3},
			{Path: "billing/legacy.go", Status: "D"},
		},
		hashes: map[string]string{
			"billing/webhook.go": "deadbeef",
		},
	}
}

func fixtureInput(repo model.RepoInspector) Input {
	return Input{
		Task:   model.TaskRef{Title: "make webhook handling idempotent"},
		TaskID: fixtureTaskID,
		Events: fixtureEvents(),
		Repo:   repo,
		Clock:  newClock(),
	}
}

func mustState(t *testing.T, in Input) *model.EngineeringState {
	t.Helper()
	s, err := State(context.Background(), in)
	if err != nil {
		t.Fatalf("State() returned error: %v", err)
	}
	return s
}

func TestStateFromFixture(t *testing.T) {
	repo := fixtureRepo()
	in := fixtureInput(repo)
	in.BaseRef = "main"
	s := mustState(t, in)

	if s.SchemaVersion != model.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", s.SchemaVersion, model.SchemaVersion)
	}
	if s.Task.ID != fixtureTaskID {
		t.Errorf("Task.ID = %q, want it filled from Input.TaskID", s.Task.ID)
	}
	if repo.baseRefSeen != "main" {
		t.Errorf("ChangedFiles base ref = %q, want %q", repo.baseRefSeen, "main")
	}
	if s.GeneratedAt != at(120) {
		t.Errorf("GeneratedAt = %v, want the clock instant %v", s.GeneratedAt, at(120))
	}

	// Sessions: the interrupted main session plus its Hermes sub-agent.
	if got, want := len(s.Sessions), 2; got != want {
		t.Fatalf("len(Sessions) = %d, want %d", got, want)
	}
	main, sub := s.Sessions[0], s.Sessions[1]
	if main.SessionID != mainSession || sub.SessionID != subSession {
		t.Fatalf("sessions = %q,%q; want main first (chronological)", main.SessionID, sub.SessionID)
	}
	if !main.Interrupted || !main.EndedAt.IsZero() {
		t.Errorf("main session = %+v, want interrupted with no clean end", main)
	}
	if main.Active() {
		t.Error("interrupted session reported Active(); its state is not final")
	}
	if sub.ParentSessionID != mainSession || sub.Agent != model.AgentHermes || sub.Role != model.RoleTest {
		t.Errorf("sub-agent session = %+v, want parented Hermes test session", sub)
	}
	if sub.EndedAt != at(25) {
		t.Errorf("sub-agent EndedAt = %v, want %v", sub.EndedAt, at(25))
	}

	// Checkpoints.
	if got, want := len(s.Checkpoints), 1; got != want {
		t.Fatalf("len(Checkpoints) = %d, want %d", got, want)
	}
	cp := s.Checkpoints[0]
	if cp.CheckpointID != "ckpt_001" || cp.Label != "before refactor" || cp.CommitSHA != "abc123" {
		t.Errorf("checkpoint = %+v, want the attributes carried on the event", cp)
	}

	// Tests: the later pass supersedes the earlier failure, the other stays failed.
	if got, want := len(s.Tests), 2; got != want {
		t.Fatalf("len(Tests) = %d, want %d", got, want)
	}
	webhook, ok := s.Test("TestDuplicateWebhook")
	if !ok {
		t.Fatal("TestDuplicateWebhook missing from state")
	}
	if webhook.Status != model.TestPassed {
		t.Errorf("TestDuplicateWebhook status = %q, want %q (later result supersedes)", webhook.Status, model.TestPassed)
	}
	if len(webhook.Evidence) != 2 {
		t.Errorf("TestDuplicateWebhook evidence = %d records, want both runs retained", len(webhook.Evidence))
	}
	if webhook.Package != "billing" || webhook.Command != "go test ./billing/..." {
		t.Errorf("TestDuplicateWebhook = %+v, want command and package recorded", webhook)
	}
	if failing := s.FailingTests(); len(failing) != 1 || failing[0].Name != "TestRefundReplay" {
		t.Errorf("FailingTests() = %+v, want only TestRefundReplay", failing)
	}

	// Constraints.
	if got, want := len(s.Constraints), 1; got != want {
		t.Fatalf("len(Constraints) = %d, want %d", got, want)
	}
	c := s.Constraints[0]
	if c.ID != "C1" || c.Source != "product owner" || c.AddedAt != at(12) {
		t.Errorf("constraint = %+v, want C1 from the product owner at %v", c, at(12))
	}
	if len(c.Evidence) != 1 || c.Evidence[0].Kind != model.EvidenceEvent {
		t.Errorf("constraint evidence = %+v, want a Level 3 event citation", c.Evidence)
	}

	// Repository facts.
	if s.Repo.CommitSHA != "abc123" || s.Repo.Branch != "feat/idempotency" || !s.Repo.Dirty {
		t.Errorf("Repo = %+v, want the inspector's head verbatim", s.Repo)
	}
	if s.Repo.ObservedAt != at(120) {
		t.Errorf("Repo.ObservedAt = %v, want the clock instant", s.Repo.ObservedAt)
	}
	if got, want := len(s.ChangedFiles), 2; got != want {
		t.Fatalf("len(ChangedFiles) = %d, want %d", got, want)
	}
	// Sorted by path: legacy.go then webhook.go.
	deleted, modified := s.ChangedFiles[0], s.ChangedFiles[1]
	if deleted.Path != "billing/legacy.go" || modified.Path != "billing/webhook.go" {
		t.Fatalf("changed files = %q,%q; want path order", deleted.Path, modified.Path)
	}
	if got := fileHashDetail(modified); got != "sha256:deadbeef" {
		t.Errorf("modified file hash detail = %q, want the sha256-prefixed contract value", got)
	}
	if got := fileHashDetail(deleted); got != "" {
		t.Errorf("deleted file hash detail = %q, want none: a deleted file has no content", got)
	}
	if !reflect.DeepEqual(repo.hashedPaths, []string{"billing/webhook.go"}) {
		t.Errorf("hashed paths = %v, want only the surviving file: a deletion has nothing to hash", repo.hashedPaths)
	}
	// A deletion must not count as a missing hash, or every deleted file would
	// read as degraded drift coverage.
	if hasNote(s.Capture.Notes, "content hashes unavailable") {
		t.Errorf("notes = %v, want no hash gap reported", s.Capture.Notes)
	}

	// A single failing test with no requirements is still only "active"; this
	// package never claims progress it cannot prove.
	if s.Status != model.StatusActive {
		t.Errorf("Status = %q, want %q", s.Status, model.StatusActive)
	}
}

// fileHashDetail returns the content fingerprint recorded on a changed file, or
// "" when none was available.
func fileHashDetail(f model.ChangedFile) string {
	for _, e := range f.Evidence {
		if e.Kind == model.EvidenceFile && strings.HasPrefix(e.Detail, hashPrefix) {
			return e.Detail
		}
	}
	return ""
}

func TestStateProducesNoSemanticClaims(t *testing.T) {
	s := mustState(t, fixtureInput(fixtureRepo()))

	// Plan §30: interpretation belongs to the semantic layer. If this package
	// ever starts filling these, the fact/inference boundary has been lost.
	checks := []struct {
		name string
		n    int
	}{
		{"Requirements", len(s.Requirements)},
		{"Decisions", len(s.Decisions)},
		{"Rejected", len(s.Rejected)},
		{"Assumptions", len(s.Assumptions)},
		{"NextActions", len(s.NextActions)},
		{"CompletedWork", len(s.CompletedWork)},
		{"InProgress", len(s.InProgress)},
		{"Risks", len(s.Risks)},
		{"FailedAttempts", len(s.FailedAttempts)},
	}
	for _, c := range checks {
		if c.n != 0 {
			t.Errorf("%s = %d entries, want 0: derive must not interpret", c.name, c.n)
		}
	}
	if s.Capture.SemanticExtraction {
		t.Error("Capture.SemanticExtraction = true, but no extraction ran")
	}
	if s.Capture.GraphAvailable {
		t.Error("Capture.GraphAvailable = true, but Graph was never consulted")
	}
}

func TestStateEvidenceLevels(t *testing.T) {
	s := mustState(t, fixtureInput(fixtureRepo()))

	// Collected from the containers rather than from AllEvidence, which does not
	// walk ChangedFiles or Constraints (see contract note in the package report).
	all := s.AllEvidence()
	for _, f := range s.ChangedFiles {
		all = append(all, f.Evidence...)
	}
	for _, c := range s.Constraints {
		all = append(all, c.Evidence...)
	}

	kinds := map[model.EvidenceKind]int{}
	for _, e := range all {
		kinds[e.Kind]++
		if e.Ref == "" {
			t.Errorf("evidence %+v has no Ref; an uncitable citation is not evidence", e)
		}
	}
	for _, want := range []model.EvidenceKind{
		model.EvidenceCommit, model.EvidenceFile, model.EvidenceTest,
		model.EvidenceCheckpoint, model.EvidenceSession, model.EvidenceEvent,
	} {
		if kinds[want] == 0 {
			t.Errorf("no %s evidence produced", want)
		}
	}
	if kinds[model.EvidenceInference] != 0 {
		t.Error("derive produced inference evidence; every fact here is observed")
	}
}

func TestStateCaptureDegradation(t *testing.T) {
	unavailable := fmt.Errorf("git: %w", model.ErrUnavailable)

	tests := []struct {
		name        string
		mutate      func(*Input)
		wantMissing []string
		wantGit     bool
		wantNote    string
	}{
		{
			name:        "everything available",
			mutate:      func(*Input) {},
			wantMissing: nil,
			wantGit:     true,
		},
		{
			name:        "nil repo degrades instead of failing",
			mutate:      func(in *Input) { in.Repo = nil },
			wantMissing: []string{missingGit},
			wantGit:     false,
			wantNote:    "no repository inspector was supplied",
		},
		{
			name: "unavailable repo degrades instead of failing",
			mutate: func(in *Input) {
				in.Repo = &fakeRepo{headErr: unavailable}
			},
			wantMissing: []string{missingGit},
			wantGit:     false,
			wantNote:    "repository head unavailable",
		},
		{
			name: "head succeeds but diff fails: git still available",
			mutate: func(in *Input) {
				r := fixtureRepo()
				r.filesErr = unavailable
				in.Repo = r
			},
			wantMissing: nil,
			wantGit:     true,
			wantNote:    "changed files unavailable",
		},
		{
			name: "repo failure that is not ErrUnavailable is still not fatal",
			mutate: func(in *Input) {
				in.Repo = &fakeRepo{headErr: errors.New("exec: git not found")}
			},
			wantMissing: []string{missingGit},
			wantGit:     false,
			wantNote:    "repository head could not be read: exec: git not found",
		},
		{
			name:   "no events at all",
			mutate: func(in *Input) { in.Events = nil },
			// Capture.Missing is sorted by EngineeringState.Sort.
			wantMissing: []string{missingEvents, missingCheckpoints, missingTests, missingTranscript},
			wantGit:     true,
		},
		{
			name: "malformed events are dropped and reported",
			mutate: func(in *Input) {
				in.Events = append(in.Events, model.AgentEvent{Type: model.ToolUsed})
			},
			wantMissing: nil,
			wantGit:     true,
			wantNote:    "ignored 1 malformed agent event(s)",
		},
		{
			name: "events from another task are dropped and reported",
			mutate: func(in *Input) {
				other := event(model.SessionStarted, 1, "sess_other")
				other.TaskID = "task_someone_else"
				in.Events = append(in.Events, other)
			},
			wantMissing: nil,
			wantGit:     true,
			wantNote:    "ignored 1 agent event(s) belonging to another task",
		},
		{
			name: "checkpoint event without an id cannot be resumed from",
			mutate: func(in *Input) {
				in.Events = []model.AgentEvent{
					event(model.SessionStarted, 0, mainSession),
					event(model.CheckpointCreated, 1, mainSession),
				}
			},
			wantMissing: []string{missingCheckpoints, missingTests, missingTranscript},
			wantGit:     true,
			wantNote:    "carried no " + attrCheckpointID,
		},
		{
			name: "file hashes unavailable degrade drift detection",
			mutate: func(in *Input) {
				r := fixtureRepo()
				r.hashErr = model.ErrUnavailable
				in.Repo = r
			},
			wantMissing: nil,
			wantGit:     true,
			wantNote:    "content hashes unavailable for 1 of 1 changed file(s)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := fixtureInput(fixtureRepo())
			tc.mutate(&in)

			s, err := State(context.Background(), in)
			if err != nil {
				t.Fatalf("State() returned error %v; capture gaps must never be fatal (Rule 8)", err)
			}
			if s.Capture.GitAvailable != tc.wantGit {
				t.Errorf("Capture.GitAvailable = %v, want %v", s.Capture.GitAvailable, tc.wantGit)
			}
			if !reflect.DeepEqual(s.Missing(), tc.wantMissing) {
				t.Errorf("Capture.Missing = %v, want %v", s.Missing(), tc.wantMissing)
			}
			if tc.wantNote != "" && !hasNote(s.Capture.Notes, tc.wantNote) {
				t.Errorf("Capture.Notes = %v, want one containing %q", s.Capture.Notes, tc.wantNote)
			}
			// Capture.Complete is what the renderer keys off to decide whether it
			// may present the state as whole; it must track Missing exactly.
			if want := len(tc.wantMissing) == 0; s.Capture.Complete() != want {
				t.Errorf("Capture.Complete() = %v, want %v (%+v)", s.Capture.Complete(), want, s.Capture)
			}
		})
	}
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

func TestDeriveSessions(t *testing.T) {
	tests := []struct {
		name      string
		events    []model.AgentEvent
		want      []model.SessionNode
		wantNotes []string
	}{
		{
			name: "clean start and end",
			events: []model.AgentEvent{
				event(model.SessionStarted, 0, mainSession),
				event(model.SessionEnded, 5, mainSession),
			},
			want: []model.SessionNode{{
				SessionID: mainSession, Agent: model.AgentOpenClaw, Role: model.RoleMain,
				StartedAt: at(0), EndedAt: at(5),
			}},
		},
		{
			name: "interrupted session has no end time",
			events: []model.AgentEvent{
				event(model.SessionStarted, 0, mainSession),
				event(model.SessionInterrupted, 5, mainSession),
			},
			want: []model.SessionNode{{
				SessionID: mainSession, Agent: model.AgentOpenClaw, Role: model.RoleMain,
				StartedAt: at(0), Interrupted: true,
			}},
		},
		{
			name: "session observed without a start event is kept and reported",
			events: []model.AgentEvent{
				event(model.ToolUsed, 3, mainSession),
				event(model.SessionEnded, 4, mainSession),
			},
			want: []model.SessionNode{{
				SessionID: mainSession, Agent: model.AgentOpenClaw, Role: model.RoleMain,
				StartedAt: at(3), EndedAt: at(4),
			}},
			wantNotes: []string{
				`session "sess_main" was observed without a start event; its lineage is incomplete`,
			},
		},
		{
			name: "sub-agent becomes a parented session",
			events: func() []model.AgentEvent {
				child := event(model.SubagentStarted, 2, subSession)
				child.Agent = model.AgentHermes
				child.Role = model.RoleResearch
				child.ParentSessionID = mainSession
				done := event(model.SubagentEnded, 6, subSession)
				done.Agent = model.AgentHermes
				done.Role = model.RoleResearch
				done.ParentSessionID = mainSession
				return []model.AgentEvent{event(model.SessionStarted, 0, mainSession), child, done}
			}(),
			want: []model.SessionNode{
				{SessionID: mainSession, Agent: model.AgentOpenClaw, Role: model.RoleMain, StartedAt: at(0)},
				{
					SessionID: subSession, ParentSessionID: mainSession, Agent: model.AgentHermes,
					Role: model.RoleResearch, StartedAt: at(2), EndedAt: at(6),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := deriveSessions(tc.events)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deriveSessions() sessions =\n%+v\nwant\n%+v", got, tc.want)
			}
			if len(tc.wantNotes) == 0 && len(notes) != 0 {
				t.Errorf("deriveSessions() notes = %v, want none", notes)
			}
			for _, want := range tc.wantNotes {
				if !hasNote(notes, want) {
					t.Errorf("deriveSessions() notes = %v, want one containing %q", notes, want)
				}
			}
		})
	}
}

func TestDeriveTests(t *testing.T) {
	result := func(minute int, name, status string) model.AgentEvent {
		return withAttrs(event(model.ToolUsed, minute, mainSession),
			attrTestName, name, attrTestStatus, status, attrTestCommand, "go test ./...")
	}

	tests := []struct {
		name       string
		events     []model.AgentEvent
		wantStatus map[string]model.TestStatus
		wantCount  int
		wantNote   string
	}{
		{
			name:       "status synonyms are accepted",
			events:     []model.AgentEvent{result(1, "A", "PASS"), result(2, "B", "fail"), result(3, "C", "skip")},
			wantStatus: map[string]model.TestStatus{"A": model.TestPassed, "B": model.TestFailed, "C": model.TestSkipped},
			wantCount:  3,
		},
		{
			name:       "unrecognised status is unknown, never passed",
			events:     []model.AgentEvent{result(1, "A", "probably fine")},
			wantStatus: map[string]model.TestStatus{"A": model.TestUnknown},
			wantCount:  1,
			wantNote:   "carried no recognised test_status",
		},
		{
			name:       "a later pass supersedes an earlier failure",
			events:     []model.AgentEvent{result(1, "A", "failed"), result(9, "A", "passed")},
			wantStatus: map[string]model.TestStatus{"A": model.TestPassed},
			wantCount:  1,
		},
		{
			name:       "a later unknown never clears a failure",
			events:     []model.AgentEvent{result(1, "A", "failed"), result(9, "A", "")},
			wantStatus: map[string]model.TestStatus{"A": model.TestFailed},
			wantCount:  1,
			wantNote:   "carried no recognised test_status",
		},
		{
			name:       "a later failure overrides an earlier pass",
			events:     []model.AgentEvent{result(1, "A", "passed"), result(9, "A", "failed")},
			wantStatus: map[string]model.TestStatus{"A": model.TestFailed},
			wantCount:  1,
		},
		{
			name: "TurnEnded carries results too",
			events: []model.AgentEvent{withAttrs(event(model.TurnEnded, 1, mainSession),
				attrTestName, "A", attrTestStatus, "passed")},
			wantStatus: map[string]model.TestStatus{"A": model.TestPassed},
			wantCount:  1,
		},
		{
			name: "events that are not tool or turn events are ignored",
			events: []model.AgentEvent{withAttrs(event(model.SessionEnded, 1, mainSession),
				attrTestName, "A", attrTestStatus, "passed")},
			wantCount: 0,
		},
		{
			name: "a test command with no test name yields no result",
			events: []model.AgentEvent{withAttrs(event(model.ToolUsed, 1, mainSession),
				attrTestCommand, "go test ./...")},
			wantCount: 0,
			wantNote:  "reported a test command with no test_name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := deriveTests(tc.events)
			if len(got) != tc.wantCount {
				t.Fatalf("deriveTests() = %d results, want %d (%+v)", len(got), tc.wantCount, got)
			}
			for _, r := range got {
				want, ok := tc.wantStatus[r.Name]
				if !ok {
					t.Errorf("unexpected test %q in result", r.Name)
					continue
				}
				if r.Status != want {
					t.Errorf("test %q status = %q, want %q", r.Name, r.Status, want)
				}
				if len(r.Evidence) == 0 {
					t.Errorf("test %q carries no evidence", r.Name)
				}
			}
			if tc.wantNote != "" && !hasNote(notes, tc.wantNote) {
				t.Errorf("deriveTests() notes = %v, want one containing %q", notes, tc.wantNote)
			}
			if tc.wantNote == "" && len(notes) != 0 {
				t.Errorf("deriveTests() notes = %v, want none", notes)
			}
		})
	}
}

func TestDeriveTestsRetainsSupersededEvidence(t *testing.T) {
	events := []model.AgentEvent{
		withAttrs(event(model.ToolUsed, 1, mainSession), attrTestName, "A", attrTestStatus, "failed"),
		withAttrs(event(model.ToolUsed, 9, mainSession), attrTestName, "A", attrTestStatus, "passed"),
	}
	got, _ := deriveTests(events)
	if len(got) != 1 {
		t.Fatalf("deriveTests() = %d results, want 1", len(got))
	}
	if n := len(got[0].Evidence); n != 2 {
		t.Fatalf("evidence = %d records, want both runs kept for traceability", n)
	}
	var sawFailure bool
	for _, e := range got[0].Evidence {
		if e.Result == string(model.TestFailed) {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("the superseded failure was erased; history must stay citable (plan §32)")
	}
}

func TestDeriveConstraints(t *testing.T) {
	constraint := func(minute int, kv ...string) model.AgentEvent {
		return withAttrs(event(model.ConstraintAdded, minute, mainSession), kv...)
	}

	tests := []struct {
		name       string
		events     []model.AgentEvent
		wantIDs    []string
		wantText   []string
		wantSource []string
		wantNote   string
	}{
		{
			name: "ids are generated in stream order when absent",
			events: []model.AgentEvent{
				constraint(1, attrConstraintText, "first", attrConstraintSource, "human"),
				constraint(2, attrConstraintText, "second", attrConstraintSource, "human"),
			},
			wantIDs:    []string{"C1", "C2"},
			wantText:   []string{"first", "second"},
			wantSource: []string{"human", "human"},
		},
		{
			name: "supplied ids are normalised and never collide with generated ones",
			events: []model.AgentEvent{
				constraint(1, attrConstraintText, "generated"),
				constraint(2, attrConstraintID, " c1 ", attrConstraintText, "explicit"),
			},
			wantIDs:  []string{"C2", "C1"},
			wantText: []string{"generated", "explicit"},
			wantSource: []string{
				"session:" + mainSession,
				"session:" + mainSession,
			},
		},
		{
			name: "summary is used when no constraint text attribute is present",
			events: []model.AgentEvent{
				func() model.AgentEvent {
					e := constraint(1)
					e.Summary = "must support partial refunds"
					return e
				}(),
			},
			wantIDs:    []string{"C1"},
			wantText:   []string{"must support partial refunds"},
			wantSource: []string{"session:" + mainSession},
		},
		{
			name:       "a constraint with no text is kept and reported, never dropped",
			events:     []model.AgentEvent{constraint(1)},
			wantIDs:    []string{"C1"},
			wantText:   []string{""},
			wantSource: []string{"session:" + mainSession},
			wantNote:   "constraint C1 was recorded without text",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := deriveConstraints(tc.events)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("deriveConstraints() = %d constraints, want %d", len(got), len(tc.wantIDs))
			}
			for i, c := range got {
				if c.ID != tc.wantIDs[i] {
					t.Errorf("constraint[%d].ID = %q, want %q", i, c.ID, tc.wantIDs[i])
				}
				if c.Text != tc.wantText[i] {
					t.Errorf("constraint[%d].Text = %q, want %q", i, c.Text, tc.wantText[i])
				}
				if c.Source != tc.wantSource[i] {
					t.Errorf("constraint[%d].Source = %q, want %q", i, c.Source, tc.wantSource[i])
				}
				if len(c.Evidence) == 0 {
					t.Errorf("constraint[%d] carries no evidence", i)
				}
			}
			if tc.wantNote != "" && !hasNote(notes, tc.wantNote) {
				t.Errorf("deriveConstraints() notes = %v, want one containing %q", notes, tc.wantNote)
			}
			if tc.wantNote == "" && len(notes) != 0 {
				t.Errorf("deriveConstraints() notes = %v, want none", notes)
			}
		})
	}
}

func TestDeriveCheckpoints(t *testing.T) {
	full := withAttrs(event(model.CheckpointCreated, 1, mainSession),
		attrCheckpointID, "ckpt_a", attrCommitSHA, "sha1", attrBranch, "main", attrStateHash, "sha256:xyz")
	labelled := withAttrs(event(model.CheckpointCreated, 2, mainSession), attrCheckpointID, "ckpt_b")
	labelled.Summary = "summary becomes the label"
	duplicate := withAttrs(event(model.CheckpointCreated, 3, mainSession), attrCheckpointID, "ckpt_a")

	got, notes := deriveCheckpoints([]model.AgentEvent{
		full, labelled, duplicate,
		event(model.CheckpointCreated, 4, mainSession), // no checkpoint_id
	})

	if len(got) != 2 {
		t.Fatalf("deriveCheckpoints() = %d checkpoints, want 2 (%+v)", len(got), got)
	}
	if got[0].CheckpointID != "ckpt_a" || got[0].StateHash != "sha256:xyz" || got[0].CommitSHA != "sha1" {
		t.Errorf("checkpoint[0] = %+v, want the full attribute set", got[0])
	}
	if got[0].CreatedAt != at(1) {
		t.Errorf("duplicate id overwrote the first sighting: CreatedAt = %v", got[0].CreatedAt)
	}
	if got[1].Label != "summary becomes the label" {
		t.Errorf("checkpoint[1].Label = %q, want the event summary as fallback", got[1].Label)
	}
	if !hasNote(notes, "carried no checkpoint_id") {
		t.Errorf("notes = %v, want the unidentified checkpoint reported", notes)
	}
}

func TestStateIsDeterministic(t *testing.T) {
	// Same inputs, same clock: byte-identical serialization.
	first, err := json.Marshal(mustState(t, fixtureInput(fixtureRepo())))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	shuffled := fixtureInput(fixtureRepo())
	shuffled.Events = reverse(shuffled.Events)
	second, err := json.Marshal(mustState(t, shuffled))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("two runs over the same events in different order serialized differently:\n%s\n%s", first, second)
	}

	// Different clocks: the state hash must still match, because the hash
	// fingerprints the engineering conclusion and not the moment it was read
	// (see model.EngineeringState.Hash). Anything else makes the handoff
	// receipt state_hash meaningless.
	later := fixtureInput(fixtureRepo())
	later.Clock = &clock{at: at(9999)}
	a := mustState(t, fixtureInput(fixtureRepo()))
	b := mustState(t, later)
	if a.Hash() != b.Hash() {
		t.Errorf("Hash() differs across observation times: %s vs %s", a.Hash(), b.Hash())
	}
	if a.Hash() == "" {
		t.Error("Hash() is empty")
	}
}

func TestStateReadsTheClockExactlyOnce(t *testing.T) {
	c := newClock()
	in := fixtureInput(fixtureRepo())
	in.Clock = c
	if _, err := State(context.Background(), in); err != nil {
		t.Fatalf("State() returned error: %v", err)
	}
	if c.calls != 1 {
		t.Errorf("Clock.Now() called %d times, want exactly 1 observation instant", c.calls)
	}
}

func TestStateDoesNotMutateCallerEvents(t *testing.T) {
	in := fixtureInput(fixtureRepo())
	in.Events = reverse(in.Events)
	before := append([]model.AgentEvent(nil), in.Events...)

	if _, err := State(context.Background(), in); err != nil {
		t.Fatalf("State() returned error: %v", err)
	}
	for i := range before {
		if !before[i].Timestamp.Equal(in.Events[i].Timestamp) || before[i].Type != in.Events[i].Type {
			t.Fatalf("caller's event slice was reordered at index %d", i)
		}
	}
}

func TestStateInputErrors(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		in      Input
		wantErr string
	}{
		{
			name:    "missing clock is a caller bug, not a capture gap",
			ctx:     context.Background(),
			in:      Input{TaskID: fixtureTaskID},
			wantErr: "model.Clock is required",
		},
		{
			name:    "cancelled context",
			ctx:     cancelled,
			in:      fixtureInput(fixtureRepo()),
			wantErr: context.Canceled.Error(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := State(tc.ctx, tc.in)
			if err == nil {
				t.Fatalf("State() = %+v, nil; want an error", s)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("State() error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestStateCancelledMidRepoIsNotReportedAsAnOutage guards the honesty boundary
// between "git is unavailable" and "the caller gave up".
func TestStateCancelledMidRepoIsNotReportedAsAnOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := fixtureInput(&cancellingRepo{cancel: cancel})

	if _, err := State(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("State() error = %v, want context.Canceled rather than a degraded state", err)
	}
}

// cancellingRepo cancels the context and then fails, the way a real inspector
// behaves when the caller walks away mid-call.
type cancellingRepo struct {
	cancel context.CancelFunc
}

func (r *cancellingRepo) Head(context.Context) (model.RepoState, error) {
	r.cancel()
	return model.RepoState{}, errors.New("context canceled")
}

func (r *cancellingRepo) ChangedFiles(context.Context, string) ([]model.ChangedFile, error) {
	return nil, nil
}
func (r *cancellingRepo) FileHash(context.Context, string) (string, error) { return "", nil }
func (r *cancellingRepo) Exists(context.Context, string) bool              { return false }

func TestNormalizeHash(t *testing.T) {
	tests := []struct{ in, want string }{
		{"deadbeef", "sha256:deadbeef"},
		{"sha256:deadbeef", "sha256:deadbeef"},
		{"  deadbeef  ", "sha256:deadbeef"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range tests {
		if got := normalizeHash(tc.in); got != tc.want {
			t.Errorf("normalizeHash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestChangedFileWithoutPathIsDropped(t *testing.T) {
	repo := fixtureRepo()
	repo.files = append(repo.files, model.ChangedFile{Path: "  ", Status: "M"}, model.ChangedFile{Path: "x.go"})
	in := fixtureInput(repo)

	s := mustState(t, in)
	for _, f := range s.ChangedFiles {
		if strings.TrimSpace(f.Path) == "" {
			t.Fatalf("a pathless changed file survived: %+v", f)
		}
	}
	if !hasNote(s.Capture.Notes, "reported without a path") {
		t.Errorf("notes = %v, want the dropped file reported", s.Capture.Notes)
	}
	// A file the inspector gave no status for is marked unknown, not silently
	// treated as unchanged.
	found, ok := changedFile(s, "x.go")
	if !ok {
		t.Fatal("x.go missing from changed files")
	}
	if found.Status != "?" {
		t.Errorf("x.go status = %q, want %q", found.Status, "?")
	}
}

func changedFile(s *model.EngineeringState, path string) (model.ChangedFile, bool) {
	for _, f := range s.ChangedFiles {
		if f.Path == path {
			return f, true
		}
	}
	return model.ChangedFile{}, false
}

func reverse(in []model.AgentEvent) []model.AgentEvent {
	out := make([]model.AgentEvent, len(in))
	for i, ev := range in {
		out[len(in)-1-i] = ev
	}
	return out
}

// TestSelectEventsKeepsEventsThatShareAModelKey guards against the event stream
// being thinned by a lossy identity. model.AgentEvent.Key covers type, session,
// payload ref, timestamp and summary but not Attrs, so two results reported in
// the same instant look identical to it. De-duplicating on that key silently
// deletes one of them, which is the failure mode this package exists to prevent.
func TestSelectEventsKeepsEventsThatShareAModelKey(t *testing.T) {
	a := withAttrs(event(model.ToolUsed, 5, mainSession),
		attrTestName, "TestAlpha", attrTestStatus, "failed")
	b := withAttrs(event(model.ToolUsed, 5, mainSession),
		attrTestName, "TestBeta", attrTestStatus, "failed")
	if a.Key() != b.Key() {
		t.Fatalf("fixture no longer exercises the collision: %q vs %q", a.Key(), b.Key())
	}

	s := mustState(t, Input{TaskID: fixtureTaskID, Events: []model.AgentEvent{a, b}, Clock: newClock()})
	if got, want := len(s.Tests), 2; got != want {
		t.Fatalf("len(Tests) = %d, want %d: a distinct result was dropped as a replay (%+v)", got, want, s.Tests)
	}
	if failing := s.FailingTests(); len(failing) != 2 {
		t.Errorf("FailingTests() = %d, want 2; a failure vanished", len(failing))
	}

	// A byte-identical repeat is a replay and carries nothing new, so it is
	// still collapsed.
	replayed := mustState(t, Input{TaskID: fixtureTaskID, Events: []model.AgentEvent{a, a}, Clock: newClock()})
	if got := len(replayed.Tests); got != 1 {
		t.Errorf("len(Tests) = %d, want 1: an exact replay must still collapse", got)
	}
}

// TestStateIsDeterministicWhenTimestampsTie covers the ordering model.SortEvents
// leaves to the caller's slice order. Constraint identifiers are assigned by
// position in the stream, so an unbroken tie makes the same inputs produce a
// different Hash depending on how the caller stacked the slice.
func TestStateIsDeterministicWhenTimestampsTie(t *testing.T) {
	tied := func() []model.AgentEvent {
		return []model.AgentEvent{
			withAttrs(event(model.ConstraintAdded, 5, mainSession), attrConstraintText, "alpha"),
			withAttrs(event(model.ConstraintAdded, 5, mainSession), attrConstraintText, "beta"),
			withAttrs(event(model.ToolUsed, 5, mainSession), attrTestName, "A", attrTestStatus, "passed"),
			withAttrs(event(model.ToolUsed, 5, mainSession), attrTestName, "B", attrTestStatus, "failed"),
		}
	}
	forward := mustState(t, Input{TaskID: fixtureTaskID, Events: tied(), Clock: newClock()})
	backward := mustState(t, Input{TaskID: fixtureTaskID, Events: reverse(tied()), Clock: newClock()})

	if len(forward.Constraints) != 2 || len(forward.Tests) != 2 {
		t.Fatalf("tied events were collapsed: %d constraints, %d tests", len(forward.Constraints), len(forward.Tests))
	}
	first, err := json.Marshal(forward)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(backward)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("input order changed the output for identically timed events:\n%s\n%s", first, second)
	}
	if forward.Hash() != backward.Hash() {
		t.Errorf("Hash() = %s vs %s; identical inputs must hash identically", forward.Hash(), backward.Hash())
	}
}

// TestDeriveTestsSupersessionRequiresAConclusiveRun is the honesty rule behind
// plan section 32: only a run that actually reached a verdict may replace an
// earlier one. A skip means the test did not run, which proves nothing about a
// recorded failure.
func TestDeriveTestsSupersessionRequiresAConclusiveRun(t *testing.T) {
	result := func(minute int, status string) model.AgentEvent {
		return withAttrs(event(model.ToolUsed, minute, mainSession),
			attrTestName, "A", attrTestStatus, status)
	}

	tests := []struct {
		name       string
		first      string
		second     string
		want       model.TestStatus
		wantNote   bool
		wantFailed bool
	}{
		{name: "skip does not clear a failure", first: "failed", second: "skipped",
			want: model.TestFailed, wantNote: true, wantFailed: true},
		{name: "unknown does not clear a failure", first: "failed", second: "nonsense",
			want: model.TestFailed, wantNote: true, wantFailed: true},
		{name: "a pass clears a failure", first: "failed", second: "passed",
			want: model.TestPassed},
		{name: "a failure overrides a pass", first: "passed", second: "failed",
			want: model.TestFailed, wantFailed: true},
		{name: "skip does not clear a pass", first: "passed", second: "skipped",
			want: model.TestPassed},
		{name: "a skip is still better than nothing known", first: "nonsense", second: "skipped",
			want: model.TestSkipped},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := []model.AgentEvent{result(1, tc.first), result(9, tc.second)}
			got, notes := deriveTests(events)
			if len(got) != 1 {
				t.Fatalf("deriveTests() = %d results, want 1", len(got))
			}
			if got[0].Status != tc.want {
				t.Errorf("status = %q, want %q", got[0].Status, tc.want)
			}
			// Both runs stay citable either way (plan section 32).
			if n := len(got[0].Evidence); n != 2 {
				t.Errorf("evidence = %d records, want both runs retained", n)
			}
			if has := hasNote(notes, "does not prove the recorded failure was resolved"); has != tc.wantNote {
				t.Errorf("retained-failure note present = %v, want %v (%v)", has, tc.wantNote, notes)
			}

			// The state-level consequence: FailingTests is what a handoff counts
			// as a critical failure, so a wrongly cleared failure is invisible.
			s := mustState(t, Input{TaskID: fixtureTaskID, Events: events, Clock: newClock()})
			if failing := len(s.FailingTests()); (failing > 0) != tc.wantFailed {
				t.Errorf("FailingTests() = %d, want failing = %v", failing, tc.wantFailed)
			}
		})
	}
}

// TestDeriveTestsReportsRetainedFailureOnlyOnce keeps the note from multiplying
// when an adapter reports the same inconclusive run repeatedly.
func TestDeriveTestsReportsRetainedFailureOnlyOnce(t *testing.T) {
	result := func(minute int, status string) model.AgentEvent {
		return withAttrs(event(model.ToolUsed, minute, mainSession),
			attrTestName, "A", attrTestStatus, status)
	}
	_, notes := deriveTests([]model.AgentEvent{
		result(1, "failed"), result(2, "skipped"), result(3, "skipped"), result(4, "skipped"),
	})
	var n int
	for _, note := range notes {
		if strings.Contains(note, "does not prove the recorded failure was resolved") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("retained-failure note appeared %d times, want 1 (%v)", n, notes)
	}
}

// TestStateScopesEventsByTaskRefID covers a caller who identified the task in
// Input.Task instead of Input.TaskID. Accepting every event then would splice
// another task's sessions into this task's timeline and present the result as
// its history.
func TestStateScopesEventsByTaskRefID(t *testing.T) {
	intruder := event(model.SessionStarted, 1, "sess_intruder")
	intruder.TaskID = "task_someone_else"

	s := mustState(t, Input{
		Task:   model.TaskRef{ID: fixtureTaskID, Title: "make webhook handling idempotent"},
		Events: append(fixtureEvents(), intruder),
		Clock:  newClock(),
	})

	for _, sess := range s.Sessions {
		if sess.SessionID == "sess_intruder" {
			t.Fatalf("another task's session entered the timeline: %+v", s.Sessions)
		}
	}
	if !hasNote(s.Capture.Notes, "belonging to another task") {
		t.Errorf("notes = %v, want the out-of-scope event reported", s.Capture.Notes)
	}

	// With neither identity field filled there is nothing to scope by, so every
	// well-formed event is still accepted.
	open := mustState(t, Input{Events: []model.AgentEvent{intruder}, Clock: newClock()})
	if len(open.Sessions) != 1 {
		t.Errorf("len(Sessions) = %d, want 1 when no task id was supplied", len(open.Sessions))
	}
}

// TestDeriveConstraintsReportsDuplicateIDs: constraints are never dropped, so
// two events reusing one id both survive; the reader has to be told that the id
// no longer identifies one of them.
func TestDeriveConstraintsReportsDuplicateIDs(t *testing.T) {
	got, notes := deriveConstraints([]model.AgentEvent{
		withAttrs(event(model.ConstraintAdded, 1, mainSession), attrConstraintID, "C9", attrConstraintText, "one"),
		withAttrs(event(model.ConstraintAdded, 2, mainSession), attrConstraintID, " c9 ", attrConstraintText, "two"),
	})
	if len(got) != 2 {
		t.Fatalf("deriveConstraints() = %d constraints, want both kept", len(got))
	}
	if !hasNote(notes, "constraint id C9 was supplied by more than one event") {
		t.Errorf("notes = %v, want the id collision reported", notes)
	}

	// Distinct ids stay unreported, and a generated id is still allocated around
	// the adapter-supplied one.
	unique, uniqueNotes := deriveConstraints([]model.AgentEvent{
		withAttrs(event(model.ConstraintAdded, 1, mainSession), attrConstraintID, "C9", attrConstraintText, "one"),
		withAttrs(event(model.ConstraintAdded, 2, mainSession), attrConstraintText, "two"),
	})
	if len(uniqueNotes) != 0 {
		t.Errorf("notes = %v, want none for distinct ids", uniqueNotes)
	}
	if unique[1].ID == unique[0].ID {
		t.Errorf("generated id collided with the supplied one: %+v", unique)
	}
}

// TestChangedFileDeletionStatusVariants: status codes arrive verbatim from an
// inspector, so " D" and "d" are the same fact as "D". Reading them as live
// files makes the derivation ask for the hash of a deleted path and then report
// the inevitable miss as degraded drift coverage.
func TestChangedFileDeletionStatusVariants(t *testing.T) {
	repo := fixtureRepo()
	repo.files = []model.ChangedFile{
		{Path: "a.go", Status: "d"},
		{Path: "b.go", Status: " D "},
		{Path: "c.go", Status: "DD"},
		{Path: "d.go", Status: " M "},
	}
	repo.hashes = map[string]string{"d.go": "cafe"}

	s := mustState(t, fixtureInput(repo))

	if !reflect.DeepEqual(repo.hashedPaths, []string{"d.go"}) {
		t.Errorf("hashed paths = %v, want only the surviving file", repo.hashedPaths)
	}
	if hasNote(s.Capture.Notes, "content hashes unavailable") {
		t.Errorf("notes = %v, want no drift gap: every live file was hashed", s.Capture.Notes)
	}

	// Status is normalised so a padded code does not read as a distinct one.
	modified, ok := changedFile(s, "d.go")
	if !ok {
		t.Fatal("d.go missing from changed files")
	}
	if modified.Status != "M" {
		t.Errorf("d.go status = %q, want %q", modified.Status, "M")
	}
	if got := fileHashDetail(modified); got != "sha256:cafe" {
		t.Errorf("d.go hash detail = %q, want the sha256-prefixed contract value", got)
	}
	for _, path := range []string{"a.go", "b.go", "c.go"} {
		f, ok := changedFile(s, path)
		if !ok {
			t.Fatalf("%s missing from changed files", path)
		}
		if got := fileHashDetail(f); got != "" {
			t.Errorf("%s hash detail = %q, want none: a deleted file has no content", path, got)
		}
	}
}

// TestStateDoesNotMutateInspectorEvidence: the inspector still owns the slices
// it handed over, so attaching our citation must not write into its capacity.
func TestStateDoesNotMutateInspectorEvidence(t *testing.T) {
	prior := model.Evidence{Kind: model.EvidenceRuntime, Ref: "runtime-check"}
	shared := make([]model.Evidence, 1, 4)
	shared[0] = prior

	repo := fixtureRepo()
	repo.files = []model.ChangedFile{{Path: "billing/webhook.go", Status: "M", Evidence: shared}}

	if _, err := State(context.Background(), fixtureInput(repo)); err != nil {
		t.Fatalf("State() returned error: %v", err)
	}
	if len(shared) != 1 || shared[0] != prior {
		t.Fatalf("caller's evidence slice was modified: %+v", shared)
	}
	if got := shared[:cap(shared)][1]; got != (model.Evidence{}) {
		t.Errorf("spare capacity of the caller's slice was written into: %+v", got)
	}
}
