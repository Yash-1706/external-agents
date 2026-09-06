package entire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// fixedStart is the instant every fixture clock in this file starts from.
// Golden ids below are derived from it, so changing it changes them.
var fixedStart = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func testClock(step time.Duration) model.Clock {
	return &model.FixedClock{Current: fixedStart, Step: step}
}

// newTestLocal opens a store in a throwaway directory and fails the test rather
// than returning an error, so the tests below stay about behaviour.
func newTestLocal(t *testing.T, root string, clock model.Clock) model.EntireClient {
	t.Helper()
	c, err := NewLocal(root, clock)
	if err != nil {
		t.Fatalf("NewLocal(%q): unexpected error: %v", root, err)
	}
	return c
}

// sampleState builds a small but non-empty EngineeringState so round-trip tests
// prove the state really travels with the checkpoint rather than being dropped.
func sampleState() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:             "task_demo",
		Title:          "Fix billing proration",
		OriginalIntent: "Make proration correct for mid-cycle plan changes",
	})
	s.Status = model.StatusPartial
	s.Requirements = []model.Requirement{
		{ID: "R2", Description: "Refund the unused remainder", Status: model.ReqUnresolved, Confidence: model.Unknown},
		{ID: "R1", Description: "Charge the pro-rated difference", Status: model.ReqComplete, Confidence: model.Observed,
			Evidence: []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}}},
	}
	return s
}

func TestNewLocalRejectsUnusableRoot(t *testing.T) {
	occupied := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(occupied, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	tests := []struct {
		name string
		root string
	}{
		{name: "empty root", root: ""},
		{name: "whitespace root", root: "   "},
		{name: "root under a regular file", root: filepath.Join(occupied, "store")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewLocal(tt.root, testClock(0))
			if err == nil {
				t.Fatalf("NewLocal(%q) = %v, want error", tt.root, c)
			}
			if c != nil {
				t.Errorf("NewLocal returned a client alongside an error: %v", c)
			}
		})
	}
}

func TestLocalRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	c := newTestLocal(t, root, testClock(time.Minute))

	if !c.Available(ctx) {
		t.Fatal("Available() = false on a freshly opened store")
	}

	labels := []string{"CP1", "CP2", "CP3"}
	created := make([]model.Checkpoint, 0, len(labels))
	for _, label := range labels {
		cp, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
			TaskID:    "task_demo",
			SessionID: "sess_root",
			Label:     label,
			Agent:     model.AgentOpenClaw,
			Message:   "milestone " + label,
			State:     sampleState(),
			Meta:      map[string]string{metaCommitSHA: "abc1234", metaBranch: "master"},
		})
		if err != nil {
			t.Fatalf("CreateCheckpoint(%s): %v", label, err)
		}
		created = append(created, cp)
	}

	// The derived ids are pinned: the derivation is part of the on-disk
	// contract, so an accidental change to it must break a test rather than
	// quietly fork history on the next run.
	wantIDs := []string{"cp_a705026eb3c1", "cp_fad87a381471", "cp_f16510a9c534"}
	for i, cp := range created {
		if cp.ID != wantIDs[i] {
			t.Errorf("checkpoint %d id = %q, want %q", i, cp.ID, wantIDs[i])
		}
	}

	// Metadata the request type has nowhere to put must survive.
	if got := created[0].CommitSHA; got != "abc1234" {
		t.Errorf("CommitSHA = %q, want %q (lifted from meta)", got, "abc1234")
	}
	if got := created[0].Branch; got != "master" {
		t.Errorf("Branch = %q, want %q (lifted from meta)", got, "master")
	}
	if got := created[0].Meta[metaMessage]; got != "milestone CP1" {
		t.Errorf("Meta[%q] = %q, want the request message", metaMessage, got)
	}

	// Reading one back must be indistinguishable from what the write returned.
	for _, want := range created {
		got, err := c.Checkpoint(ctx, want.ID)
		if err != nil {
			t.Fatalf("Checkpoint(%s): %v", want.ID, err)
		}
		if got.ID != want.ID || got.Label != want.Label || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("Checkpoint(%s) = %+v, want %+v", want.ID, got, want)
		}
		if got.State == nil {
			t.Fatalf("Checkpoint(%s) lost the engineering state", want.ID)
		}
		if got.State.Hash() != want.State.Hash() {
			t.Errorf("Checkpoint(%s) state hash = %s, want %s", want.ID, got.State.Hash(), want.State.Hash())
		}
	}

	list, err := c.Checkpoints(ctx, "task_demo")
	if err != nil {
		t.Fatalf("Checkpoints: %v", err)
	}
	if len(list) != len(labels) {
		t.Fatalf("Checkpoints returned %d checkpoints, want %d", len(list), len(labels))
	}
	for i, cp := range list {
		if cp.Label != labels[i] {
			t.Errorf("Checkpoints[%d].Label = %q, want %q (oldest first)", i, cp.Label, labels[i])
		}
		if i > 0 && cp.CreatedAt.Before(list[i-1].CreatedAt) {
			t.Errorf("Checkpoints[%d] created before its predecessor: not oldest-first", i)
		}
	}

	// A different task must not inherit this task's history.
	other, err := c.Checkpoints(ctx, "task_unrelated")
	if err != nil {
		t.Fatalf("Checkpoints(task_unrelated): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("Checkpoints(task_unrelated) = %d checkpoints, want 0", len(other))
	}
}

func TestCheckpointIDDerivation(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)

	tests := []struct {
		name      string
		taskID    string
		sessionID string
		label     string
		createdAt time.Time
		want      string
	}{
		{"baseline", "task_demo", "sess_root", "CP1 initial understanding", fixedStart, "cp_dc994a92b5e9"},
		{"label changes the id", "task_demo", "sess_root", "CP2 pre-curveball", fixedStart, "cp_3ebb085699fc"},
		{"session changes the id", "task_demo", "sess_other", "CP1 initial understanding", fixedStart, "cp_6b9286d4ac06"},
		{"time changes the id", "task_demo", "sess_root", "CP1 initial understanding", fixedStart.Add(time.Minute), "cp_428bac04e650"},
		{"empty label is still derivable", "task_demo", "sess_root", "", fixedStart, "cp_e376e2ef219c"},
		// The same instant in another zone is the same instant: the id must not
		// depend on how the caller happened to carry its offset.
		{"zone does not change the id", "task_demo", "sess_root", "CP1 initial understanding", fixedStart.In(ist), "cp_dc994a92b5e9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkpointID(tt.taskID, tt.sessionID, tt.label, tt.createdAt)
			if got != tt.want {
				t.Errorf("checkpointID = %q, want %q", got, tt.want)
			}
			if again := checkpointID(tt.taskID, tt.sessionID, tt.label, tt.createdAt); again != got {
				t.Errorf("checkpointID is not stable: %q then %q", got, again)
			}
		})
	}
}

// TestLocalWriteIsByteIdentical is the determinism guarantee: two runs over the
// same inputs must produce the same bytes, including for the metadata map whose
// Go iteration order is deliberately random.
func TestLocalWriteIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	meta := map[string]string{
		"zulu": "z", "alpha": "a", "mike": "m", metaCommitSHA: "abc1234", metaBranch: "master",
	}

	write := func() []byte {
		t.Helper()
		root := t.TempDir()
		c := newTestLocal(t, root, testClock(time.Minute))
		cp, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
			TaskID:    "task_demo",
			SessionID: "sess_root",
			Label:     "CP1",
			Agent:     model.AgentHermes,
			Message:   "stable point",
			State:     sampleState(),
			Meta:      meta,
		})
		if err != nil {
			t.Fatalf("CreateCheckpoint: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(root, checkpointsDir, cp.ID+".json"))
		if err != nil {
			t.Fatalf("reading record: %v", err)
		}
		return b
	}

	first, second := write(), write()
	if string(first) != string(second) {
		t.Errorf("two runs over identical inputs produced different bytes:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(first), backendLocal) {
		t.Errorf("record does not stamp the backend %q; a stray file could be mistaken for Entire history:\n%s", backendLocal, first)
	}
}

// TestLocalCreateDoesNotMutateCaller guards the caller's inputs: a request
// handed to the store must come back unchanged, or a caller reusing it would
// silently record something else next time.
func TestLocalCreateDoesNotMutateCaller(t *testing.T) {
	ctx := context.Background()
	c := newTestLocal(t, t.TempDir(), testClock(time.Minute))

	meta := map[string]string{metaCommitSHA: "abc1234"}
	state := sampleState()
	firstReq := state.Requirements[0].ID

	if _, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: "task_demo", SessionID: "sess_root", Label: "CP1",
		Message: "note", State: state, Meta: meta,
	}); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	if len(meta) != 1 {
		t.Errorf("caller meta was mutated: %v", meta)
	}
	if state.Requirements[0].ID != firstReq {
		t.Errorf("caller state was re-sorted in place: requirement 0 is now %q, was %q", state.Requirements[0].ID, firstReq)
	}
}

func TestLocalCreateRejects(t *testing.T) {
	ctx := context.Background()
	c := newTestLocal(t, t.TempDir(), testClock(time.Minute))

	futureState := sampleState()
	futureState.SchemaVersion = model.SchemaVersion + 1

	tests := []struct {
		name    string
		req     model.CheckpointRequest
		wantErr string
	}{
		{
			name:    "no task id",
			req:     model.CheckpointRequest{SessionID: "sess_root", Label: "CP1"},
			wantErr: "task id must not be empty",
		},
		{
			// A state we cannot fully interpret must not be stored as though we
			// had, or the next reader inherits silently dropped fields.
			name:    "state schema from the future",
			req:     model.CheckpointRequest{TaskID: "task_demo", Label: "CP1", State: futureState},
			wantErr: "newer than this build understands",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateCheckpoint(ctx, tt.req)
			if err == nil {
				t.Fatalf("CreateCheckpoint = nil error, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CreateCheckpoint error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestLocalRefusesToOverwriteHistory pins the append-only rule: a recorded
// checkpoint is evidence, and evidence is never silently replaced (plan §32).
func TestLocalRefusesToOverwriteHistory(t *testing.T) {
	ctx := context.Background()
	// Step 0 means both writes derive the same timestamp, hence the same id.
	c := newTestLocal(t, t.TempDir(), testClock(0))

	req := model.CheckpointRequest{TaskID: "task_demo", SessionID: "sess_root", Label: "CP1"}
	first, err := c.CreateCheckpoint(ctx, req)
	if err != nil {
		t.Fatalf("first CreateCheckpoint: %v", err)
	}
	if _, err := c.CreateCheckpoint(ctx, req); err == nil {
		t.Fatalf("second CreateCheckpoint on id %s = nil error, want refusal", first.ID)
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v, want it to say it refused to overwrite", err)
	}
}

func TestLocalCheckpointNotFound(t *testing.T) {
	ctx := context.Background()
	c := newTestLocal(t, t.TempDir(), testClock(time.Minute))

	tests := []struct {
		name string
		id   string
	}{
		{"absent id", "cp_000000000000"},
		{"empty id", ""},
		{"only dots", "../.."},
		{"path traversal", "../../secrets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Checkpoint(ctx, tt.id)
			if !errors.Is(err, model.ErrNotFound) {
				t.Errorf("Checkpoint(%q) error = %v, want ErrNotFound", tt.id, err)
			}
			// Absent is not the same as unreachable, and conflating them would
			// let a caller degrade for the wrong reason (plan §48).
			if errors.Is(err, model.ErrUnavailable) {
				t.Errorf("Checkpoint(%q) reported ErrUnavailable for a missing checkpoint", tt.id)
			}
		})
	}
}

// TestLocalPathTraversalStaysInsideRoot proves sanitisation is load-bearing and
// not decorative: an id from an untrusted adapter must not steer a read out of
// the store.
//
// The records below are planted OUTSIDE the checkpoints directory and are
// perfectly well-formed, so the only thing that can keep them out of the
// answer is sanitisation. Asserting merely that a traversal id returns
// ErrNotFound proves nothing, because a read that escaped into an empty
// location returns ErrNotFound too.
func TestLocalPathTraversalStaysInsideRoot(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	c := newTestLocal(t, root, testClock(time.Minute))

	targets := map[string]string{
		// <root>/escaped.json, one level above the checkpoints directory.
		"../escaped": filepath.Join(root, "escaped.json"),
		// <parent>/sibling.json, outside the store root entirely.
		`..\..\sibling`: filepath.Join(parent, "sibling.json"),
	}
	for id, path := range targets {
		// The planted record names itself with the very id being requested, so
		// the id-mismatch guard cannot be what rejects it. Only sanitisation
		// can: this test fails the moment the read is allowed to leave the
		// checkpoints directory.
		planted := fmt.Sprintf(`{"record_version":1,"id":%q,"task_id":"task_demo","created_at":"2026-03-01T09:00:00Z"}`, id)
		if err := os.WriteFile(path, []byte(planted), 0o644); err != nil {
			t.Fatalf("planting %s: %v", path, err)
		}
	}

	for id, path := range targets {
		cp, err := c.Checkpoint(ctx, id)
		if !errors.Is(err, model.ErrNotFound) {
			t.Errorf("Checkpoint(%q) error = %v, want ErrNotFound", id, err)
		}
		if cp.ID != "" {
			t.Errorf("Checkpoint(%q) reached the planted record at %s and returned %q", id, path, cp.ID)
		}
	}

	// A write cannot escape either: ids are derived, and the file lands under
	// the checkpoints directory.
	cp, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{TaskID: "../../task", SessionID: "../s", Label: "../CP"})
	if err != nil {
		t.Fatalf("CreateCheckpoint with traversal-shaped fields: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, checkpointsDir, cp.ID+".json")); err != nil {
		t.Errorf("checkpoint %s was not written inside the checkpoints directory: %v", cp.ID, err)
	}
}

// TestLocalCheckpointRefusesAMismatchedRecord covers id confusion. Ids are
// squashed onto a safe path segment, so distinct ids can share a file. Handing
// back whoever occupies it would let checkpoint B be cited as evidence for a
// request that named A (plan §11) - the requested id simply is not there.
func TestLocalCheckpointRefusesAMismatchedRecord(t *testing.T) {
	ctx := context.Background()
	c := newTestLocal(t, t.TempDir(), testClock(time.Minute))

	real, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: "task_demo", SessionID: "sess_root", Label: "CP1",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	// Same file after sanitisation ("/" and ":" both become "_"), different id.
	for _, bogus := range []string{"cp/" + real.ID[3:], "cp:" + real.ID[3:]} {
		if name, _ := checkpointFileName(bogus); name != real.ID+".json" {
			t.Fatalf("test premise broken: %q sanitises to %q, not to %s.json", bogus, name, real.ID)
		}
		got, err := c.Checkpoint(ctx, bogus)
		if !errors.Is(err, model.ErrNotFound) {
			t.Errorf("Checkpoint(%q) error = %v, want ErrNotFound", bogus, err)
		}
		if got.ID != "" {
			t.Errorf("Checkpoint(%q) returned checkpoint %q, which is not the one that was asked for", bogus, got.ID)
		}
	}

	// The genuine id still resolves, so the check did not break the round trip.
	if got, err := c.Checkpoint(ctx, real.ID); err != nil || got.ID != real.ID {
		t.Errorf("Checkpoint(%q) = %q, %v; want the record back unchanged", real.ID, got.ID, err)
	}
}

// TestLocalReportsAVanishedStoreAsUnavailable separates "no such checkpoint"
// from "no such store". Only the second is a gap the caller must record in
// Capture; reporting it as ErrNotFound would state as a fact that the
// checkpoint does not exist, when we never managed to look (plan §48).
func TestLocalReportsAVanishedStoreAsUnavailable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	c := newTestLocal(t, root, testClock(time.Minute))

	if _, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: "task_demo", SessionID: "sess_root", Label: "CP1",
	}); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	// While the store is healthy, an absent id is absent, not unavailable.
	if _, err := c.Checkpoint(ctx, "cp_000000000000"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Checkpoint on a healthy store error = %v, want ErrNotFound", err)
	}

	if err := os.RemoveAll(filepath.Join(root, checkpointsDir)); err != nil {
		t.Fatalf("removing the store: %v", err)
	}

	if c.Available(ctx) {
		t.Error("Available() = true after the store directory was removed")
	}

	calls := map[string]func() error{
		"Checkpoint": func() error {
			_, err := c.Checkpoint(ctx, "cp_000000000000")
			return err
		},
		"Checkpoints": func() error {
			_, err := c.Checkpoints(ctx, "task_demo")
			return err
		},
		"CreateCheckpoint": func() error {
			_, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{TaskID: "task_demo", Label: "CP2"})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if !errors.Is(err, model.ErrUnavailable) {
			t.Errorf("%s with the store removed error = %v, want ErrUnavailable", name, err)
		}
		if errors.Is(err, model.ErrNotFound) {
			t.Errorf("%s reported ErrNotFound for a store that is not there: %v", name, err)
		}
	}
}

func TestLocalCheckpointsRejectsEmptyTaskID(t *testing.T) {
	ctx := context.Background()
	c := newTestLocal(t, t.TempDir(), testClock(time.Minute))

	for _, id := range []string{"", "   "} {
		if _, err := c.Checkpoints(ctx, id); err == nil {
			t.Errorf("Checkpoints(%q) = nil error; an empty id would attribute unrelated history to the task", id)
		}
	}
}

// TestLocalCheckpointsReportsUnreadableFiles is the honesty path: a store with
// a damaged record returns what it could read AND says what it could not, so
// the caller cannot mistake a truncated history for a complete one.
func TestLocalCheckpointsReportsUnreadableFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	c := newTestLocal(t, root, testClock(time.Minute))

	good, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: "task_demo", SessionID: "sess_root", Label: "CP1",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	dir := filepath.Join(root, checkpointsDir)
	damaged := map[string]string{
		"corrupt.json":  "{ this is not json",
		"no-id.json":    `{"record_version":1,"task_id":"task_demo","created_at":"2026-03-01T09:00:00Z"}`,
		"no-time.json":  `{"record_version":1,"id":"cp_x","task_id":"task_demo"}`,
		"newer.json":    `{"record_version":99,"id":"cp_y","task_id":"task_demo","created_at":"2026-03-01T09:00:00Z"}`,
		"newstate.json": `{"record_version":1,"id":"cp_z","task_id":"task_demo","created_at":"2026-03-01T09:00:00Z","state":{"schema_version":99}}`,
		// Not a record at all: skipped without complaint, because it never
		// claimed to be one.
		"notes.txt": "scratch",
	}
	for name, body := range damaged {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	list, err := c.Checkpoints(ctx, "task_demo")
	if err == nil {
		t.Fatal("Checkpoints = nil error; unreadable records were dropped silently")
	}
	for _, want := range []string{"corrupt.json", "no-id.json", "no-time.json", "newer.json", "newstate.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("error names a non-record file: %v", err)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error = %v, want it to say the returned history is incomplete", err)
	}

	// The readable history still comes back, so a caller may degrade rather
	// than lose everything.
	if len(list) != 1 || list[0].ID != good.ID {
		t.Fatalf("Checkpoints returned %+v, want the one readable checkpoint %s", list, good.ID)
	}
}

func TestLocalAgentNormalisation(t *testing.T) {
	ctx := context.Background()
	c := newTestLocal(t, t.TempDir(), testClock(time.Minute))

	tests := []struct {
		name  string
		agent model.AgentKind
		want  model.AgentKind
	}{
		{"unset", "", model.AgentUnknown},
		{"third party keeps its name", model.AgentKind("borg"), model.AgentKind("borg")},
		{"malformed becomes unknown", model.AgentKind("Borg Corp!!"), model.AgentUnknown},
		{"known", model.AgentHermes, model.AgentHermes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{
				TaskID: "task_demo", SessionID: "sess_" + tt.name, Label: "CP", Agent: tt.agent,
			})
			if err != nil {
				t.Fatalf("CreateCheckpoint: %v", err)
			}
			if cp.Agent != tt.want {
				t.Errorf("Agent = %q, want %q", cp.Agent, tt.want)
			}
		})
	}
}

func TestLocalHonoursCancelledContext(t *testing.T) {
	root := t.TempDir()
	c := newTestLocal(t, root, testClock(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if c.Available(ctx) {
		t.Error("Available(cancelled) = true")
	}
	if _, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{TaskID: "task_demo"}); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateCheckpoint error = %v, want context.Canceled", err)
	}
	if _, err := c.Checkpoint(ctx, "cp_a705026eb3c1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Checkpoint error = %v, want context.Canceled", err)
	}
	if _, err := c.Checkpoints(ctx, "task_demo"); !errors.Is(err, context.Canceled) {
		t.Errorf("Checkpoints error = %v, want context.Canceled", err)
	}

	// A cancelled write must leave nothing behind.
	entries, err := os.ReadDir(filepath.Join(root, checkpointsDir))
	if err != nil {
		t.Fatalf("reading store: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cancelled CreateCheckpoint wrote %d file(s)", len(entries))
	}
}

func TestSanitiseSegment(t *testing.T) {
	long := strings.Repeat("a", 200)

	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{name: "plain id", in: "cp_a705026eb3c1", want: "cp_a705026eb3c1", wantOK: true},
		{name: "separators neutralised", in: "../../etc/passwd", want: ".._.._etc_passwd", wantOK: true},
		{name: "windows separators neutralised", in: `..\..\secrets`, want: ".._.._secrets", wantOK: true},
		{name: "null byte neutralised", in: "cp\x00evil", want: "cp_evil", wantOK: true},
		{name: "unicode neutralised", in: "cp_é", want: "cp__", wantOK: true},
		{name: "empty rejected", in: "", wantOK: false},
		{name: "dot rejected", in: ".", wantOK: false},
		{name: "dotdot rejected", in: "..", wantOK: false},
		{name: "windows device name escaped", in: "con", want: "_con", wantOK: true},
		{name: "windows device name with suffix escaped", in: "NUL.dat", want: "_NUL.dat", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitiseSegment(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("sanitiseSegment(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("sanitiseSegment(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if ok && (strings.ContainsAny(got, `/\`) || got == "." || got == "..") {
				t.Errorf("sanitiseSegment(%q) = %q, which is not a single safe segment", tt.in, got)
			}
		})
	}

	t.Run("long ids stay distinct", func(t *testing.T) {
		a, okA := sanitiseSegment(long + "one")
		b, okB := sanitiseSegment(long + "two")
		if !okA || !okB {
			t.Fatalf("sanitiseSegment rejected a long id: %v %v", okA, okB)
		}
		if a == b {
			t.Errorf("two distinct long ids collided onto %q", a)
		}
		if again, _ := sanitiseSegment(long + "one"); again != a {
			t.Errorf("sanitiseSegment is not deterministic for long ids: %q then %q", a, again)
		}
	})
}
