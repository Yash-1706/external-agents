// Package validate enforces the honesty rules of an EngineeringState.
//
// It offers two operations that deliberately do different jobs:
//
//	State   reports what is wrong and changes nothing.
//	Enforce returns a corrected clone in which every claim has been weakened
//	        until the attached evidence can carry it.
//
// Both exist because the product's promise is that a resumed task is
// trustworthy. A claim that outruns its evidence is worse than no claim at all:
// it tells the next worker something is done when nobody checked (plan §11,
// §12, §32).
//
// Enforce only ever weakens. It never promotes a requirement, never raises a
// confidence and never adds evidence, because a validator that could strengthen
// a claim would itself be a fabrication path (plan §32, "prefer uncertainty
// over fabrication"). model.Unknown is therefore always left alone: "we do not
// know" is a permitted final answer, never something to be improved upon.
//
// Every function here is deterministic. No map is ever iterated, no clock is
// read, and findings are emitted in a fixed traversal order, so two runs over
// the same state produce byte-identical output.
package validate

import (
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Severity separates a claim that must not be trusted from one that is merely
// thin. Error means the state would mislead a reader if published as-is; Warn
// marks honest-but-incomplete material the next worker should still know about,
// which is the graceful-degradation case from plan §33.
type Severity string

const (
	Error Severity = "error"
	Warn  Severity = "warning"
)

// Finding is one rule violation located at a concrete element of the state.
//
// Path addresses the offending element the way the rest of the system talks
// about it, e.g. "requirements[R3]", "tests[TestDuplicateWebhook]" or
// "requirements[R3].evidence[0]". Elements that carry no id are addressed
// positionally as "completed_work[#0]". A finding that could not be traced back
// to a specific element would repeat the very failure this package exists to
// prevent, so every finding has a Path.
type Finding struct {
	Severity Severity
	Code     string
	Path     string
	Message  string
}

// Rule codes. These are kept unexported because the package's exported surface
// is fixed by contract; callers match on the string values, which are stable.
const (
	// plan §32: "Do not mark a requirement complete without sufficient evidence."
	codeReqCompleteNoEvidence = "REQ_COMPLETE_NO_EVIDENCE"
	// plan Step 11: "No test pass without result."
	codeTestPassNoResult = "TEST_PASS_NO_RESULT"
	// plan Step 11: "No decision without source/context."
	codeDecisionNoSource = "DECISION_NO_SOURCE"
	// plan §11: every important semantic claim should point at evidence.
	codeClaimNoEvidence = "CLAIM_NO_EVIDENCE"
	// plan §12: a claim may not be graded above what its evidence supports.
	codeConfidenceExceedsEvidence = "CONFIDENCE_EXCEEDS_EVIDENCE"
	// plan §12: a next action is a proposal, never a statement of fact.
	codeNextActionNotRecommended = "NEXT_ACTION_NOT_RECOMMENDED"
	// An enum value outside the frozen domain vocabulary.
	codeInvalidEnum = "INVALID_ENUM"
	// The state was written by a different wire format than this build knows.
	codeSchemaVersion = "SCHEMA_VERSION"
	// Two elements share an id, so one of them is unreachable by lookup.
	codeDuplicateID = "DUPLICATE_ID"
	// The stored status is not the status the evidence implies.
	codeStatusInconsistent = "STATUS_INCONSISTENT"
	// Evidence that points at nothing at all.
	codeOrphanEvidence = "ORPHAN_EVIDENCE"
)

// knownEvidenceKinds is the closed vocabulary from plan §11. model.EvidenceKind
// has no Valid method, and EvidenceKind.Level silently returns LevelInference
// for anything it does not recognise, so an unknown kind would quietly become a
// weak-but-accepted citation. Naming the vocabulary here turns that silence
// into an INVALID_ENUM finding. The map is only ever looked up, never ranged
// over, so it cannot leak iteration order into output.
var knownEvidenceKinds = map[model.EvidenceKind]bool{
	model.EvidenceCommit:     true,
	model.EvidenceFile:       true,
	model.EvidenceTest:       true,
	model.EvidenceRuntime:    true,
	model.EvidenceGraph:      true,
	model.EvidenceCheckpoint: true,
	model.EvidenceSession:    true,
	model.EvidenceEvent:      true,
	model.EvidencePrompt:     true,
	model.EvidenceInference:  true,
}

// State reports every rule violation in s without modifying it.
//
// Findings are returned in a fixed traversal order: schema, status, duplicate
// ids, then each collection in the order it is declared on the state. A nil
// state yields no findings rather than a panic; there is nothing to say about
// a state that does not exist.
func State(s *model.EngineeringState) []Finding {
	if s == nil {
		return nil
	}
	var out []Finding
	checkSchema(s, &out)
	checkStatus(s, &out)
	checkDuplicates(s, &out)
	checkRequirements(s, &out)
	checkWorkItems(&out, "completed_work", s.CompletedWork)
	checkWorkItems(&out, "in_progress", s.InProgress)
	checkFailedAttempts(s, &out)
	checkDecisions(s, &out)
	checkRejected(s, &out)
	checkAssumptions(s, &out)
	checkTests(s, &out)
	checkChangedFiles(s, &out)
	checkRisks(s, &out)
	checkNextActions(s, &out)
	checkConstraints(s, &out)
	checkLineage(s, &out)
	checkEvidenceSlice(&out, "", s.Evidence)
	return out
}

func checkSchema(s *model.EngineeringState, out *[]Finding) {
	if s.SchemaVersion == model.SchemaVersion {
		return
	}
	// Reading a state whose wire format we do not know is exactly the moment to
	// say so rather than to interpret fields that may have moved (plan §33).
	add(out, Error, codeSchemaVersion, "schema_version",
		fmt.Sprintf("state declares schema version %d but this build understands %d; fields may be absent or mean something else",
			s.SchemaVersion, model.SchemaVersion))
}

func checkStatus(s *model.EngineeringState, out *[]Finding) {
	if !s.Status.Valid() {
		add(out, Error, codeInvalidEnum, "status",
			fmt.Sprintf("task status %q is not a defined status", s.Status))
		// An undefined status cannot be meaningfully compared with the derived
		// one, and reporting a second finding about it would only add noise.
		return
	}
	derived := s.DeriveStatus()
	if derived == s.Status {
		return
	}
	// DeriveStatus only ever yields new/active/blocked/partial/verified, so the
	// lifecycle-only labels (resumed, complete) always surface here. That is the
	// intended signal, not a false positive: it says the stored label was set by
	// a workflow step rather than derived from evidence, which is precisely what
	// a reader needs to know before trusting it. Warn, not Error: the label is
	// unverified, but nothing in it is provably false.
	add(out, Warn, codeStatusInconsistent, "status",
		fmt.Sprintf("stored status %q disagrees with the status derived from evidence (%q)", s.Status, derived))
}

// checkDuplicates reports ids that appear twice. A duplicate id is not cosmetic:
// model lookups such as EngineeringState.Requirement return the first match, so
// the second claim is silently unreachable and a merge would drop it.
func checkDuplicates(s *model.EngineeringState, out *[]Finding) {
	reqs := make([]dupEntry, 0, len(s.Requirements))
	for i, r := range s.Requirements {
		reqs = append(reqs, dupEntry{model.NormalizeID(r.ID), elemPath("requirements", r.ID, i)})
	}
	reportDuplicates(out, "requirement", reqs)

	decs := make([]dupEntry, 0, len(s.Decisions))
	for i, d := range s.Decisions {
		decs = append(decs, dupEntry{model.NormalizeID(d.ID), elemPath("decisions", d.ID, i)})
	}
	reportDuplicates(out, "decision", decs)

	rej := make([]dupEntry, 0, len(s.Rejected))
	for i, r := range s.Rejected {
		rej = append(rej, dupEntry{model.NormalizeID(r.ID), elemPath("rejected_approaches", r.ID, i)})
	}
	reportDuplicates(out, "rejected approach", rej)

	cons := make([]dupEntry, 0, len(s.Constraints))
	for i, c := range s.Constraints {
		cons = append(cons, dupEntry{model.NormalizeID(c.ID), elemPath("constraints", c.ID, i)})
	}
	reportDuplicates(out, "constraint", cons)

	// Test names and file paths are case-sensitive identifiers in the toolchains
	// they come from, so they are compared verbatim rather than normalized.
	tests := make([]dupEntry, 0, len(s.Tests))
	for i, t := range s.Tests {
		tests = append(tests, dupEntry{t.Name, elemPath("tests", t.Name, i)})
	}
	reportDuplicates(out, "test", tests)

	files := make([]dupEntry, 0, len(s.ChangedFiles))
	for i, f := range s.ChangedFiles {
		files = append(files, dupEntry{f.Path, elemPath("changed_files", f.Path, i)})
	}
	reportDuplicates(out, "changed file", files)

	sessions := make([]dupEntry, 0, len(s.Sessions))
	for i, n := range s.Sessions {
		sessions = append(sessions, dupEntry{n.SessionID, elemPath("sessions", n.SessionID, i)})
	}
	reportDuplicates(out, "session", sessions)

	cps := make([]dupEntry, 0, len(s.Checkpoints))
	for i, c := range s.Checkpoints {
		cps = append(cps, dupEntry{c.CheckpointID, elemPath("checkpoints", c.CheckpointID, i)})
	}
	reportDuplicates(out, "checkpoint", cps)
}

func checkRequirements(s *model.EngineeringState, out *[]Finding) {
	for i, r := range s.Requirements {
		p := elemPath("requirements", r.ID, i)
		if !r.Status.Valid() {
			add(out, Error, codeInvalidEnum, p+".status",
				fmt.Sprintf("requirement status %q is not a defined status", r.Status))
		}
		if !r.Confidence.Valid() {
			add(out, Error, codeInvalidEnum, p+".confidence",
				fmt.Sprintf("confidence %q is not a defined confidence", r.Confidence))
		}
		checkEvidenceSlice(out, p, r.Evidence)
		if len(r.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("requirement %q carries no evidence, so a reader cannot check it", brief(r.Description)))
		}
		// plan §32: complete is the one requirement status that tells the next
		// worker they can stop looking, so it demands evidence they can re-check
		// themselves - a repository fact or an Entire checkpoint (plan §31).
		if r.Status == model.ReqComplete && !hasVerifiableEvidence(r.Evidence) {
			add(out, Error, codeReqCompleteNoEvidence, p,
				"requirement is marked complete without repository or checkpoint evidence; an agent statement or model inference cannot establish completion")
		}
		reportExcessConfidence(out, p+".confidence", r.Confidence, r.Evidence)
	}
}

func checkWorkItems(out *[]Finding, collection string, items []model.WorkItem) {
	for i, w := range items {
		p := elemPath(collection, "", i)
		if !w.Confidence.Valid() {
			add(out, Error, codeInvalidEnum, p+".confidence",
				fmt.Sprintf("confidence %q is not a defined confidence", w.Confidence))
		}
		checkEvidenceSlice(out, p, w.Evidence)
		if len(w.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("work item %q carries no evidence", brief(w.Description)))
		}
		reportExcessConfidence(out, p+".confidence", w.Confidence, w.Evidence)
	}
}

func checkFailedAttempts(s *model.EngineeringState, out *[]Finding) {
	for i, f := range s.FailedAttempts {
		p := elemPath("failed_attempts", "", i)
		checkEvidenceSlice(out, p, f.Evidence)
		if len(f.Evidence) == 0 {
			// A failure is the conservative direction of claim, so this is a
			// warning: it costs the next worker time, not correctness.
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("failed attempt %q carries no evidence, so the next worker cannot see how it failed", brief(f.Description)))
		}
	}
}

func checkDecisions(s *model.EngineeringState, out *[]Finding) {
	for i, d := range s.Decisions {
		p := elemPath("decisions", d.ID, i)
		checkEvidenceSlice(out, p, d.Evidence)
		if len(d.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("decision %q carries no evidence", brief(d.Decision)))
		}
		// plan Step 11: "No decision without source/context". A session or
		// checkpoint id counts as context even without evidence, because a
		// reader can still go and look. With none of the three the decision is
		// unattributable, which is the fabrication risk itself.
		if len(d.Evidence) == 0 && d.SessionID == "" && d.CheckpointID == "" {
			add(out, Error, codeDecisionNoSource, p,
				"decision has no evidence, session or checkpoint; its origin cannot be traced")
		}
	}
}

func checkRejected(s *model.EngineeringState, out *[]Finding) {
	for i, r := range s.Rejected {
		p := elemPath("rejected_approaches", r.ID, i)
		if !r.Confidence.Valid() {
			add(out, Error, codeInvalidEnum, p+".confidence",
				fmt.Sprintf("confidence %q is not a defined confidence", r.Confidence))
		}
		checkEvidenceSlice(out, p, r.Evidence)
		if len(r.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("rejected approach %q carries no evidence, so a later worker cannot tell whether it truly failed", brief(r.Approach)))
		}
		reportExcessConfidence(out, p+".confidence", r.Confidence, r.Evidence)
	}
}

func checkAssumptions(s *model.EngineeringState, out *[]Finding) {
	for i, a := range s.Assumptions {
		p := elemPath("assumptions", "", i)
		if !a.Confidence.Valid() {
			add(out, Error, codeInvalidEnum, p+".confidence",
				fmt.Sprintf("confidence %q is not a defined confidence", a.Confidence))
		}
		checkEvidenceSlice(out, p, a.Evidence)
		if len(a.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("assumption %q carries no evidence", brief(a.Statement)))
		}
		reportExcessConfidence(out, p+".confidence", a.Confidence, a.Evidence)
	}
}

func checkTests(s *model.EngineeringState, out *[]Finding) {
	for i, t := range s.Tests {
		p := elemPath("tests", t.Name, i)
		if !t.Status.Valid() {
			add(out, Error, codeInvalidEnum, p+".status",
				fmt.Sprintf("test status %q is not a defined status", t.Status))
		}
		checkEvidenceSlice(out, p, t.Evidence)
		if len(t.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p, "test result carries no evidence")
		}
		// plan Step 11: "No test pass without result". A pass is the single most
		// damaging false claim the product can make, because it is the one that
		// tells the next worker to stop. A recorded failure is not held to the
		// same bar: failing is the safe direction, and plan §32 keeps a failure
		// standing until a later result disproves it.
		if t.Status == model.TestPassed && !hasRunResult(t.Evidence) {
			add(out, Error, codeTestPassNoResult, p,
				"test is recorded as passed but no test or runtime evidence carries an actual result")
		}
	}
}

func checkChangedFiles(s *model.EngineeringState, out *[]Finding) {
	for i, f := range s.ChangedFiles {
		checkEvidenceSlice(out, elemPath("changed_files", f.Path, i), f.Evidence)
	}
}

func checkRisks(s *model.EngineeringState, out *[]Finding) {
	for i, r := range s.Risks {
		p := elemPath("risks", "", i)
		if !r.Confidence.Valid() {
			add(out, Error, codeInvalidEnum, p+".confidence",
				fmt.Sprintf("confidence %q is not a defined confidence", r.Confidence))
		}
		checkEvidenceSlice(out, p, r.Evidence)
		if len(r.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("risk %q carries no evidence", brief(r.Description)))
		}
		reportExcessConfidence(out, p+".confidence", r.Confidence, r.Evidence)
	}
}

func checkNextActions(s *model.EngineeringState, out *[]Finding) {
	for i, a := range s.NextActions {
		p := elemPath("next_actions", "", i)
		checkEvidenceSlice(out, p, a.Evidence)
		if len(a.Evidence) == 0 {
			add(out, Warn, codeClaimNoEvidence, p,
				fmt.Sprintf("next action %q carries no evidence, so its rationale cannot be checked", brief(a.Description)))
		}
		// A next action's confidence is definitional rather than derived
		// (plan §12): it is a proposal, so it is RECOMMENDED and nothing else.
		// That makes both INVALID_ENUM and CONFIDENCE_EXCEEDS_EVIDENCE redundant
		// here - this one rule already rejects every other value - so they are
		// deliberately not applied to next actions.
		if a.Confidence != model.Recommended {
			add(out, Error, codeNextActionNotRecommended, p+".confidence",
				fmt.Sprintf("next action is graded %q; a proposed action is never a statement of fact and must be %q",
					a.Confidence, model.Recommended))
		}
	}
}

// checkConstraints validates the evidence hanging off a mid-task constraint.
//
// A constraint is the curveball mechanism (plan §41): it is the one element that
// can change what the task means after work has already started, so a reader has
// more reason to check its citation than almost any other. Its evidence is
// optional - a constraint delivered by a human instruction legitimately has
// none, so there is no CLAIM_NO_EVIDENCE here - but any citation it does carry
// is held to the same bar as every other citation in the state.
func checkConstraints(s *model.EngineeringState, out *[]Finding) {
	for i, c := range s.Constraints {
		checkEvidenceSlice(out, elemPath("constraints", c.ID, i), c.Evidence)
	}
}

func checkLineage(s *model.EngineeringState, out *[]Finding) {
	for i, n := range s.Sessions {
		if !n.Agent.Valid() {
			// model.AgentUnknown exists so that "we do not know which runtime"
			// can be stated explicitly; an empty or bogus value is a gap that
			// was never declared.
			add(out, Error, codeInvalidEnum, elemPath("sessions", n.SessionID, i)+".agent",
				fmt.Sprintf("agent %q is not a recognised runtime; use %q to say it is unknown", n.Agent, model.AgentUnknown))
		}
	}
	for i, c := range s.Checkpoints {
		if !c.Agent.Valid() {
			add(out, Error, codeInvalidEnum, elemPath("checkpoints", c.CheckpointID, i)+".agent",
				fmt.Sprintf("agent %q is not a recognised runtime; use %q to say it is unknown", c.Agent, model.AgentUnknown))
		}
	}
}

// checkEvidenceSlice validates one evidence list hanging off base. base is ""
// for the state-level evidence collection.
func checkEvidenceSlice(out *[]Finding, base string, ev []model.Evidence) {
	for i, e := range ev {
		p := fmt.Sprintf("evidence[%d]", i)
		if base != "" {
			p = fmt.Sprintf("%s.evidence[%d]", base, i)
		}
		if !knownEvidenceKinds[e.Kind] {
			add(out, Error, codeInvalidEnum, p,
				fmt.Sprintf("evidence kind %q is not a defined kind, so its priority level cannot be trusted", e.Kind))
		}
		// plan §11: the user must be able to inspect the basis of a claim. An
		// evidence record with neither a ref nor a path is a citation to
		// nothing, which reads as support while providing none - and supporting
		// drops it for exactly that reason, so the two stay in step.
		if !citable(e) {
			add(out, Error, codeOrphanEvidence, p,
				"evidence has neither a ref nor a path, so the claim it supports cannot be inspected")
		}
	}
}

func reportExcessConfidence(out *[]Finding, path string, c model.Confidence, ev []model.Evidence) {
	if !c.Valid() {
		// Already reported as INVALID_ENUM; grading an unrecognised value
		// against the evidence would be guesswork.
		return
	}
	derived := derivedConfidence(ev)
	if confidenceRank(c) <= confidenceRank(derived) {
		return
	}
	add(out, Error, codeConfidenceExceedsEvidence, path,
		fmt.Sprintf("claim is graded %s but its evidence supports no more than %s", c.Label(), derived.Label()))
}

// citable reports whether e points at something a reader can actually go and
// look at. It is the same test ORPHAN_EVIDENCE applies, named once so that the
// rule and the consequence cannot drift apart.
func citable(e model.Evidence) bool {
	return strings.TrimSpace(e.Ref) != "" || strings.TrimSpace(e.Path) != ""
}

// supporting returns the subset of ev that is allowed to carry a claim.
//
// A record with neither a ref nor a path is, in this package's own words, "a
// citation to nothing, which reads as support while providing none" - so it
// must not be counted as support either. Without this filter an adapter that
// emitted model.Evidence{Kind: model.EvidenceCommit} with no sha would launder
// any claim up to OBSERVED and certify any requirement as complete, because
// EvidenceKind.Level looks only at the kind. That is precisely the fabrication
// path this package exists to close (plan §11: the basis of a claim must be
// inspectable).
//
// Filtering can only ever remove records, so every derived judgement moves
// weaker or stays put. Enforce therefore still only ever downgrades.
//
// CLAIM_NO_EVIDENCE deliberately keeps using the unfiltered length: "carries no
// evidence at all" and "carries evidence that points nowhere" are different
// defects, and the second is already reported as an ORPHAN_EVIDENCE error.
func supporting(ev []model.Evidence) []model.Evidence {
	out := make([]model.Evidence, 0, len(ev))
	for _, e := range ev {
		if citable(e) {
			out = append(out, e)
		}
	}
	return out
}

// derivedConfidence is the strongest grade ev can defend. model.ConfidenceFor is
// the single chokepoint for that judgement (plan §12); this wrapper exists only
// to make sure both State and Enforce feed it the same filtered evidence.
func derivedConfidence(ev []model.Evidence) model.Confidence {
	return model.ConfidenceFor(supporting(ev))
}

// hasVerifiableEvidence reports whether ev holds at least one citable Level 1 or
// Level 2 record. Those are the only levels a later reader can independently
// re-check: repository facts and Entire checkpoints. A Level 3 agent statement
// or a Level 4 model inference is somebody's word for it (plan §31).
func hasVerifiableEvidence(ev []model.Evidence) bool {
	level, ok := model.StrongestLevel(supporting(ev))
	return ok && level <= model.LevelCheckpoint
}

// hasRunResult reports whether ev records the actual outcome of a run: a citable
// test or runtime record carrying a Result. Plan Step 11 draws this line
// explicitly, and it is stricter than "has evidence" on purpose - a commit that
// touched the test file is not a report that the test passed, and a bare
// "passed" naming no run is not one either.
func hasRunResult(ev []model.Evidence) bool {
	for _, e := range supporting(ev) {
		switch e.Kind {
		case model.EvidenceTest, model.EvidenceRuntime:
			if strings.TrimSpace(e.Result) != "" {
				return true
			}
		}
	}
	return false
}

// confidenceRank orders the four confidence values by how much they assert.
// Unknown asserts nothing, Recommended asserts a proposal rather than a fact,
// and Inferred and Observed are increasingly strong statements about reality
// (plan §12). Enforce uses this ordering to guarantee it only ever moves a
// claim downwards.
//
// A value outside the vocabulary ranks 0 so that Enforce leaves it untouched:
// State reports it as INVALID_ENUM, and replacing a value we do not understand
// with one we invented would be exactly the fabrication this package prevents.
func confidenceRank(c model.Confidence) int {
	switch c {
	case model.Observed:
		return 3
	case model.Inferred:
		return 2
	case model.Recommended:
		return 1
	case model.Unknown:
		return 0
	}
	return 0
}

type dupEntry struct {
	key  string
	path string
}

func reportDuplicates(out *[]Finding, kind string, entries []dupEntry) {
	seen := make(map[string]int, len(entries))
	for i, e := range entries {
		if e.key == "" {
			// A missing id is a different defect from a repeated one; treating
			// two unnamed elements as duplicates of each other would be wrong.
			continue
		}
		if first, ok := seen[e.key]; ok {
			add(out, Error, codeDuplicateID, e.path,
				fmt.Sprintf("%s at index %d repeats an id first used at index %d; lookups return only the first, so the later claim is unreachable", kind, i, first))
			continue
		}
		seen[e.key] = i
	}
}

// elemPath addresses one element of a collection. Elements that carry an id use
// it, because ids survive the re-ordering that state.Sort performs; the rest
// fall back to a positional key, which is still stable for a given state.
func elemPath(collection, id string, idx int) string {
	if id == "" {
		return fmt.Sprintf("%s[#%d]", collection, idx)
	}
	return collection + "[" + id + "]"
}

// brief collapses free text onto one bounded line so a finding stays readable in
// CLI output. Truncation is by rune, never by byte, so multi-byte text is not
// cut in half.
func brief(s string) string {
	const max = 60
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func add(out *[]Finding, sev Severity, code, path, msg string) {
	*out = append(*out, Finding{Severity: sev, Code: code, Path: path, Message: msg})
}
