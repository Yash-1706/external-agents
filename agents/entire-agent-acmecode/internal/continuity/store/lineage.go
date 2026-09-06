package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// PutSession records a session node directly, replacing any node already stored
// under that id.
//
// Unlike the event fold this is a wholesale replace: the caller assembled a
// complete node and is stating it, so the store does not second-guess which of
// its fields were meant to be empty.
//
// Interrupted is the single exception. foldLifecycle records it from a
// SessionInterrupted event and treats it as sticky, because a session that was
// cut off had its state captured mid-flight and no later observation makes that
// capture complete (plan §48). A caller assembling a node from its own view of
// the world normally has no idea the interruption happened, so honouring a false
// here would flip an incomplete capture back to SessionNode.Active() and hand the
// resume path a session that reads as clean. Setting the flag still works; only
// clearing an already-recorded interruption does not.
func (f *FS) PutSession(ctx context.Context, taskID string, s model.SessionNode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := f.requireTask(taskID)
	if err != nil {
		return err
	}
	if err := validID(s.SessionID); err != nil {
		return fmt.Errorf("store: session id: %w", err)
	}
	// The session index is maintained here as well as in AppendEvent. A session
	// added straight to the lineage but missing from the index would be invisible
	// to ResolveTask, so a later agent announcing that session id would fail to
	// find the task it belongs to.
	if err := f.bindSession(s.SessionID, taskID); err != nil {
		return err
	}
	path, err := join(dir, sessionsDirName, s.SessionID+".json")
	if err != nil {
		return err
	}
	if !s.Interrupted {
		var prev model.SessionNode
		if err := readJSON(path, &prev); err != nil && !errors.Is(err, model.ErrNotFound) {
			return err
		}
		s.Interrupted = prev.Interrupted
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: create %q: %w", filepath.Dir(path), err)
	}
	return writeJSON(path, s)
}

// PutCheckpoint records a checkpoint node directly, replacing any node already
// stored under that id.
//
// The node is an index entry pointing at Entire, which remains the authoritative
// history (plan §5). The store does not verify that CheckpointNode.SessionID is
// a session of this task; model.Lineage.Validate exists for that, and running it
// here would turn a reportable inconsistency into an unreadable lineage.
func (f *FS) PutCheckpoint(ctx context.Context, taskID string, c model.CheckpointNode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := f.requireTask(taskID)
	if err != nil {
		return err
	}
	if err := validID(c.CheckpointID); err != nil {
		return fmt.Errorf("store: checkpoint id: %w", err)
	}
	path, err := join(dir, checkpointsDirName, c.CheckpointID+".json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: create %q: %w", filepath.Dir(path), err)
	}
	return writeJSON(path, c)
}

// PutHandoff records a handoff receipt.
//
// Handoffs live in one directory keyed by their own id rather than under the
// task, because a receipt is the durable proof that a transfer happened and must
// stay readable by id alone (plan §25). The owning task must exist: a receipt
// pointing at a task the store cannot produce is a claim nobody can verify.
func (f *FS) PutHandoff(ctx context.Context, h model.HandoffNode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := validID(h.ID); err != nil {
		return fmt.Errorf("store: handoff id: %w", err)
	}
	if _, err := f.requireTask(h.TaskID); err != nil {
		return err
	}
	path, err := join(f.root, handoffsDirName, h.ID+".json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: create %q: %w", filepath.Dir(path), err)
	}
	return writeJSON(path, h)
}

// Lineage assembles the cross-agent graph of a task: which sessions worked on
// it, which checkpoints they left, and which handoffs moved it between workers
// (plan §26).
//
// The result is sorted but not validated. model.Lineage.Validate reports
// structural problems such as a checkpoint attributed to an unknown session, and
// that is a finding the caller should surface to the user, not an error that
// hides the rest of the graph.
func (f *FS) Lineage(ctx context.Context, taskID string) (model.Lineage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return model.Lineage{}, err
	}

	dir, err := f.requireTask(taskID)
	if err != nil {
		return model.Lineage{}, err
	}
	t, err := f.loadTask(taskID)
	if err != nil {
		return model.Lineage{}, err
	}

	sessions, err := readNodes[model.SessionNode](filepath.Join(dir, sessionsDirName))
	if err != nil {
		return model.Lineage{}, err
	}
	checkpoints, err := readNodes[model.CheckpointNode](filepath.Join(dir, checkpointsDirName))
	if err != nil {
		return model.Lineage{}, err
	}
	all, err := readNodes[model.HandoffNode](filepath.Join(f.root, handoffsDirName))
	if err != nil {
		return model.Lineage{}, err
	}
	handoffs := make([]model.HandoffNode, 0, len(all))
	for _, h := range all {
		if h.TaskID == taskID {
			handoffs = append(handoffs, h)
		}
	}

	l := model.Lineage{
		Task:        t,
		Sessions:    sessions,
		Checkpoints: checkpoints,
		Handoffs:    handoffs,
	}
	l.Sort()
	return l, nil
}
