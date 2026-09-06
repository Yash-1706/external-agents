package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// AppendEvent records one normalized agent event and folds its lifecycle meaning
// into the task's lineage.
//
// The call is idempotent on model.AgentEvent.Key(). Adapters replay: a hook that
// fires twice, a transcript re-scanned after a crash, or a session resumed from
// the middle of its own log will all re-emit events that are already stored.
// Appending them again would inflate the evidence base with duplicates that look
// like extra work actually happening (plan §29).
//
// The lifecycle fold runs on every call, including a duplicate. It is idempotent
// too, so replaying a log repairs a lineage that was left incomplete by a process
// that died between writing the event line and writing the session node.
func (f *FS) AppendEvent(ctx context.Context, ev model.AgentEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := ev.Validate(); err != nil {
		return fmt.Errorf("store: reject event: %w", err)
	}
	dir, err := f.requireTask(ev.TaskID)
	if err != nil {
		return err
	}
	if err := validID(ev.SessionID); err != nil {
		return fmt.Errorf("store: session id: %w", err)
	}
	// Every id this event will turn into a path is checked before the first
	// write, so a rejected event leaves the store exactly as it found it. Doing
	// this inside the fold instead would let a crafted checkpoint id still
	// register its session in the index on the way past.
	cpID, err := checkpointIDOf(ev)
	if err != nil {
		return err
	}

	// The existing log is read before anything is written, for the same reason:
	// a log this store cannot fully read is a log it must not append to, because
	// it can no longer tell whether this event is already in there.
	path := filepath.Join(dir, eventsFileName)
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: read %q: %w", path, err)
	}
	seen, err := eventKeys(raw, path)
	if err != nil {
		return err
	}

	// The index goes first among the writes: whatever else happens, the session
	// that produced this event is resolvable back to its task, which is what a
	// fresh agent needs in order to find the work it is continuing (plan §8).
	if err := f.bindSession(ev.SessionID, ev.TaskID); err != nil {
		return err
	}
	if err := f.foldLifecycle(dir, ev, cpID); err != nil {
		return err
	}
	if seen[ev.Key()] {
		return nil
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("store: encode event: %w", err)
	}
	next := make([]byte, 0, len(raw)+len(line)+2)
	next = append(next, raw...)
	if len(next) > 0 && next[len(next)-1] != '\n' {
		// Defensive: a log that somehow lost its final newline must not have the
		// next event fused onto the end of the previous one.
		next = append(next, '\n')
	}
	next = append(next, line...)
	next = append(next, '\n')

	// The whole log is rewritten through a rename rather than opened O_APPEND.
	// Idempotency already forces a full read on every call, so the rewrite costs
	// nothing extra asymptotically, and it buys crash-atomicity: the log is
	// either the old set of events or the old set plus this one, never a torn
	// final line.
	return writeAtomic(path, next)
}

// Events returns every event recorded for a task, in the canonical order defined
// by model.SortEvents.
//
// The order is normalised rather than left as arrival order because adapters
// replay out of order; two agents that saw the same events must derive the same
// state from them, and sorting here is what makes that true regardless of who
// wrote first.
func (f *FS) Events(ctx context.Context, taskID string) ([]model.AgentEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dir, err := f.requireTask(taskID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, eventsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A task with no events is a task nobody has reported work on yet.
			// That is a fact worth returning cleanly, not an error.
			return []model.AgentEvent{}, nil
		}
		return nil, fmt.Errorf("store: read %q: %w", path, err)
	}
	out, err := decodeEvents(raw, path)
	if err != nil {
		return nil, err
	}
	sortEventsTotal(out)
	return out, nil
}

// sortEventsTotal orders events canonically and, unlike model.SortEvents alone,
// totally.
//
// model.SortEvents compares timestamp, then session, then type, and is a stable
// sort, so events that tie on all three come back in whatever order they happen
// to sit in the file — which is arrival order. Adapters that stamp at second
// granularity produce those ties routinely (two ToolUsed events in one session
// inside the same second), and two agents replaying the same events in different
// orders would then read the log differently and derive different state. Sorting
// by model.AgentEvent.Key() first pins the ties: the key is the identity
// AppendEvent already de-duplicates on, so within one log it is unique, and the
// stable canonical sort layered on top leaves that tiebreak intact.
func sortEventsTotal(evs []model.AgentEvent) {
	type keyed struct {
		key string
		ev  model.AgentEvent
	}
	// Keys are computed once rather than inside the comparator, which would
	// re-render every event O(n log n) times.
	tmp := make([]keyed, len(evs))
	for i, e := range evs {
		tmp[i] = keyed{key: e.Key(), ev: e}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].key < tmp[j].key })
	for i := range tmp {
		evs[i] = tmp[i].ev
	}
	model.SortEvents(evs)
}

// decodeEvents parses an NDJSON log.
//
// A line that will not decode aborts the read. Skipping it would return a log
// that silently omits work the agent actually did, and a caller cannot tell an
// incomplete history from a short one; an error at least names the damage
// (plan §33).
func decodeEvents(raw []byte, path string) ([]model.AgentEvent, error) {
	lines := bytes.Split(raw, []byte("\n"))
	out := make([]model.AgentEvent, 0, len(lines))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev model.AgentEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("store: %s line %d is not a decodable event: %w", filepath.Base(path), i+1, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// eventKeys returns the identity of every event already in the log.
func eventKeys(raw []byte, path string) (map[string]bool, error) {
	evs, err := decodeEvents(raw, path)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(evs))
	for _, e := range evs {
		seen[e.Key()] = true
	}
	return seen, nil
}

// checkpointIDOf returns the already-validated checkpoint id a CheckpointCreated
// event will produce a node for.
//
// An empty result is not an error. It means the adapter reported that a
// checkpoint happened but not which one: a checkpoint node with no id is a claim
// with nothing behind it, and the resume path treats checkpoints as the thing it
// is safe to resume from. The event is still recorded as evidence that something
// happened; capture degrades rather than inventing a node (plan §33, §48).
func checkpointIDOf(ev model.AgentEvent) (string, error) {
	if ev.Type != model.CheckpointCreated {
		return "", nil
	}
	id := ev.Attr("checkpoint_id")
	if id == "" {
		return "", nil
	}
	if err := validID(id); err != nil {
		return "", fmt.Errorf("store: checkpoint id: %w", err)
	}
	return id, nil
}

// foldLifecycle projects an event onto the task's lineage nodes. checkpointID is
// the result of checkpointIDOf for this event.
//
// Only the event types that carry lifecycle meaning produce a node. Everything
// else (turns, tool use, handoffs, constraints) stays in the event log, where it
// is still evidence, but it does not invent a session or a checkpoint that the
// agent never reported.
func (f *FS) foldLifecycle(taskDir string, ev model.AgentEvent, checkpointID string) error {
	switch ev.Type {
	case model.SessionStarted, model.SubagentStarted:
		return f.upsertSessionNode(taskDir, ev.SessionID, func(n *model.SessionNode) {
			if n.ParentSessionID == "" {
				n.ParentSessionID = ev.ParentSessionID
			}
			if !agentKnown(n.Agent) && agentKnown(ev.Agent) {
				n.Agent = ev.Agent
			}
			if n.Role == "" {
				n.Role = ev.Role
			}
			earliest(&n.StartedAt, ev.Timestamp)
		})

	case model.SessionEnded, model.SubagentEnded:
		return f.upsertSessionNode(taskDir, ev.SessionID, func(n *model.SessionNode) {
			if !agentKnown(n.Agent) && agentKnown(ev.Agent) {
				n.Agent = ev.Agent
			}
			earliest(&n.EndedAt, ev.Timestamp)
		})

	case model.SessionInterrupted:
		return f.upsertSessionNode(taskDir, ev.SessionID, func(n *model.SessionNode) {
			if !agentKnown(n.Agent) && agentKnown(ev.Agent) {
				n.Agent = ev.Agent
			}
			// Interruption is sticky. A session that was cut off once had its
			// state captured mid-flight, and no later event makes that capture
			// complete again (plan §48).
			n.Interrupted = true
		})

	case model.CheckpointCreated:
		if checkpointID == "" {
			// Unidentified checkpoint: see checkpointIDOf.
			return nil
		}
		return f.upsertCheckpointNode(taskDir, checkpointID, func(n *model.CheckpointNode) {
			if n.SessionID == "" {
				n.SessionID = ev.SessionID
			}
			if !agentKnown(n.Agent) && agentKnown(ev.Agent) {
				n.Agent = ev.Agent
			}
			// Optional detail. These attributes are absent for adapters that do
			// not expose them, and absent stays empty: an empty CommitSHA reads
			// as "unknown", never as "no commit".
			if n.Label == "" {
				n.Label = ev.Attr("label")
			}
			if n.CommitSHA == "" {
				n.CommitSHA = ev.Attr("commit_sha")
			}
			if n.Branch == "" {
				n.Branch = ev.Attr("branch")
			}
			earliest(&n.CreatedAt, ev.Timestamp)
		})
	}
	return nil
}

// upsertSessionNode merges changes into a session node, creating it when absent.
//
// Merging rather than replacing matters in both directions: a SessionEnded event
// that arrives before its SessionStarted must not blank out a start time written
// later, and an event fold must not overwrite the richer node an explicit
// PutSession already stored.
func (f *FS) upsertSessionNode(taskDir, sessionID string, apply func(*model.SessionNode)) error {
	if err := validID(sessionID); err != nil {
		return fmt.Errorf("store: session id: %w", err)
	}
	path, err := join(taskDir, sessionsDirName, sessionID+".json")
	if err != nil {
		return err
	}
	var n model.SessionNode
	if err := readJSON(path, &n); err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	n.SessionID = sessionID
	apply(&n)
	n.Agent = knownOrUnknown(n.Agent)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: create %q: %w", filepath.Dir(path), err)
	}
	return writeJSON(path, n)
}

// upsertCheckpointNode merges changes into a checkpoint node, creating it when
// absent. See upsertSessionNode for why this merges.
func (f *FS) upsertCheckpointNode(taskDir, checkpointID string, apply func(*model.CheckpointNode)) error {
	if err := validID(checkpointID); err != nil {
		return fmt.Errorf("store: checkpoint id: %w", err)
	}
	path, err := join(taskDir, checkpointsDirName, checkpointID+".json")
	if err != nil {
		return err
	}
	var n model.CheckpointNode
	if err := readJSON(path, &n); err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	n.CheckpointID = checkpointID
	apply(&n)
	n.Agent = knownOrUnknown(n.Agent)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: create %q: %w", filepath.Dir(path), err)
	}
	return writeJSON(path, n)
}

// earliest keeps the first moment there is evidence for.
//
// Folding must not depend on arrival order: adapters replay, and a resumed
// session can re-emit its own history out of sequence. Taking the earliest of
// the observed timestamps makes the fold commutative, so the same set of events
// always produces the same lineage no matter what order it is fed in.
func earliest(dst *time.Time, t time.Time) {
	if t.IsZero() {
		return
	}
	if dst.IsZero() || t.Before(*dst) {
		*dst = t
	}
}

// agentKnown reports whether an agent kind carries information. Both "" and
// model.AgentUnknown mean "nobody told us", so either may be upgraded by a later
// event that does know.
func agentKnown(a model.AgentKind) bool {
	return a != "" && a != model.AgentUnknown
}

// knownOrUnknown renders "nobody told us" as the value the model has for it.
//
// model.AgentEvent.Validate accepts model.AgentUnknown, so an adapter that
// cannot identify its own runtime says so in a way the model recognises. A node
// folded only from such events would otherwise be written with an empty
// AgentKind, which fails model.AgentKind.Valid and reads downstream as a missing
// field rather than as a reported unknown. The distinction is not cosmetic: it
// is the difference between "the agent told us it does not know" and "something
// dropped the value". A later event that does know still upgrades the node,
// because agentKnown treats AgentUnknown as no information.
func knownOrUnknown(a model.AgentKind) model.AgentKind {
	if a == "" {
		return model.AgentUnknown
	}
	return a
}
