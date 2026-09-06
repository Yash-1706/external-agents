package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the version of the EngineeringState wire format.
const SchemaVersion = 1

// ReqStatus is the requirement lifecycle from plan §10. A requirement is never
// promoted to ReqComplete without supporting evidence; see the validate package.
type ReqStatus string

const (
	ReqComplete   ReqStatus = "complete"
	ReqPartial    ReqStatus = "partial"
	ReqUnresolved ReqStatus = "unresolved"
	ReqBlocked    ReqStatus = "blocked"
	ReqUnknown    ReqStatus = "unknown"
)

// Valid reports whether s is a defined requirement status.
func (s ReqStatus) Valid() bool {
	switch s {
	case ReqComplete, ReqPartial, ReqUnresolved, ReqBlocked, ReqUnknown:
		return true
	}
	return false
}

// Glyph renders the compact status marker used in handoff and status output.
func (s ReqStatus) Glyph() string {
	switch s {
	case ReqComplete:
		return "✓"
	case ReqPartial:
		return "⚠"
	case ReqUnresolved:
		return "✗"
	case ReqBlocked:
		return "⛔"
	default:
		return "?"
	}
}

// Rank orders requirement statuses from weakest to strongest claim. Merge uses
// this to guarantee a requirement is never silently downgraded from a stronger
// evidence-backed conclusion by a weaker later observation.
func (s ReqStatus) Rank() int {
	switch s {
	case ReqUnknown:
		return 0
	case ReqUnresolved:
		return 1
	case ReqBlocked:
		return 2
	case ReqPartial:
		return 3
	case ReqComplete:
		return 4
	}
	return 0
}

// TestStatus is the outcome of a recorded test run.
type TestStatus string

const (
	TestPassed  TestStatus = "passed"
	TestFailed  TestStatus = "failed"
	TestSkipped TestStatus = "skipped"
	TestUnknown TestStatus = "unknown"
)

// Valid reports whether s is a defined test status.
func (s TestStatus) Valid() bool {
	switch s {
	case TestPassed, TestFailed, TestSkipped, TestUnknown:
		return true
	}
	return false
}

// Requirement is one extracted obligation of the task (plan §13).
type Requirement struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Status      ReqStatus  `json:"status"`
	Confidence  Confidence `json:"confidence"`
	// Source records where the requirement came from: the original prompt, a
	// discovered constraint, or a mid-task constraint such as the curveball.
	Source   string     `json:"source,omitempty"`
	Evidence []Evidence `json:"evidence"`
	// SupersededBy names a constraint that changed this requirement, if any.
	SupersededBy string    `json:"superseded_by,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// WorkItem describes a unit of completed or in-progress work.
type WorkItem struct {
	Description string     `json:"description"`
	Confidence  Confidence `json:"confidence"`
	Evidence    []Evidence `json:"evidence"`
	SessionID   string     `json:"session_id,omitempty"`
}

// Decision is a durable engineering choice with its reason and evidence (§14).
type Decision struct {
	ID       string     `json:"id"`
	Decision string     `json:"decision"`
	Reason   string     `json:"reason"`
	Evidence []Evidence `json:"evidence"`
	// SessionID and CheckpointID record where the decision was made, so a later
	// reader can trace it back to its origin.
	SessionID    string    `json:"session_id,omitempty"`
	CheckpointID string    `json:"checkpoint_id,omitempty"`
	MadeAt       time.Time `json:"made_at,omitempty"`
	// SupersededBy names the decision that replaced this one. Superseded
	// decisions are retained, never deleted (plan §32).
	SupersededBy string `json:"superseded_by,omitempty"`
}

// Active reports whether the decision still stands.
func (d Decision) Active() bool { return d.SupersededBy == "" }

// RejectedApproach records an approach that was tried and abandoned, so a later
// worker does not rediscover the same failure (plan §15).
type RejectedApproach struct {
	ID       string     `json:"id"`
	Approach string     `json:"approach"`
	Reason   string     `json:"reason"`
	Evidence []Evidence `json:"evidence"`
	// Confidence distinguishes an approach observed to fail from one inferred
	// to have been abandoned.
	Confidence   Confidence `json:"confidence"`
	SessionID    string     `json:"session_id,omitempty"`
	CheckpointID string     `json:"checkpoint_id,omitempty"`
	RejectedAt   time.Time  `json:"rejected_at,omitempty"`
}

// FailedAttempt records a concrete failure encountered during the task.
type FailedAttempt struct {
	Description string     `json:"description"`
	Evidence    []Evidence `json:"evidence"`
	SessionID   string     `json:"session_id,omitempty"`
	OccurredAt  time.Time  `json:"occurred_at,omitempty"`
}

// Assumption is something the task currently relies on that is not proven.
type Assumption struct {
	Statement  string     `json:"statement"`
	Confidence Confidence `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
	// Invalidated marks an assumption that later evidence contradicted.
	Invalidated bool `json:"invalidated,omitempty"`
}

// TestResult is a recorded test outcome (plan §16). A failing test stays failed
// until a later result proves otherwise.
type TestResult struct {
	Name     string     `json:"name"`
	Status   TestStatus `json:"status"`
	Command  string     `json:"command,omitempty"`
	Package  string     `json:"package,omitempty"`
	RanAt    time.Time  `json:"ran_at,omitempty"`
	Files    []string   `json:"files,omitempty"`
	Evidence []Evidence `json:"evidence"`
}

// ChangedFile is a file touched during the task, with its git status.
type ChangedFile struct {
	Path string `json:"path"`
	// Status is a short git-style code: M, A, D, R or "?" when unknown.
	Status   string     `json:"status,omitempty"`
	Insert   int        `json:"insertions,omitempty"`
	Delete   int        `json:"deletions,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Risk is a known hazard that the next worker should be aware of.
type Risk struct {
	Description string     `json:"description"`
	Severity    string     `json:"severity,omitempty"`
	Confidence  Confidence `json:"confidence"`
	Evidence    []Evidence `json:"evidence"`
}

// NextAction is a recommended continuation step. Its confidence is always
// Recommended: it is a proposal, never a statement of fact (plan §12).
type NextAction struct {
	Description string `json:"description"`
	Rationale   string `json:"rationale,omitempty"`
	// Target names the symbol or file the action operates on, which is what the
	// Graph verification layer resolves for impact analysis.
	Target     string     `json:"target,omitempty"`
	Priority   int        `json:"priority"`
	Confidence Confidence `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
	// GraphVerified records whether Graph impact analysis backed this action.
	GraphVerified bool `json:"graph_verified"`
	// SuggestedTests are the tests Graph identified as affected by this action.
	SuggestedTests []string `json:"suggested_tests,omitempty"`
}

// Constraint is a requirement introduced mid-task, such as the Buildathon
// Noon Curveball (plan §41). Constraints are additive: they never overwrite the
// original intent, they annotate which requirements and assumptions they affect.
type Constraint struct {
	ID      string    `json:"id"`
	Text    string    `json:"text"`
	Source  string    `json:"source"`
	AddedAt time.Time `json:"added_at"`
	// AffectedRequirements lists requirement IDs this constraint changes.
	AffectedRequirements []string `json:"affected_requirements,omitempty"`
	// ChangedAssumptions lists assumption statements this constraint invalidates.
	ChangedAssumptions []string   `json:"changed_assumptions,omitempty"`
	Evidence           []Evidence `json:"evidence,omitempty"`
}

// Capture records which inputs were actually available when the state was built.
// It is what lets the product degrade honestly instead of inventing detail when
// an external agent did not expose everything (plan §33 and §47).
type Capture struct {
	GitAvailable        bool `json:"git_available"`
	CheckpointAvailable bool `json:"checkpoint_available"`
	TranscriptAvailable bool `json:"transcript_available"`
	EventsAvailable     bool `json:"events_available"`
	GraphAvailable      bool `json:"graph_available"`
	TestResultsParsed   bool `json:"test_results_parsed"`
	SemanticExtraction  bool `json:"semantic_extraction"`
	// Missing names the inputs that were expected but unavailable, so the
	// renderer can say what it does not know instead of staying silent.
	Missing []string `json:"missing,omitempty"`
	// Notes carries human-readable explanations for degraded capture.
	Notes []string `json:"notes,omitempty"`
}

// Canonical names for the capture inputs recorded in Capture.Missing. They are
// constants here rather than in the package that first writes them so that the
// deriving side and the merging side cannot disagree about what a gap is
// called — a disagreement would leave a stale gap in Missing forever.
const (
	MissingGit         = "git"
	MissingCheckpoints = "checkpoints"
	MissingTranscript  = "transcript"
	MissingEvents      = "agent events"
	MissingTests       = "structured test results"
)

// missingFor pairs each capture flag with the name reported when it is false.
func (c Capture) missingFor() []struct {
	ok   bool
	name string
} {
	return []struct {
		ok   bool
		name string
	}{
		{c.GitAvailable, MissingGit},
		{c.CheckpointAvailable, MissingCheckpoints},
		{c.TranscriptAvailable, MissingTranscript},
		{c.EventsAvailable, MissingEvents},
		{c.TestResultsParsed, MissingTests},
	}
}

// Or combines two captures for a merged state.
//
// The flags are OR-ed because they describe which inputs contributed facts to
// the state, and a merged state contains the facts of both sides: if either
// half actually read git, the merged state genuinely carries git-derived
// evidence. Replacing the flags wholesale instead would let a contributor that
// observes nothing — the semantic extractor, which reads no repository — erase
// the record that the deterministic half did observe one, producing a state
// that reports "git unavailable" while displaying a commit sha it verified.
// Contradicting itself is the one thing this product must never do.
//
// Missing is filtered against the merged flags rather than blindly unioned, so
// a gap that one side has since filled does not survive as a phantom.
func (c Capture) Or(other Capture) Capture {
	out := Capture{
		GitAvailable:        c.GitAvailable || other.GitAvailable,
		CheckpointAvailable: c.CheckpointAvailable || other.CheckpointAvailable,
		TranscriptAvailable: c.TranscriptAvailable || other.TranscriptAvailable,
		EventsAvailable:     c.EventsAvailable || other.EventsAvailable,
		GraphAvailable:      c.GraphAvailable || other.GraphAvailable,
		TestResultsParsed:   c.TestResultsParsed || other.TestResultsParsed,
		SemanticExtraction:  c.SemanticExtraction || other.SemanticExtraction,
	}

	out.Missing = append(append([]string{}, c.Missing...), other.Missing...)
	seenNote := map[string]bool{}
	for _, n := range append(append([]string{}, c.Notes...), other.Notes...) {
		if seenNote[n] {
			continue
		}
		seenNote[n] = true
		out.Notes = append(out.Notes, n)
	}
	return out.PruneMissing()
}

// PruneMissing drops any recorded gap whose corresponding flag is now
// satisfied, and de-duplicates the rest.
//
// It exists because Missing and the flags are two views of the same fact, and a
// merge can satisfy a gap that an earlier state recorded. Without pruning, a
// state whose checkpoints were later found still carried "checkpoints" in
// Missing, and the rendered output listed entire checkpoints under *Verified*
// and again under *Unknown* — the reader cannot tell which half to believe, and
// a self-contradicting report is worse than either answer alone.
//
// Names this package does not own are preserved untouched: a contributor that
// records a gap of its own is entitled to keep it.
func (c Capture) PruneMissing() Capture {
	resolved := make(map[string]bool, 5)
	for _, f := range c.missingFor() {
		if f.ok {
			resolved[f.name] = true
		}
	}
	seen := make(map[string]bool, len(c.Missing))
	kept := make([]string, 0, len(c.Missing))
	for _, name := range c.Missing {
		if resolved[name] || seen[name] {
			continue
		}
		seen[name] = true
		kept = append(kept, name)
	}
	c.Missing = kept
	return c
}

// Complete reports whether every capture input was available.
func (c Capture) Complete() bool {
	return c.GitAvailable && c.CheckpointAvailable && c.EventsAvailable &&
		c.TranscriptAvailable && len(c.Missing) == 0
}

// RepoState is the deterministic git-derived portion of the state (plan §30).
type RepoState struct {
	Repo      string `json:"repo,omitempty"`
	Branch    string `json:"branch,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Dirty     bool   `json:"dirty"`
	// TreeHash fingerprints the contents of the files the task cares about,
	// which is what drift detection compares across time.
	TreeHash   string    `json:"tree_hash,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// TaskRef is the identity block embedded in a serialized state document.
type TaskRef struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	OriginalIntent string `json:"original_intent"`
}

// EngineeringState is the complete, evidence-backed engineering state of a task
// (plan §10). Both the human handoff and the machine handoff are rendered from
// this one object, which is what keeps them consistent (plan §24).
type EngineeringState struct {
	SchemaVersion int        `json:"schema_version"`
	Task          TaskRef    `json:"task"`
	Status        TaskStatus `json:"status"`

	Requirements   []Requirement      `json:"requirements"`
	CompletedWork  []WorkItem         `json:"completed_work"`
	InProgress     []WorkItem         `json:"in_progress"`
	FailedAttempts []FailedAttempt    `json:"failed_attempts"`
	Decisions      []Decision         `json:"decisions"`
	Rejected       []RejectedApproach `json:"rejected_approaches"`
	Assumptions    []Assumption       `json:"assumptions"`
	Tests          []TestResult       `json:"tests"`
	ChangedFiles   []ChangedFile      `json:"changed_files"`
	Risks          []Risk             `json:"risks"`
	NextActions    []NextAction       `json:"next_actions"`
	Constraints    []Constraint       `json:"constraints"`

	Sessions    []SessionNode    `json:"sessions"`
	Checkpoints []CheckpointNode `json:"checkpoints"`
	Evidence    []Evidence       `json:"evidence"`

	Repo    RepoState `json:"repo"`
	Capture Capture   `json:"capture"`

	GeneratedAt time.Time `json:"generated_at,omitempty"`
}

// NewEngineeringState returns an empty state with the current schema version and
// all slices initialised, so JSON output never contains nulls.
func NewEngineeringState(task TaskRef) *EngineeringState {
	return &EngineeringState{
		SchemaVersion:  SchemaVersion,
		Task:           task,
		Status:         StatusNew,
		Requirements:   []Requirement{},
		CompletedWork:  []WorkItem{},
		InProgress:     []WorkItem{},
		FailedAttempts: []FailedAttempt{},
		Decisions:      []Decision{},
		Rejected:       []RejectedApproach{},
		Assumptions:    []Assumption{},
		Tests:          []TestResult{},
		ChangedFiles:   []ChangedFile{},
		Risks:          []Risk{},
		NextActions:    []NextAction{},
		Constraints:    []Constraint{},
		Sessions:       []SessionNode{},
		Checkpoints:    []CheckpointNode{},
		Evidence:       []Evidence{},
	}
}

// Requirement returns the requirement with the given id.
func (s *EngineeringState) Requirement(id string) (Requirement, bool) {
	for _, r := range s.Requirements {
		if r.ID == id {
			return r, true
		}
	}
	return Requirement{}, false
}

// Test returns the recorded result for the named test.
func (s *EngineeringState) Test(name string) (TestResult, bool) {
	for _, t := range s.Tests {
		if t.Name == name {
			return t, true
		}
	}
	return TestResult{}, false
}

// CountRequirements returns how many requirements are complete and how many
// exist in total.
func (s *EngineeringState) CountRequirements() (complete, total int) {
	for _, r := range s.Requirements {
		total++
		if r.Status == ReqComplete {
			complete++
		}
	}
	return complete, total
}

// Outstanding returns the requirements that are not complete.
func (s *EngineeringState) Outstanding() []Requirement {
	var out []Requirement
	for _, r := range s.Requirements {
		if r.Status != ReqComplete {
			out = append(out, r)
		}
	}
	return out
}

// FailingTests returns every test currently recorded as failed.
func (s *EngineeringState) FailingTests() []TestResult {
	var out []TestResult
	for _, t := range s.Tests {
		if t.Status == TestFailed {
			out = append(out, t)
		}
	}
	return out
}

// ActiveDecisions returns decisions that have not been superseded.
func (s *EngineeringState) ActiveDecisions() []Decision {
	var out []Decision
	for _, d := range s.Decisions {
		if d.Active() {
			out = append(out, d)
		}
	}
	return out
}

// PrimaryNextAction returns the highest-priority recommended next action.
func (s *EngineeringState) PrimaryNextAction() (NextAction, bool) {
	if len(s.NextActions) == 0 {
		return NextAction{}, false
	}
	best := s.NextActions[0]
	for _, a := range s.NextActions[1:] {
		if a.Priority < best.Priority {
			best = a
		}
	}
	return best, true
}

// DeriveStatus computes the task status implied by the evidence, independent of
// whatever status is currently stored. A task with a failing test is at best
// blocked; a task with unresolved requirements is at best partial; complete
// requires every requirement complete and no failing test.
func (s *EngineeringState) DeriveStatus() TaskStatus {
	if len(s.Requirements) == 0 {
		if len(s.Sessions) == 0 {
			return StatusNew
		}
		return StatusActive
	}
	complete, total := s.CountRequirements()
	failing := len(s.FailingTests()) > 0
	blocked := false
	for _, r := range s.Requirements {
		if r.Status == ReqBlocked {
			blocked = true
		}
	}
	switch {
	case blocked:
		return StatusBlocked
	case failing:
		return StatusPartial
	case complete == total:
		return StatusVerified
	default:
		return StatusPartial
	}
}

// Sort normalises the ordering of every collection so that two states built
// from the same inputs serialize byte-identically. Hash depends on this.
func (s *EngineeringState) Sort() {
	sort.SliceStable(s.Requirements, func(i, j int) bool { return s.Requirements[i].ID < s.Requirements[j].ID })
	sort.SliceStable(s.Decisions, func(i, j int) bool { return s.Decisions[i].ID < s.Decisions[j].ID })
	sort.SliceStable(s.Rejected, func(i, j int) bool { return s.Rejected[i].ID < s.Rejected[j].ID })
	sort.SliceStable(s.Tests, func(i, j int) bool { return s.Tests[i].Name < s.Tests[j].Name })
	sort.SliceStable(s.ChangedFiles, func(i, j int) bool { return s.ChangedFiles[i].Path < s.ChangedFiles[j].Path })
	sort.SliceStable(s.Constraints, func(i, j int) bool { return s.Constraints[i].ID < s.Constraints[j].ID })
	sort.SliceStable(s.CompletedWork, func(i, j int) bool { return s.CompletedWork[i].Description < s.CompletedWork[j].Description })
	sort.SliceStable(s.InProgress, func(i, j int) bool { return s.InProgress[i].Description < s.InProgress[j].Description })
	sort.SliceStable(s.FailedAttempts, func(i, j int) bool { return s.FailedAttempts[i].Description < s.FailedAttempts[j].Description })
	sort.SliceStable(s.Assumptions, func(i, j int) bool { return s.Assumptions[i].Statement < s.Assumptions[j].Statement })
	sort.SliceStable(s.Risks, func(i, j int) bool { return s.Risks[i].Description < s.Risks[j].Description })
	sort.SliceStable(s.NextActions, func(i, j int) bool {
		if s.NextActions[i].Priority != s.NextActions[j].Priority {
			return s.NextActions[i].Priority < s.NextActions[j].Priority
		}
		return s.NextActions[i].Description < s.NextActions[j].Description
	})
	for i := range s.Requirements {
		SortEvidence(s.Requirements[i].Evidence)
	}
	for i := range s.Decisions {
		SortEvidence(s.Decisions[i].Evidence)
	}
	for i := range s.Rejected {
		SortEvidence(s.Rejected[i].Evidence)
	}
	for i := range s.Tests {
		SortEvidence(s.Tests[i].Evidence)
	}
	SortEvidence(s.Evidence)
	sort.Strings(s.Capture.Missing)
	sort.Strings(s.Capture.Notes)
	l := &Lineage{Sessions: s.Sessions, Checkpoints: s.Checkpoints}
	l.Sort()
	s.Sessions, s.Checkpoints = l.Sessions, l.Checkpoints
}

// Missing returns the capture gaps recorded on the state.
func (s *EngineeringState) Missing() []string { return s.Capture.Missing }

// Hash fingerprints the semantic content of the state. Timestamps that vary
// per run are excluded so the same engineering conclusion always hashes the
// same, which is what makes the handoff receipt state_hash meaningful.
func (s *EngineeringState) Hash() string {
	clone := *s
	clone.GeneratedAt = time.Time{}
	clone.Repo.ObservedAt = time.Time{}
	clone.Sort()
	b, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:32]
}

// Clone returns a deep copy of the state, so merge operations never mutate the
// caller's input.
func (s *EngineeringState) Clone() *EngineeringState {
	b, err := json.Marshal(s)
	if err != nil {
		return NewEngineeringState(s.Task)
	}
	var out EngineeringState
	if err := json.Unmarshal(b, &out); err != nil {
		return NewEngineeringState(s.Task)
	}
	return &out
}

// AllEvidence collects every evidence record referenced anywhere in the state.
func (s *EngineeringState) AllEvidence() []Evidence {
	var out []Evidence
	out = append(out, s.Evidence...)
	for _, r := range s.Requirements {
		out = append(out, r.Evidence...)
	}
	for _, d := range s.Decisions {
		out = append(out, d.Evidence...)
	}
	for _, r := range s.Rejected {
		out = append(out, r.Evidence...)
	}
	for _, t := range s.Tests {
		out = append(out, t.Evidence...)
	}
	for _, w := range s.CompletedWork {
		out = append(out, w.Evidence...)
	}
	for _, w := range s.InProgress {
		out = append(out, w.Evidence...)
	}
	for _, f := range s.FailedAttempts {
		out = append(out, f.Evidence...)
	}
	for _, a := range s.Assumptions {
		out = append(out, a.Evidence...)
	}
	for _, r := range s.Risks {
		out = append(out, r.Evidence...)
	}
	for _, n := range s.NextActions {
		out = append(out, n.Evidence...)
	}
	return DedupeEvidence(out)
}

// NormalizeID upper-cases and trims an identifier such as a requirement id, so
// that "r1", "R1 " and "R1" refer to the same requirement across extractions.
func NormalizeID(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
}
