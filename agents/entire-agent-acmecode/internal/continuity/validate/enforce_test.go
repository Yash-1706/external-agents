package validate

import (
	"reflect"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func TestEnforceNilIsNotAnError(t *testing.T) {
	got, findings := Enforce(nil)
	if got != nil || findings != nil {
		t.Fatalf("Enforce(nil) = (%v, %v), want (nil, nil)", got, findings)
	}
}

func TestEnforceLeavesACleanStateAlone(t *testing.T) {
	in := clean()
	out, findings := Enforce(in)
	if len(findings) != 0 {
		t.Errorf("clean state was corrected:\n%s", format(findings))
	}
	if in.Hash() != out.Hash() {
		t.Errorf("clean state changed under Enforce")
	}
}

func TestEnforceCorrections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.EngineeringState)
		check  func(*testing.T, *model.EngineeringState)
		wants  []want
	}{
		{
			name: "a completion backed only by inference is downgraded to partial",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Confidence = model.Inferred
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceInference, Ref: "extractor"}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.Requirements[0].Status; got != model.ReqPartial {
					t.Errorf("status = %q, want %q", got, model.ReqPartial)
				}
				if got := s.Requirements[0].Confidence; got != model.Inferred {
					t.Errorf("confidence = %q, want it left at %q", got, model.Inferred)
				}
			},
			wants: []want{{code: codeReqCompleteNoEvidence, path: "requirements[R1]", severity: Error}},
		},
		{
			name: "a completion with no evidence loses both its status and its grade",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = nil
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.Requirements[0].Status; got != model.ReqPartial {
					t.Errorf("status = %q, want %q", got, model.ReqPartial)
				}
				if got := s.Requirements[0].Confidence; got != model.Unknown {
					t.Errorf("confidence = %q, want %q", got, model.Unknown)
				}
			},
			wants: []want{
				{code: codeReqCompleteNoEvidence, path: "requirements[R1]", severity: Error},
				{code: codeConfidenceExceedsEvidence, path: "requirements[R1].confidence", severity: Error},
			},
		},
		{
			// Before this was fixed, Enforce made no correction at all here:
			// EvidenceKind.Level reads only the kind, so a commit citation with
			// no sha certified the requirement complete at OBSERVED while
			// pointing at nothing a reader could open.
			name: "a completion backed only by a citation to nothing is downgraded",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceCommit}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.Requirements[0].Status; got != model.ReqPartial {
					t.Errorf("status = %q, want %q", got, model.ReqPartial)
				}
				if got := s.Requirements[0].Confidence; got != model.Unknown {
					t.Errorf("confidence = %q, want %q", got, model.Unknown)
				}
				// The orphaned citation is reported, never deleted: Enforce
				// weakens claims, it does not tidy away the evidence trail.
				if len(s.Requirements[0].Evidence) != 1 {
					t.Errorf("Enforce dropped the orphaned evidence: %v", s.Requirements[0].Evidence)
				}
				if _, ok := find(State(s), codeOrphanEvidence, "requirements[R1].evidence[0]"); !ok {
					t.Error("State no longer reports ORPHAN_EVIDENCE on the enforced state")
				}
			},
			wants: []want{
				{code: codeReqCompleteNoEvidence, path: "requirements[R1]", severity: Error},
				{code: codeConfidenceExceedsEvidence, path: "requirements[R1].confidence", severity: Error},
			},
		},
		{
			name: "a checkpoint-backed completion stands",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceCheckpoint, Ref: "cp_1"}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.Requirements[0].Status; got != model.ReqComplete {
					t.Errorf("status = %q, want it left at %q", got, model.ReqComplete)
				}
			},
			wants: []want{{code: codeReqCompleteNoEvidence, absent: true}},
		},
		{
			name: "observed on statement evidence falls to inferred",
			mutate: func(s *model.EngineeringState) {
				s.Risks = []model.Risk{{
					Description: "retries may double-charge",
					Confidence:  model.Observed,
					Evidence:    []model.Evidence{{Kind: model.EvidencePrompt, Ref: "p_1"}},
				}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.Risks[0].Confidence; got != model.Inferred {
					t.Errorf("confidence = %q, want %q", got, model.Inferred)
				}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, path: "risks[#0].confidence", severity: Error}},
		},
		{
			name: "recommended on a work item is a factual claim it cannot support",
			mutate: func(s *model.EngineeringState) {
				s.InProgress = []model.WorkItem{{Description: "refactor the dispatcher", Confidence: model.Recommended}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.InProgress[0].Confidence; got != model.Unknown {
					t.Errorf("confidence = %q, want %q", got, model.Unknown)
				}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, path: "in_progress[#0].confidence", severity: Error}},
		},
		{
			name: "unknown survives evidence that would justify more (plan §32)",
			mutate: func(s *model.EngineeringState) {
				s.Assumptions = []model.Assumption{{
					Statement:  "staging mirrors production",
					Confidence: model.Unknown,
					Evidence:   []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}},
				}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.Assumptions[0].Confidence; got != model.Unknown {
					t.Errorf("confidence = %q, want it left at %q", got, model.Unknown)
				}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, absent: true}},
		},
		{
			name: "a confidence outside the vocabulary is reported, never rewritten",
			mutate: func(s *model.EngineeringState) {
				s.CompletedWork = []model.WorkItem{{Description: "wired the retry queue", Confidence: "certain"}}
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.CompletedWork[0].Confidence; got != model.Confidence("certain") {
					t.Errorf("confidence = %q, want it left untouched", got)
				}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, absent: true}},
		},
		{
			name: "a next action graded as fact is reset to recommended",
			mutate: func(s *model.EngineeringState) {
				s.NextActions[0].Confidence = model.Observed
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.NextActions[0].Confidence; got != model.Recommended {
					t.Errorf("confidence = %q, want %q", got, model.Recommended)
				}
			},
			wants: []want{{code: codeNextActionNotRecommended, path: "next_actions[#0].confidence", severity: Error}},
		},
		{
			name: "an unset next-action confidence is still normalised",
			mutate: func(s *model.EngineeringState) {
				s.NextActions[0].Confidence = ""
				s.NextActions[0].Evidence = nil
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				if got := s.NextActions[0].Confidence; got != model.Recommended {
					t.Errorf("confidence = %q, want %q", got, model.Recommended)
				}
				if len(s.NextActions[0].Evidence) != 0 {
					t.Errorf("Enforce invented evidence for a next action: %v", s.NextActions[0].Evidence)
				}
			},
			wants: []want{{code: codeNextActionNotRecommended, path: "next_actions[#0].confidence", severity: Error}},
		},
		{
			name: "a passing test with no result is reported by State but never rewritten here",
			mutate: func(s *model.EngineeringState) {
				s.Tests[0].Evidence = nil
			},
			check: func(t *testing.T, s *model.EngineeringState) {
				// Enforce cannot know whether the test passed, failed or never
				// ran, so it refuses to guess; State still flags it as an Error.
				if got := s.Tests[0].Status; got != model.TestPassed {
					t.Errorf("status = %q, want it left at %q", got, model.TestPassed)
				}
				if _, ok := find(State(s), codeTestPassNoResult, "tests[TestDuplicateWebhook]"); !ok {
					t.Error("State no longer reports TEST_PASS_NO_RESULT on the enforced state")
				}
			},
			wants: []want{{code: codeTestPassNoResult, absent: true}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := clean()
			tc.mutate(in)
			before := in.Hash()

			out, findings := Enforce(in)

			if got := in.Hash(); got != before {
				t.Errorf("Enforce mutated its input: hash %s -> %s", before, got)
			}
			for _, w := range tc.wants {
				if w.absent {
					if f, ok := findByCode(findings, w.code); ok {
						t.Errorf("unexpected correction %s at %q: %s", f.Code, f.Path, f.Message)
					}
					continue
				}
				f, ok := find(findings, w.code, w.path)
				if !ok {
					t.Errorf("missing correction %s at %q; got:\n%s", w.code, w.path, format(findings))
					continue
				}
				if f.Severity != w.severity {
					t.Errorf("%s at %q: severity = %q, want %q", w.code, w.path, f.Severity, w.severity)
				}
			}
			tc.check(t, out)
		})
	}
}

// corpus is the set of hand-built states the invariant tests run over. Between
// them they cover every evidence level, every confidence value, absent
// evidence, orphaned evidence and values outside the frozen vocabulary.
func corpus() map[string]*model.EngineeringState {
	overclaimed := clean()
	overclaimed.Requirements[0].Evidence = nil
	overclaimed.CompletedWork = []model.WorkItem{{Description: "shipped it", Confidence: model.Observed}}
	overclaimed.InProgress = []model.WorkItem{{Description: "still going", Confidence: model.Inferred}}
	overclaimed.Assumptions = []model.Assumption{{Statement: "queue is ordered", Confidence: model.Observed}}
	overclaimed.Risks = []model.Risk{{Description: "double charge", Confidence: model.Observed}}
	overclaimed.Rejected = []model.RejectedApproach{{ID: "X1", Approach: "dedupe at the LB", Confidence: model.Observed}}
	overclaimed.NextActions[0].Confidence = model.Observed

	inferenceOnly := clean()
	inf := []model.Evidence{{Kind: model.EvidenceInference, Ref: "extractor"}}
	inferenceOnly.Requirements[0].Evidence = inf
	inferenceOnly.CompletedWork = []model.WorkItem{{Description: "shipped it", Confidence: model.Observed, Evidence: inf}}
	inferenceOnly.Assumptions = []model.Assumption{{Statement: "queue is ordered", Confidence: model.Observed, Evidence: inf}}

	statementOnly := clean()
	stmt := []model.Evidence{{Kind: model.EvidenceEvent, Ref: "ev_9"}}
	statementOnly.Requirements[0].Evidence = stmt
	statementOnly.Risks = []model.Risk{{Description: "double charge", Confidence: model.Observed, Evidence: stmt}}

	allUnknown := clean()
	allUnknown.Requirements[0].Confidence = model.Unknown
	allUnknown.CompletedWork = []model.WorkItem{{
		Description: "shipped it",
		Confidence:  model.Unknown,
		Evidence:    []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}},
	}}
	allUnknown.Assumptions = []model.Assumption{{
		Statement:  "queue is ordered",
		Confidence: model.Unknown,
		Evidence:   []model.Evidence{{Kind: model.EvidenceCheckpoint, Ref: "cp_1"}},
	}}

	garbage := clean()
	garbage.Requirements[0].Confidence = "certain"
	garbage.Requirements[0].Status = "done"
	garbage.Risks = []model.Risk{{Description: "?", Confidence: "probably", Evidence: []model.Evidence{{Kind: "vibes"}}}}

	orphaned := clean()
	orphaned.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceCommit}}

	return map[string]*model.EngineeringState{
		"empty":          model.NewEngineeringState(model.TaskRef{ID: "task_1"}),
		"clean":          clean(),
		"messy":          messy(),
		"overclaimed":    overclaimed,
		"inference_only": inferenceOnly,
		"statement_only": statementOnly,
		"all_unknown":    allUnknown,
		"garbage_enums":  garbage,
		"orphaned":       orphaned,
	}
}

// corpusNames lists the corpus keys in a fixed order. Ranging over the map
// directly would let Go's randomised map iteration decide the order in which
// subtests run and report, which is exactly the non-determinism this project
// forbids in its own output.
func corpusNames() []string {
	return []string{
		"empty", "clean", "messy", "overclaimed", "inference_only",
		"statement_only", "all_unknown", "garbage_enums", "orphaned",
	}
}

func TestEnforceIsIdempotent(t *testing.T) {
	states := corpus()
	for _, name := range corpusNames() {
		t.Run(name, func(t *testing.T) {
			once, first := Enforce(states[name])
			twice, second := Enforce(once)

			if !reflect.DeepEqual(once, twice) {
				t.Errorf("Enforce(Enforce(x)) != Enforce(x)\nonce:  %+v\ntwice: %+v", once, twice)
			}
			if once.Hash() != twice.Hash() {
				t.Errorf("hash changed on the second pass: %s -> %s", once.Hash(), twice.Hash())
			}
			// A second pass finding anything to correct would mean the first
			// pass left the state indefensible.
			if len(second) != 0 {
				t.Errorf("second pass still corrected %d things (first pass found %d):\n%s",
					len(second), len(first), format(second))
			}
		})
	}
}

func TestEnforceNeverStrengthensAClaim(t *testing.T) {
	states := corpus()
	for _, name := range corpusNames() {
		t.Run(name, func(t *testing.T) {
			// Compare against a clone of the input: Enforce works on a clone, so
			// this keeps the JSON round-trip out of the comparison and leaves
			// only Enforce's own edits visible.
			before := states[name].Clone()
			after, _ := Enforce(states[name])

			assertSameShape(t, before, after)

			for i := range before.Requirements {
				b, a := before.Requirements[i], after.Requirements[i]
				if a.Status.Rank() > b.Status.Rank() {
					t.Errorf("requirements[%s]: status promoted %q -> %q", b.ID, b.Status, a.Status)
				}
				if a.ID != b.ID || a.Description != b.Description {
					t.Errorf("requirements[%d]: identity rewritten: %q/%q -> %q/%q", i, b.ID, b.Description, a.ID, a.Description)
				}
			}

			// Every confidence that states something about reality may only move
			// down. Next actions are excluded: their grade is definitional
			// (plan §12) and is checked separately below.
			//
			// The comparison uses the test's own ordering, not the package's
			// confidenceRank. Grading Enforce with the same scale Enforce grades
			// itself by would make this assertion a tautology: invert
			// confidenceRank and both sides invert together, so the test would
			// keep passing while Enforce stopped downgrading anything.
			bc, ac := factualConfidences(before), factualConfidences(after)
			for i := range bc {
				if asserts(ac[i]) > asserts(bc[i]) {
					t.Errorf("factual confidence %d strengthened: %q -> %q", i, bc[i], ac[i])
				}
			}

			for i := range before.NextActions {
				if got := after.NextActions[i].Confidence; got != model.Recommended {
					t.Errorf("next_actions[%d]: confidence = %q, want %q", i, got, model.Recommended)
				}
				if b, a := before.NextActions[i], after.NextActions[i]; b.Description != a.Description ||
					!reflect.DeepEqual(b.Evidence, a.Evidence) || b.Priority != a.Priority {
					t.Errorf("next_actions[%d]: Enforce edited more than the confidence", i)
				}
			}

			// Nothing may be cited that was not cited before. Enforce weakens
			// claims; it is not allowed to go and find support for them.
			if b, a := before.AllEvidence(), after.AllEvidence(); !reflect.DeepEqual(b, a) {
				t.Errorf("evidence changed under Enforce:\nbefore: %v\nafter:  %v", b, a)
			}
		})
	}
}

func TestEnforceOutputSatisfiesItsOwnCorrectionRules(t *testing.T) {
	// The three rules Enforce implements must have nothing left to say about
	// the state it returns; anything else it reports is a rule it deliberately
	// does not rewrite (see Enforce's doc comment).
	corrected := map[string]bool{
		codeReqCompleteNoEvidence:     true,
		codeConfidenceExceedsEvidence: true,
		codeNextActionNotRecommended:  true,
	}
	states := corpus()
	for _, name := range corpusNames() {
		t.Run(name, func(t *testing.T) {
			out, _ := Enforce(states[name])
			for _, f := range State(out) {
				if corrected[f.Code] {
					t.Errorf("Enforce left a correctable finding behind: %s at %q: %s", f.Code, f.Path, f.Message)
				}
			}
		})
	}
}

func TestEnforceIsDeterministic(t *testing.T) {
	states := corpus()
	for _, name := range corpusNames() {
		t.Run(name, func(t *testing.T) {
			a, fa := Enforce(states[name])
			b, fb := Enforce(states[name])
			if !reflect.DeepEqual(a, b) {
				t.Error("two runs over the same input produced different states")
			}
			if !reflect.DeepEqual(fa, fb) {
				t.Errorf("two runs over the same input produced different findings:\n%s\n%s", format(fa), format(fb))
			}
		})
	}
}

// asserts scores how much a confidence value claims about reality, written out
// here independently of the package's own confidenceRank so that the invariant
// tests measure Enforce against a fixed external scale rather than against the
// scale Enforce uses. If the two ever disagree, one of them is wrong and the
// tests say so instead of moving in lockstep.
//
// A value outside the vocabulary scores 0: it claims nothing this test can read,
// and Enforce is required to leave it untouched rather than guess.
func asserts(c model.Confidence) int {
	switch c {
	case model.Observed:
		return 3
	case model.Inferred:
		return 2
	case model.Recommended:
		return 1
	}
	return 0
}

// TestAssertsAgreesWithConfidenceRank keeps the test's independent scale honest.
// It is allowed to be a second opinion; it is not allowed to be a stale one.
func TestAssertsAgreesWithConfidenceRank(t *testing.T) {
	for _, c := range []model.Confidence{model.Observed, model.Inferred, model.Recommended, model.Unknown, "certain"} {
		if asserts(c) != confidenceRank(c) {
			t.Errorf("asserts(%q)=%d but confidenceRank(%q)=%d", c, asserts(c), c, confidenceRank(c))
		}
	}
}

// TestEnforceLowersEveryOverclaimToAConcreteValue is the value-level companion to
// the invariant tests: it names the exact confidence Enforce must land on, so a
// change to the ordering cannot be absorbed silently by a relative comparison.
func TestEnforceLowersEveryOverclaimToAConcreteValue(t *testing.T) {
	cases := []struct {
		name string
		in   model.Confidence
		ev   []model.Evidence
		want model.Confidence
	}{
		{"observed on nothing falls to unknown", model.Observed, nil, model.Unknown},
		{"observed on a prompt falls to inferred", model.Observed,
			[]model.Evidence{{Kind: model.EvidencePrompt, Ref: "p_1"}}, model.Inferred},
		{"observed on a commit stands", model.Observed,
			[]model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}}, model.Observed},
		{"observed on an uncitable commit falls to unknown", model.Observed,
			[]model.Evidence{{Kind: model.EvidenceCommit}}, model.Unknown},
		{"inferred on nothing falls to unknown", model.Inferred, nil, model.Unknown},
		{"inferred on an inference stands", model.Inferred,
			[]model.Evidence{{Kind: model.EvidenceInference, Ref: "extractor"}}, model.Inferred},
		{"recommended on nothing falls to unknown", model.Recommended, nil, model.Unknown},
		{"unknown is never raised", model.Unknown,
			[]model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}}, model.Unknown},
		{"an unrecognised value is never rewritten", "certain", nil, "certain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := clean()
			s.Risks = []model.Risk{{Description: "double charge", Confidence: tc.in, Evidence: tc.ev}}
			out, _ := Enforce(s)
			if got := out.Risks[0].Confidence; got != tc.want {
				t.Errorf("Enforce lowered %q to %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// factualConfidences collects, in a fixed order, every confidence that asserts
// something about the world. Next-action confidence is not among them.
func factualConfidences(s *model.EngineeringState) []model.Confidence {
	var out []model.Confidence
	for _, r := range s.Requirements {
		out = append(out, r.Confidence)
	}
	for _, w := range s.CompletedWork {
		out = append(out, w.Confidence)
	}
	for _, w := range s.InProgress {
		out = append(out, w.Confidence)
	}
	for _, r := range s.Rejected {
		out = append(out, r.Confidence)
	}
	for _, a := range s.Assumptions {
		out = append(out, a.Confidence)
	}
	for _, r := range s.Risks {
		out = append(out, r.Confidence)
	}
	return out
}

// assertSameShape checks that Enforce added or dropped nothing. A downgrade pass
// that quietly deleted an inconvenient claim would be as dishonest as one that
// invented a claim.
func assertSameShape(t *testing.T, before, after *model.EngineeringState) {
	t.Helper()
	lengths := []struct {
		name          string
		before, after int
	}{
		{"requirements", len(before.Requirements), len(after.Requirements)},
		{"completed_work", len(before.CompletedWork), len(after.CompletedWork)},
		{"in_progress", len(before.InProgress), len(after.InProgress)},
		{"failed_attempts", len(before.FailedAttempts), len(after.FailedAttempts)},
		{"decisions", len(before.Decisions), len(after.Decisions)},
		{"rejected_approaches", len(before.Rejected), len(after.Rejected)},
		{"assumptions", len(before.Assumptions), len(after.Assumptions)},
		{"tests", len(before.Tests), len(after.Tests)},
		{"changed_files", len(before.ChangedFiles), len(after.ChangedFiles)},
		{"risks", len(before.Risks), len(after.Risks)},
		{"next_actions", len(before.NextActions), len(after.NextActions)},
		{"constraints", len(before.Constraints), len(after.Constraints)},
		{"sessions", len(before.Sessions), len(after.Sessions)},
		{"checkpoints", len(before.Checkpoints), len(after.Checkpoints)},
		{"evidence", len(before.Evidence), len(after.Evidence)},
	}
	for _, l := range lengths {
		if l.before != l.after {
			// Fatal, not Error: the caller indexes after[i] for every i in
			// before, so continuing past a length mismatch panics with an index
			// error that buries the real finding.
			t.Fatalf("%s: %d elements before, %d after", l.name, l.before, l.after)
		}
	}
	// Decisions, failed attempts, rejected approaches and tests are historical
	// record (plan §32); Enforce touches none of their content.
	if !reflect.DeepEqual(before.Decisions, after.Decisions) {
		t.Error("Enforce rewrote decisions, which are historical record")
	}
	if !reflect.DeepEqual(before.FailedAttempts, after.FailedAttempts) {
		t.Error("Enforce rewrote failed attempts, which are historical record")
	}
	if !reflect.DeepEqual(before.Tests, after.Tests) {
		t.Error("Enforce rewrote test results")
	}
	if !reflect.DeepEqual(before.Constraints, after.Constraints) {
		t.Error("Enforce rewrote constraints, which are additive by design (plan §41)")
	}
	if before.Status != after.Status {
		t.Errorf("Enforce rewrote the task status %q -> %q", before.Status, after.Status)
	}
}
