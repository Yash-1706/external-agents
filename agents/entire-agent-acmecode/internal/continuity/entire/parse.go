package entire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// wireCheckpoint is the one JSON shape this build accepts from the entire
// binary.
//
// One shape, not several. A parser that quietly accepts many layouts makes a
// malformed payload indistinguishable from a valid one, and a checkpoint we
// misread is worse than a checkpoint we admit we cannot read: the first is
// presented to the next agent as fact, the second is recorded as a gap
// (plan §33, §48).
type wireCheckpoint struct {
	ID        string                  `json:"id"`
	TaskID    string                  `json:"task_id"`
	SessionID string                  `json:"session_id"`
	Label     string                  `json:"label"`
	CommitSHA string                  `json:"commit_sha"`
	Branch    string                  `json:"branch"`
	CreatedAt time.Time               `json:"created_at"`
	Agent     model.AgentKind         `json:"agent"`
	Meta      map[string]string       `json:"meta"`
	State     *model.EngineeringState `json:"state"`
}

// parseCheckpoint turns one JSON object printed by the binary into a
// Checkpoint. It is pure so the wire contract can be pinned by tests over
// sample payloads without an entire binary anywhere near them.
//
// A literal "null" is the binary saying the checkpoint does not exist, which is
// model.ErrNotFound - genuinely different from the binary being unreachable.
func parseCheckpoint(b []byte) (model.Checkpoint, error) {
	trimmedPayload := bytes.TrimSpace(b)
	if len(trimmedPayload) == 0 {
		return model.Checkpoint{}, errors.New("empty checkpoint payload")
	}

	var w *wireCheckpoint
	if err := json.Unmarshal(trimmedPayload, &w); err != nil {
		return model.Checkpoint{}, fmt.Errorf("malformed checkpoint payload: %w", err)
	}
	if w == nil {
		return model.Checkpoint{}, fmt.Errorf("checkpoint payload is null: %w", model.ErrNotFound)
	}
	return w.toModel()
}

// parseCheckpointList turns a JSON array of checkpoint objects into a
// oldest-first slice, as model.EntireClient promises.
//
// An empty array and a literal "null" both mean the task has no checkpoints,
// which is a legitimate answer and not an error; an unreadable element is an
// error, because dropping it would understate the history.
func parseCheckpointList(b []byte) ([]model.Checkpoint, error) {
	trimmedPayload := bytes.TrimSpace(b)
	if len(trimmedPayload) == 0 {
		return nil, errors.New("empty checkpoint list payload")
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(trimmedPayload, &raw); err != nil {
		return nil, fmt.Errorf("malformed checkpoint list payload: %w", err)
	}
	if len(raw) == 0 {
		// Distinct from nil-with-error: the binary answered, and the answer is
		// "none". Callers must still not read that as "nothing happened".
		return []model.Checkpoint{}, nil
	}

	out := make([]model.Checkpoint, 0, len(raw))
	for i, item := range raw {
		cp, err := parseCheckpoint(item)
		if err != nil {
			// %v, not %w: an element-level failure must not carry
			// model.ErrNotFound out of a list call. A null element is a
			// malformed list, and a caller testing errors.Is(err, ErrNotFound)
			// would otherwise read a broken payload as the legitimate answer
			// "this task has no checkpoints" - a contract violation upgraded
			// into a fact about the task (plan §33).
			return nil, fmt.Errorf("checkpoint list entry %d: %v", i, err)
		}
		out = append(out, cp)
	}
	sortCheckpoints(out)
	return out, nil
}

// toModel validates a decoded payload and projects it onto the port's type.
func (w *wireCheckpoint) toModel() (model.Checkpoint, error) {
	if w.ID == "" {
		// A checkpoint that cannot name itself cannot be cited as evidence
		// (plan §11), so it is refused rather than carried along anonymously.
		return model.Checkpoint{}, errors.New("checkpoint payload has no id")
	}
	if w.CreatedAt.IsZero() {
		// Ordering is part of the contract; without a timestamp "oldest first"
		// would be a guess dressed up as a fact.
		return model.Checkpoint{}, fmt.Errorf("checkpoint %q has no created_at", w.ID)
	}
	if err := checkStateSchema(w.State); err != nil {
		return model.Checkpoint{}, fmt.Errorf("checkpoint %q: %w", w.ID, err)
	}
	return model.Checkpoint{
		ID:        w.ID,
		SessionID: w.SessionID,
		TaskID:    w.TaskID,
		Label:     w.Label,
		CommitSHA: w.CommitSHA,
		Branch:    w.Branch,
		// Normalised to UTC so a binary reporting +05:30 and one reporting Z
		// render and hash identically.
		CreatedAt: w.CreatedAt.UTC(),
		Agent:     normalizeAgent(w.Agent),
		Meta:      copyMeta(w.Meta),
		State:     w.State,
	}, nil
}
