package merge

import (
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// ApplyConstraint folds a mid-task constraint — the Buildathon Noon Curveball is
// the motivating case — into a state and returns a new state (plan §41).
//
// A constraint is additive. It records that the ground moved, and it annotates
// what the move affects:
//
//	ORIGINAL INTENT + NEW CONSTRAINT + AFFECTED REQUIREMENTS + CHANGED ASSUMPTIONS
//
// Task.OriginalIntent is therefore never touched here. Plan §41 is explicit:
// "Do not overwrite original intent." A reader must always be able to see both
// what was originally asked and what changed underneath it — overwriting the
// intent would erase the very thing that makes the curveball response legible.
//
// Requirements the constraint names are marked superseded by it and any
// completion they claimed is downgraded to partial: a new constraint invalidates
// prior completion, because the thing that was finished was finished against the
// old rules. Named assumptions are marked invalidated.
//
// The input state is never mutated; s may be nil.
func ApplyConstraint(s *model.EngineeringState, c model.Constraint, opts Options) *model.EngineeringState {
	stamp, stamped := opts.now()

	prevStatus := model.TaskStatus("")
	var out *model.EngineeringState
	if s == nil {
		out = model.NewEngineeringState(model.TaskRef{})
	} else {
		prevStatus = s.Status
		out = s.Clone()
	}

	// A constraint that arrived without a recorded time is stamped with the time
	// we learned about it. That is a fact we hold; the wall clock is not.
	if c.AddedAt.IsZero() && stamped {
		c.AddedAt = stamp
	}
	out.Constraints = unionBy(out.Constraints, []model.Constraint{c}, constraintKey, mergeConstraint)

	id := strings.TrimSpace(c.ID)
	if id == "" && len(c.AffectedRequirements) > 0 {
		// SupersededBy == "" means "still active" (model.Decision.Active), so an
		// unidentified constraint cannot be used to retire anything. We still
		// downgrade the requirements it names, and we say what we could not do
		// instead of leaving a silent no-op behind (plan §33).
		//
		// The note is only warranted when there is something it failed to
		// supersede. Reporting a degradation that did not occur costs the reader
		// exactly what an unreported one does: it makes the notes untrustworthy.
		out.Capture.Notes = append(out.Capture.Notes, fmt.Sprintf(
			"constraint %q carries no id, so affected requirements could not be marked superseded (plan §41)",
			c.Text))
	}

	for _, want := range c.AffectedRequirements {
		key := model.NormalizeID(want)
		found := false
		for i := range out.Requirements {
			if model.NormalizeID(out.Requirements[i].ID) != key {
				continue
			}
			found = true
			r := &out.Requirements[i]
			changed := false
			if id != "" && r.SupersededBy != id {
				r.SupersededBy = id
				changed = true
			}
			if r.Status == model.ReqComplete {
				// plan §41: a new constraint invalidates prior completion. What
				// was complete was complete against the previous requirements,
				// and claiming otherwise is the fabrication this package exists
				// to prevent.
				r.Status = model.ReqPartial
				changed = true
			}
			// Only a real change moves UpdatedAt, so replaying the same
			// constraint event twice leaves the state byte-identical.
			if changed && stamped {
				r.UpdatedAt = stamp
			}
		}
		if !found {
			out.Capture.Notes = append(out.Capture.Notes, fmt.Sprintf(
				"constraint %q names requirement %q, which is not present in this state (plan §41)",
				constraintLabel(c), want))
		}
	}

	for _, want := range c.ChangedAssumptions {
		key := normalizeText(want)
		found := false
		for i := range out.Assumptions {
			if normalizeText(out.Assumptions[i].Statement) != key {
				continue
			}
			found = true
			out.Assumptions[i].Invalidated = true
		}
		if !found {
			// We do not invent the assumption. Recording an assumption we never
			// observed, purely so it can be crossed out, would be fabrication.
			out.Capture.Notes = append(out.Capture.Notes, fmt.Sprintf(
				"constraint %q names assumption %q, which is not present in this state (plan §41)",
				constraintLabel(c), want))
		}
	}

	// Downgrading a requirement changes what the evidence implies about the task
	// as a whole, so the status is recomputed here for the same reason States
	// recomputes it: a document that says "verified" next to a requirement it
	// just marked partial contradicts itself.
	applyDerivedStatus(out, prevStatus)

	if stamped {
		out.GeneratedAt = stamp
	}

	finalize(out)
	return out
}

// constraintLabel names a constraint in a note, preferring its id and falling
// back to its text so the note still identifies something concrete.
func constraintLabel(c model.Constraint) string {
	if id := strings.TrimSpace(c.ID); id != "" {
		return id
	}
	return c.Text
}
