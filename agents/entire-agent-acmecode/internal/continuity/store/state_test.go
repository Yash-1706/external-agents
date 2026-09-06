package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func sampleState() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:             testTaskID,
		Title:          "Implement subscription pause",
		OriginalIntent: "Let a subscriber pause billing for up to three months",
	})
	s.Status = model.StatusPartial
	// Deliberately out of order: PutState must normalise before writing.
	s.Requirements = []model.Requirement{
		{ID: "R2", Description: "Resume restores the original billing date", Status: model.ReqUnresolved, Confidence: model.Unknown},
		{ID: "R1", Description: "Pause endpoint exists", Status: model.ReqComplete, Confidence: model.Observed,
			Evidence: []model.Evidence{{Kind: model.EvidenceCommit, Ref: "9f1c2b3"}}},
	}
	s.Capture = model.Capture{
		GitAvailable: true,
		Missing:      []string{"transcript", "graph"},
		Notes:        []string{"Entire was unreachable during capture"},
	}
	return s
}

func TestStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	in := sampleState()
	if err := f.PutState(ctx, testTaskID, in); err != nil {
		t.Fatalf("PutState: %v", err)
	}

	// The store must not reorder the caller's live object underneath it.
	if in.Requirements[0].ID != "R2" {
		t.Errorf("PutState mutated the caller's state: first requirement is now %q", in.Requirements[0].ID)
	}

	out, err := f.State(ctx, testTaskID)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if out.Task.ID != testTaskID || out.Status != model.StatusPartial {
		t.Errorf("state identity = %q/%q, want %q/partial", out.Task.ID, out.Status, testTaskID)
	}
	if len(out.Requirements) != 2 || out.Requirements[0].ID != "R1" {
		t.Fatalf("requirements = %+v, want R1 first after normalisation", out.Requirements)
	}
	if len(out.Requirements[0].Evidence) != 1 || out.Requirements[0].Evidence[0].Ref != "9f1c2b3" {
		t.Errorf("evidence did not survive the round trip: %+v", out.Requirements[0].Evidence)
	}
	// Capture gaps are the honesty record; losing them would turn a degraded
	// capture into one that looks complete (plan §33).
	if len(out.Capture.Missing) != 2 || out.Capture.Missing[0] != "graph" {
		t.Errorf("Capture.Missing = %v, want the sorted gaps", out.Capture.Missing)
	}
	if out.Capture.Complete() {
		t.Error("a state with recorded gaps must not read as complete")
	}
	if in.Hash() != out.Hash() {
		t.Errorf("hash changed across the round trip: %q -> %q", in.Hash(), out.Hash())
	}

	// Overwriting is how a task's latest merged state advances.
	in.Status = model.StatusVerified
	if err := f.PutState(ctx, testTaskID, in); err != nil {
		t.Fatalf("PutState overwrite: %v", err)
	}
	out, err = f.State(ctx, testTaskID)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if out.Status != model.StatusVerified {
		t.Errorf("Status = %q, want verified", out.Status)
	}
}

func TestStateRejections(t *testing.T) {
	ctx := context.Background()

	t.Run("nil state", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		if err := f.PutState(ctx, testTaskID, nil); err == nil {
			t.Fatal("PutState(nil) = nil error, want rejection")
		}
	})

	t.Run("state for a different task", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		s := sampleState()
		s.Task.ID = "task_somebody_else"
		err := f.PutState(ctx, testTaskID, s)
		if err == nil {
			t.Fatal("PutState with a mismatched TaskRef = nil error, want rejection")
		}
		if _, err := f.State(ctx, testTaskID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("a rejected state was still written: %v", err)
		}
	})

	t.Run("state with no TaskRef id is accepted", func(t *testing.T) {
		// An extractor that never learned the id is not lying about it, so this
		// is uncertainty rather than a conflict.
		f := newStore(t)
		seedTask(t, f)
		s := sampleState()
		s.Task.ID = ""
		if err := f.PutState(ctx, testTaskID, s); err != nil {
			t.Fatalf("PutState: %v", err)
		}
	})

	t.Run("unknown task", func(t *testing.T) {
		f := newStore(t)
		if err := f.PutState(ctx, "task_absent", sampleState()); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("PutState(absent) error = %v, want model.ErrNotFound", err)
		}
		if _, err := f.State(ctx, "task_absent"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("State(absent) error = %v, want model.ErrNotFound", err)
		}
	})

	t.Run("task with no state yet", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		_, err := f.State(ctx, testTaskID)
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("State error = %v, want model.ErrNotFound so the caller says 'nothing known yet'", err)
		}
	})

	t.Run("a damaged state file is reported, not treated as absent", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		if err := f.PutState(ctx, testTaskID, sampleState()); err != nil {
			t.Fatalf("PutState: %v", err)
		}
		path := filepath.Join(f.Root(), tasksDirName, testTaskID, stateFileName)
		if err := os.WriteFile(path, []byte("{oops"), filePerm); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := f.State(ctx, testTaskID)
		if err == nil {
			t.Fatal("State = nil error, want the damage reported")
		}
		if errors.Is(err, model.ErrNotFound) {
			t.Errorf("corruption reported as absence: %v", err)
		}
	})
}

func TestPutStateIsAtomic(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	if err := f.PutState(ctx, testTaskID, sampleState()); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	// A rejected second write must leave the first one intact rather than
	// truncating it.
	bad := sampleState()
	bad.Task.ID = "task_somebody_else"
	if err := f.PutState(ctx, testTaskID, bad); err == nil {
		t.Fatal("expected rejection")
	}
	out, err := f.State(ctx, testTaskID)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if out.Task.ID != testTaskID {
		t.Errorf("stored state id = %q, want the surviving %q", out.Task.ID, testTaskID)
	}

	// No temp file may be left lying around in the task directory.
	entries, err := os.ReadDir(filepath.Join(f.Root(), tasksDirName, testTaskID))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("temp file %q survived the write", e.Name())
		}
	}
}
