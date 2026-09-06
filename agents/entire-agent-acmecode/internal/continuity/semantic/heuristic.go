package semantic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// heuristic is a rule-based model.Extractor. It holds no state beyond its clock,
// so two extractions over the same input are independent and identical.
type heuristic struct {
	clock model.Clock
}

// NewHeuristic returns the deterministic, rule-based Extractor.
//
// It is always Available: unlike a network-backed extractor it cannot fail to
// answer, which is precisely why it is the safe default. Its ceiling is low and
// it says so — nothing it emits is ever better than model.Inferred unless a
// Level 1 artifact from the deterministic state backs it up.
//
// A nil clock falls back to model.SystemClock; callers that need reproducible
// output pass model.FixedClock (plan §33).
func NewHeuristic(clock model.Clock) model.Extractor {
	if clock == nil {
		clock = model.SystemClock{}
	}
	return &heuristic{clock: clock}
}

// Available reports true: rule evaluation needs nothing external.
func (h *heuristic) Available(context.Context) bool { return true }

// Describe names the backing implementation. The user must always be able to
// see that no model was consulted, so that "requirement status unknown" reads
// as a limitation of the rules rather than as a model's judgement (plan §33).
func (h *heuristic) Describe() string {
	return "heuristic extractor (deterministic keyword rules, no model backend)"
}

// Extract builds the model-assisted half of the state from the prompt, the
// event stream and the deterministic state it is given.
func (h *heuristic) Extract(ctx context.Context, in model.ExtractionInput) (*model.EngineeringState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// One extraction is one instant. Taking the clock exactly once stamps every
	// claim identically and removes clock-call ordering as a source of variance
	// between runs (determinism is a product requirement, plan §33).
	now := h.clock.Now()

	st := model.NewEngineeringState(in.Task)
	st.GeneratedAt = now

	// Status is left at the constructor default on purpose: an Extractor is not
	// entitled to declare where the task stands (Rule 6). EngineeringState.
	// DeriveStatus and the merge layer own that, from evidence.

	events := model.DedupeEvents(in.Events)
	model.SortEvents(events)

	st.Requirements = requirements(in, now)
	st.Decisions = decisions(events)
	st.Rejected = rejected(events)
	st.Assumptions = assumptions(in.OriginalPrompt, events, in.Transcript, now)
	st.NextActions = nextActions(in.Deterministic, st.Requirements, now)

	// The extractor ran, so the semantic half of capture is satisfied. Every
	// other Capture flag belongs to whoever collected that input, and this
	// extractor inspects no repository, reads no checkpoint and runs no test.
	//
	// Those flags are therefore inherited from the deterministic state this
	// extraction was derived from rather than left false. Leaving them false
	// would be read downstream as "checked and absent": the merge rule takes
	// the incoming capture wholesale so that a fresh derivation can downgrade a
	// checkpoint's stale claim, and a silent contributor would ride that rule to
	// erase a truth it never examined. The observed symptom was a state
	// reporting "git unavailable" directly beside the commit sha it had just
	// verified. Inheriting is also simply accurate: this output describes the
	// same capture as its input, plus semantic extraction.
	if in.Deterministic != nil {
		st.Capture = in.Deterministic.Capture.Or(st.Capture)
	}
	st.Capture.SemanticExtraction = true
	if strings.TrimSpace(in.OriginalPrompt) == "" {
		st.Capture.Missing = append(st.Capture.Missing, sourceOriginalPrompt)
		st.Capture.Notes = append(st.Capture.Notes,
			"no original prompt was captured: requirements could not be extracted")
	}
	if len(events) == 0 {
		st.Capture.Missing = append(st.Capture.Missing, "agent_events")
		st.Capture.Notes = append(st.Capture.Notes,
			"no agent events were captured: decisions and rejected approaches could not be extracted")
	}
	if in.Deterministic == nil {
		st.Capture.Missing = append(st.Capture.Missing, "deterministic_state")
		st.Capture.Notes = append(st.Capture.Notes,
			"no deterministic state to correlate against: every requirement status is unverified")
	}

	// Final chokepoint for the "no claim without evidence" invariant. The
	// builders above already refuse to emit an unevidenced claim; this is the
	// place that guarantees it, so a future builder cannot quietly break it.
	pruneUnevidenced(st)
	return st, nil
}

// promptCandidate is one span of the original prompt that looks like an
// obligation, along with why it was selected.
type promptCandidate struct {
	text string
	line int
	form string
	// id is the label the source gave this requirement, when it gave one.
	id string
}

// promptCandidates scans the original prompt in source order. Numbered and
// bulleted lines are taken whole; unmarked prose contributes only the sentences
// that carry an obligation cue, because a narrative sentence is not a
// requirement and inventing one would be fabrication (plan §32).
func promptCandidates(prompt string) []promptCandidate {
	// A document that says where its requirements are is taken at its word.
	// Scanning the whole of a PRD turns its problem statement, its out-of-scope
	// list and its acceptance prose into requirements, which is how a five
	// requirement spec became eleven — including three the document explicitly
	// placed out of scope.
	scoped := hasRequirementsSection(prompt)
	inSection := !scoped

	var out []promptCandidate
	for i, raw := range strings.Split(prompt, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			continue
		}
		if scoped {
			switch {
			case requirementsHeading.MatchString(line):
				inSection = true
				continue
			case anyHeading.MatchString(line):
				inSection = false
				continue
			}
			if !inSection {
				continue
			}
		}
		if text, id, form, ok := stripMarker(line); ok {
			out = append(out, promptCandidate{text: tidy(text), line: i + 1, form: form, id: id})
			continue
		}
		for _, s := range sentences(line) {
			if c, ok := matchCue(s, requirementCues); ok {
				out = append(out, promptCandidate{text: tidy(s), line: i + 1, form: "cue:" + string(c)})
			}
		}
	}
	return out
}

// requirements extracts obligations from the original prompt and correlates each
// one against the deterministic state. IDs are R1..Rn in source order so that
// the same prompt always yields the same identifiers across agents and runs.
func requirements(in model.ExtractionInput, now time.Time) []model.Requirement {
	cands := promptCandidates(in.OriginalPrompt)
	out := make([]model.Requirement, 0, len(cands))
	usedIDs := make(map[string]bool, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		key := strings.ToLower(c.text)
		if seen[key] {
			continue // the same obligation restated is one requirement
		}
		kw := keywords(c.text)
		if len(kw) == 0 {
			// Nothing meaning-bearing survives filtering ("- yes", "1. done"),
			// so there is no way to correlate or verify it. Silence beats noise.
			continue
		}
		seen[key] = true

		ev := []model.Evidence{{
			Kind:       model.EvidencePrompt,
			Ref:        sourceOriginalPrompt,
			LineStart:  c.line,
			Detail:     fmt.Sprintf("original prompt line %d (%s)", c.line, c.form),
			CapturedAt: now,
		}}
		status, extra := correlate(kw, in.Deterministic, now)
		ev = model.DedupeEvidence(append(ev, extra...))
		model.SortEvidence(ev)

		out = append(out, model.Requirement{
			// The source's own label wins. An identifier exists so the same
			// requirement can be named in the spec, the ticket and the pull
			// request; renumbering R3 to R7 severs exactly that.
			ID:          requirementID(c, usedIDs),
			Description: c.text,
			Status:      status,
			// Confidence flows through the single chokepoint so that a claim
			// backed only by the prompt can never read as observed fact.
			Confidence: model.ConfidenceFor(ev),
			Source:     sourceOriginalPrompt,
			Evidence:   ev,
			UpdatedAt:  now,
		})
	}
	return out
}

// correlate maps a requirement onto the deterministic half of the state by
// keyword overlap, returning the status and the artifact that justifies it.
//
// It can return ReqBlocked, ReqPartial or ReqUnknown and NEVER ReqComplete.
// Overlapping vocabulary shows that work touched the same subject; it is not
// evidence that the obligation was met, and plan §32 forbids marking a
// requirement complete without sufficient evidence. Certifying completion is
// the deterministic and validate layers' job, never a heuristic's.
//
// A failing test outranks a changed file because §32 also says a failing test
// stays failed until a later result proves otherwise: if work on this subject
// is currently red, "blocked" is the honest answer.
func correlate(kw []string, det *model.EngineeringState, now time.Time) (model.ReqStatus, []model.Evidence) {
	if det == nil {
		return model.ReqUnknown, nil
	}
	if t, ok := bestTest(kw, failingTests(det)); ok {
		return model.ReqBlocked, testEvidence(t, now)
	}
	if f, ok := bestFile(kw, det.ChangedFiles); ok {
		return model.ReqPartial, fileEvidence(f, now)
	}
	return model.ReqUnknown, nil
}

// failingTests returns the failing tests that can actually be cited, ordered by
// name so that neither correlation nor the action list depends on how the
// caller happened to order det.Tests.
//
// A test with no name is dropped. It cannot be cited, cannot be named in "Fix
// failing test ...", and cannot be re-run by the reader, so it cannot be the
// reason a requirement is declared blocked — and blocked is not a soft label:
// EngineeringState.DeriveStatus turns a single blocked requirement into
// StatusBlocked for the whole task. The returned slice is freshly allocated,
// so sorting it never reorders the caller's deterministic state.
func failingTests(det *model.EngineeringState) []model.TestResult {
	if det == nil {
		return nil
	}
	all := det.FailingTests()
	out := make([]model.TestResult, 0, len(all))
	for _, t := range all {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// bestTest picks the failing test sharing the most vocabulary with the
// requirement. Ties break on test name rather than on slice position, so the
// choice does not depend on how the caller happened to order its input.
func bestTest(kw []string, tests []model.TestResult) (model.TestResult, bool) {
	var best model.TestResult
	bestScore := 0
	for _, t := range tests {
		score := overlap(kw, keywordSet(t.Name, t.Package, strings.Join(t.Files, " ")))
		if score == 0 {
			continue
		}
		if score > bestScore || (score == bestScore && t.Name < best.Name) {
			best, bestScore = t, score
		}
	}
	return best, bestScore > 0
}

// bestFile picks the changed file sharing the most vocabulary with the
// requirement, breaking ties on path for the same reason as bestTest.
func bestFile(kw []string, files []model.ChangedFile) (model.ChangedFile, bool) {
	var best model.ChangedFile
	bestScore := 0
	for _, f := range files {
		score := overlap(kw, keywordSet(f.Path))
		if score == 0 {
			continue
		}
		if score > bestScore || (score == bestScore && f.Path < best.Path) {
			best, bestScore = f, score
		}
	}
	return best, bestScore > 0
}

// testEvidence cites a test result. The test's own evidence records are carried
// along so the citation keeps pointing at the commit or run that produced it.
func testEvidence(t model.TestResult, now time.Time) []model.Evidence {
	at := t.RanAt
	if at.IsZero() {
		at = now
	}
	ev := []model.Evidence{{
		Kind:       model.EvidenceTest,
		Ref:        t.Name,
		Result:     string(t.Status),
		Detail:     t.Command,
		CapturedAt: at,
	}}
	return append(ev, t.Evidence...)
}

// fileEvidence cites a changed file, preserving whatever stronger evidence the
// deterministic layer already attached to it.
func fileEvidence(f model.ChangedFile, now time.Time) []model.Evidence {
	status := f.Status
	if status == "" {
		status = "?" // unknown git status; say so rather than implying "modified"
	}
	ev := []model.Evidence{{
		Kind:       model.EvidenceFile,
		Ref:        f.Path,
		Path:       f.Path,
		Detail:     "changed file (" + status + ")",
		CapturedAt: now,
	}}
	return append(ev, f.Evidence...)
}

// decisions extracts durable engineering choices from event summaries.
//
// Only Summary is read. Plan §34 keeps raw tool payloads out of the extractor
// precisely so credential-bearing output can never reach persisted state.
func decisions(events []model.AgentEvent) []model.Decision {
	out := make([]model.Decision, 0, len(events))
	for _, ev := range events {
		if !agentAuthored(ev) {
			continue
		}
		s := tidy(ev.Summary)
		if s == "" {
			continue
		}
		if _, ok := matchCue(s, decisionCues); !ok {
			continue
		}
		cite := ev.AsEvidence()
		if !usable(cite) {
			// AgentEvent.AsEvidence uses the event type as its ref, so an event
			// an adapter never typed cites nothing. Dropping it here rather than
			// in pruneUnevidenced is what keeps D1..Dn contiguous.
			continue
		}
		out = append(out, model.Decision{
			ID:       fmt.Sprintf("D%d", len(out)+1),
			Decision: s,
			Reason:   reasonFrom(s),
			Evidence: []model.Evidence{cite},
			// Session and checkpoint are the decision's origin, which is what
			// makes it traceable back to the moment it was made (plan §14).
			SessionID:    ev.SessionID,
			CheckpointID: ev.Attr("checkpoint_id"),
			MadeAt:       ev.Timestamp,
		})
	}
	return out
}

// rejected extracts approaches that were tried and abandoned.
//
// An event is classified independently against each vocabulary, so a summary
// like "reverted the mutex approach and went with channels" yields both a
// rejection and a decision. It genuinely records both, and dropping either half
// would lose exactly the knowledge plan §15 exists to preserve.
func rejected(events []model.AgentEvent) []model.RejectedApproach {
	out := make([]model.RejectedApproach, 0, len(events))
	for _, ev := range events {
		if !agentAuthored(ev) {
			continue
		}
		s := tidy(ev.Summary)
		if s == "" {
			continue
		}
		if _, ok := matchCue(s, rejectionCues); !ok {
			continue
		}
		cite := ev.AsEvidence()
		if !usable(cite) { // an untyped event cites nothing; see decisions
			continue
		}
		e := []model.Evidence{cite}
		out = append(out, model.RejectedApproach{
			ID:       fmt.Sprintf("X%d", len(out)+1),
			Approach: s,
			Reason:   reasonFrom(s),
			Evidence: e,
			// An agent statement is Level 3, so this reads as inferred: we know
			// the approach was abandoned, not that it was observed to fail.
			Confidence:   model.ConfidenceFor(e),
			SessionID:    ev.SessionID,
			CheckpointID: ev.Attr("checkpoint_id"),
			RejectedAt:   ev.Timestamp,
		})
	}
	return out
}

// reasonFrom returns the clause after "because", which is the one causal form
// common enough in agent summaries to be worth reading. Anything subtler would
// be interpretation, and an empty Reason is an honest "not stated".
//
// The match is located with indexFold, against s itself. Searching a
// strings.ToLower copy and slicing s with the offset it returns is a crash:
// lower-casing can lengthen a string, so the offset overruns s and the slice
// panics. Event summaries are free text from an external agent, and plan §48
// says nothing here may be fatal to the host agent's own work.
func reasonFrom(s string) string {
	const marker = "because "
	i := indexFold(s, marker)
	if i < 0 {
		return ""
	}
	return tidy(s[i+len(marker):])
}

// assumptions extracts unproven things the task relies on, from every source the
// extractor is allowed to read, in a fixed order: prompt, then events, then
// transcript. A statement repeated across sources is recorded once, with the
// first source as its citation.
func assumptions(prompt string, events []model.AgentEvent, transcript []string, now time.Time) []model.Assumption {
	out := make([]model.Assumption, 0, 4)
	seen := make(map[string]bool)
	add := func(stmt string, ev model.Evidence) {
		stmt = tidy(stmt)
		key := strings.ToLower(stmt)
		if stmt == "" || seen[key] || !usable(ev) {
			return
		}
		seen[key] = true
		e := []model.Evidence{ev}
		out = append(out, model.Assumption{
			Statement:  stmt,
			Confidence: model.ConfidenceFor(e),
			Evidence:   e,
			// Invalidated is left false: contradicting an assumption requires
			// later evidence, which only the merge layer can see (plan §32).
		})
	}

	for i, raw := range strings.Split(prompt, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		for _, s := range sentences(line) {
			if _, ok := matchCue(s, assumptionCues); ok {
				add(s, model.Evidence{
					Kind:       model.EvidencePrompt,
					Ref:        sourceOriginalPrompt,
					LineStart:  i + 1,
					Detail:     fmt.Sprintf("original prompt line %d", i+1),
					CapturedAt: now,
				})
			}
		}
	}
	for _, ev := range events {
		for _, s := range sentences(ev.Summary) {
			if _, ok := matchCue(s, assumptionCues); ok {
				add(s, ev.AsEvidence())
			}
		}
	}
	for i, line := range transcript {
		for _, s := range sentences(line) {
			if _, ok := matchCue(s, assumptionCues); ok {
				add(s, model.Evidence{
					Kind:       model.EvidencePrompt,
					Ref:        refTranscript,
					LineStart:  i + 1,
					Detail:     fmt.Sprintf("transcript line %d", i+1),
					CapturedAt: now,
				})
			}
		}
	}
	return out
}

// nextActions proposes the continuation, failing tests first and then the
// requirements that are not complete.
//
// Failing tests lead because a red test is the one thing here backed by Level 1
// evidence; everything after it rests on keyword correlation. Every action is
// model.Recommended without exception: a next action is a proposal, never a
// statement of fact (plan §12).
func nextActions(det *model.EngineeringState, reqs []model.Requirement, now time.Time) []model.NextAction {
	out := make([]model.NextAction, 0, len(reqs)+2)
	for _, t := range failingTests(det) {
		ev := usableEvidence(model.DedupeEvidence(testEvidence(t, now)))
		model.SortEvidence(ev)
		out = append(out, model.NextAction{
			Description: "Fix failing test " + t.Name,
			Rationale:   "A failing test stays failed until a later result proves otherwise (plan §32).",
			Target:      symbolFromTest(t.Name),
			Priority:    len(out),
			Confidence:  model.Recommended,
			Evidence:    ev,
		})
	}
	for _, r := range reqs {
		if r.Status == model.ReqComplete {
			continue
		}
		// usableEvidence, not r.Evidence: the action needs its own array. Sharing
		// the requirement's would let a later append by either side overwrite the
		// other's citation.
		ev := usableEvidence(r.Evidence)
		if len(ev) == 0 {
			continue
		}
		out = append(out, model.NextAction{
			Description: fmt.Sprintf("Resolve %s: %s", r.ID, r.Description),
			Rationale:   rationaleFor(r),
			Target:      targetFor(r),
			Priority:    len(out),
			Confidence:  model.Recommended,
			Evidence:    ev,
			// GraphVerified stays false and SuggestedTests stays empty: only the
			// Graph verification layer may set those (plan §35).
		})
	}
	return out
}

// rationaleFor says why the action is proposed and, just as importantly, how
// weak the link is. Naming keyword correlation as the basis is what stops a
// reader from mistaking a heuristic guess for a finding.
func rationaleFor(r model.Requirement) string {
	switch r.Status {
	case model.ReqBlocked:
		return fmt.Sprintf("%s shares vocabulary with failing test %s (keyword correlation, not proof).",
			r.ID, firstRef(r.Evidence, model.EvidenceTest))
	case model.ReqPartial:
		return fmt.Sprintf("%s shares vocabulary with changed file %s, but nothing shows it is finished (keyword correlation, not proof).",
			r.ID, firstRef(r.Evidence, model.EvidenceFile))
	default:
		return fmt.Sprintf("%s has no repository evidence yet, and a heuristic cannot certify completion (plan §32).", r.ID)
	}
}

// targetFor names what the action operates on. model.NextAction.Target accepts a
// symbol or a file; for a file-correlated requirement this returns the path
// rather than guessing at a symbol inside it, because the extractor has no
// parser and a fabricated symbol would send Graph impact analysis nowhere.
func targetFor(r model.Requirement) string {
	if p := firstRef(r.Evidence, model.EvidenceFile); p != "" {
		return p
	}
	if n := firstRef(r.Evidence, model.EvidenceTest); n != "" {
		return symbolFromTest(n)
	}
	return ""
}

// firstRef returns the ref of the first evidence record of the given kind.
// Evidence slices are sorted before this runs, so "first" is deterministic.
func firstRef(ev []model.Evidence, kind model.EvidenceKind) string {
	for _, e := range ev {
		if e.Kind == kind {
			return e.Ref
		}
	}
	return ""
}

// usable reports whether an evidence record actually points at something a
// reader can go and check.
//
// A Kind on its own is not a citation. An Evidence{Kind: EvidenceTest} with an
// empty Ref names no test, yet it still counts as "has evidence" by length and
// model.ConfidenceFor still reads its kind as Level 1 and rates the claim
// Observed. That is uncertainty upgraded to certainty by an empty string, which
// is the one thing this package exists to prevent, so an uncitable record is
// treated as absent (plan §11, §31).
func usable(e model.Evidence) bool {
	return e.Kind != "" && strings.TrimSpace(e.Ref) != ""
}

// usableEvidence returns the citations that point at something, in source order,
// always in a freshly allocated slice with no spare capacity.
//
// The copy carries as much weight as the filter. Two claims must never share one
// evidence array: a later merge step appending a citation to one of them would
// write into the array the other is still using, so a requirement would silently
// end up citing a next action's evidence. Clipping the capacity makes any such
// append reallocate instead.
func usableEvidence(in []model.Evidence) []model.Evidence {
	out := make([]model.Evidence, 0, len(in))
	for _, e := range in {
		if usable(e) {
			out = append(out, e)
		}
	}
	return out[:len(out):len(out)]
}

// pruneUnevidenced drops every citation that points at nothing, then every claim
// left holding none, and re-derives confidence from what actually survived.
//
// This is the invariant from the package doc made mechanical: a claim nobody can
// check is worse than an absent claim, so it is removed rather than shipped with
// a low confidence. Re-deriving confidence matters because the deterministic
// state's own Evidence records are copied through verbatim by testEvidence and
// fileEvidence: a caller-supplied {Kind: commit, Ref: ""} would otherwise rate a
// requirement Observed on the strength of a citation nobody can follow.
//
// IDs already assigned are left alone; renumbering would break references held
// by anything that saw an earlier extraction. That is why the builders above
// refuse an uncitable source outright rather than leaving the gap to this pass.
func pruneUnevidenced(st *model.EngineeringState) {
	reqs := st.Requirements[:0]
	for _, r := range st.Requirements {
		if r.Evidence = usableEvidence(r.Evidence); len(r.Evidence) == 0 {
			continue
		}
		r.Confidence = model.ConfidenceFor(r.Evidence)
		reqs = append(reqs, r)
	}
	st.Requirements = reqs

	decs := st.Decisions[:0]
	for _, d := range st.Decisions {
		if d.Evidence = usableEvidence(d.Evidence); len(d.Evidence) == 0 {
			continue
		}
		decs = append(decs, d)
	}
	st.Decisions = decs

	rej := st.Rejected[:0]
	for _, x := range st.Rejected {
		if x.Evidence = usableEvidence(x.Evidence); len(x.Evidence) == 0 {
			continue
		}
		x.Confidence = model.ConfidenceFor(x.Evidence)
		rej = append(rej, x)
	}
	st.Rejected = rej

	asm := st.Assumptions[:0]
	for _, a := range st.Assumptions {
		if a.Evidence = usableEvidence(a.Evidence); len(a.Evidence) == 0 {
			continue
		}
		a.Confidence = model.ConfidenceFor(a.Evidence)
		asm = append(asm, a)
	}
	st.Assumptions = asm

	// A next action keeps model.Recommended whatever survives: it is a proposal,
	// never a statement of fact (plan §12).
	acts := st.NextActions[:0]
	for _, a := range st.NextActions {
		if a.Evidence = usableEvidence(a.Evidence); len(a.Evidence) == 0 {
			continue
		}
		acts = append(acts, a)
	}
	st.NextActions = acts
}

// agentAuthored reports whether an event's summary is the agent's own account
// of its work, as opposed to something said to it or about it.
//
// Only agent-authored text is mined for decisions and rejected approaches. The
// distinction is not cosmetic: a user prompt states what the task *is*, and
// mining it for rejection cues turns the goal into a warning. The observed
// failure was a prompt reading "coupons should be rejected if expired" being
// recorded as a rejected approach, which tells the next worker not to build the
// very thing they were asked for — worse than extracting nothing at all.
//
// Unknown records are excluded too: a record whose lifecycle name we could not
// map is not text we are entitled to interpret (plan §9).
func agentAuthored(ev model.AgentEvent) bool {
	switch ev.Type {
	case model.TurnEnded, model.ToolUsed, model.SubagentEnded:
		return true
	}
	return false
}
