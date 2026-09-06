// Package merge implements the incremental state merge (plan §31, §32 and §41).
//
// Merging is where the product stops fabricating. A merged state may only claim
// what one of its inputs could actually prove:
//
//   - a requirement is never promoted to complete on a claim alone, only on
//     repository- or checkpoint-level evidence (plan §32);
//   - a failing test stays failed until a strictly newer run proves resolution
//     (plan §16, §32);
//   - decisions and rejected approaches are historical knowledge and survive a
//     later capture that simply forgot them (plan §32, §15);
//   - when two inputs disagree, the stronger evidence level wins — including
//     when the stronger evidence makes the state look worse (plan §31,
//     "Level 1 wins").
//
// Every timestamp flows through the injected model.Clock and no map iteration
// order reaches the output, so two merges over the same inputs are
// byte-identical.
package merge

import (
	"fmt"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Options carries the ambient dependencies of a merge.
type Options struct {
	// Clock supplies the merge timestamp. A nil Clock means the merge stamps
	// nothing rather than reaching for the wall clock: a fabricated timestamp
	// would break fixture determinism and is not a fact we were handed.
	Clock model.Clock
}

// now returns the merge timestamp and whether one is available at all.
func (o Options) now() (time.Time, bool) {
	if o.Clock == nil {
		return time.Time{}, false
	}
	return o.Clock.Now(), true
}

// States merges next into prev and returns a new state. Neither input is
// mutated: both are cloned before anything is read out of them, so a caller can
// keep holding the previous state after a merge (plan §32).
//
// Either side may be nil. A nil side is treated as "nothing known from there",
// never as a reason to fail: an interrupted session that produced no new state
// must still leave the previous state usable (plan §48).
func States(prev, next *model.EngineeringState, opts Options) *model.EngineeringState {
	stamp, stamped := opts.now()

	// prevStatus is captured before any substitution so that a synthetic empty
	// prev does not manufacture a bogus "new -> partial" transition note below.
	prevStatus := model.TaskStatus("")
	hasNext := next != nil

	if prev != nil {
		prevStatus = prev.Status
		prev = prev.Clone()
	} else {
		prev = model.NewEngineeringState(model.TaskRef{})
	}
	if hasNext {
		next = next.Clone()
	} else {
		next = model.NewEngineeringState(prev.Task)
	}

	out := model.NewEngineeringState(mergeTaskRef(prev.Task, next.Task))

	// Identity-keyed collections. Requirements, decisions, rejected approaches
	// and constraints all match on model.NormalizeID so that case drift between
	// two extractions ("r1" versus "R1") cannot fork one item into two.
	//
	// Incoming requirements pass the §32 completion guard *before* the union, so
	// the rule holds for a requirement this merge is seeing for the first time as
	// well as for one it can weigh against a previous record. Whether an earlier
	// capture happened to mention a requirement cannot decide whether we believe
	// the claim that it is finished.
	out.Requirements = unionBy(prev.Requirements, guardCompletions(next.Requirements), requirementKey, mergeRequirement)
	out.Decisions = unionBy(prev.Decisions, next.Decisions, decisionKey, mergeDecision)
	out.Rejected = unionBy(prev.Rejected, next.Rejected, rejectedKey, mergeRejected)
	out.Constraints = unionBy(prev.Constraints, next.Constraints, constraintKey, mergeConstraint)

	// Tests match on the exact recorded name: a test name is an identifier of a
	// concrete run target, not prose, and must not be folded case-insensitively.
	out.Tests = unionBy(prev.Tests, next.Tests, testKey, mergeTest)

	// Description-keyed collections (plan §32: union, evidence merged).
	out.CompletedWork = unionBy(prev.CompletedWork, next.CompletedWork, workKey, mergeWorkItem)
	out.InProgress = unionBy(prev.InProgress, next.InProgress, workKey, mergeWorkItem)
	out.FailedAttempts = unionBy(prev.FailedAttempts, next.FailedAttempts, failedKey, mergeFailedAttempt)
	out.Risks = unionBy(prev.Risks, next.Risks, riskKey, mergeRisk)
	out.Assumptions = unionBy(prev.Assumptions, next.Assumptions, assumptionKey, mergeAssumption)

	// Lineage and raw evidence are pure unions: they are records of things that
	// happened, and a thing that happened does not stop having happened.
	out.Sessions = unionBy(prev.Sessions, next.Sessions, sessionKey, mergeSession)
	out.Checkpoints = unionBy(prev.Checkpoints, next.Checkpoints, checkpointKey, mergeCheckpoint)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)

	files, stale := mergeChangedFiles(prev, next, hasNext)
	out.ChangedFiles = files

	// NextActions are proposals about the future, not accumulated history, so a
	// fresh set replaces the old one wholesale. An empty incoming set is silence,
	// not a retraction, so the previous recommendation is kept.
	if len(next.NextActions) > 0 {
		out.NextActions = next.NextActions
	} else {
		out.NextActions = orEmpty(prev.NextActions)
	}

	// RepoState is a snapshot of the working tree, so the incoming observation
	// wins whenever there is one. When the incoming capture observed no repo at
	// all we keep the older observation rather than blanking the state.
	out.Repo = prev.Repo
	if repoObserved(next.Repo) {
		out.Repo = next.Repo
	}

	// The staleness annotation describes *this* merge's file list, so it is
	// re-derived rather than inherited. Carrying a previous merge's note forward
	// would have the state warn that a list it just refreshed from an available
	// git may be stale — a false statement of uncertainty is still a false
	// statement.
	out.Capture = mergeCapture(prev.Capture, next.Capture, hasNext)
	note := ""
	if stale {
		note = staleFilesNote
	}
	out.Capture.Notes = replaceDerivedNote(out.Capture.Notes, staleFilesNotePrefix, note)

	applyDerivedStatus(out, prevStatus)

	if stamped {
		out.GeneratedAt = stamp
	} else {
		// No clock: report the newest generation time we were actually given
		// rather than inventing one.
		out.GeneratedAt = laterTime(prev.GeneratedAt, next.GeneratedAt)
	}

	finalize(out)
	return out
}

// Notes this package authors about its own derivation. Each describes the state
// it is attached to right now, not something that happened once, so each is
// re-derived on every operation instead of being inherited: replaceDerivedNote
// drops the stale copy before the current one is appended. Without that, a
// one-off annotation would ride along forever and end up describing a state it
// no longer applies to.
//
// Both prefixes are namespaced to this package on purpose. replaceDerivedNote
// deletes by prefix, and Capture.Notes is a shared channel: a bare prefix such
// as "status: " could match a note some other stage of the pipeline wrote, and
// merge would quietly eat somebody else's honesty annotation.
const (
	notePrefix = "merge: "

	staleFilesNotePrefix = notePrefix + "changed files were retained"
	staleFilesNote       = staleFilesNotePrefix + " from the previous state: the incoming capture reported git unavailable, so the list may be stale (plan §48)"

	statusNotePrefix = notePrefix + "status "
)

// applyDerivedStatus recomputes the task status from the merged evidence.
//
// The stored status enum is only ever a summary of the evidence, and evidence
// outranks a summary (plan §31): if the merged state contains a failing test,
// the task is not "complete" no matter what enum a previous capture stored. When
// the resulting move is not one the §17 state machine declares, we still keep
// the derived value — the alternative is to render a status the evidence
// contradicts — and say so in Capture.Notes so the jump is visible rather than
// silently smoothed over.
//
// The note explains the status this state actually carries, so an inherited one
// from an earlier merge is replaced rather than kept beside it: two notes about
// two different transitions leave a reader unable to tell which describes the
// status in front of them.
func applyDerivedStatus(s *model.EngineeringState, prevStatus model.TaskStatus) {
	derived := s.DeriveStatus()
	s.Status = derived
	note := ""
	if prevStatus.Valid() && !model.CanTransition(prevStatus, derived) {
		note = fmt.Sprintf(
			"%s%q -> %q is not a declared transition (plan §17); kept the evidence-derived status",
			statusNotePrefix, prevStatus, derived)
	}
	s.Capture.Notes = replaceDerivedNote(s.Capture.Notes, statusNotePrefix, note)
}

// replaceDerivedNote drops every note this package previously derived under
// prefix and appends note when there is one, so a re-derived annotation replaces
// its predecessor instead of accumulating beside it. Order is otherwise
// preserved; the map-free loop keeps the result deterministic.
func replaceDerivedNote(notes []string, prefix, note string) []string {
	out := make([]string, 0, len(notes)+1)
	for _, n := range notes {
		if strings.HasPrefix(n, prefix) {
			continue
		}
		out = append(out, n)
	}
	if note != "" {
		out = append(out, note)
	}
	return out
}

// finalize deduplicates the capture annotations and normalises ordering so that
// repeated merges neither accumulate identical notes nor reorder anything.
func finalize(s *model.EngineeringState) {
	// A status outside the §10 lifecycle is not a claim, it is a hole. Plan §32
	// says to record unknown when the evidence is insufficient, and an empty enum
	// in the serialized document would read as a defect in the tool rather than a
	// gap in what we know. This is the last place the document is touched before
	// it is hashed and checkpointed, so it is where the enum is made valid.
	for i := range s.Requirements {
		s.Requirements[i].Status = normalizeReqStatus(s.Requirements[i].Status)
	}
	s.Capture.Missing = dedupeStrings(s.Capture.Missing)
	s.Capture.Notes = dedupeStrings(s.Capture.Notes)
	sortNestedEvidence(s)
	s.Sort()
}

// sortNestedEvidence canonicalises the evidence orderings that
// EngineeringState.Sort does not reach. Sort covers requirements, decisions,
// rejected approaches, tests and the top-level list; without this, two states
// carrying identical evidence that merely arrived in a different order would
// serialize differently and therefore hash differently — and that hash is the
// receipt a handoff is checked against.
func sortNestedEvidence(s *model.EngineeringState) {
	for i := range s.Assumptions {
		model.SortEvidence(s.Assumptions[i].Evidence)
	}
	for i := range s.CompletedWork {
		model.SortEvidence(s.CompletedWork[i].Evidence)
	}
	for i := range s.InProgress {
		model.SortEvidence(s.InProgress[i].Evidence)
	}
	for i := range s.FailedAttempts {
		model.SortEvidence(s.FailedAttempts[i].Evidence)
	}
	for i := range s.Risks {
		model.SortEvidence(s.Risks[i].Evidence)
	}
	for i := range s.NextActions {
		model.SortEvidence(s.NextActions[i].Evidence)
	}
	for i := range s.Constraints {
		model.SortEvidence(s.Constraints[i].Evidence)
	}
	for i := range s.ChangedFiles {
		model.SortEvidence(s.ChangedFiles[i].Evidence)
	}
}

// mergeTaskRef preserves identity instead of overwriting it. OriginalIntent in
// particular is the one field a later capture must never replace: plan §41 says
// a mid-task constraint annotates the task, it does not rewrite what was asked.
func mergeTaskRef(prev, next model.TaskRef) model.TaskRef {
	return model.TaskRef{
		ID:             firstNonEmpty(prev.ID, next.ID),
		Title:          firstNonEmpty(prev.Title, next.Title),
		OriginalIntent: firstNonEmpty(prev.OriginalIntent, next.OriginalIntent),
	}
}

// ---------------------------------------------------------------------------
// Requirements
// ---------------------------------------------------------------------------

func requirementKey(r model.Requirement) string { return identityKey(r.ID, r.Description) }

// mergeRequirement combines two records of the same requirement. The prose is
// taken from the previous record so that a re-worded extraction does not churn
// the document, while the status is re-decided from evidence by mergeReqStatus.
func mergeRequirement(prev, next model.Requirement) model.Requirement {
	out := prev
	out.Description = firstNonEmpty(prev.Description, next.Description)
	out.Source = firstNonEmpty(prev.Source, next.Source)
	out.Status = mergeReqStatus(prev, next)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	// The evidence set changed, so the confidence derived from it must be
	// recomputed; model.ConfidenceFor is the chokepoint that stops a claim from
	// outrunning what backs it (plan §12).
	out.Confidence = model.ConfidenceFor(out.Evidence)
	// SupersededBy is sticky for the same reason it is on decisions: retirement
	// is explicit, and a capture that forgot it does not undo it (plan §32).
	out.SupersededBy = firstNonEmpty(prev.SupersededBy, next.SupersededBy)
	out.UpdatedAt = laterTime(prev.UpdatedAt, next.UpdatedAt)
	return out
}

// mergeReqStatus decides the surviving status of one requirement.
//
// Three rules, in order:
//
//  1. plan §32 — a requirement is never marked complete on a claim alone. An
//     incoming "complete" carrying only statement- or inference-level evidence
//     is recorded as partial: work was claimed, not proven.
//  2. plan §31 — Level 1 wins. A repository fact outranks anything weaker even
//     when it downgrades the requirement. This is the whole point of the
//     hierarchy: the git tree does not have to agree with what a model said.
//  3. otherwise the higher-ranked status survives, so a weaker later
//     observation cannot silently erase a stronger earlier conclusion.
func mergeReqStatus(prev, next model.Requirement) model.ReqStatus {
	prevLevel, _ := model.StrongestLevel(prev.Evidence)
	nextLevel, _ := model.StrongestLevel(next.Evidence)

	// guardCompletions has already applied rule 1 to everything arriving through
	// States; applying it again here is a no-op, and it keeps mergeReqStatus
	// correct on its own terms for any other caller.
	incoming := guardCompletion(next).Status

	switch {
	case !incoming.Valid():
		// The incoming record states no usable status, so it moves nothing.
		return normalizeReqStatus(prev.Status)
	case nextLevel == model.LevelRepository && prevLevel > model.LevelRepository:
		return normalizeReqStatus(incoming)
	case incoming.Rank() > prev.Status.Rank():
		return normalizeReqStatus(incoming)
	default:
		return normalizeReqStatus(prev.Status)
	}
}

// guardCompletions applies the §32 completion guard to a whole incoming
// collection, returning a new slice so the caller's records are untouched.
func guardCompletions(in []model.Requirement) []model.Requirement {
	out := make([]model.Requirement, 0, len(in))
	for _, r := range in {
		out = append(out, guardCompletion(r))
	}
	return out
}

// guardCompletion downgrades a claimed completion that nothing proves.
//
// plan §32: a requirement is complete only when repository- or checkpoint-level
// evidence says so. "The agent told us it finished" is a statement about a
// conversation, not about the tree, so it is recorded as partial — work claimed,
// not work proven. Note that this is deliberately *not* conditioned on whether a
// previous state knew about the requirement: a first sighting is exactly when an
// unproven claim is easiest to smuggle in, since there is nothing to contradict
// it.
func guardCompletion(r model.Requirement) model.Requirement {
	if r.Status == model.ReqComplete && !provenComplete(r.Evidence) {
		r.Status = model.ReqPartial
	}
	return r
}

// provenComplete reports whether an evidence set can support a completion claim.
func provenComplete(ev []model.Evidence) bool {
	level, ok := model.StrongestLevel(ev)
	return ok && level <= model.LevelCheckpoint
}

// normalizeReqStatus maps an unset or unrecognised status onto ReqUnknown:
// plan §32 says to use unknown when the evidence is insufficient, and an empty
// enum in rendered output would read as a gap in the tool rather than a gap in
// what we know.
func normalizeReqStatus(s model.ReqStatus) model.ReqStatus {
	if !s.Valid() {
		return model.ReqUnknown
	}
	return s
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func testKey(t model.TestResult) string { return "test:" + t.Name }

// mergeTest keeps the governing run's outcome but the union of both runs'
// evidence: both runs really happened and both citations remain checkable.
func mergeTest(prev, next model.TestResult) model.TestResult {
	out := governingRun(prev, next)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	return out
}

// governingRun picks which recorded run of a test governs.
//
// plan §16 and §32: a failing test remains failed until a later result proves
// resolution. Two things have to be true of that later result. It must be
// strictly newer — so a newer pass clears a failure and a newer failure replaces
// an older pass — and it must actually say the test now passes. A newer skipped
// or unknown run reports that the test did not produce a verdict, which is not
// evidence that the failure went away; letting it govern would quietly drop a
// known failure out of FailingTests and off the handoff. When the timestamps
// cannot separate the two runs (both unstamped, or identical), the failure is
// again what survives: an unprovable resolution is not a resolution, and
// reporting a green test we cannot date would be exactly the fabrication this
// package exists to prevent.
func governingRun(prev, next model.TestResult) model.TestResult {
	var win, lose model.TestResult
	switch {
	case next.RanAt.After(prev.RanAt):
		win, lose = next, prev
	case prev.RanAt.After(next.RanAt):
		win, lose = prev, next
	case next.Status == model.TestFailed:
		win, lose = next, prev
	default:
		win, lose = prev, next
	}
	// Only a pass resolves a failure. A later run that failed again legitimately
	// governs (it carries the current failure detail); anything else leaves the
	// recorded failure standing.
	if lose.Status == model.TestFailed && win.Status != model.TestPassed && win.Status != model.TestFailed {
		return lose
	}
	return win
}

// ---------------------------------------------------------------------------
// Decisions, rejected approaches, assumptions
// ---------------------------------------------------------------------------

func decisionKey(d model.Decision) string { return identityKey(d.ID, d.Decision) }

// mergeDecision keeps the original wording and origin of a decision: it is a
// historical record (plan §14). SupersededBy is the only retirement mechanism
// (plan §32), so it is sticky — a later capture that forgot the supersession
// does not bring a retired decision back to life.
func mergeDecision(prev, next model.Decision) model.Decision {
	out := prev
	out.Decision = firstNonEmpty(prev.Decision, next.Decision)
	out.Reason = firstNonEmpty(prev.Reason, next.Reason)
	out.SessionID = firstNonEmpty(prev.SessionID, next.SessionID)
	out.CheckpointID = firstNonEmpty(prev.CheckpointID, next.CheckpointID)
	out.SupersededBy = firstNonEmpty(prev.SupersededBy, next.SupersededBy)
	out.MadeAt = earlierNonZero(prev.MadeAt, next.MadeAt)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	return out
}

func rejectedKey(r model.RejectedApproach) string { return identityKey(r.ID, r.Approach) }

// mergeRejected retains a rejected approach forever (plan §15): the value of the
// record is that a later worker does not rediscover the same dead end.
func mergeRejected(prev, next model.RejectedApproach) model.RejectedApproach {
	out := prev
	out.Approach = firstNonEmpty(prev.Approach, next.Approach)
	out.Reason = firstNonEmpty(prev.Reason, next.Reason)
	out.SessionID = firstNonEmpty(prev.SessionID, next.SessionID)
	out.CheckpointID = firstNonEmpty(prev.CheckpointID, next.CheckpointID)
	out.RejectedAt = earlierNonZero(prev.RejectedAt, next.RejectedAt)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	out.Confidence = model.ConfidenceFor(out.Evidence)
	return out
}

func assumptionKey(a model.Assumption) string { return "text:" + normalizeText(a.Statement) }

// mergeAssumption keeps assumptions and makes Invalidated sticky: once evidence
// has contradicted an assumption, a later state that omits the flag is missing
// information, not proof that the assumption came back.
func mergeAssumption(prev, next model.Assumption) model.Assumption {
	out := prev
	out.Invalidated = prev.Invalidated || next.Invalidated
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	out.Confidence = model.ConfidenceFor(out.Evidence)
	return out
}

// ---------------------------------------------------------------------------
// Work items, failures, risks
// ---------------------------------------------------------------------------

func workKey(w model.WorkItem) string        { return "text:" + normalizeText(w.Description) }
func failedKey(f model.FailedAttempt) string { return "text:" + normalizeText(f.Description) }
func riskKey(r model.Risk) string            { return "text:" + normalizeText(r.Description) }

func mergeWorkItem(prev, next model.WorkItem) model.WorkItem {
	out := prev
	out.SessionID = firstNonEmpty(prev.SessionID, next.SessionID)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	out.Confidence = model.ConfidenceFor(out.Evidence)
	return out
}

func mergeFailedAttempt(prev, next model.FailedAttempt) model.FailedAttempt {
	out := prev
	out.SessionID = firstNonEmpty(prev.SessionID, next.SessionID)
	out.OccurredAt = earlierNonZero(prev.OccurredAt, next.OccurredAt)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	return out
}

func mergeRisk(prev, next model.Risk) model.Risk {
	out := prev
	out.Severity = firstNonEmpty(prev.Severity, next.Severity)
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	out.Confidence = model.ConfidenceFor(out.Evidence)
	return out
}

// ---------------------------------------------------------------------------
// Constraints and lineage
// ---------------------------------------------------------------------------

func constraintKey(c model.Constraint) string { return identityKey(c.ID, c.Text) }

// mergeConstraint keeps the first record of a constraint (its text and the time
// it arrived are history) and unions the annotations, so replaying the same
// ConstraintAdded event twice cannot fork or churn the state.
func mergeConstraint(prev, next model.Constraint) model.Constraint {
	out := prev
	out.Text = firstNonEmpty(prev.Text, next.Text)
	out.Source = firstNonEmpty(prev.Source, next.Source)
	out.AddedAt = earlierNonZero(prev.AddedAt, next.AddedAt)
	out.AffectedRequirements = dedupeStrings(concat(prev.AffectedRequirements, next.AffectedRequirements))
	out.ChangedAssumptions = dedupeStrings(concat(prev.ChangedAssumptions, next.ChangedAssumptions))
	out.Evidence = unionEvidence(prev.Evidence, next.Evidence)
	return out
}

func sessionKey(s model.SessionNode) string       { return "session:" + s.SessionID }
func checkpointKey(c model.CheckpointNode) string { return "checkpoint:" + c.CheckpointID }

// mergeSession fills in what a later capture learned about a session the earlier
// one saw still running. Interrupted is sticky: a session that stopped without a
// clean end event cannot retroactively acquire one, and treating its captured
// state as incomplete is the safe reading (plan §22).
func mergeSession(prev, next model.SessionNode) model.SessionNode {
	out := prev
	out.ParentSessionID = firstNonEmpty(prev.ParentSessionID, next.ParentSessionID)
	if out.Agent == "" || out.Agent == model.AgentUnknown {
		out.Agent = next.Agent
	}
	if out.Role == "" {
		out.Role = next.Role
	}
	out.StartedAt = earlierNonZero(prev.StartedAt, next.StartedAt)
	out.EndedAt = laterTime(prev.EndedAt, next.EndedAt)
	out.Interrupted = prev.Interrupted || next.Interrupted
	return out
}

func mergeCheckpoint(prev, next model.CheckpointNode) model.CheckpointNode {
	out := prev
	out.SessionID = firstNonEmpty(prev.SessionID, next.SessionID)
	out.Label = firstNonEmpty(prev.Label, next.Label)
	out.CommitSHA = firstNonEmpty(prev.CommitSHA, next.CommitSHA)
	out.Branch = firstNonEmpty(prev.Branch, next.Branch)
	out.StateHash = firstNonEmpty(prev.StateHash, next.StateHash)
	if out.Agent == "" || out.Agent == model.AgentUnknown {
		out.Agent = next.Agent
	}
	out.CreatedAt = earlierNonZero(prev.CreatedAt, next.CreatedAt)
	return out
}

// ---------------------------------------------------------------------------
// Tree snapshots and capture
// ---------------------------------------------------------------------------

// mergeChangedFiles replaces the previous file list with the incoming one: the
// changed-file set is a snapshot of the working tree, not an accumulating log,
// and a union would keep reporting files as dirty long after they were reverted
// or committed.
//
// The one exception is an incoming capture that says outright that git was not
// available (plan §48): dropping the evidence we already had would present a
// degraded capture as a clean tree, so we keep the old list and mark it as
// possibly stale in Capture.Notes. Reported staleness beats silent loss.
func mergeChangedFiles(prev, next *model.EngineeringState, hasNext bool) (files []model.ChangedFile, stale bool) {
	if !hasNext {
		// There was no incoming capture at all, so the previous list is simply
		// unchanged rather than stale; annotating it would invent a gap.
		return orEmpty(prev.ChangedFiles), false
	}
	if len(next.ChangedFiles) == 0 && !next.Capture.GitAvailable && len(prev.ChangedFiles) > 0 {
		return prev.ChangedFiles, true
	}
	return orEmpty(next.ChangedFiles), false
}

// repoObserved reports whether a RepoState carries an actual observation. Dirty
// and ObservedAt are ignored deliberately: a false Dirty on an otherwise empty
// struct is a zero value, not a claim that the tree is clean.
func repoObserved(r model.RepoState) bool {
	return r.Repo != "" || r.Branch != "" || r.CommitSHA != "" || r.TreeHash != ""
}

// mergeCapture takes the incoming capture flags when there is an incoming state,
// because they describe the capture the reader is about to act on. A fresh
// derivation that finds git unavailable *now* must be able to downgrade a
// checkpoint's older claim that it was available, otherwise stale changed-file
// lists would keep reading as verified. Missing and Notes are unioned so that
// no recorded gap is ever silently dropped.
//
// This rule assumes the incoming capture actually speaks to every input — that
// a false flag means "checked and absent", not "never looked". A contributor
// that inspects nothing, such as the semantic extractor, would otherwise erase
// the record that git had been read and produce a state reporting "git
// unavailable" beside a commit sha it had verified. That is why such a
// contributor inherits the capture of the state it derived from before it gets
// here (see internal/semantic), rather than merge trying to guess who produced
// what.
func mergeCapture(prev, next model.Capture, hasNext bool) model.Capture {
	out := prev
	if hasNext {
		out = next
	}
	out.Missing = dedupeStrings(concat(prev.Missing, next.Missing))
	out.Notes = dedupeStrings(concat(prev.Notes, next.Notes))
	// A gap one side recorded may have been satisfied by the other. Carrying it
	// forward regardless listed the same input as both verified and unknown in
	// the rendered output.
	return out.PruneMissing()
}

// ---------------------------------------------------------------------------
// Generic helpers
// ---------------------------------------------------------------------------

// unionBy merges two slices keyed by key. Records from prev keep their position
// and records new to next are appended in next's order; the lookup map never
// decides ordering, so the result is deterministic (a product requirement, not a
// nicety: the state hash in a handoff receipt depends on it).
func unionBy[T any](prev, next []T, key func(T) string, combine func(prev, next T) T) []T {
	out := make([]T, 0, len(prev)+len(next))
	at := make(map[string]int, len(prev)+len(next))
	add := func(v T) {
		k := key(v)
		if i, ok := at[k]; ok {
			out[i] = combine(out[i], v)
			return
		}
		at[k] = len(out)
		out = append(out, v)
	}
	for _, v := range prev {
		add(v)
	}
	for _, v := range next {
		add(v)
	}
	return out
}

// identityKey keys an identified record, falling back to its prose when the id
// is missing so that two unidentified copies of the same record still collapse
// into one. The prefix keeps the two key spaces from colliding.
func identityKey(id, fallback string) string {
	if n := model.NormalizeID(id); n != "" {
		return "id:" + n
	}
	return "text:" + normalizeText(fallback)
}

func unionEvidence(prev, next []model.Evidence) []model.Evidence {
	return model.DedupeEvidence(concat(prev, next))
}

func concat[T any](a, b []T) []T {
	out := make([]T, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}

func orEmpty[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// normalizeText keys a prose-identified record. It trims only: two extractions
// wording the same item differently are genuinely two records as far as this
// package can tell, and pretending otherwise would be a guess.
func normalizeText(s string) string { return strings.TrimSpace(s) }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlierNonZero(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	default:
		return a
	}
}
