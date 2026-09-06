package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TaskStatus is the task state machine from plan §17.
type TaskStatus string

const (
	StatusNew      TaskStatus = "new"
	StatusActive   TaskStatus = "active"
	StatusPartial  TaskStatus = "partial"
	StatusBlocked  TaskStatus = "blocked"
	StatusResumed  TaskStatus = "resumed"
	StatusVerified TaskStatus = "verified"
	StatusComplete TaskStatus = "complete"
)

// Valid reports whether s is a defined task status.
func (s TaskStatus) Valid() bool {
	switch s {
	case StatusNew, StatusActive, StatusPartial, StatusBlocked, StatusResumed, StatusVerified, StatusComplete:
		return true
	}
	return false
}

// Terminal reports whether the task has reached a finished state.
func (s TaskStatus) Terminal() bool { return s == StatusComplete }

// allowedTransitions encodes the §17 state machine. Transitions outside this
// map are rejected so state cannot silently jump from partial to complete.
var allowedTransitions = map[TaskStatus][]TaskStatus{
	StatusNew:      {StatusActive},
	StatusActive:   {StatusPartial, StatusBlocked, StatusVerified, StatusComplete},
	StatusPartial:  {StatusBlocked, StatusResumed, StatusActive},
	StatusBlocked:  {StatusPartial, StatusResumed, StatusActive},
	StatusResumed:  {StatusActive, StatusPartial, StatusBlocked, StatusVerified},
	StatusVerified: {StatusComplete, StatusPartial, StatusBlocked, StatusActive},
	StatusComplete: {StatusPartial, StatusActive},
}

// CanTransition reports whether from -> to is a legal task status transition.
func CanTransition(from, to TaskStatus) bool {
	if from == to {
		return true
	}
	for _, ok := range allowedTransitions[from] {
		if ok == to {
			return true
		}
	}
	return false
}

// Task is the stable, agent-independent identity of a unit of software work
// (plan §7). Sessions and checkpoints are lineage nodes belonging to a task.
type Task struct {
	ID            string     `json:"task_id"`
	Repo          string     `json:"repo"`
	Branch        string     `json:"branch"`
	RootSessionID string     `json:"root_session_id"`
	Title         string     `json:"title"`
	Status        TaskStatus `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// NewTaskID derives a deterministic task identifier from repository, branch and
// root session. Determinism matters: the same task resolves to the same id
// across agents and machines, which is what makes cross-agent lineage work.
func NewTaskID(repo, branch, rootSession string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{repo, branch, rootSession}, "\x00")))
	return "task_" + hex.EncodeToString(sum[:])[:16]
}

// SessionNode records one agent session participating in a task.
type SessionNode struct {
	SessionID       string    `json:"session_id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	Agent           AgentKind `json:"agent"`
	Role            Role      `json:"role,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at,omitempty"`
	// Interrupted marks a session that stopped without a clean end event; its
	// captured state is treated as incomplete rather than final.
	Interrupted bool `json:"interrupted,omitempty"`
}

// Active reports whether the session has neither ended nor been interrupted.
func (s SessionNode) Active() bool { return s.EndedAt.IsZero() && !s.Interrupted }

// CheckpointNode records an Entire checkpoint belonging to a task.
type CheckpointNode struct {
	CheckpointID string    `json:"checkpoint_id"`
	SessionID    string    `json:"session_id"`
	Agent        AgentKind `json:"agent"`
	Label        string    `json:"label,omitempty"`
	CommitSHA    string    `json:"commit_sha,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// StateHash fingerprints the EngineeringState captured at this checkpoint.
	StateHash string `json:"state_hash,omitempty"`
}

// HandoffNode records a transfer of the task between workers (plan §25).
type HandoffNode struct {
	ID               string    `json:"id"`
	TaskID           string    `json:"task_id"`
	FromSessionID    string    `json:"from_session,omitempty"`
	FromAgent        AgentKind `json:"from_agent,omitempty"`
	ToSessionID      string    `json:"to_session,omitempty"`
	ToAgent          AgentKind `json:"to_agent,omitempty"`
	SourceCheckpoint string    `json:"source_checkpoint,omitempty"`
	StateHash        string    `json:"state_hash"`
	CreatedAt        time.Time `json:"created_at"`
	NextAction       string    `json:"next_action,omitempty"`
	// Outstanding counts requirements not yet complete at handoff time.
	Outstanding int `json:"outstanding"`
	// CriticalFailures counts failing tests at handoff time.
	CriticalFailures int `json:"critical_failures"`
}

// Lineage is the full cross-agent graph of a task (plan §26 and §51).
type Lineage struct {
	Task        Task             `json:"task"`
	Sessions    []SessionNode    `json:"sessions"`
	Checkpoints []CheckpointNode `json:"checkpoints"`
	Handoffs    []HandoffNode    `json:"handoffs"`
}

// Sort orders every lineage collection chronologically for deterministic output.
func (l *Lineage) Sort() {
	sort.SliceStable(l.Sessions, func(i, j int) bool {
		if !l.Sessions[i].StartedAt.Equal(l.Sessions[j].StartedAt) {
			return l.Sessions[i].StartedAt.Before(l.Sessions[j].StartedAt)
		}
		return l.Sessions[i].SessionID < l.Sessions[j].SessionID
	})
	sort.SliceStable(l.Checkpoints, func(i, j int) bool {
		if !l.Checkpoints[i].CreatedAt.Equal(l.Checkpoints[j].CreatedAt) {
			return l.Checkpoints[i].CreatedAt.Before(l.Checkpoints[j].CreatedAt)
		}
		return l.Checkpoints[i].CheckpointID < l.Checkpoints[j].CheckpointID
	})
	sort.SliceStable(l.Handoffs, func(i, j int) bool {
		if !l.Handoffs[i].CreatedAt.Equal(l.Handoffs[j].CreatedAt) {
			return l.Handoffs[i].CreatedAt.Before(l.Handoffs[j].CreatedAt)
		}
		return l.Handoffs[i].ID < l.Handoffs[j].ID
	})
}

// Session returns the named session node, or false when it is not part of the task.
func (l *Lineage) Session(id string) (SessionNode, bool) {
	for _, s := range l.Sessions {
		if s.SessionID == id {
			return s, true
		}
	}
	return SessionNode{}, false
}

// LatestCheckpoint returns the most recent checkpoint, or false when the task
// has none. Callers must treat "no checkpoint" as "cannot claim safe resume"
// rather than falling back to an optimistic default (plan §48).
func (l *Lineage) LatestCheckpoint() (CheckpointNode, bool) {
	l.Sort()
	if len(l.Checkpoints) == 0 {
		return CheckpointNode{}, false
	}
	return l.Checkpoints[len(l.Checkpoints)-1], true
}

// Children returns the sessions delegated directly from the given parent.
func (l *Lineage) Children(parentID string) []SessionNode {
	var out []SessionNode
	for _, s := range l.Sessions {
		if s.ParentSessionID == parentID {
			out = append(out, s)
		}
	}
	return out
}

// Agents lists the distinct agent runtimes that have worked on the task, in
// first-seen order. A task touched by more than one is the observable proof of
// cross-agent continuity (plan §44).
func (l *Lineage) Agents() []AgentKind {
	l.Sort()
	seen := map[AgentKind]bool{}
	var out []AgentKind
	for _, s := range l.Sessions {
		if !seen[s.Agent] {
			seen[s.Agent] = true
			out = append(out, s.Agent)
		}
	}
	return out
}

// Validate checks the lineage for structural problems such as a checkpoint
// attributed to a session that is not part of the task.
func (l *Lineage) Validate() error {
	ids := map[string]bool{}
	for _, s := range l.Sessions {
		if ids[s.SessionID] {
			return fmt.Errorf("duplicate session %q in lineage", s.SessionID)
		}
		ids[s.SessionID] = true
	}
	for _, s := range l.Sessions {
		if s.ParentSessionID != "" && !ids[s.ParentSessionID] {
			return fmt.Errorf("session %q references unknown parent %q", s.SessionID, s.ParentSessionID)
		}
	}
	for _, c := range l.Checkpoints {
		if c.SessionID != "" && !ids[c.SessionID] {
			return fmt.Errorf("checkpoint %q references unknown session %q", c.CheckpointID, c.SessionID)
		}
	}
	return nil
}
