package validate

import (
	"reflect"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// clean returns a state that every rule in this package accepts. Each test case
// starts from a fresh copy and breaks exactly one thing, so a finding can only
// come from the mutation under test.
func clean() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{ID: "task_1", Title: "billing webhooks"})
	s.Status = model.StatusVerified
	s.Requirements = []model.Requirement{{
		ID:          "R1",
		Description: "webhook handling is idempotent",
		Status:      model.ReqComplete,
		Confidence:  model.Observed,
		Evidence:    []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}},
	}}
	s.Tests = []model.TestResult{{
		Name:     "TestDuplicateWebhook",
		Status:   model.TestPassed,
		Evidence: []model.Evidence{{Kind: model.EvidenceTest, Ref: "TestDuplicateWebhook", Result: "passed"}},
	}}
	s.Decisions = []model.Decision{{
		ID:           "D1",
		Decision:     "store webhook ids in postgres",
		Reason:       "the queue cannot guarantee exactly-once delivery",
		CheckpointID: "cp_1",
		Evidence:     []model.Evidence{{Kind: model.EvidenceCheckpoint, Ref: "cp_1"}},
	}}
	s.NextActions = []model.NextAction{{
		Description: "add an idempotency key to the webhook handler",
		Priority:    1,
		Confidence:  model.Recommended,
		Evidence:    []model.Evidence{{Kind: model.EvidenceFile, Path: "billing/webhooks.go", LineStart: 84, LineEnd: 110}},
	}}
	s.Sessions = []model.SessionNode{{SessionID: "s1", Agent: model.AgentOpenClaw, Role: model.RoleMain}}
	s.Checkpoints = []model.CheckpointNode{{CheckpointID: "cp_1", SessionID: "s1", Agent: model.AgentOpenClaw}}
	return s
}

// want is one finding a case requires (or, with absent set, forbids).
type want struct {
	code     string
	path     string
	severity Severity
	absent   bool
}

func TestStateAcceptsAFullyEvidencedState(t *testing.T) {
	if got := State(clean()); len(got) != 0 {
		t.Fatalf("clean state produced %d findings, want none:\n%s", len(got), format(got))
	}
}

func TestStateNilIsNotAnError(t *testing.T) {
	if got := State(nil); got != nil {
		t.Fatalf("State(nil) = %v, want nil", got)
	}
}

func TestStateRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.EngineeringState)
		wants  []want
	}{
		{
			name: "REQ_COMPLETE_NO_EVIDENCE: complete backed only by model inference",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Confidence = model.Inferred
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceInference, Ref: "extractor"}}
			},
			wants: []want{
				{code: codeReqCompleteNoEvidence, path: "requirements[R1]", severity: Error},
				// Inferred is exactly what an inference-level citation supports,
				// so the confidence rule must stay quiet here.
				{code: codeConfidenceExceedsEvidence, absent: true},
			},
		},
		{
			name: "REQ_COMPLETE_NO_EVIDENCE: an agent statement is not proof of completion",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Confidence = model.Inferred
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceEvent, Ref: "ev_9"}}
			},
			wants: []want{{code: codeReqCompleteNoEvidence, path: "requirements[R1]", severity: Error}},
		},
		{
			name: "REQ_COMPLETE_NO_EVIDENCE: a checkpoint is enough",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceCheckpoint, Ref: "cp_1"}}
			},
			wants: []want{{code: codeReqCompleteNoEvidence, absent: true}},
		},
		{
			name: "REQ_COMPLETE_NO_EVIDENCE does not fire on a partial requirement",
			mutate: func(s *model.EngineeringState) {
				s.Status = model.StatusPartial
				s.Requirements[0].Status = model.ReqPartial
				s.Requirements[0].Confidence = model.Inferred
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceInference, Ref: "extractor"}}
			},
			wants: []want{{code: codeReqCompleteNoEvidence, absent: true}},
		},
		{
			name: "TEST_PASS_NO_RESULT: a commit is not a test report",
			mutate: func(s *model.EngineeringState) {
				s.Tests[0].Evidence = []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}}
			},
			wants: []want{{code: codeTestPassNoResult, path: "tests[TestDuplicateWebhook]", severity: Error}},
		},
		{
			name: "TEST_PASS_NO_RESULT: test evidence with no recorded result",
			mutate: func(s *model.EngineeringState) {
				s.Tests[0].Evidence = []model.Evidence{{Kind: model.EvidenceTest, Ref: "TestDuplicateWebhook"}}
			},
			wants: []want{{code: codeTestPassNoResult, path: "tests[TestDuplicateWebhook]", severity: Error}},
		},
		{
			name: "TEST_PASS_NO_RESULT: a failure is not held to the same bar (plan §32)",
			mutate: func(s *model.EngineeringState) {
				s.Status = model.StatusPartial
				s.Tests[0].Status = model.TestFailed
				s.Tests[0].Evidence = []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}}
			},
			wants: []want{{code: codeTestPassNoResult, absent: true}},
		},
		{
			name: "DECISION_NO_SOURCE: no evidence, no session, no checkpoint",
			mutate: func(s *model.EngineeringState) {
				s.Decisions[0].Evidence = nil
				s.Decisions[0].CheckpointID = ""
			},
			wants: []want{
				{code: codeDecisionNoSource, path: "decisions[D1]", severity: Error},
				{code: codeClaimNoEvidence, path: "decisions[D1]", severity: Warn},
			},
		},
		{
			name: "DECISION_NO_SOURCE: a session id is traceable context",
			mutate: func(s *model.EngineeringState) {
				s.Decisions[0].Evidence = nil
				s.Decisions[0].CheckpointID = ""
				s.Decisions[0].SessionID = "s1"
			},
			wants: []want{
				{code: codeDecisionNoSource, absent: true},
				{code: codeClaimNoEvidence, path: "decisions[D1]", severity: Warn},
			},
		},
		{
			name: "CLAIM_NO_EVIDENCE: an unknown-graded risk is honest but uncheckable",
			mutate: func(s *model.EngineeringState) {
				s.Risks = []model.Risk{{Description: "retries may double-charge", Confidence: model.Unknown}}
			},
			wants: []want{
				{code: codeClaimNoEvidence, path: "risks[#0]", severity: Warn},
				// plan §32: unknown is always allowed, so nothing escalates.
				{code: codeConfidenceExceedsEvidence, absent: true},
			},
		},
		{
			name: "CLAIM_NO_EVIDENCE: work item with no citation",
			mutate: func(s *model.EngineeringState) {
				s.CompletedWork = []model.WorkItem{{Description: "wired the retry queue", Confidence: model.Unknown}}
			},
			wants: []want{{code: codeClaimNoEvidence, path: "completed_work[#0]", severity: Warn}},
		},
		{
			name: "CONFIDENCE_EXCEEDS_EVIDENCE: observed on statement-level evidence",
			mutate: func(s *model.EngineeringState) {
				s.Assumptions = []model.Assumption{{
					Statement:  "the staging database mirrors production",
					Confidence: model.Observed,
					Evidence:   []model.Evidence{{Kind: model.EvidencePrompt, Ref: "p_1"}},
				}}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, path: "assumptions[#0].confidence", severity: Error}},
		},
		{
			name: "CONFIDENCE_EXCEEDS_EVIDENCE: observed on nothing at all",
			mutate: func(s *model.EngineeringState) {
				s.InProgress = []model.WorkItem{{Description: "refactoring the dispatcher", Confidence: model.Observed}}
			},
			wants: []want{
				{code: codeConfidenceExceedsEvidence, path: "in_progress[#0].confidence", severity: Error},
				{code: codeClaimNoEvidence, path: "in_progress[#0]", severity: Warn},
			},
		},
		{
			name: "CONFIDENCE_EXCEEDS_EVIDENCE: inferred is fine on inference evidence",
			mutate: func(s *model.EngineeringState) {
				s.Rejected = []model.RejectedApproach{{
					ID:         "X1",
					Approach:   "dedupe in the load balancer",
					Confidence: model.Inferred,
					Evidence:   []model.Evidence{{Kind: model.EvidenceInference, Ref: "extractor"}},
				}}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, absent: true}},
		},
		{
			name: "NEXT_ACTION_NOT_RECOMMENDED: a proposal graded as fact",
			mutate: func(s *model.EngineeringState) {
				s.NextActions[0].Confidence = model.Observed
			},
			wants: []want{{code: codeNextActionNotRecommended, path: "next_actions[#0].confidence", severity: Error}},
		},
		{
			name: "NEXT_ACTION_NOT_RECOMMENDED: an unset confidence is not silently accepted",
			mutate: func(s *model.EngineeringState) {
				s.NextActions[0].Confidence = ""
			},
			wants: []want{
				{code: codeNextActionNotRecommended, path: "next_actions[#0].confidence", severity: Error},
				// The rule above subsumes the enum check for next actions, so
				// the reader is not shown the same defect twice.
				{code: codeInvalidEnum, path: "next_actions[#0].confidence", absent: true},
			},
		},
		{
			name: "INVALID_ENUM: requirement status outside the vocabulary",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Status = "done"
			},
			wants: []want{{code: codeInvalidEnum, path: "requirements[R1].status", severity: Error}},
		},
		{
			name: "INVALID_ENUM: task status outside the vocabulary suppresses the derived comparison",
			mutate: func(s *model.EngineeringState) {
				s.Status = "finished"
			},
			wants: []want{
				{code: codeInvalidEnum, path: "status", severity: Error},
				{code: codeStatusInconsistent, absent: true},
			},
		},
		{
			name: "INVALID_ENUM: evidence kind outside the vocabulary",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{
					{Kind: model.EvidenceCommit, Ref: "abc1234"},
					{Kind: "vibes", Ref: "trust me"},
				}
			},
			wants: []want{{code: codeInvalidEnum, path: "requirements[R1].evidence[1]", severity: Error}},
		},
		{
			name: "INVALID_ENUM: test status outside the vocabulary",
			mutate: func(s *model.EngineeringState) {
				s.Tests[0].Status = "green"
			},
			wants: []want{{code: codeInvalidEnum, path: "tests[TestDuplicateWebhook].status", severity: Error}},
		},
		{
			name: "INVALID_ENUM: an undeclared session agent",
			mutate: func(s *model.EngineeringState) {
				s.Sessions[0].Agent = ""
			},
			wants: []want{{code: codeInvalidEnum, path: "sessions[s1].agent", severity: Error}},
		},
		{
			name: "INVALID_ENUM: agent unknown is a legitimate declaration",
			mutate: func(s *model.EngineeringState) {
				s.Sessions[0].Agent = model.AgentUnknown
			},
			wants: []want{{code: codeInvalidEnum, absent: true}},
		},
		{
			name: "SCHEMA_VERSION: written by a format this build does not know",
			mutate: func(s *model.EngineeringState) {
				s.SchemaVersion = model.SchemaVersion + 7
			},
			wants: []want{{code: codeSchemaVersion, path: "schema_version", severity: Error}},
		},
		{
			name: "SCHEMA_VERSION: an unset version is not treated as current",
			mutate: func(s *model.EngineeringState) {
				s.SchemaVersion = 0
			},
			wants: []want{{code: codeSchemaVersion, path: "schema_version", severity: Error}},
		},
		{
			name: "DUPLICATE_ID: requirement ids collide after normalization",
			mutate: func(s *model.EngineeringState) {
				dup := s.Requirements[0]
				dup.ID = " r1 " // model.NormalizeID resolves this to R1
				s.Requirements = append(s.Requirements, dup)
			},
			wants: []want{{code: codeDuplicateID, path: "requirements[ r1 ]", severity: Error}},
		},
		{
			name: "DUPLICATE_ID: two elements with no id are not duplicates of each other",
			mutate: func(s *model.EngineeringState) {
				s.Risks = []model.Risk{
					{Description: "a", Confidence: model.Unknown},
					{Description: "b", Confidence: model.Unknown},
				}
			},
			wants: []want{{code: codeDuplicateID, absent: true}},
		},
		{
			name: "DUPLICATE_ID: repeated test names",
			mutate: func(s *model.EngineeringState) {
				s.Tests = append(s.Tests, s.Tests[0])
			},
			wants: []want{{code: codeDuplicateID, path: "tests[TestDuplicateWebhook]", severity: Error}},
		},
		{
			name: "STATUS_INCONSISTENT: stored status trails the evidence",
			mutate: func(s *model.EngineeringState) {
				s.Status = model.StatusNew
			},
			wants: []want{{code: codeStatusInconsistent, path: "status", severity: Warn}},
		},
		{
			name: "STATUS_INCONSISTENT: a lifecycle-only label is flagged as underived, not as false",
			mutate: func(s *model.EngineeringState) {
				// DeriveStatus never yields resumed, so this surfaces by design:
				// the label came from a workflow step, not from evidence.
				s.Status = model.StatusResumed
			},
			wants: []want{{code: codeStatusInconsistent, path: "status", severity: Warn}},
		},
		{
			name: "STATUS_INCONSISTENT: a failing test contradicts a verified status",
			mutate: func(s *model.EngineeringState) {
				s.Tests[0].Status = model.TestFailed
			},
			wants: []want{{code: codeStatusInconsistent, path: "status", severity: Warn}},
		},
		{
			name: "ORPHAN_EVIDENCE: a citation that points nowhere",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{
					{Kind: model.EvidenceCommit, Ref: "abc1234"},
					{Kind: model.EvidenceFile, Detail: "somewhere in the billing package"},
				}
			},
			wants: []want{{code: codeOrphanEvidence, path: "requirements[R1].evidence[1]", severity: Error}},
		},
		{
			name: "ORPHAN_EVIDENCE: a path alone is a real citation",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceFile, Path: "billing/webhooks.go", LineStart: 84}}
			},
			wants: []want{{code: codeOrphanEvidence, absent: true}},
		},
		{
			name: "ORPHAN_EVIDENCE: state-level evidence is checked too",
			mutate: func(s *model.EngineeringState) {
				s.Evidence = []model.Evidence{{Kind: model.EvidenceSession}}
			},
			wants: []want{{code: codeOrphanEvidence, path: "evidence[0]", severity: Error}},
		},
		{
			name: "ORPHAN_EVIDENCE: a whitespace-only ref cites nothing either",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceCommit, Ref: "   "}}
			},
			wants: []want{{code: codeOrphanEvidence, path: "requirements[R1].evidence[0]", severity: Error}},
		},
		{
			name: "ORPHAN_EVIDENCE: a constraint's citation is held to the same bar",
			mutate: func(s *model.EngineeringState) {
				s.Constraints = []model.Constraint{{
					ID:       "C1",
					Text:     "webhooks must be processed within 200ms",
					Source:   "noon curveball",
					Evidence: []model.Evidence{{Kind: model.EvidencePrompt, Detail: "somebody said so"}},
				}}
			},
			wants: []want{{code: codeOrphanEvidence, path: "constraints[C1].evidence[0]", severity: Error}},
		},
		{
			name: "INVALID_ENUM: a constraint's evidence kind is checked too",
			mutate: func(s *model.EngineeringState) {
				s.Constraints = []model.Constraint{{
					ID:       "C1",
					Text:     "webhooks must be processed within 200ms",
					Evidence: []model.Evidence{{Kind: "vibes", Ref: "trust me"}},
				}}
			},
			wants: []want{{code: codeInvalidEnum, path: "constraints[C1].evidence[0]", severity: Error}},
		},
		{
			// The laundering path this package exists to close: EvidenceKind.Level
			// looks only at the kind, so a commit citation with no sha would
			// otherwise certify completion at OBSERVED while pointing at nothing.
			name: "REQ_COMPLETE_NO_EVIDENCE: a citation that points nowhere cannot certify completion",
			mutate: func(s *model.EngineeringState) {
				s.Requirements[0].Evidence = []model.Evidence{{Kind: model.EvidenceCommit}}
			},
			wants: []want{
				{code: codeOrphanEvidence, path: "requirements[R1].evidence[0]", severity: Error},
				{code: codeReqCompleteNoEvidence, path: "requirements[R1]", severity: Error},
				{code: codeConfidenceExceedsEvidence, path: "requirements[R1].confidence", severity: Error},
			},
		},
		{
			name: "CONFIDENCE_EXCEEDS_EVIDENCE: an uninspectable checkpoint does not support observed",
			mutate: func(s *model.EngineeringState) {
				s.Risks = []model.Risk{{
					Description: "retries may double-charge",
					Confidence:  model.Observed,
					Evidence:    []model.Evidence{{Kind: model.EvidenceCheckpoint, Detail: "some checkpoint"}},
				}}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, path: "risks[#0].confidence", severity: Error}},
		},
		{
			// A citable record alongside an orphaned one still carries the claim:
			// the filter drops the empty citation, not the whole slice.
			name: "CONFIDENCE_EXCEEDS_EVIDENCE: one real citation is still enough",
			mutate: func(s *model.EngineeringState) {
				s.Risks = []model.Risk{{
					Description: "retries may double-charge",
					Confidence:  model.Observed,
					Evidence: []model.Evidence{
						{Kind: model.EvidenceCheckpoint},
						{Kind: model.EvidenceCommit, Ref: "abc1234"},
					},
				}}
			},
			wants: []want{{code: codeConfidenceExceedsEvidence, absent: true}},
		},
		{
			name: "TEST_PASS_NO_RESULT: a result that names no run is not a run",
			mutate: func(s *model.EngineeringState) {
				s.Tests[0].Evidence = []model.Evidence{{Kind: model.EvidenceTest, Result: "passed"}}
			},
			wants: []want{{code: codeTestPassNoResult, path: "tests[TestDuplicateWebhook]", severity: Error}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := clean()
			tc.mutate(s)
			got := State(s)
			for _, w := range tc.wants {
				if w.absent {
					if f, ok := findByCode(got, w.code); ok {
						t.Errorf("unexpected finding %s at %q: %s", f.Code, f.Path, f.Message)
					}
					continue
				}
				f, ok := find(got, w.code, w.path)
				if !ok {
					t.Errorf("missing finding %s at %q; got:\n%s", w.code, w.path, format(got))
					continue
				}
				if f.Severity != w.severity {
					t.Errorf("%s at %q: severity = %q, want %q", w.code, w.path, f.Severity, w.severity)
				}
				if f.Message == "" {
					t.Errorf("%s at %q: empty message; a finding a reader cannot act on is not a finding", w.code, w.path)
				}
			}
		})
	}
}

// messy is a state that trips many rules at once, used to check that repeated
// validation is stable and side-effect free.
func messy() *model.EngineeringState {
	s := clean()
	s.SchemaVersion = 99
	s.Status = model.StatusComplete
	s.Requirements = append(s.Requirements, model.Requirement{
		ID: "R2", Description: "emit a metric per webhook", Status: model.ReqComplete, Confidence: model.Observed,
	})
	s.Requirements = append(s.Requirements, model.Requirement{
		ID: "r2", Description: "emit a metric per webhook", Status: "done", Confidence: "certain",
	})
	s.Tests = append(s.Tests, model.TestResult{Name: "TestRetry", Status: model.TestPassed})
	s.Decisions = append(s.Decisions, model.Decision{ID: "D2", Decision: "drop the queue"})
	s.Assumptions = []model.Assumption{{Statement: "staging mirrors production", Confidence: model.Observed}}
	s.Risks = []model.Risk{{Description: "double charges", Confidence: model.Observed, Evidence: []model.Evidence{{Kind: "vibes"}}}}
	s.NextActions = append(s.NextActions, model.NextAction{Description: "rerun the suite", Priority: 2, Confidence: model.Observed})
	s.Constraints = []model.Constraint{
		{ID: "C1", Text: "process webhooks within 200ms", Source: "noon curveball",
			Evidence: []model.Evidence{{Kind: model.EvidencePrompt, Detail: "no ref, no path"}}},
		{ID: "c1", Text: "process webhooks within 200ms", Source: "noon curveball"},
	}
	return s
}

func TestStateIsDeterministicAndDoesNotMutate(t *testing.T) {
	s := messy()
	before := s.Hash()

	first := State(s)
	second := State(s)

	if len(first) == 0 {
		t.Fatal("messy state produced no findings; the fixture is not exercising anything")
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("State is not deterministic:\nfirst:\n%s\nsecond:\n%s", format(first), format(second))
	}
	if after := s.Hash(); after != before {
		t.Errorf("State mutated its input: hash %s -> %s", before, after)
	}
}

func TestStateFindingsAlwaysCarryACodeAndPath(t *testing.T) {
	// Every finding has to be traceable to an element, or it repeats the very
	// failure this package exists to prevent (plan §11).
	for _, f := range State(messy()) {
		if f.Code == "" || f.Path == "" {
			t.Errorf("finding without code or path: %+v", f)
		}
		if f.Severity != Error && f.Severity != Warn {
			t.Errorf("finding %s at %q has severity %q", f.Code, f.Path, f.Severity)
		}
	}
}

func TestBriefCollapsesAndTruncates(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"short", "short"},
		{"multi\nline   text", "multi line text"},
		{"", ""},
		{
			// 70 runes, all multi-byte, so a byte-wise cut would corrupt them.
			"ααααααααααααααααααααααααααααααααααααααααααααααααααααααααααααααααααααα",
			"ααααααααααααααααααααααααααααααααααααααααααααααααααααααααααα…",
		},
	}
	for _, tc := range cases {
		if got := brief(tc.in); got != tc.want {
			t.Errorf("brief(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestConfidenceRankIsOrderedStrongestFirst pins the ordering itself.
//
// Every "Enforce never strengthens a claim" guarantee is expressed in terms of
// confidenceRank, so a test that merely compares confidenceRank against itself
// proves nothing: inverting the function inverts the test with it. These are
// absolute assertions about the direction of the scale.
func TestConfidenceRankIsOrderedStrongestFirst(t *testing.T) {
	// OBSERVED asserts most about reality, UNKNOWN asserts nothing, and
	// RECOMMENDED sits between them: it proposes rather than states (plan §12).
	descending := []model.Confidence{model.Observed, model.Inferred, model.Recommended, model.Unknown}
	for i := 1; i < len(descending); i++ {
		strong, weak := descending[i-1], descending[i]
		if confidenceRank(strong) <= confidenceRank(weak) {
			t.Errorf("confidenceRank(%q)=%d must be greater than confidenceRank(%q)=%d",
				strong, confidenceRank(strong), weak, confidenceRank(weak))
		}
	}
	if confidenceRank(model.Unknown) != 0 {
		t.Errorf("confidenceRank(unknown) = %d, want 0: unknown must be the floor so it is never raised (plan §32)",
			confidenceRank(model.Unknown))
	}
	// An unrecognised value ranks at the floor so Enforce leaves it alone
	// instead of substituting a value it invented.
	if got := confidenceRank("certain"); got != 0 {
		t.Errorf("confidenceRank(%q) = %d, want 0", "certain", got)
	}
}

// TestSupportingDropsOnlyUncitableRecords guards the filter that decides which
// evidence is allowed to carry a claim.
func TestSupportingDropsOnlyUncitableRecords(t *testing.T) {
	in := []model.Evidence{
		{Kind: model.EvidenceCommit, Ref: "abc1234"},
		{Kind: model.EvidenceCommit},                       // cites nothing
		{Kind: model.EvidenceFile, Path: "billing/x.go"},   // path alone is a citation
		{Kind: model.EvidenceCheckpoint, Ref: " \t "},      // whitespace is not a citation
		{Kind: model.EvidenceSession, Detail: "a session"}, // detail is not a citation
	}
	got := supporting(in)
	want := []model.Evidence{in[0], in[2]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("supporting() = %v, want %v", got, want)
	}
	// The strongest level of an all-orphan slice must be "none", not Level 1.
	if _, ok := model.StrongestLevel(supporting([]model.Evidence{{Kind: model.EvidenceCommit}})); ok {
		t.Error("an uncitable commit still reports a strongest evidence level")
	}
	if got := derivedConfidence([]model.Evidence{{Kind: model.EvidenceCommit}}); got != model.Unknown {
		t.Errorf("derivedConfidence(orphan commit) = %q, want %q", got, model.Unknown)
	}
}

func find(fs []Finding, code, path string) (Finding, bool) {
	for _, f := range fs {
		if f.Code == code && f.Path == path {
			return f, true
		}
	}
	return Finding{}, false
}

func findByCode(fs []Finding, code string) (Finding, bool) {
	for _, f := range fs {
		if f.Code == code {
			return f, true
		}
	}
	return Finding{}, false
}

func format(fs []Finding) string {
	out := ""
	for _, f := range fs {
		out += "  " + string(f.Severity) + " " + f.Code + " " + f.Path + ": " + f.Message + "\n"
	}
	return out
}
