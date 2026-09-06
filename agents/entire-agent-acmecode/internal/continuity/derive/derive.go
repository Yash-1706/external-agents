// Package derive builds the deterministic half of an EngineeringState: the facts
// that follow from system data alone — git state, changed files, session
// lineage, checkpoints, structured test results and mid-task constraints
// (plan §30, Step 9).
//
// Nothing here interprets. Requirements, decisions, rejected approaches,
// assumptions and next actions are the semantic layer's job and a state
// produced by this package deliberately carries none of them. Keeping the two
// halves apart is what lets the product tell a reader which part of a handoff
// is observed fact and which part is model inference (plan §12, §31).
//
// Every input is optional. A missing repository, an empty event stream or an
// adapter that never reported a checkpoint degrades the result and is recorded
// in Capture; it is never returned as an error. Continuity infrastructure must
// not be fatal to the agent whose work it is describing (plan §48, Rule 8).
package derive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Attribute keys carried on model.AgentEvent.Attrs. They are the contract
// between the adapters and this package. An adapter that cannot supply one
// omits it, and the corresponding fact is reported as unknown rather than
// guessed (plan §33).
const (
	attrCheckpointID    = "checkpoint_id"
	attrCheckpointLabel = "label"
	attrCommitSHA       = "commit_sha"
	attrBranch          = "branch"
	attrStateHash       = "state_hash"

	attrTestName    = "test_name"
	attrTestStatus  = "test_status"
	attrTestCommand = "test_command"
	attrTestPackage = "test_package"

	attrConstraintID     = "constraint_id"
	attrConstraintText   = "constraint_text"
	attrConstraintSource = "constraint_source"
)

// Human-readable names for the capture gaps recorded in Capture.Missing. The
// renderer prints these verbatim, so they are phrased for a reader rather than
// for a machine (plan §33).
const (
	missingGit         = "git"
	missingCheckpoints = "checkpoints"
	missingTranscript  = "transcript"
	missingEvents      = "agent events"
	missingTests       = "structured test results"
)

// Input is everything the deterministic derivation is allowed to read.
//
// Repo may be nil and Events may be empty; both cases degrade rather than fail.
// Clock is mandatory because no code in this system may call time.Now directly:
// two runs over the same inputs must be byte-identical.
type Input struct {
	// Task is the identity block copied onto the resulting state. When its ID
	// is empty it is filled from TaskID.
	Task model.TaskRef
	// TaskID selects which events belong to this task. When it is empty the
	// scope falls back to Task.ID, so a caller who identified the task in one
	// field alone still gets that task's timeline rather than everyone's.
	// Only when both are empty is every well-formed event accepted.
	TaskID string
	// Events is the normalized agent event stream, in any order. It is never
	// mutated: the derivation sorts a copy.
	Events []model.AgentEvent
	// Repo reads Level 1 repository facts. May be nil.
	Repo model.RepoInspector
	// BaseRef is the ref changed files are computed against. Empty means
	// "working tree versus HEAD" (see model.RepoInspector).
	BaseRef string
	// Clock supplies the single observation timestamp stamped on the result.
	Clock model.Clock
}

// State derives the deterministic portion of the engineering state for a task.
//
// It returns an error only for problems that are the caller's own: a missing
// Clock, or a cancelled context. Every external gap — no repository, an
// unavailable repository, a truncated event stream — is recorded in Capture and
// the derivation continues, because a continuity layer that refuses to answer
// when git is missing is worse than one that answers honestly (plan §48).
//
// The returned state contains no Requirements, Decisions, Rejected approaches,
// Assumptions or NextActions. Those require interpretation and belong to the
// semantic layer (plan §30).
func State(ctx context.Context, in Input) (*model.EngineeringState, error) {
	if in.Clock == nil {
		return nil, errors.New("derive: a model.Clock is required; deterministic output may not depend on time.Now")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("derive: %w", err)
	}

	// One observation instant for the whole derivation. Taking the clock once
	// keeps the result independent of how many times the code happens to ask.
	now := in.Clock.Now()

	task := in.Task
	if task.ID == "" {
		task.ID = in.TaskID
	}
	s := model.NewEngineeringState(task)
	s.GeneratedAt = now

	// Either identity field scopes the stream. A state whose Task.ID says one
	// task while its timeline quietly contains another task's sessions is a
	// fabricated history, so the scope is taken from whichever field the caller
	// filled (plan §7).
	scope := in.TaskID
	if scope == "" {
		scope = in.Task.ID
	}
	events, notes := selectEvents(scope, in.Events)
	s.Capture.Notes = append(s.Capture.Notes, notes...)

	sessions, sessionNotes := deriveSessions(events)
	s.Sessions = sessions
	s.Capture.Notes = append(s.Capture.Notes, sessionNotes...)

	checkpoints, checkpointNotes := deriveCheckpoints(events)
	s.Checkpoints = checkpoints
	s.Capture.Notes = append(s.Capture.Notes, checkpointNotes...)

	tests, testNotes := deriveTests(events)
	s.Tests = tests
	s.Capture.Notes = append(s.Capture.Notes, testNotes...)

	constraints, constraintNotes := deriveConstraints(events)
	s.Constraints = constraints
	s.Capture.Notes = append(s.Capture.Notes, constraintNotes...)

	repo, err := deriveRepo(ctx, in.Repo, in.BaseRef, now)
	if err != nil {
		return nil, err
	}
	s.Repo = repo.state
	s.ChangedFiles = repo.files
	s.Capture.Notes = append(s.Capture.Notes, repo.notes...)

	// Level 1 and Level 2 evidence for the facts whose model types carry no
	// Evidence field of their own (plan §31).
	s.Evidence = append(s.Evidence, repo.evidence...)
	s.Evidence = append(s.Evidence, sessionEvidence(s.Sessions)...)
	s.Evidence = append(s.Evidence, checkpointEvidence(s.Checkpoints)...)
	s.Evidence = model.DedupeEvidence(s.Evidence)

	// Recover the intent from the stream only when the caller did not already
	// carry one. A caller-supplied intent came from the task record, which is
	// the authoritative statement; the stream is the fallback for a task created
	// by ingestion, where nobody typed one in.
	if strings.TrimSpace(s.Task.OriginalIntent) == "" {
		s.Task.OriginalIntent = originalIntentFrom(events)
	}

	s.Capture.GitAvailable = repo.available
	s.Capture.EventsAvailable = len(events) > 0
	s.Capture.CheckpointAvailable = len(s.Checkpoints) > 0
	s.Capture.TranscriptAvailable = anyPayloadRef(events)
	s.Capture.TestResultsParsed = len(s.Tests) > 0
	// GraphAvailable and SemanticExtraction stay false: this package performs
	// neither, and claiming otherwise would be exactly the fabrication the
	// product exists to remove (plan §48, Rule 7).
	s.Capture.Missing = append(s.Capture.Missing, missingNames(s.Capture)...)

	// A retained unknown event is a record the adapter could not interpret. It
	// is stored so the timeline is not silently short, but the reader has to be
	// told, or a partially-understood transcript renders exactly like a fully
	// understood one — the failure this product exists to prevent (plan §33).
	if n := countUnknownEvents(events); n > 0 {
		s.Capture.TranscriptAvailable = false
		s.Capture.Notes = append(s.Capture.Notes, fmt.Sprintf(
			"%d event(s) used a lifecycle name this build does not map; they are kept in the "+
				"timeline as unknown records, so this task's history is incomplete", n))
		s.Capture.Missing = append(s.Capture.Missing, "complete agent lifecycle")
	}

	// Status is derived from the evidence rather than asserted, so a
	// deterministic-only state never over-claims progress (plan §17).
	s.Status = s.DeriveStatus()

	s.Sort()
	return s, nil
}

// missingNames lists the human-readable name of every capture input this
// package is responsible for and did not get.
func missingNames(c model.Capture) []string {
	var out []string
	if !c.GitAvailable {
		out = append(out, missingGit)
	}
	if !c.CheckpointAvailable {
		out = append(out, missingCheckpoints)
	}
	if !c.TranscriptAvailable {
		out = append(out, missingTranscript)
	}
	if !c.EventsAvailable {
		out = append(out, missingEvents)
	}
	if !c.TestResultsParsed {
		out = append(out, missingTests)
	}
	return out
}

// selectEvents narrows the raw stream to well-formed events belonging to this
// task, then orders and de-duplicates them.
//
// The caller's slice is copied before sorting so replaying a fixture never
// reorders the fixture. Both kinds of drop are reported rather than silent:
// presenting a filtered timeline as a complete one is the dishonesty this
// product exists to remove (plan §33, Rule 7).
//
// Ordering and de-duplication both run on the full fingerprint rather than on
// model.AgentEvent.Key. Key covers type, session, payload ref, timestamp and
// summary but not Attrs, and model.SortEvents stops at (timestamp, session,
// type). Two events that differ only in what they report — two test results
// emitted in the same instant, two constraints added in the same turn — are
// therefore indistinguishable to both. Using Key to de-duplicate would delete
// one of them with no record, and leaving the sort tie to input order would let
// the caller's slice order pick which survives and change the state Hash.
func selectEvents(taskID string, in []model.AgentEvent) ([]model.AgentEvent, []string) {
	type keyed struct {
		ev model.AgentEvent
		fp string
	}
	kept := make([]keyed, 0, len(in))
	seen := make(map[string]bool, len(in))
	var malformed, foreign int
	for _, ev := range in {
		if err := ev.Validate(); err != nil {
			malformed++
			continue
		}
		if taskID != "" && ev.TaskID != taskID {
			foreign++
			continue
		}
		// A byte-identical repeat is an adapter replaying the same record and
		// carries no information, so it is dropped without a note. Anything
		// that differs in any field, Attrs included, is a distinct observation
		// and is kept.
		fp := eventFingerprint(ev)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		kept = append(kept, keyed{ev: ev, fp: fp})
	}

	// The primary ordering is model.SortEvents': timestamp, then session, then
	// type. The fingerprint tail only breaks ties model leaves to input order.
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i].ev, kept[j].ev
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return kept[i].fp < kept[j].fp
	})

	events := make([]model.AgentEvent, len(kept))
	for i, k := range kept {
		events[i] = k.ev
	}

	var notes []string
	if malformed > 0 {
		notes = append(notes, fmt.Sprintf("ignored %d malformed agent event(s); the timeline is incomplete", malformed))
	}
	if foreign > 0 {
		notes = append(notes, fmt.Sprintf("ignored %d agent event(s) belonging to another task", foreign))
	}
	return events, notes
}

// eventFingerprint renders every field of an event, Attrs included, as one
// canonical string. Attribute keys are sorted, so the fingerprint never depends
// on Go's map iteration order and two runs over the same input always agree.
func eventFingerprint(ev model.AgentEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		ev.Type, ev.Timestamp.UTC().UnixNano(), ev.TaskID, ev.SessionID,
		ev.ParentSessionID, ev.Agent, ev.Role, ev.PayloadRef, ev.Summary)
	keys := make([]string, 0, len(ev.Attrs))
	for k := range ev.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("\x00")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(ev.Attrs[k])
	}
	return b.String()
}

// anyPayloadRef reports whether any event points at a durable transcript
// artifact. Events inline only a redaction-safe summary, so a PayloadRef is the
// only proof that the underlying transcript still exists (plan §34).
func anyPayloadRef(events []model.AgentEvent) bool {
	for _, ev := range events {
		if ev.PayloadRef != "" {
			return true
		}
	}
	return false
}
