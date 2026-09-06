package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// EventType enumerates the normalized agent lifecycle events from plan §9.
// Adapters map their host agent's real lifecycle onto these and nothing else;
// an adapter must never invent a lifecycle semantic its host does not expose.
type EventType string

const (
	SessionStarted     EventType = "SessionStarted"
	SessionEnded       EventType = "SessionEnded"
	SessionInterrupted EventType = "SessionInterrupted"
	TurnStarted        EventType = "TurnStarted"
	TurnEnded          EventType = "TurnEnded"
	ToolUsed           EventType = "ToolUsed"
	SubagentStarted    EventType = "SubagentStarted"
	SubagentEnded      EventType = "SubagentEnded"
	CheckpointCreated  EventType = "CheckpointCreated"
	TaskResumed        EventType = "TaskResumed"
	HandoffCreated     EventType = "HandoffCreated"
	// ConstraintAdded carries a new requirement injected mid-task, such as the
	// Buildathon Noon Curveball (plan §41). It never overwrites original intent.
	ConstraintAdded EventType = "ConstraintAdded"

	// EventUnknown is a record a host emitted whose lifecycle name this build
	// does not map. It is a first-class event rather than a dropped one.
	//
	// Skipping such records silently was the original design, and it was wrong
	// once host formats started changing underneath us: a timeline missing three
	// records it could not read still renders as a clean, complete timeline, and
	// the reader has no way to know. Retaining the record — with its original
	// kind in Attrs["raw_kind"] — keeps the gap visible without inventing a
	// lifecycle semantic the host never exposed, which plan §9 forbids.
	EventUnknown EventType = "Unknown"
)

// AllEventTypes lists every normalized event type, used for validation.
func AllEventTypes() []EventType {
	return []EventType{
		SessionStarted, SessionEnded, SessionInterrupted,
		TurnStarted, TurnEnded, ToolUsed,
		SubagentStarted, SubagentEnded,
		CheckpointCreated, TaskResumed, HandoffCreated, ConstraintAdded,
		EventUnknown,
	}
}

// AttrRawKind holds the host's own name for a record mapped to EventUnknown.
const AttrRawKind = "raw_kind"

// AttrAgentName holds the runtime name a host reported for itself when that
// name is not one this build integrates with directly.
const AttrAgentName = "agent_name"

// Valid reports whether t is a known normalized event type.
func (t EventType) Valid() bool {
	for _, k := range AllEventTypes() {
		if k == t {
			return true
		}
	}
	return false
}

// AgentKind identifies which external agent runtime produced an event.
type AgentKind string

const (
	AgentOpenClaw AgentKind = "openclaw"
	AgentHermes   AgentKind = "hermes"
	AgentHuman    AgentKind = "human"
	AgentUnknown  AgentKind = "unknown"
)

// Valid reports whether a is usable as an agent identity.
//
// The four constants above are the runtimes this build integrates with deeply,
// but the set of runtimes is not closed: a host can rename itself or a new one
// can appear, and a transcript naming a runtime we have never heard of is still
// a real transcript. Coercing such a runtime to AgentUnknown would discard the
// one fact the host stated plainly about itself.
//
// So any safe identifier token is accepted. This is a strict widening — every
// value that validated before still validates — which is what makes it safe to
// change a rule three packages depend on (normalize, derive and the store all
// call AgentEvent.Validate). The restriction that remains is on shape, not
// membership: an agent name is used in output and as a map key, so it must be a
// short, lower-case, printable token and nothing else.
func (a AgentKind) Valid() bool {
	switch a {
	case AgentOpenClaw, AgentHermes, AgentHuman, AgentUnknown:
		return true
	}
	if len(a) == 0 || len(a) > 32 {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_' || c == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}

// NormalizeAgent turns a host-reported runtime name into an AgentKind, mapping
// the runtimes we integrate with onto their constants and otherwise keeping the
// host's own name in a safe form. It returns AgentUnknown only when the name
// cannot be made into a usable token at all.
func NormalizeAgent(raw string) AgentKind {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return AgentUnknown
	}
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			if b.Len() > 0 {
				b.WriteRune('-')
			}
		}
	}
	candidate := AgentKind(strings.Trim(b.String(), "-"))
	if len(candidate) > 32 {
		candidate = candidate[:32]
	}
	if !candidate.Valid() {
		return AgentUnknown
	}
	return candidate
}

// Display returns the presentation name used in lineage and handoff output.
// A runtime this build does not know is shown under its own reported name
// rather than as "Unknown", because that is what the host actually said.
func (a AgentKind) Display() string {
	switch a {
	case AgentOpenClaw:
		return "OpenClaw"
	case AgentHermes:
		return "Hermes"
	case AgentHuman:
		return "Human"
	case AgentUnknown, "":
		return "Unknown"
	}
	parts := strings.Split(string(a), "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// Role is the function a session performs within a task. Sub-agents carry the
// role assigned by their parent; a main session carries RoleMain.
type Role string

const (
	RoleMain       Role = "main"
	RolePlanning   Role = "planning"
	RoleCoding     Role = "coding"
	RoleTest       Role = "test"
	RoleReview     Role = "review"
	RoleResearch   Role = "research"
	RoleBackground Role = "background"
	RoleHuman      Role = "human"
)

// AgentEvent is the single normalized representation that every adapter emits.
// OpenClaw and Hermes both funnel into this type so the task-state engine stays
// agent-independent (plan §29).
type AgentEvent struct {
	Type            EventType `json:"type"`
	Timestamp       time.Time `json:"timestamp"`
	TaskID          string    `json:"task_id"`
	SessionID       string    `json:"session_id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	Agent           AgentKind `json:"agent"`
	Role            Role      `json:"role,omitempty"`

	// PayloadRef points at the durable artifact backing this event rather than
	// inlining it, so large or secret-bearing payloads are never copied wholesale
	// into task state (plan §34).
	PayloadRef string `json:"payload_ref,omitempty"`

	// Summary is a short, redaction-safe description. Adapters must not place
	// raw tool output or credential-bearing text here.
	Summary string `json:"summary,omitempty"`

	// Attrs carries a small set of scalar, adapter-specific fields such as
	// tool_name, checkpoint_id, test_command, exit_code or constraint_source.
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Attr returns the named attribute, or "" when absent.
func (e AgentEvent) Attr(key string) string {
	if e.Attrs == nil {
		return ""
	}
	return e.Attrs[key]
}

// WithAttr returns a copy of the event carrying an additional attribute.
func (e AgentEvent) WithAttr(key, value string) AgentEvent {
	next := make(map[string]string, len(e.Attrs)+1)
	for k, v := range e.Attrs {
		next[k] = v
	}
	next[key] = value
	e.Attrs = next
	return e
}

// Key is a stable identity used to de-duplicate events replayed from adapters.
func (e AgentEvent) Key() string {
	return fmt.Sprintf("%s|%s|%s|%d|%s",
		e.Type, e.SessionID, e.PayloadRef, e.Timestamp.UTC().UnixNano(), e.Summary)
}

// Validate reports why an event cannot be accepted, or nil when it is well formed.
func (e AgentEvent) Validate() error {
	if !e.Type.Valid() {
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	if strings.TrimSpace(e.SessionID) == "" {
		return fmt.Errorf("event %s: session_id is required", e.Type)
	}
	if strings.TrimSpace(e.TaskID) == "" {
		return fmt.Errorf("event %s: task_id is required", e.Type)
	}
	if !e.Agent.Valid() {
		return fmt.Errorf("event %s: unknown agent %q", e.Type, e.Agent)
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("event %s: timestamp is required", e.Type)
	}
	if e.ParentSessionID != "" && e.ParentSessionID == e.SessionID {
		return fmt.Errorf("event %s: session %q cannot be its own parent", e.Type, e.SessionID)
	}
	return nil
}

// AsEvidence converts the event into a Level 3 evidence citation.
func (e AgentEvent) AsEvidence() Evidence {
	return Evidence{
		Kind:       EvidenceEvent,
		Ref:        string(e.Type),
		Detail:     e.Summary,
		SessionID:  e.SessionID,
		CapturedAt: e.Timestamp,
	}
}

// SortEvents orders events chronologically, breaking ties on session and type
// so that replaying a fixture is fully deterministic.
func SortEvents(in []AgentEvent) {
	sort.SliceStable(in, func(i, j int) bool {
		if !in[i].Timestamp.Equal(in[j].Timestamp) {
			return in[i].Timestamp.Before(in[j].Timestamp)
		}
		if in[i].SessionID != in[j].SessionID {
			return in[i].SessionID < in[j].SessionID
		}
		return in[i].Type < in[j].Type
	})
}

// DedupeEvents removes replayed duplicates while preserving order.
func DedupeEvents(in []AgentEvent) []AgentEvent {
	seen := make(map[string]bool, len(in))
	out := make([]AgentEvent, 0, len(in))
	for _, e := range in {
		k := e.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}
