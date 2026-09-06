package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// EvidenceKind enumerates the kinds of artifact an Evidence record can point at.
// See implementation plan §11 ("Evidence Must Be First-Class").
type EvidenceKind string

const (
	EvidenceCommit     EvidenceKind = "commit"
	EvidenceFile       EvidenceKind = "file"
	EvidenceTest       EvidenceKind = "test"
	EvidenceRuntime    EvidenceKind = "runtime"
	EvidenceGraph      EvidenceKind = "graph"
	EvidenceCheckpoint EvidenceKind = "checkpoint"
	EvidenceSession    EvidenceKind = "session"
	EvidenceEvent      EvidenceKind = "event"
	EvidencePrompt     EvidenceKind = "prompt"
	EvidenceInference  EvidenceKind = "inference"
)

// EvidenceLevel is the priority hierarchy from plan §31. Lower is stronger:
// a Level 1 repository fact always wins over a Level 4 model inference.
type EvidenceLevel int

const (
	LevelRepository EvidenceLevel = 1 // git / source / test results
	LevelCheckpoint EvidenceLevel = 2 // Entire session + checkpoint metadata
	LevelStatement  EvidenceLevel = 3 // explicit agent prompts / responses
	LevelInference  EvidenceLevel = 4 // derived model interpretation
)

func (l EvidenceLevel) String() string {
	switch l {
	case LevelRepository:
		return "repository"
	case LevelCheckpoint:
		return "checkpoint"
	case LevelStatement:
		return "statement"
	case LevelInference:
		return "inference"
	}
	return "unknown"
}

// Level maps an evidence kind onto the §31 priority hierarchy. Graph results are
// treated as Level 1 because they are computed from the source tree itself.
func (k EvidenceKind) Level() EvidenceLevel {
	switch k {
	case EvidenceCommit, EvidenceFile, EvidenceTest, EvidenceRuntime, EvidenceGraph:
		return LevelRepository
	case EvidenceCheckpoint, EvidenceSession:
		return LevelCheckpoint
	case EvidenceEvent, EvidencePrompt:
		return LevelStatement
	case EvidenceInference:
		return LevelInference
	}
	return LevelInference
}

// Evidence is a pointer to the concrete artifact that supports a claim. Every
// semantic claim in an EngineeringState should carry at least one of these.
type Evidence struct {
	Kind EvidenceKind `json:"kind"`
	// Ref is the primary identifier: checkpoint id, commit sha, test name,
	// session id, or file path depending on Kind.
	Ref        string    `json:"ref"`
	Path       string    `json:"path,omitempty"`
	LineStart  int       `json:"line_start,omitempty"`
	LineEnd    int       `json:"line_end,omitempty"`
	Result     string    `json:"result,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	CapturedAt time.Time `json:"captured_at,omitempty"`
}

// Level reports the §31 priority of this evidence record.
func (e Evidence) Level() EvidenceLevel { return e.Kind.Level() }

// Key is a stable identity used to de-duplicate evidence during state merges.
func (e Evidence) Key() string {
	return fmt.Sprintf("%s|%s|%s|%d|%d", e.Kind, e.Ref, e.Path, e.LineStart, e.LineEnd)
}

// String renders a compact human-readable citation.
func (e Evidence) String() string {
	var b strings.Builder
	b.WriteString(string(e.Kind))
	if e.Ref != "" {
		b.WriteString(" " + e.Ref)
	}
	if e.Path != "" {
		b.WriteString(" " + e.Path)
		if e.LineStart > 0 {
			if e.LineEnd > e.LineStart {
				fmt.Fprintf(&b, ":%d-%d", e.LineStart, e.LineEnd)
			} else {
				fmt.Fprintf(&b, ":%d", e.LineStart)
			}
		}
	}
	if e.Result != "" {
		b.WriteString(" (" + e.Result + ")")
	}
	return b.String()
}

// DedupeEvidence removes duplicates while preserving first-seen order.
func DedupeEvidence(in []Evidence) []Evidence {
	seen := make(map[string]bool, len(in))
	out := make([]Evidence, 0, len(in))
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

// StrongestLevel returns the best (numerically lowest) evidence level present.
// Returns LevelInference and false when the slice is empty.
func StrongestLevel(in []Evidence) (EvidenceLevel, bool) {
	if len(in) == 0 {
		return LevelInference, false
	}
	best := LevelInference
	for _, e := range in {
		if l := e.Level(); l < best {
			best = l
		}
	}
	return best, true
}

// SortEvidence orders evidence strongest-first, then by kind and ref, so that
// rendered output and serialized state are deterministic.
func SortEvidence(in []Evidence) {
	sort.SliceStable(in, func(i, j int) bool {
		li, lj := in[i].Level(), in[j].Level()
		if li != lj {
			return li < lj
		}
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		return in[i].Key() < in[j].Key()
	})
}

// Confidence encodes the fact/inference/unknown distinction from plan §12.
type Confidence string

const (
	// Observed is directly supported by repository, checkpoint, test or event evidence.
	Observed Confidence = "observed"
	// Inferred is a conclusion derived from observed evidence.
	Inferred Confidence = "inferred"
	// Unknown means the claim is not established.
	Unknown Confidence = "unknown"
	// Recommended marks a proposed next action rather than a statement of fact.
	Recommended Confidence = "recommended"
)

// Label renders the confidence in the uppercase form used by the CLI output.
func (c Confidence) Label() string { return strings.ToUpper(string(c)) }

// Valid reports whether c is one of the four defined confidence values.
func (c Confidence) Valid() bool {
	switch c {
	case Observed, Inferred, Unknown, Recommended:
		return true
	}
	return false
}

// ConfidenceFor derives the strongest defensible confidence for a claim given
// its supporting evidence. A claim with no evidence is never better than Unknown,
// and a claim supported only by model inference is never better than Inferred.
// This is the single chokepoint that enforces "prefer uncertainty over fabrication".
func ConfidenceFor(ev []Evidence) Confidence {
	level, ok := StrongestLevel(ev)
	if !ok {
		return Unknown
	}
	switch level {
	case LevelRepository, LevelCheckpoint:
		return Observed
	case LevelStatement:
		return Inferred
	default:
		return Inferred
	}
}
