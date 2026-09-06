package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// PutState records the latest merged engineering state for a task.
//
// The state is cloned and sorted before it is written. Cloning keeps the store
// from mutating a caller's live object, and sorting is what makes the file
// byte-identical for the same engineering conclusion, which is the same property
// model.EngineeringState.Hash depends on and therefore what makes a handoff
// receipt's state_hash comparable across machines (plan §25).
//
// A state whose TaskRef names a different task is rejected. Writing task A's
// conclusions into task B's slot would hand the next agent a confident, fully
// evidenced, entirely wrong picture of what it is resuming.
func (f *FS) PutState(ctx context.Context, taskID string, s *model.EngineeringState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return errors.New("store: refusing to record a nil state")
	}
	dir, err := f.requireTask(taskID)
	if err != nil {
		return err
	}
	if s.Task.ID != "" && s.Task.ID != taskID {
		return fmt.Errorf("store: state names task %q but is being written to task %q", s.Task.ID, taskID)
	}

	out := s.Clone()
	out.Sort()
	return writeJSON(filepath.Join(dir, stateFileName), out)
}

// State returns the latest merged state for a task.
//
// model.ErrNotFound covers both "no such task" and "this task has no state yet".
// The second is the ordinary case for a task that has only just been created,
// and the caller must treat it as "nothing is known yet" rather than reaching
// for a default: an empty state presented as a real one is precisely the
// fabrication the product refuses (plan §48).
func (f *FS) State(ctx context.Context, taskID string) (*model.EngineeringState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dir, err := f.requireTask(taskID)
	if err != nil {
		return nil, err
	}
	var s model.EngineeringState
	if err := readJSON(filepath.Join(dir, stateFileName), &s); err != nil {
		return nil, fmt.Errorf("store: state for task %q: %w", taskID, err)
	}
	return &s, nil
}
