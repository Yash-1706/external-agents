package model

import (
	"context"
	"errors"
	"time"
)

// ErrUnavailable is returned by any port whose backing system is not reachable.
// Callers must record the gap in Capture and degrade the output honestly rather
// than substituting a guess (plan §48).
var ErrUnavailable = errors.New("capability unavailable")

// ErrNotFound is returned when a requested task, checkpoint or session does not
// exist. It is distinct from ErrUnavailable: the system worked, the thing is
// simply absent.
var ErrNotFound = errors.New("not found")

// Clock supplies the current time. Every timestamp in the system flows through
// a Clock so that fixtures and golden files are fully deterministic.
type Clock interface {
	Now() time.Time
}

// Checkpoint is an Entire checkpoint as seen by the continuity layer. State is
// the engineering state captured at that point, when Entire carried one.
type Checkpoint struct {
	ID        string            `json:"id"`
	SessionID string            `json:"session_id"`
	TaskID    string            `json:"task_id,omitempty"`
	Label     string            `json:"label,omitempty"`
	CommitSHA string            `json:"commit_sha,omitempty"`
	Branch    string            `json:"branch,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Agent     AgentKind         `json:"agent,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	State     *EngineeringState `json:"state,omitempty"`
}

// CheckpointRequest describes a checkpoint to create.
type CheckpointRequest struct {
	TaskID    string
	SessionID string
	Label     string
	Agent     AgentKind
	Message   string
	State     *EngineeringState
	Meta      map[string]string
}

// EntireClient is the port onto Entire, the authoritative development-history
// layer. The continuity layer never becomes a competing source of truth: it
// reads and writes checkpoints through this interface (plan §5).
//
// Every method may return ErrUnavailable. No caller may treat that as fatal to
// the host agent's own work (plan §48, Rule 8).
type EntireClient interface {
	// Available reports whether Entire is reachable right now.
	Available(ctx context.Context) bool
	// CreateCheckpoint records a milestone and returns the created checkpoint.
	CreateCheckpoint(ctx context.Context, req CheckpointRequest) (Checkpoint, error)
	// Checkpoint loads a single checkpoint by id.
	Checkpoint(ctx context.Context, id string) (Checkpoint, error)
	// Checkpoints lists every checkpoint belonging to a task, oldest first.
	Checkpoints(ctx context.Context, taskID string) ([]Checkpoint, error)
	// Describe returns a short human-readable description of the backing
	// implementation, used in output so the user always knows which Entire
	// they are talking to.
	Describe() string
}

// GraphSymbol is a definition located by Entire Graph.
type GraphSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Path      string `json:"path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// GraphImpact is the relationship and blast-radius analysis for a symbol.
type GraphImpact struct {
	Target GraphSymbol `json:"target"`
	// Callers are the symbols that invoke the target directly.
	Callers []GraphSymbol `json:"callers"`
	// Dependents are the higher-level components affected transitively.
	Dependents []string `json:"dependents"`
	// Tests are candidate tests that exercise the target or its callers.
	Tests []string `json:"tests"`
	// Files lists the source files involved in the impact set.
	Files []string `json:"files"`
}

// SemanticChange is one entry in a Graph semantic diff.
type SemanticChange struct {
	Symbol string `json:"symbol"`
	Path   string `json:"path,omitempty"`
	// Change is one of added, removed, modified or moved.
	Change string `json:"change"`
	Detail string `json:"detail,omitempty"`
}

// GraphClient is the port onto Entire Graph. Graph is not the product: it is
// the verification layer wrapped around continuation (plan §35). Raw Graph
// output is never surfaced directly; callers turn it into a decision.
type GraphClient interface {
	Available(ctx context.Context) bool
	// Search resolves a next-action target to concrete definitions.
	Search(ctx context.Context, query string) ([]GraphSymbol, error)
	// Impact returns relationship and blast-radius analysis for a symbol.
	Impact(ctx context.Context, symbol string) (GraphImpact, error)
	// SemanticDiff compares two revisions and returns the semantic changes.
	SemanticDiff(ctx context.Context, fromRef, toRef string) ([]SemanticChange, error)
	Describe() string
}

// RepoInspector reads deterministic facts out of the working repository. These
// are Level 1 evidence and outrank every model inference (plan §31).
type RepoInspector interface {
	Head(ctx context.Context) (RepoState, error)
	// ChangedFiles lists files modified relative to the given base ref. An empty
	// base means "working tree versus HEAD".
	ChangedFiles(ctx context.Context, baseRef string) ([]ChangedFile, error)
	// FileHash returns a content fingerprint for one path, used by drift
	// detection to tell whether a specific relevant file moved underneath us.
	FileHash(ctx context.Context, path string) (string, error)
	// Exists reports whether a path is present in the working tree.
	Exists(ctx context.Context, path string) bool
}

// ExtractionInput is everything the semantic extractor is allowed to read. It
// deliberately excludes raw tool payloads so that credential-bearing output
// never reaches a model or the persisted state (plan §34).
type ExtractionInput struct {
	Task           TaskRef
	OriginalPrompt string
	Events         []AgentEvent
	Deterministic  *EngineeringState
	Transcript     []string
}

// Extractor produces the model-assisted portion of the state: intent,
// requirements, decisions, rejected approaches and next actions (plan §30).
//
// An Extractor is never the canonical source of truth (Rule 6). Its output is
// merged underneath deterministic facts and every claim it makes must carry
// evidence or be downgraded by the validate package.
type Extractor interface {
	Available(ctx context.Context) bool
	Extract(ctx context.Context, in ExtractionInput) (*EngineeringState, error)
	Describe() string
}

// EventSink receives normalized events from an adapter.
type EventSink interface {
	Emit(ctx context.Context, ev AgentEvent) error
}

// Store persists tasks, lineage, events and states locally. It is an index over
// Entire history, not a replacement for it: everything durable enough to matter
// is also written to a checkpoint.
type Store interface {
	CreateTask(ctx context.Context, t Task) error
	UpdateTask(ctx context.Context, t Task) error
	Task(ctx context.Context, id string) (Task, error)
	// ResolveTask finds a task by id, by an owning session id, or by the
	// repo+branch pair when id is empty.
	ResolveTask(ctx context.Context, ref, repo, branch string) (Task, error)
	Tasks(ctx context.Context) ([]Task, error)

	AppendEvent(ctx context.Context, ev AgentEvent) error
	Events(ctx context.Context, taskID string) ([]AgentEvent, error)

	PutSession(ctx context.Context, taskID string, s SessionNode) error
	PutCheckpoint(ctx context.Context, taskID string, c CheckpointNode) error
	PutHandoff(ctx context.Context, h HandoffNode) error
	Lineage(ctx context.Context, taskID string) (Lineage, error)

	// PutState records the latest merged state for a task.
	PutState(ctx context.Context, taskID string, s *EngineeringState) error
	// State returns the latest merged state, or ErrNotFound.
	State(ctx context.Context, taskID string) (*EngineeringState, error)
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is a deterministic Clock for tests and fixtures. Successive calls
// advance by Step, so ordering is stable without being all-identical.
type FixedClock struct {
	Current time.Time
	Step    time.Duration
}

// Now returns the current fixed time and advances it by Step.
func (c *FixedClock) Now() time.Time {
	now := c.Current
	if c.Step > 0 {
		c.Current = c.Current.Add(c.Step)
	}
	return now.UTC()
}
