package entire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// missingBin is a binary name chosen to not exist, so Detect is forced down the
// fallback path this machine actually takes: entire is not installed here.
const missingBin = "entire-binary-that-does-not-exist-6f2a"

// TestFallbackDescribeIsHonest is the test that matters most in this package.
// The local store is a pile of JSON files that no other machine can verify. If
// its own description let a reader believe they were holding Entire history,
// every downstream claim built on it would inherit that lie (plan §5, §48).
func TestFallbackDescribeIsHonest(t *testing.T) {
	// A literal root, so the assertions below cannot be perturbed by whatever
	// the temp directory happens to be called.
	got := (&localClient{root: `D:\work\repo\.continuity`}).Describe()

	for _, want := range []string{"local fallback", "not Entire", `D:\work\repo\.continuity`} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to contain %q", got, want)
		}
	}

	// Every mention of Entire must be a denial. Anything else is the fallback
	// borrowing authority it does not have.
	for i := 0; i < len(got); {
		idx := strings.Index(got[i:], "Entire")
		if idx < 0 {
			break
		}
		at := i + idx
		if at < 4 || got[at-4:at] != "not " {
			t.Errorf("Describe() = %q mentions Entire at offset %d without denying it", got, at)
		}
		i = at + len("Entire")
	}

	lower := strings.ToLower(got)
	for _, forbidden := range []string{"authoritative", "entire cli", "entire session", "history layer at"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("Describe() = %q, which claims %q; the fallback is not Entire", got, forbidden)
		}
	}

	// It must also be distinguishable from the real backend at a glance.
	if got == NewCLI("", "").Describe() {
		t.Error("the fallback and the CLI describe themselves identically")
	}
}

func TestDetectFallsBackToLocal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	clock := &model.FixedClock{Current: fixedStart, Step: time.Minute}

	client, note := Detect(missingBin, root, t.TempDir(), clock)
	if client == nil {
		t.Fatal("Detect returned a nil client; a missing Entire must degrade, not crash (plan §48)")
	}

	// The note names the backend actually chosen, and says the real one was not
	// used, so a user reading the output is never guessing.
	for _, want := range []string{"checkpoint backend:", "local fallback", "not Entire", "the entire binary did not answer"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to contain %q", note, want)
		}
	}
	if !strings.Contains(note, client.Describe()) {
		t.Errorf("note = %q does not carry the chosen backend's own description %q", note, client.Describe())
	}

	// And the chosen backend actually works.
	ctx := context.Background()
	cp, err := client.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: "task_demo", SessionID: "sess_root", Label: "CP1",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint through the detected backend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, checkpointsDir, cp.ID+".json")); err != nil {
		t.Errorf("checkpoint was not written under %s: %v", root, err)
	}
}

// TestDetectWithNoBackendAtAll covers the worst case: no binary and no usable
// place to fall back to. The caller still gets a working client that reports
// ErrUnavailable, because Entire is never a fatal dependency of the host
// agent's own work (plan §48, Rule 8).
func TestDetectWithNoBackendAtAll(t *testing.T) {
	occupied := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(occupied, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	client, note := Detect(missingBin, filepath.Join(occupied, "store"), "", nil)
	if client == nil {
		t.Fatal("Detect returned a nil client")
	}

	for _, want := range []string{"checkpoint backend: none", "safe resume cannot be claimed"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to contain %q", note, want)
		}
	}

	ctx := context.Background()
	if client.Available(ctx) {
		t.Error("Available() = true with no backend")
	}
	if !strings.Contains(client.Describe(), "no checkpoint backend") {
		t.Errorf("Describe() = %q, want it to say there is no backend", client.Describe())
	}

	calls := map[string]func() error{
		"CreateCheckpoint": func() error {
			_, err := client.CreateCheckpoint(ctx, model.CheckpointRequest{TaskID: "task_demo"})
			return err
		},
		"Checkpoint": func() error {
			_, err := client.Checkpoint(ctx, "cp_a")
			return err
		},
		"Checkpoints": func() error {
			_, err := client.Checkpoints(ctx, "task_demo")
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, model.ErrUnavailable) {
			t.Errorf("%s error = %v, want ErrUnavailable", name, err)
		}
	}
}

// TestDetectIsDeterministic pins the note: it is rendered into user-facing
// output, so two runs over the same inputs must produce the same text.
func TestDetectIsDeterministic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")

	_, first := Detect(missingBin, root, `D:\repo`, &model.FixedClock{Current: fixedStart, Step: time.Minute})
	_, second := Detect(missingBin, root, `D:\repo`, &model.FixedClock{Current: fixedStart, Step: time.Minute})
	if first != second {
		t.Errorf("Detect note differs between runs:\n%q\n%q", first, second)
	}
}

func TestCheckStateSchema(t *testing.T) {
	tests := []struct {
		name    string
		version int
		state   *model.EngineeringState
		wantErr bool
	}{
		{name: "nil state is fine", state: nil},
		{name: "current version", version: model.SchemaVersion},
		// An unstamped state is accepted and left at 0: back-filling a version
		// would be asserting a compatibility we never checked.
		{name: "unstamped version", version: 0},
		{name: "newer version rejected", version: model.SchemaVersion + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.state
			if tt.name != "nil state is fine" {
				s = model.NewEngineeringState(model.TaskRef{ID: "task_demo"})
				s.SchemaVersion = tt.version
			}
			err := checkStateSchema(s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkStateSchema(version %d) error = %v, wantErr %v", tt.version, err, tt.wantErr)
			}
			if s != nil && s.SchemaVersion != tt.version {
				t.Errorf("checkStateSchema rewrote the schema version to %d", s.SchemaVersion)
			}
		})
	}
}

func TestCanonicalStateIsStableAndDetached(t *testing.T) {
	if canonicalState(nil) != nil {
		t.Error("canonicalState(nil) should stay nil")
	}

	original := sampleState()
	firstID := original.Requirements[0].ID

	a := canonicalState(original)
	b := canonicalState(original)
	if a.Hash() != b.Hash() {
		t.Errorf("canonicalState is not stable: %s vs %s", a.Hash(), b.Hash())
	}
	if original.Requirements[0].ID != firstID {
		t.Errorf("canonicalState mutated its input: requirement 0 is now %q", original.Requirements[0].ID)
	}

	// Reach through the copy and edit shared structure. Reassigning a field on
	// `a` could never touch `original` whatever the copy depth, so it would
	// assert nothing; writing through the slice and its nested evidence is what
	// actually distinguishes a deep copy from a shallow one.
	wantDesc := original.Requirements[0].Description
	a.Requirements[0].Description = "MUTATED THROUGH THE COPY"
	if original.Requirements[0].Description != wantDesc {
		t.Errorf("canonicalState shares its requirement backing array: description is now %q, want %q",
			original.Requirements[0].Description, wantDesc)
	}

	var withEvidence int
	for i, r := range original.Requirements {
		if len(r.Evidence) > 0 {
			withEvidence = i
		}
	}
	if len(original.Requirements[withEvidence].Evidence) == 0 {
		t.Fatal("test premise broken: sampleState carries no evidence to mutate")
	}
	// canonicalState sorts, so locate the same requirement inside the copy.
	for i := range a.Requirements {
		if a.Requirements[i].ID != original.Requirements[withEvidence].ID {
			continue
		}
		wantRef := original.Requirements[withEvidence].Evidence[0].Ref
		a.Requirements[i].Evidence[0].Ref = "MUTATED"
		if got := original.Requirements[withEvidence].Evidence[0].Ref; got != wantRef {
			t.Errorf("canonicalState shares nested evidence: ref is now %q, want %q", got, wantRef)
		}
	}
}
