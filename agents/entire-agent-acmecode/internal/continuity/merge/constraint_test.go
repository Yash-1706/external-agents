package merge

import (
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// curveball is the shape plan §41 describes: a requirement arriving mid-task
// that annotates existing work rather than replacing the original intent.
func curveball(mutate func(c *model.Constraint)) model.Constraint {
	c := model.Constraint{
		ID:                   "C1",
		Text:                 "the client must work with no network at all",
		Source:               "buildathon_curveball",
		AffectedRequirements: []string{"R1"},
		ChangedAssumptions:   []string{"the payment API is reachable"},
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func constrainedState(mutate func(s *model.EngineeringState)) *model.EngineeringState {
	return newState(func(s *model.EngineeringState) {
		s.Status = model.StatusVerified
		s.Requirements = []model.Requirement{
			{
				ID: "R1", Description: "retry on 5xx", Status: model.ReqComplete,
				Confidence: model.Observed, Evidence: []model.Evidence{commitEv("abc123")},
			},
			{
				ID: "R2", Description: "log every attempt", Status: model.ReqComplete,
				Confidence: model.Observed, Evidence: []model.Evidence{commitEv("def456")},
			},
		}
		s.Assumptions = []model.Assumption{
			{Statement: "the payment API is reachable", Confidence: model.Inferred},
			{Statement: "retries are cheap", Confidence: model.Inferred},
		}
		if mutate != nil {
			mutate(s)
		}
	})
}

func TestApplyConstraint(t *testing.T) {
	tests := []struct {
		name  string
		state *model.EngineeringState
		c     model.Constraint
		check func(t *testing.T, got *model.EngineeringState)
	}{
		{
			name:  "ApplyConstraint leaves OriginalIntent untouched",
			state: constrainedState(nil),
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				want := "add retry with backoff to the payment client"
				if got.Task.OriginalIntent != want {
					// plan §41: "Do not overwrite original intent."
					t.Fatalf("original intent = %q, want %q", got.Task.OriginalIntent, want)
				}
				if got.Task.Title != "Add retry to the payment client" {
					t.Fatalf("title = %q, want the original title", got.Task.Title)
				}
				if got.Task.ID != "task_abc" {
					t.Fatalf("task id = %q, want task_abc", got.Task.ID)
				}
				if len(got.Constraints) != 1 || got.Constraints[0].Text != "the client must work with no network at all" {
					t.Fatalf("constraints = %+v, want the new constraint recorded alongside the intent", got.Constraints)
				}
			},
		},
		{
			name:  "a named requirement is superseded and its completion downgraded",
			state: constrainedState(nil),
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				r1 := requireRequirement(t, got, "R1")
				if r1.Status != model.ReqPartial {
					t.Fatalf("R1 status = %q, want partial: a new constraint invalidates prior completion", r1.Status)
				}
				if r1.SupersededBy != "C1" {
					t.Fatalf("R1 superseded by = %q, want C1", r1.SupersededBy)
				}
				if !r1.UpdatedAt.Equal(at(0)) {
					t.Fatalf("R1 updated at = %v, want the injected clock time", r1.UpdatedAt)
				}
				if !hasEvidence(r1.Evidence, commitEv("abc123")) {
					t.Fatal("the evidence for the work already done was dropped")
				}
			},
		},
		{
			name:  "requirements the constraint does not name are left alone",
			state: constrainedState(nil),
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				r2 := requireRequirement(t, got, "R2")
				if r2.Status != model.ReqComplete {
					t.Fatalf("R2 status = %q, want complete", r2.Status)
				}
				if r2.SupersededBy != "" {
					t.Fatalf("R2 superseded by = %q, want empty", r2.SupersededBy)
				}
				if !r2.UpdatedAt.IsZero() {
					t.Fatalf("R2 updated at = %v, want untouched", r2.UpdatedAt)
				}
			},
		},
		{
			name: "an affected requirement that was not complete is only superseded",
			state: constrainedState(func(s *model.EngineeringState) {
				s.Requirements[0].Status = model.ReqUnresolved
			}),
			c: curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R1"); r.Status != model.ReqUnresolved {
					t.Fatalf("R1 status = %q, want unresolved: the constraint downgrades completion, it does not invent progress", r.Status)
				}
			},
		},
		{
			name:  "named assumptions are invalidated and others are not",
			state: constrainedState(nil),
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				for _, a := range got.Assumptions {
					switch a.Statement {
					case "the payment API is reachable":
						if !a.Invalidated {
							t.Fatal("the assumption the constraint contradicts was not invalidated")
						}
					case "retries are cheap":
						if a.Invalidated {
							t.Fatal("an unrelated assumption was invalidated")
						}
					}
				}
			},
		},
		{
			name:  "a constraint with no id downgrades but reports it cannot supersede",
			state: constrainedState(nil),
			c:     curveball(func(c *model.Constraint) { c.ID = "" }),
			check: func(t *testing.T, got *model.EngineeringState) {
				r1 := requireRequirement(t, got, "R1")
				if r1.Status != model.ReqPartial {
					t.Fatalf("R1 status = %q, want partial", r1.Status)
				}
				if r1.SupersededBy != "" {
					t.Fatalf("R1 superseded by = %q; an empty id would read as still active", r1.SupersededBy)
				}
				if !noteContaining(got, "carries no id") {
					t.Fatalf("the degraded application was not reported: %v", got.Capture.Notes)
				}
			},
		},
		{
			// The note is a report of a degradation. With nothing to supersede,
			// nothing was degraded, and reporting one anyway teaches the reader to
			// discount the notes that do matter.
			name:  "a constraint with no id and nothing to supersede reports nothing",
			state: constrainedState(nil),
			c: curveball(func(c *model.Constraint) {
				c.ID = ""
				c.AffectedRequirements = nil
				c.ChangedAssumptions = nil
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if noteContaining(got, "carries no id") {
					t.Fatalf("reported a failure to supersede requirements the constraint never named: %v", got.Capture.Notes)
				}
				if len(got.Constraints) != 1 {
					t.Fatalf("constraints = %d, want the constraint still recorded", len(got.Constraints))
				}
			},
		},
		{
			name:  "a requirement the constraint names but the state lacks is reported",
			state: constrainedState(nil),
			c:     curveball(func(c *model.Constraint) { c.AffectedRequirements = []string{"R1", "R9"} }),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Requirements) != 2 {
					t.Fatalf("requirements = %d, want 2: a missing requirement is not invented", len(got.Requirements))
				}
				if !noteContaining(got, `requirement "R9"`) {
					t.Fatalf("the dangling reference was silently ignored: %v", got.Capture.Notes)
				}
			},
		},
		{
			name:  "an assumption the constraint names but the state lacks is reported",
			state: constrainedState(nil),
			c:     curveball(func(c *model.Constraint) { c.ChangedAssumptions = []string{"the clock is monotonic"} }),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Assumptions) != 2 {
					t.Fatalf("assumptions = %d, want 2: a missing assumption is not invented so it can be crossed out", len(got.Assumptions))
				}
				if !noteContaining(got, `assumption "the clock is monotonic"`) {
					t.Fatalf("the dangling reference was silently ignored: %v", got.Capture.Notes)
				}
			},
		},
		{
			name:  "requirement ids are matched after normalization",
			state: constrainedState(nil),
			c:     curveball(func(c *model.Constraint) { c.AffectedRequirements = []string{" r1 "} }),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R1"); r.SupersededBy != "C1" {
					t.Fatalf("R1 superseded by = %q, want C1", r.SupersededBy)
				}
				if noteContaining(got, "not present in this state") {
					t.Fatalf("case drift was treated as a dangling reference: %v", got.Capture.Notes)
				}
			},
		},
		{
			name:  "the task status is recomputed once completion is downgraded",
			state: constrainedState(nil),
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				if got.Status != model.StatusPartial {
					t.Fatalf("status = %q, want partial: a document must not claim verified next to a requirement it just downgraded", got.Status)
				}
			},
		},
		{
			name:  "an undated constraint is stamped from the injected clock",
			state: constrainedState(nil),
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				if !got.Constraints[0].AddedAt.Equal(at(0)) {
					t.Fatalf("added at = %v, want the injected clock time", got.Constraints[0].AddedAt)
				}
			},
		},
		{
			name:  "a constraint that carries its own time keeps it",
			state: constrainedState(nil),
			c:     curveball(func(c *model.Constraint) { c.AddedAt = at(30) }),
			check: func(t *testing.T, got *model.EngineeringState) {
				if !got.Constraints[0].AddedAt.Equal(at(30)) {
					t.Fatalf("added at = %v, want %v", got.Constraints[0].AddedAt, at(30))
				}
			},
		},
		{
			name:  "a nil state yields a state carrying only the constraint",
			state: nil,
			c:     curveball(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Constraints) != 1 {
					t.Fatalf("constraints = %d, want 1", len(got.Constraints))
				}
				if len(got.Requirements) != 0 || len(got.Assumptions) != 0 {
					t.Fatal("nothing may be invented to satisfy the constraint's references")
				}
				if !noteContaining(got, "not present in this state") {
					t.Fatalf("the dangling references were not reported: %v", got.Capture.Notes)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyConstraint(tc.state, tc.c, testOpts())
			if got == nil {
				t.Fatal("ApplyConstraint returned nil")
			}
			tc.check(t, got)
		})
	}
}

// TestApplyConstraintDoesNotMutateInput protects the caller's copy the same way
// States does.
func TestApplyConstraintDoesNotMutateInput(t *testing.T) {
	s := constrainedState(nil)
	before := mustJSON(t, s)

	ApplyConstraint(s, curveball(nil), testOpts())

	if got := mustJSON(t, s); got != before {
		t.Fatalf("input state was mutated:\n%s\n%s", before, got)
	}
}

// TestApplyConstraintIsIdempotent matters because constraints arrive as replayed
// ConstraintAdded events: applying the same one twice must not fork the
// constraint list, duplicate the notes, or churn UpdatedAt.
func TestApplyConstraintIsIdempotent(t *testing.T) {
	opts := testOpts()
	once := ApplyConstraint(constrainedState(nil), curveball(nil), opts)
	twice := ApplyConstraint(once, curveball(nil), opts)

	if len(twice.Constraints) != 1 {
		t.Fatalf("constraints = %d, want 1", len(twice.Constraints))
	}
	if once.Hash() != twice.Hash() {
		t.Fatalf("re-applying the same constraint changed the state:\n%s\n%s",
			mustJSON(t, once), mustJSON(t, twice))
	}
}

// TestApplyConstraintThenMergeKeepsTheCurveball walks the §42 curveball path:
// the constraint is applied, the task is checkpointed, and a later capture from
// a fresh agent merges in without losing either the constraint or the intent.
func TestApplyConstraintThenMergeKeepsTheCurveball(t *testing.T) {
	opts := testOpts()
	constrained := ApplyConstraint(constrainedState(nil), curveball(nil), opts)

	// A fresh session re-extracts the task and, knowing nothing of the
	// curveball, reports R1 as complete again — with an inference only.
	fresh := model.NewEngineeringState(model.TaskRef{ID: "task_abc"})
	fresh.Requirements = []model.Requirement{{
		ID: "R1", Status: model.ReqComplete, Evidence: []model.Evidence{inferEv("looks done to me")},
	}}

	got := States(constrained, fresh, opts)

	if got.Task.OriginalIntent != "add retry with backoff to the payment client" {
		t.Fatalf("original intent = %q, want it preserved across the merge", got.Task.OriginalIntent)
	}
	if len(got.Constraints) != 1 {
		t.Fatalf("constraints = %d, want the curveball retained", len(got.Constraints))
	}
	r1 := requireRequirement(t, got, "R1")
	if r1.Status != model.ReqPartial {
		t.Fatalf("R1 status = %q, want partial: an unproven claim cannot undo the constraint's downgrade", r1.Status)
	}
	if r1.SupersededBy != "C1" {
		t.Fatalf("R1 superseded by = %q, want C1 to survive the merge", r1.SupersededBy)
	}
	for _, a := range got.Assumptions {
		if a.Statement == "the payment API is reachable" && !a.Invalidated {
			t.Fatal("the invalidated assumption was revalidated by the merge")
		}
	}
}
