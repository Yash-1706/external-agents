package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func TestLineageWritesRequireAnExistingTask(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(*FS) error
	}{
		{
			name: "PutSession",
			call: func(f *FS) error {
				return f.PutSession(ctx, "task_absent", model.SessionNode{SessionID: "sess_x", Agent: model.AgentHermes})
			},
		},
		{
			name: "PutCheckpoint",
			call: func(f *FS) error {
				return f.PutCheckpoint(ctx, "task_absent", model.CheckpointNode{CheckpointID: "cp_x", SessionID: "sess_x"})
			},
		},
		{
			name: "PutHandoff",
			call: func(f *FS) error {
				return f.PutHandoff(ctx, model.HandoffNode{ID: "handoff_x", TaskID: "task_absent"})
			},
		},
		{
			name: "Lineage",
			call: func(f *FS) error {
				_, err := f.Lineage(ctx, "task_absent")
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			seedTask(t, f)
			if err := tc.call(f); !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("%s on an absent task = %v, want model.ErrNotFound", tc.name, err)
			}
		})
	}
}

func TestLineageWritesRejectUnusableIDs(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(*FS) error
	}{
		{"PutSession", func(f *FS) error {
			return f.PutSession(ctx, testTaskID, model.SessionNode{SessionID: "../../evil"})
		}},
		{"PutSession empty id", func(f *FS) error {
			return f.PutSession(ctx, testTaskID, model.SessionNode{})
		}},
		{"PutCheckpoint", func(f *FS) error {
			return f.PutCheckpoint(ctx, testTaskID, model.CheckpointNode{CheckpointID: `..\..\evil`})
		}},
		{"PutHandoff", func(f *FS) error {
			return f.PutHandoff(ctx, model.HandoffNode{ID: "../evil", TaskID: testTaskID})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			seedTask(t, f)
			if err := tc.call(f); err == nil {
				t.Fatalf("%s with an unusable id = nil error, want rejection", tc.name)
			}
		})
	}
}

// TestPutSessionIndexesTheSession keeps the two write paths consistent: a
// session added straight to the lineage must still resolve back to its task, or
// a later agent announcing that session id would never find the work.
func TestPutSessionIndexesTheSession(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	node := model.SessionNode{
		SessionID: "sess_human",
		Agent:     model.AgentHuman,
		Role:      model.RoleHuman,
		StartedAt: at(0),
	}
	if err := f.PutSession(ctx, testTaskID, node); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	got, err := f.ResolveTask(ctx, "sess_human", "", "")
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}
	if got.ID != testTaskID {
		t.Errorf("resolved %q, want %q", got.ID, testTaskID)
	}
}

// TestEventFoldDoesNotClobberExplicitNodes covers the ordering hazard: a rich
// node written by the Entire client must survive a later lifecycle event that
// only knows part of the story.
func TestEventFoldDoesNotClobberExplicitNodes(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	full := model.CheckpointNode{
		CheckpointID: "cp_001",
		SessionID:    "sess_main",
		Agent:        model.AgentOpenClaw,
		Label:        "pre-noon stable",
		CommitSHA:    "9f1c2b3",
		Branch:       testBranch,
		CreatedAt:    at(5),
		StateHash:    "sha256:deadbeef",
	}
	if err := f.PutCheckpoint(ctx, testTaskID, full); err != nil {
		t.Fatalf("PutCheckpoint: %v", err)
	}
	// The adapter's event arrives afterwards carrying only the id.
	if err := f.AppendEvent(ctx, model.AgentEvent{
		Type:      model.CheckpointCreated,
		Timestamp: at(9),
		TaskID:    testTaskID,
		SessionID: "sess_main",
		Agent:     model.AgentOpenClaw,
		Attrs:     map[string]string{"checkpoint_id": "cp_001"},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(l.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(l.Checkpoints))
	}
	got := l.Checkpoints[0]
	if got.StateHash != full.StateHash || got.Label != full.Label || got.Branch != full.Branch {
		t.Errorf("event fold erased recorded checkpoint detail: %+v", got)
	}
	if !got.CreatedAt.Equal(at(5)) {
		t.Errorf("CreatedAt = %v, want the earliest observed %v", got.CreatedAt, at(5))
	}
}

func TestLineageIncludesOnlyThisTasksHandoffs(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)
	other := model.Task{ID: "task_other", Repo: testRepo, Branch: "other", RootSessionID: "sess_other_root"}
	if err := f.CreateTask(ctx, other); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	mine := model.HandoffNode{
		ID: "handoff_mine", TaskID: testTaskID, FromAgent: model.AgentOpenClaw, ToAgent: model.AgentHermes,
		StateHash: "sha256:abc", CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		NextAction: "wire the resume path", Outstanding: 2, CriticalFailures: 1,
	}
	theirs := model.HandoffNode{ID: "handoff_theirs", TaskID: other.ID, StateHash: "sha256:def"}
	for _, h := range []model.HandoffNode{mine, theirs} {
		if err := f.PutHandoff(ctx, h); err != nil {
			t.Fatalf("PutHandoff %q: %v", h.ID, err)
		}
	}

	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(l.Handoffs) != 1 || l.Handoffs[0].ID != "handoff_mine" {
		t.Fatalf("handoffs = %+v, want only handoff_mine", l.Handoffs)
	}
	if l.Handoffs[0].Outstanding != 2 || l.Handoffs[0].CriticalFailures != 1 {
		t.Errorf("receipt counts lost: %+v", l.Handoffs[0])
	}
	if l.Task.ID != testTaskID {
		t.Errorf("lineage task = %q, want %q", l.Task.ID, testTaskID)
	}
}

// TestLineageOfAnUntouchedTask is the honest-empty case: a task with no sessions
// has no checkpoint to resume from, and the caller must be able to see that
// rather than be handed an optimistic default (plan §48).
func TestLineageOfAnUntouchedTask(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(l.Sessions) != 0 || len(l.Checkpoints) != 0 || len(l.Handoffs) != 0 {
		t.Fatalf("lineage = %+v, want an empty graph", l)
	}
	if _, ok := l.LatestCheckpoint(); ok {
		t.Error("LatestCheckpoint reported a checkpoint on a task that has none")
	}
	if err := l.Validate(); err != nil {
		t.Errorf("empty lineage should still be structurally valid: %v", err)
	}
}

// TestPutSessionCannotClearInterrupted is the honesty rule at the boundary
// between the two write paths. The event fold treats interruption as sticky
// because a session cut off mid-flight never becomes a complete capture
// (plan §48); a wholesale PutSession from a caller that never saw the
// interruption must not quietly undo that and hand the resume path a session
// that reads as clean.
func TestPutSessionCannotClearInterrupted(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	if err := f.AppendEvent(ctx, model.AgentEvent{
		Type:      model.SessionInterrupted,
		Timestamp: at(6),
		TaskID:    testTaskID,
		SessionID: "sess_main",
		Agent:     model.AgentOpenClaw,
		Summary:   "user pressed stop",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// The Entire client states the node it knows about. It has no idea the
	// session was interrupted.
	if err := f.PutSession(ctx, testTaskID, model.SessionNode{
		SessionID: "sess_main",
		Agent:     model.AgentOpenClaw,
		Role:      model.RoleMain,
		StartedAt: at(0),
		EndedAt:   at(9),
	}); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	l, err := f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	got, ok := l.Session("sess_main")
	if !ok {
		t.Fatal("sess_main missing from lineage")
	}
	if !got.Interrupted {
		t.Error("PutSession erased a recorded interruption: an incomplete capture now reads as clean")
	}
	// Every other field is still a wholesale replace.
	if !got.EndedAt.Equal(at(9)) || got.Role != model.RoleMain {
		t.Errorf("PutSession did not replace the rest of the node: %+v", got)
	}

	// A fresh session is unaffected: the flag is sticky, not sticky-on.
	if err := f.PutSession(ctx, testTaskID, model.SessionNode{
		SessionID: "sess_clean", Agent: model.AgentHermes, StartedAt: at(10),
	}); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	l, err = f.Lineage(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if clean, _ := l.Session("sess_clean"); clean.Interrupted || !clean.Active() {
		t.Errorf("a session that was never interrupted was marked interrupted: %+v", clean)
	}
}
