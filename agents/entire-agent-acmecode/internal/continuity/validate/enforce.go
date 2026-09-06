package validate

import (
	"fmt"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Enforce returns a corrected clone of s together with one Finding per
// correction it made. The input is never modified, so a caller can show the
// before and after (plan §33 wants the degradation to be visible, not silent).
//
// Three corrections are applied, and only these three:
//
//  1. A requirement marked complete without repository or checkpoint evidence
//     is downgraded to partial (plan §32).
//  2. Any confidence stronger than model.ConfidenceFor of its own evidence is
//     lowered to that value (plan §12).
//  3. A next action's confidence is set to model.Recommended, because a
//     proposal is not a statement of fact (plan §12).
//
// Everything else State reports is left for a human or a caller to resolve.
// In particular a test recorded as passed without a result is reported but not
// rewritten: guessing whether it actually passed, failed or was never run would
// substitute one fabrication for another. Callers must treat an Error finding
// from State on the enforced state as blocking rather than assume Enforce
// cleared it.
//
// Enforce never strengthens anything. It cannot promote a requirement, cannot
// raise a confidence, and cannot add or alter evidence. model.Unknown is always
// left as it is: plan §32 makes "unknown" a legitimate final answer, so a claim
// its author declined to make is never made on their behalf. The one apparent
// exception is rule 3, which may set an Unknown next action to Recommended;
// that is a category correction rather than a factual one, since Recommended
// asserts nothing about reality, only that the step is being proposed.
//
// A nil state is returned unchanged as nil with no findings.
func Enforce(s *model.EngineeringState) (*model.EngineeringState, []Finding) {
	if s == nil {
		return nil, nil
	}
	out := s.Clone()
	var findings []Finding

	for i := range out.Requirements {
		r := &out.Requirements[i]
		p := elemPath("requirements", r.ID, i)
		if r.Status == model.ReqComplete && !hasVerifiableEvidence(r.Evidence) {
			// Partial, not unresolved: work plainly happened, we simply cannot
			// certify it finished. Downgrading further than the evidence
			// requires would be its own kind of dishonesty.
			r.Status = model.ReqPartial
			add(&findings, Error, codeReqCompleteNoEvidence, p,
				fmt.Sprintf("downgraded %q to %q: completion needs repository or checkpoint evidence",
					model.ReqComplete, model.ReqPartial))
		}
		lowerConfidence(&findings, p+".confidence", &r.Confidence, r.Evidence)
	}

	for i := range out.CompletedWork {
		w := &out.CompletedWork[i]
		lowerConfidence(&findings, elemPath("completed_work", "", i)+".confidence", &w.Confidence, w.Evidence)
	}
	for i := range out.InProgress {
		w := &out.InProgress[i]
		lowerConfidence(&findings, elemPath("in_progress", "", i)+".confidence", &w.Confidence, w.Evidence)
	}
	for i := range out.Rejected {
		r := &out.Rejected[i]
		lowerConfidence(&findings, elemPath("rejected_approaches", r.ID, i)+".confidence", &r.Confidence, r.Evidence)
	}
	for i := range out.Assumptions {
		a := &out.Assumptions[i]
		lowerConfidence(&findings, elemPath("assumptions", "", i)+".confidence", &a.Confidence, a.Evidence)
	}
	for i := range out.Risks {
		r := &out.Risks[i]
		lowerConfidence(&findings, elemPath("risks", "", i)+".confidence", &r.Confidence, r.Evidence)
	}

	for i := range out.NextActions {
		a := &out.NextActions[i]
		if a.Confidence == model.Recommended {
			continue
		}
		was := a.Confidence
		a.Confidence = model.Recommended
		add(&findings, Error, codeNextActionNotRecommended, elemPath("next_actions", "", i)+".confidence",
			fmt.Sprintf("reset confidence %q to %q: a next action is a proposal, not a statement of fact", was, model.Recommended))
	}

	return out, findings
}

// lowerConfidence weakens c to what its evidence supports, recording the
// correction. model.ConfidenceFor is the single chokepoint for that judgement
// (plan §12); this function only ever moves downwards along confidenceRank, so
// it is a no-op once applied - which is what makes Enforce idempotent.
//
// An unrecognised confidence is left alone on purpose: State reports it as
// INVALID_ENUM, and rewriting a value whose meaning is unknown would be a guess.
func lowerConfidence(out *[]Finding, path string, c *model.Confidence, ev []model.Evidence) {
	if !c.Valid() {
		return
	}
	derived := derivedConfidence(ev)
	if confidenceRank(*c) <= confidenceRank(derived) {
		return
	}
	was := *c
	*c = derived
	add(out, Error, codeConfidenceExceedsEvidence, path,
		fmt.Sprintf("lowered confidence from %s to %s: the attached evidence supports no more", was.Label(), derived.Label()))
}
