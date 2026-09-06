package merge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func at(minute int) time.Time {
	return time.Date(2026, 1, 1, 12, minute, 0, 0, time.UTC)
}

// testOpts returns options backed by a deterministic clock. Nothing in this
// package may reach for the wall clock, so every test pins time explicitly.
func testOpts() Options {
	return Options{Clock: &model.FixedClock{Current: at(0), Step: time.Second}}
}

func commitEv(sha string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceCommit, Ref: sha}
}

func fileEv(path string, line int) model.Evidence {
	return model.Evidence{Kind: model.EvidenceFile, Ref: path, Path: path, LineStart: line}
}

func testEv(name, result string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceTest, Ref: name, Result: result}
}

func ckptEv(id string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceCheckpoint, Ref: id}
}

func inferEv(detail string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceInference, Ref: detail, Detail: detail}
}

func newState(mutate func(s *model.EngineeringState)) *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:             "task_abc",
		Title:          "Add retry to the payment client",
		OriginalIntent: "add retry with backoff to the payment client",
	})
	if mutate != nil {
		mutate(s)
	}
	return s
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

func requireRequirement(t *testing.T, s *model.EngineeringState, id string) model.Requirement {
	t.Helper()
	for _, r := range s.Requirements {
		if model.NormalizeID(r.ID) == model.NormalizeID(id) {
			return r
		}
	}
	t.Fatalf("requirement %q missing from merged state (have %d)", id, len(s.Requirements))
	return model.Requirement{}
}

func requireTest(t *testing.T, s *model.EngineeringState, name string) model.TestResult {
	t.Helper()
	got, ok := s.Test(name)
	if !ok {
		t.Fatalf("test %q missing from merged state", name)
	}
	return got
}

func hasEvidence(ev []model.Evidence, want model.Evidence) bool {
	for _, e := range ev {
		if e.Key() == want.Key() {
			return true
		}
	}
	return false
}

func noteContaining(s *model.EngineeringState, substr string) bool {
	for _, n := range s.Capture.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// States
// ---------------------------------------------------------------------------

func TestStates(t *testing.T) {
	tests := []struct {
		name  string
		prev  *model.EngineeringState
		next  *model.EngineeringState
		check func(t *testing.T, got *model.EngineeringState)
	}{
		{
			name: "repository evidence promotes a requirement to complete",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Description: "retry on 5xx", Status: model.ReqPartial,
					Evidence: []model.Evidence{inferEv("looks half done")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqComplete,
					Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				r := requireRequirement(t, got, "R1")
				if r.Status != model.ReqComplete {
					t.Fatalf("status = %q, want complete", r.Status)
				}
				if r.Confidence != model.Observed {
					t.Fatalf("confidence = %q, want observed", r.Confidence)
				}
			},
		},
		{
			name: "checkpoint evidence also promotes a requirement to complete",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqUnresolved,
					Evidence: []model.Evidence{inferEv("not started")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqComplete,
					Evidence: []model.Evidence{ckptEv("ckpt_9")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R1"); r.Status != model.ReqComplete {
					t.Fatalf("status = %q, want complete", r.Status)
				}
			},
		},
		{
			name: "a completion claimed with only an inference is not promoted",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqUnresolved,
					Evidence: []model.Evidence{inferEv("nothing yet")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqComplete,
					Evidence: []model.Evidence{inferEv("the agent said it finished")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				r := requireRequirement(t, got, "R1")
				if r.Status == model.ReqComplete {
					t.Fatal("requirement was promoted to complete without repository or checkpoint evidence")
				}
				if r.Status != model.ReqPartial {
					t.Fatalf("status = %q, want partial (claimed but unproven)", r.Status)
				}
				if r.Confidence != model.Inferred {
					t.Fatalf("confidence = %q, want inferred", r.Confidence)
				}
			},
		},
		{
			name: "Level 1 evidence downgrades an inferred completion",
			prev: newState(func(s *model.EngineeringState) {
				s.Status = model.StatusVerified
				s.Requirements = []model.Requirement{{
					ID: "R1", Description: "retry on 5xx", Status: model.ReqComplete,
					Confidence: model.Inferred,
					Evidence:   []model.Evidence{inferEv("the agent claimed retry is done")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqUnresolved,
					Evidence: []model.Evidence{fileEv("client/retry.go", 12)},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				r := requireRequirement(t, got, "R1")
				// plan §31: Level 1 wins, even when it makes the state worse.
				if r.Status != model.ReqUnresolved {
					t.Fatalf("status = %q, want unresolved: a repository fact must outrank an inference", r.Status)
				}
				if !hasEvidence(r.Evidence, inferEv("the agent claimed retry is done")) {
					t.Fatal("the superseded inference was dropped; evidence is unioned, not replaced")
				}
				if !hasEvidence(r.Evidence, fileEv("client/retry.go", 12)) {
					t.Fatal("incoming repository evidence missing from merged requirement")
				}
			},
		},
		{
			name: "a weaker later observation cannot downgrade a proven completion",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqComplete,
					Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqUnresolved,
					Evidence: []model.Evidence{inferEv("the model no longer sees it")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R1"); r.Status != model.ReqComplete {
					t.Fatalf("status = %q, want complete", r.Status)
				}
			},
		},
		{
			name: "an incoming record with no status moves nothing",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqPartial,
					Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Evidence: []model.Evidence{fileEv("client/retry.go", 4)},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R1"); r.Status != model.ReqPartial {
					t.Fatalf("status = %q, want partial", r.Status)
				}
			},
		},
		{
			// A status outside the §10 lifecycle carries no meaning, and plan §32
			// says to use unknown when the evidence is insufficient. Merge only
			// rewrites records it actually merged, so this needs both sides.
			name: "an unrecognised status merges to unknown",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{ID: "R1", Status: model.ReqStatus("done-ish")}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{ID: "R1", Evidence: []model.Evidence{inferEv("saw it mentioned")}}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R1"); r.Status != model.ReqUnknown {
					t.Fatalf("status = %q, want unknown", r.Status)
				}
			},
		},
		{
			name: "requirement ids match after normalization",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{ID: "r1", Status: model.ReqPartial}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{ID: "R1 ", Status: model.ReqPartial}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Requirements) != 1 {
					t.Fatalf("requirements = %d, want 1: case drift must not fork a requirement", len(got.Requirements))
				}
			},
		},
		{
			name: "a requirement only the incoming state knows is carried through",
			prev: newState(nil),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R2", Description: "surface retry budget", Status: model.ReqUnresolved,
					Evidence: []model.Evidence{inferEv("from the prompt")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R2"); r.Status != model.ReqUnresolved {
					t.Fatalf("status = %q, want unresolved", r.Status)
				}
			},
		},
		{
			// The §32 completion guard cannot depend on whether an earlier capture
			// happened to mention the requirement: a first sighting is exactly when
			// an unproven claim is easiest to smuggle in, because there is nothing
			// on the other side to contradict it.
			name: "a first sighting is not believed complete without proof",
			prev: newState(nil),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R9", Description: "retry budget is enforced", Status: model.ReqComplete,
					Evidence: []model.Evidence{inferEv("the agent said it finished")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				r := requireRequirement(t, got, "R9")
				if r.Status == model.ReqComplete {
					t.Fatal("a requirement merge had never seen before was accepted as complete on a claim alone")
				}
				if r.Status != model.ReqPartial {
					t.Fatalf("status = %q, want partial (claimed but unproven)", r.Status)
				}
				if got.Status == model.StatusVerified {
					t.Fatal("the task was reported verified on the strength of an unproven completion")
				}
			},
		},
		{
			name: "a first sighting with no evidence at all is not believed complete",
			prev: nil,
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{ID: "R9", Status: model.ReqComplete}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R9"); r.Status != model.ReqPartial {
					t.Fatalf("status = %q, want partial: nothing backs this claim", r.Status)
				}
			},
		},
		{
			name: "a first sighting proven by the repository stays complete",
			prev: nil,
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R9", Status: model.ReqComplete,
					Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if r := requireRequirement(t, got, "R9"); r.Status != model.ReqComplete {
					t.Fatalf("status = %q, want complete: the guard downgrades unproven claims, not proven ones", r.Status)
				}
			},
		},
		{
			// finalize normalises the enum on every requirement, not only on the
			// ones this merge combined, so the serialized document never carries a
			// status outside the §10 lifecycle.
			name: "an unrecognised status on a one-sided requirement still merges to unknown",
			prev: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{ID: "R1", Status: model.ReqStatus("done-ish")}}
			}),
			next: nil,
			check: func(t *testing.T, got *model.EngineeringState) {
				r := requireRequirement(t, got, "R1")
				if r.Status != model.ReqUnknown {
					t.Fatalf("status = %q, want unknown", r.Status)
				}
				if !r.Status.Valid() {
					t.Fatalf("status %q is outside the model enum", r.Status)
				}
			},
		},
		{
			name: "a failing test survives a merge",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{
					Name: "TestRetry", Status: model.TestFailed, RanAt: at(10),
					Evidence: []model.Evidence{testEv("TestRetry", "fail")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				// An older run claiming a pass is not proof of resolution.
				s.Tests = []model.TestResult{{
					Name: "TestRetry", Status: model.TestPassed, RanAt: at(5),
					Evidence: []model.Evidence{testEv("TestRetry", "pass")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				tr := requireTest(t, got, "TestRetry")
				if tr.Status != model.TestFailed {
					t.Fatalf("status = %q, want failed: only a strictly newer run resolves a failure", tr.Status)
				}
				if len(got.FailingTests()) != 1 {
					t.Fatalf("failing tests = %d, want 1", len(got.FailingTests()))
				}
				if !hasEvidence(tr.Evidence, testEv("TestRetry", "pass")) {
					t.Fatal("evidence from the losing run was dropped; both runs happened")
				}
			},
		},
		{
			name: "an undated passing run cannot clear a failure",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestPassed}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if tr := requireTest(t, got, "TestRetry"); tr.Status != model.TestFailed {
					t.Fatalf("status = %q, want failed: an undatable pass is not proof", tr.Status)
				}
			},
		},
		{
			name: "a strictly newer passing run resolves a failure",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed, RanAt: at(5)}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestPassed, RanAt: at(10)}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if tr := requireTest(t, got, "TestRetry"); tr.Status != model.TestPassed {
					t.Fatalf("status = %q, want passed", tr.Status)
				}
			},
		},
		{
			name: "a newer failing run replaces an older passing one",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestPassed, RanAt: at(5)}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed, RanAt: at(10)}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if tr := requireTest(t, got, "TestRetry"); tr.Status != model.TestFailed {
					t.Fatalf("status = %q, want failed", tr.Status)
				}
			},
		},
		{
			// A skipped run reports that the test produced no verdict. Letting it
			// govern would drop a known failure off the handoff entirely, which is
			// the §16 rule read backwards.
			name: "a newer skipped run does not clear a failure",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{
					Name: "TestRetry", Status: model.TestFailed, RanAt: at(5),
					Evidence: []model.Evidence{testEv("TestRetry", "fail")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestSkipped, RanAt: at(10)}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				tr := requireTest(t, got, "TestRetry")
				if tr.Status != model.TestFailed {
					t.Fatalf("status = %q, want failed: a skipped run is not proof of resolution", tr.Status)
				}
				if len(got.FailingTests()) != 1 {
					t.Fatalf("failing tests = %d, want 1: the failure was dropped off the handoff", len(got.FailingTests()))
				}
			},
		},
		{
			name: "a newer unknown run does not clear a failure",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed, RanAt: at(5)}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestUnknown, RanAt: at(10)}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if tr := requireTest(t, got, "TestRetry"); tr.Status != model.TestFailed {
					t.Fatalf("status = %q, want failed: an unknown outcome resolves nothing", tr.Status)
				}
			},
		},
		{
			// The guard is about failures only: it must not pin a passing test in
			// place when a later run genuinely reported no verdict.
			name: "a newer skipped run does replace an older pass",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestPassed, RanAt: at(5)}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestSkipped, RanAt: at(10)}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if tr := requireTest(t, got, "TestRetry"); tr.Status != model.TestSkipped {
					t.Fatalf("status = %q, want skipped: the latest verdict on a non-failing test stands", tr.Status)
				}
			},
		},
		{
			name: "an undated failure surfaces over an undated pass",
			prev: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestPassed}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if tr := requireTest(t, got, "TestRetry"); tr.Status != model.TestFailed {
					t.Fatalf("status = %q, want failed", tr.Status)
				}
			},
		},
		{
			name: "a decision absent from the incoming state is kept",
			prev: newState(func(s *model.EngineeringState) {
				s.Decisions = []model.Decision{{
					ID: "D1", Decision: "use exponential backoff", Reason: "the API rate-limits bursts",
					Evidence: []model.Evidence{commitEv("abc123")}, MadeAt: at(3),
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Decisions = []model.Decision{{ID: "D2", Decision: "cap retries at five"}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Decisions) != 2 {
					t.Fatalf("decisions = %d, want 2: history is retained, not replaced", len(got.Decisions))
				}
				var d1 model.Decision
				for _, d := range got.Decisions {
					if d.ID == "D1" {
						d1 = d
					}
				}
				if d1.Reason != "the API rate-limits bursts" {
					t.Fatalf("D1 reason = %q, want the original reason preserved", d1.Reason)
				}
			},
		},
		{
			name: "a superseded decision is not revived by a later capture",
			prev: newState(func(s *model.EngineeringState) {
				s.Decisions = []model.Decision{{ID: "D1", Decision: "retry forever", SupersededBy: "D2"}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Decisions = []model.Decision{{ID: "D1", Decision: "retry forever"}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.ActiveDecisions()) != 0 {
					t.Fatal("a superseded decision came back to life")
				}
			},
		},
		{
			name: "rejected approaches are retained forever",
			prev: newState(func(s *model.EngineeringState) {
				s.Rejected = []model.RejectedApproach{{
					ID: "X1", Approach: "retry inside the HTTP transport",
					Reason: "hid the failure from the caller", Confidence: model.Observed,
					Evidence: []model.Evidence{commitEv("dead01")},
				}}
			}),
			next: newState(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Rejected) != 1 {
					t.Fatalf("rejected = %d, want 1: plan §15 keeps dead ends so they are not retried", len(got.Rejected))
				}
				if got.Rejected[0].Reason == "" {
					t.Fatal("the reason a dead end was abandoned was dropped")
				}
			},
		},
		{
			name: "assumption invalidation is sticky",
			prev: newState(func(s *model.EngineeringState) {
				s.Assumptions = []model.Assumption{{
					Statement: "the payment API is idempotent", Invalidated: true,
					Evidence: []model.Evidence{fileEv("docs/api.md", 40)},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Assumptions = []model.Assumption{{Statement: "the payment API is idempotent"}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Assumptions) != 1 {
					t.Fatalf("assumptions = %d, want 1", len(got.Assumptions))
				}
				if !got.Assumptions[0].Invalidated {
					t.Fatal("an invalidated assumption was silently revalidated")
				}
			},
		},
		{
			name: "changed files reflect the incoming tree",
			prev: newState(func(s *model.EngineeringState) {
				s.Capture.GitAvailable = true
				s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}, {Path: "b.go", Status: "M"}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Capture.GitAvailable = true
				s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.ChangedFiles) != 1 || got.ChangedFiles[0].Path != "a.go" {
					t.Fatalf("changed files = %+v, want only a.go: the list is a snapshot, not a log", got.ChangedFiles)
				}
			},
		},
		{
			name: "an empty file list from an available git means a clean tree",
			prev: newState(func(s *model.EngineeringState) {
				s.Capture.GitAvailable = true
				s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}}
			}),
			next: newState(func(s *model.EngineeringState) { s.Capture.GitAvailable = true }),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.ChangedFiles) != 0 {
					t.Fatalf("changed files = %+v, want none", got.ChangedFiles)
				}
			},
		},
		{
			name: "changed files are retained and flagged when git was unavailable",
			prev: newState(func(s *model.EngineeringState) {
				s.Capture.GitAvailable = true
				s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Capture.GitAvailable = false
				s.Capture.Missing = []string{"git"}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.ChangedFiles) != 1 {
					t.Fatalf("changed files = %d, want the previous list retained", len(got.ChangedFiles))
				}
				if !noteContaining(got, "may be stale") {
					t.Fatalf("retained files were not flagged as possibly stale: %v", got.Capture.Notes)
				}
				if got.Capture.GitAvailable {
					t.Fatal("capture claims git was available when the incoming capture said otherwise")
				}
				if len(got.Capture.Missing) != 1 || got.Capture.Missing[0] != "git" {
					t.Fatalf("missing = %v, want the recorded gap preserved", got.Capture.Missing)
				}
			},
		},
		{
			name: "next actions are replaced by a fresh set",
			prev: newState(func(s *model.EngineeringState) {
				s.NextActions = []model.NextAction{{Description: "old plan", Priority: 1}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.NextActions = []model.NextAction{{Description: "new plan", Priority: 1}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.NextActions) != 1 || got.NextActions[0].Description != "new plan" {
					t.Fatalf("next actions = %+v, want only the new plan", got.NextActions)
				}
			},
		},
		{
			name: "an empty incoming next action set keeps the previous one",
			prev: newState(func(s *model.EngineeringState) {
				s.NextActions = []model.NextAction{{Description: "old plan", Priority: 1}}
			}),
			next: newState(nil),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.NextActions) != 1 || got.NextActions[0].Description != "old plan" {
					t.Fatalf("next actions = %+v, want the previous plan kept", got.NextActions)
				}
			},
		},
		{
			name: "lineage, constraints and evidence are unioned and deduped",
			prev: newState(func(s *model.EngineeringState) {
				s.Sessions = []model.SessionNode{{SessionID: "s1", Agent: model.AgentOpenClaw, StartedAt: at(1)}}
				s.Checkpoints = []model.CheckpointNode{{CheckpointID: "c1", SessionID: "s1", CreatedAt: at(2)}}
				s.Constraints = []model.Constraint{{ID: "C1", Text: "must run offline", AddedAt: at(2)}}
				s.Evidence = []model.Evidence{commitEv("abc123")}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Sessions = []model.SessionNode{
					{SessionID: "s1", Agent: model.AgentOpenClaw, StartedAt: at(1), EndedAt: at(6)},
					{SessionID: "s2", Agent: model.AgentHermes, StartedAt: at(7)},
				}
				s.Checkpoints = []model.CheckpointNode{{CheckpointID: "c1", SessionID: "s1", CreatedAt: at(2), StateHash: "sha256:deadbeef"}}
				s.Constraints = []model.Constraint{{ID: "c1", Text: "must run offline", AddedAt: at(3)}}
				s.Evidence = []model.Evidence{commitEv("abc123"), ckptEv("c1")}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Sessions) != 2 {
					t.Fatalf("sessions = %d, want 2", len(got.Sessions))
				}
				if got.Sessions[0].EndedAt.IsZero() {
					t.Fatal("the later capture's session end time was dropped")
				}
				if len(got.Checkpoints) != 1 || got.Checkpoints[0].StateHash == "" {
					t.Fatalf("checkpoints = %+v, want one enriched checkpoint", got.Checkpoints)
				}
				if len(got.Constraints) != 1 {
					t.Fatalf("constraints = %d, want 1 after id normalization", len(got.Constraints))
				}
				if !got.Constraints[0].AddedAt.Equal(at(2)) {
					t.Fatalf("constraint AddedAt = %v, want the first arrival time", got.Constraints[0].AddedAt)
				}
				if len(got.Evidence) != 2 {
					t.Fatalf("evidence = %d, want 2 after dedupe", len(got.Evidence))
				}
			},
		},
		{
			name: "work items union by description with evidence merged",
			prev: newState(func(s *model.EngineeringState) {
				s.CompletedWork = []model.WorkItem{{
					Description: "added backoff helper", Evidence: []model.Evidence{commitEv("abc123")},
				}}
				s.FailedAttempts = []model.FailedAttempt{{
					Description: "retry loop deadlocked", Evidence: []model.Evidence{testEv("TestRetry", "fail")},
					OccurredAt: at(4),
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.CompletedWork = []model.WorkItem{{
					Description: "added backoff helper", Evidence: []model.Evidence{fileEv("client/backoff.go", 1)},
				}}
				s.FailedAttempts = []model.FailedAttempt{{Description: "retry loop deadlocked", SessionID: "s2"}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.CompletedWork) != 1 {
					t.Fatalf("completed work = %d, want 1", len(got.CompletedWork))
				}
				if len(got.CompletedWork[0].Evidence) != 2 {
					t.Fatalf("evidence = %+v, want both citations", got.CompletedWork[0].Evidence)
				}
				if got.CompletedWork[0].Confidence != model.Observed {
					t.Fatalf("confidence = %q, want observed", got.CompletedWork[0].Confidence)
				}
				if len(got.FailedAttempts) != 1 || got.FailedAttempts[0].SessionID != "s2" {
					t.Fatalf("failed attempts = %+v, want one merged record", got.FailedAttempts)
				}
				if !got.FailedAttempts[0].OccurredAt.Equal(at(4)) {
					t.Fatal("the recorded failure time was lost")
				}
			},
		},
		{
			name: "status is derived from evidence and an illegal transition is noted",
			prev: newState(func(s *model.EngineeringState) {
				s.Status = model.StatusNew
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqPartial, Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if got.Status != model.StatusPartial {
					t.Fatalf("status = %q, want partial: the evidence outranks the stored enum", got.Status)
				}
				if !noteContaining(got, "not a declared transition") {
					t.Fatalf("the illegal transition was not reported: %v", got.Capture.Notes)
				}
			},
		},
		{
			// Capture.Notes is a shared channel. Merge re-derives its own two
			// annotations by deleting them and writing them again, and that deletion
			// must not be able to reach a note some other stage recorded.
			name: "capture notes written elsewhere survive a merge",
			prev: newState(func(s *model.EngineeringState) {
				s.Status = model.StatusNew
				s.Capture.Notes = []string{
					"status of the transcript reader is unknown; no events were exposed",
					"changed files were retained by the adapter from its own cache",
				}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqPartial, Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if !noteContaining(got, "transcript reader is unknown") {
					t.Fatalf("a note from another stage was deleted: %v", got.Capture.Notes)
				}
				if !noteContaining(got, "retained by the adapter") {
					t.Fatalf("a note from another stage was deleted: %v", got.Capture.Notes)
				}
				if !noteContaining(got, "not a declared transition") {
					t.Fatalf("merge's own annotation is missing: %v", got.Capture.Notes)
				}
			},
		},
		{
			name: "a failing test keeps the task off complete",
			prev: newState(func(s *model.EngineeringState) {
				s.Status = model.StatusVerified
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqComplete, Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			next: newState(func(s *model.EngineeringState) {
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed, RanAt: at(9)}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if got.Status != model.StatusPartial {
					t.Fatalf("status = %q, want partial", got.Status)
				}
				if noteContaining(got, "not a declared transition") {
					t.Fatalf("verified -> partial is legal and should not be flagged: %v", got.Capture.Notes)
				}
			},
		},
		{
			name: "a nil previous state leaves the incoming state intact",
			prev: nil,
			next: newState(func(s *model.EngineeringState) {
				s.Requirements = []model.Requirement{{
					ID: "R1", Status: model.ReqPartial, Evidence: []model.Evidence{commitEv("abc123")},
				}}
			}),
			check: func(t *testing.T, got *model.EngineeringState) {
				if got.Task.OriginalIntent == "" {
					t.Fatal("the incoming task identity was lost")
				}
				if r := requireRequirement(t, got, "R1"); r.Status != model.ReqPartial {
					t.Fatalf("status = %q, want partial", r.Status)
				}
				if noteContaining(got, "not a declared transition") {
					t.Fatalf("a state with no predecessor cannot have made an illegal transition: %v", got.Capture.Notes)
				}
			},
		},
		{
			name: "a nil incoming state preserves everything previously known",
			prev: newState(func(s *model.EngineeringState) {
				s.Capture.GitAvailable = true
				s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}}
				s.Decisions = []model.Decision{{ID: "D1", Decision: "use exponential backoff"}}
				s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed, RanAt: at(9)}}
			}),
			next: nil,
			check: func(t *testing.T, got *model.EngineeringState) {
				if len(got.Decisions) != 1 || len(got.ChangedFiles) != 1 {
					t.Fatalf("a nil incoming state discarded known facts: %+v", got)
				}
				if !got.Capture.GitAvailable {
					t.Fatal("capture flags were wiped by an absent incoming state")
				}
				if len(got.FailingTests()) != 1 {
					t.Fatal("the failing test did not survive")
				}
				if noteContaining(got, "may be stale") {
					t.Fatalf("nothing arrived to make the file list stale: %v", got.Capture.Notes)
				}
			},
		},
		{
			name: "two nil states merge into an empty, honest state",
			prev: nil,
			next: nil,
			check: func(t *testing.T, got *model.EngineeringState) {
				if got == nil {
					t.Fatal("States returned nil; callers must always get a usable state")
				}
				if got.Status != model.StatusNew {
					t.Fatalf("status = %q, want new", got.Status)
				}
				if len(got.Capture.Notes) != 0 {
					t.Fatalf("notes = %v, want none", got.Capture.Notes)
				}
				if got.SchemaVersion != model.SchemaVersion {
					t.Fatalf("schema version = %d, want %d", got.SchemaVersion, model.SchemaVersion)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := States(tc.prev, tc.next, testOpts())
			if got == nil {
				t.Fatal("States returned nil")
			}
			tc.check(t, got)
		})
	}
}

// TestStatesPreservesOriginalIntent guards the §41 rule at the merge boundary:
// a later capture that never learned the original prompt must not erase it.
func TestStatesPreservesOriginalIntent(t *testing.T) {
	prev := newState(nil)
	next := model.NewEngineeringState(model.TaskRef{ID: "task_abc"})

	got := States(prev, next, testOpts())
	if got.Task.OriginalIntent != prev.Task.OriginalIntent {
		t.Fatalf("original intent = %q, want %q", got.Task.OriginalIntent, prev.Task.OriginalIntent)
	}
	if got.Task.Title != prev.Task.Title {
		t.Fatalf("title = %q, want %q", got.Task.Title, prev.Task.Title)
	}
}

// TestStatesDoesNotMutateInputs is the guarantee that lets a caller keep holding
// the previous state after a merge.
func TestStatesDoesNotMutateInputs(t *testing.T) {
	prev := newState(func(s *model.EngineeringState) {
		s.Status = model.StatusVerified
		s.Requirements = []model.Requirement{{
			ID: "R1", Status: model.ReqComplete, Evidence: []model.Evidence{inferEv("claimed")},
		}}
		s.Decisions = []model.Decision{{ID: "D1", Decision: "use exponential backoff"}}
		s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}}
	})
	next := newState(func(s *model.EngineeringState) {
		s.Requirements = []model.Requirement{{
			ID: "R1", Status: model.ReqUnresolved, Evidence: []model.Evidence{fileEv("client/retry.go", 12)},
		}}
		s.Tests = []model.TestResult{{Name: "TestRetry", Status: model.TestFailed, RanAt: at(9)}}
	})

	beforePrev := mustJSON(t, prev)
	beforeNext := mustJSON(t, next)

	States(prev, next, testOpts())

	if got := mustJSON(t, prev); got != beforePrev {
		t.Fatalf("prev was mutated:\n%s\n%s", beforePrev, got)
	}
	if got := mustJSON(t, next); got != beforeNext {
		t.Fatalf("next was mutated:\n%s\n%s", beforeNext, got)
	}
}

// TestStatesIsDeterministic is a product requirement, not a nicety: the state
// hash printed on a handoff receipt is only meaningful if the same inputs always
// produce the same bytes.
func TestStatesIsDeterministic(t *testing.T) {
	build := func() (*model.EngineeringState, *model.EngineeringState) {
		prev := newState(func(s *model.EngineeringState) {
			s.Requirements = []model.Requirement{
				{ID: "R2", Status: model.ReqPartial, Evidence: []model.Evidence{inferEv("b")}},
				{ID: "R1", Status: model.ReqComplete, Evidence: []model.Evidence{commitEv("abc123")}},
			}
			s.Decisions = []model.Decision{{ID: "D2"}, {ID: "D1"}}
			s.Evidence = []model.Evidence{ckptEv("c2"), commitEv("abc123"), inferEv("z")}
			s.Capture.Notes = []string{"beta", "alpha"}
		})
		next := newState(func(s *model.EngineeringState) {
			s.Requirements = []model.Requirement{
				{ID: "R3", Status: model.ReqUnresolved, Evidence: []model.Evidence{fileEv("c.go", 2)}},
				{ID: "R1", Status: model.ReqComplete, Evidence: []model.Evidence{fileEv("a.go", 1)}},
			}
			s.Tests = []model.TestResult{
				{Name: "TestB", Status: model.TestPassed, RanAt: at(8)},
				{Name: "TestA", Status: model.TestFailed, RanAt: at(9)},
			}
			s.Evidence = []model.Evidence{inferEv("z"), ckptEv("c1")}
		})
		return prev, next
	}

	p1, n1 := build()
	p2, n2 := build()
	first := mustJSON(t, States(p1, n1, testOpts()))
	second := mustJSON(t, States(p2, n2, testOpts()))
	if first != second {
		t.Fatalf("merge is not deterministic:\n%s\n%s", first, second)
	}
}

// TestStatesWithoutClockInventsNoTime proves the merge never reaches for the
// wall clock: with no Clock it reports the newest timestamp it was handed.
func TestStatesWithoutClockInventsNoTime(t *testing.T) {
	prev := newState(func(s *model.EngineeringState) { s.GeneratedAt = at(1) })
	next := newState(func(s *model.EngineeringState) { s.GeneratedAt = at(4) })

	got := States(prev, next, Options{})
	if !got.GeneratedAt.Equal(at(4)) {
		t.Fatalf("generated at = %v, want %v", got.GeneratedAt, at(4))
	}

	empty := States(nil, nil, Options{})
	if !empty.GeneratedAt.IsZero() {
		t.Fatalf("generated at = %v, want the zero time when nothing was known", empty.GeneratedAt)
	}
}

// TestStalenessNoteDescribesTheCurrentFileList guards against an annotation
// outliving the condition it reports. A note that says the file list may be
// stale, attached to a list that was just refreshed from an available git, is a
// false statement of uncertainty — and a reader who catches one stops believing
// the rest.
func TestStalenessNoteDescribesTheCurrentFileList(t *testing.T) {
	prev := newState(func(s *model.EngineeringState) {
		s.Capture.GitAvailable = true
		s.ChangedFiles = []model.ChangedFile{{Path: "a.go", Status: "M"}}
	})
	degraded := newState(func(s *model.EngineeringState) { s.Capture.GitAvailable = false })

	stale := States(prev, degraded, testOpts())
	if !noteContaining(stale, "may be stale") {
		t.Fatalf("the retained file list was not flagged: %v", stale.Capture.Notes)
	}

	recovered := newState(func(s *model.EngineeringState) {
		s.Capture.GitAvailable = true
		s.ChangedFiles = []model.ChangedFile{{Path: "b.go", Status: "A"}}
	})
	fresh := States(stale, recovered, testOpts())

	if len(fresh.ChangedFiles) != 1 || fresh.ChangedFiles[0].Path != "b.go" {
		t.Fatalf("changed files = %+v, want the freshly observed list", fresh.ChangedFiles)
	}
	if noteContaining(fresh, "may be stale") {
		t.Fatalf("a list read from an available git is still flagged as possibly stale: %v", fresh.Capture.Notes)
	}
}

// TestTransitionNoteDescribesTheCurrentStatus guards the same property for the
// §17 annotation: it explains the status the state actually carries, so a note
// left over from an earlier merge must not sit beside it describing a different
// one.
func TestTransitionNoteDescribesTheCurrentStatus(t *testing.T) {
	first := States(
		newState(func(s *model.EngineeringState) { s.Status = model.StatusNew }),
		newState(func(s *model.EngineeringState) {
			s.Requirements = []model.Requirement{{
				ID: "R1", Status: model.ReqPartial, Evidence: []model.Evidence{commitEv("abc123")},
			}}
		}),
		testOpts())
	if !noteContaining(first, `"new" -> "partial"`) {
		t.Fatalf("the illegal transition was not reported: %v", first.Capture.Notes)
	}

	second := States(first, newState(func(s *model.EngineeringState) {
		s.Requirements = []model.Requirement{{
			ID: "R1", Status: model.ReqComplete, Evidence: []model.Evidence{commitEv("abc123")},
		}}
	}), testOpts())

	if second.Status != model.StatusVerified {
		t.Fatalf("status = %q, want verified", second.Status)
	}
	if noteContaining(second, `"new" -> "partial"`) {
		t.Fatalf("a note about a superseded transition was carried forward: %v", second.Capture.Notes)
	}
	var transitionNotes int
	for _, n := range second.Capture.Notes {
		if strings.Contains(n, "not a declared transition") {
			transitionNotes++
		}
	}
	if transitionNotes != 1 {
		t.Fatalf("transition notes = %d, want exactly one describing the current status: %v",
			transitionNotes, second.Capture.Notes)
	}
}

// TestStatesCanonicalisesNestedEvidence protects the state hash. Two captures
// that cite the same artifacts in a different order are the same engineering
// conclusion, and the receipt printed on a handoff is only checkable if they
// fingerprint the same. EngineeringState.Sort does not reach evidence nested in
// work items, assumptions, risks, next actions, constraints or changed files, so
// merge canonicalises those itself.
func TestStatesCanonicalisesNestedEvidence(t *testing.T) {
	build := func(ev ...model.Evidence) *model.EngineeringState {
		return newState(func(s *model.EngineeringState) {
			s.CompletedWork = []model.WorkItem{{Description: "added backoff helper", Evidence: ev}}
			s.InProgress = []model.WorkItem{{Description: "wiring the budget", Evidence: ev}}
			s.FailedAttempts = []model.FailedAttempt{{Description: "retry loop deadlocked", Evidence: ev}}
			s.Assumptions = []model.Assumption{{Statement: "the payment API is idempotent", Evidence: ev}}
			s.Risks = []model.Risk{{Description: "retries may double-charge", Evidence: ev}}
			s.NextActions = []model.NextAction{{Description: "cap the budget", Priority: 1, Evidence: ev}}
			s.Constraints = []model.Constraint{{ID: "C1", Text: "offline only", Evidence: ev}}
			s.ChangedFiles = []model.ChangedFile{{Path: "client/retry.go", Status: "M", Evidence: ev}}
		})
	}
	a := States(nil, build(inferEv("guess"), commitEv("abc123")), testOpts())
	b := States(nil, build(commitEv("abc123"), inferEv("guess")), testOpts())

	if a.Hash() != b.Hash() {
		t.Fatalf("the same evidence cited in a different order hashed differently:\n%s\n%s",
			mustJSON(t, a), mustJSON(t, b))
	}
	// Strongest-first, matching how model.Sort orders every other evidence list.
	if got := a.CompletedWork[0].Evidence; len(got) != 2 || got[0].Kind != model.EvidenceCommit {
		t.Fatalf("nested evidence = %v, want the repository citation first", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
