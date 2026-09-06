package semantic

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// TestClaimsNeverCiteNothing is the sharp edge of "no claim without evidence".
//
// A citation with no ref is not weak evidence, it is no evidence wearing an
// evidence kind: nobody can open it, and model.ConfidenceFor still reads its
// kind and can rate the claim Observed. Counting the length of the evidence
// slice does not catch that, so this checks that the uncitable inputs never
// become claims at all.
func TestClaimsNeverCiteNothing(t *testing.T) {
	st := extract(t, hostileInput())

	for _, c := range claims(st) {
		for i, e := range c.evidence {
			if e.Kind == "" || strings.TrimSpace(e.Ref) == "" {
				t.Errorf("%s %q evidence[%d] cites nothing a reader can open: %+v",
					c.kind, c.label, i, e)
			}
		}
	}

	// The failing test has no name, so it cannot be the reason anything is
	// blocked. Unknown is the honest answer.
	for _, r := range st.Requirements {
		if r.Status == model.ReqBlocked {
			t.Errorf("requirement %s (%q) is blocked by a test that cannot be named or re-run",
				r.ID, r.Description)
		}
	}
	if got := st.DeriveStatus(); got == model.StatusBlocked {
		t.Error("an unnamed failing test propagated all the way to a blocked task status")
	}
	for _, a := range st.NextActions {
		if strings.TrimSpace(strings.TrimPrefix(a.Description, "Fix failing test")) == "" {
			t.Errorf("proposed %q, an action naming no test", a.Description)
		}
	}

	// The untyped event cites nothing, so it produces no decision. The typed one
	// is a rejection, and ids stay contiguous because the drop happens at the
	// source rather than in the final prune.
	if len(st.Decisions) != 0 {
		t.Errorf("got %d decisions from an event no adapter typed: %+v", len(st.Decisions), st.Decisions)
	}
	if len(st.Rejected) != 1 || st.Rejected[0].ID != "X1" {
		t.Errorf("rejected = %+v, want exactly X1 from the one citable event", st.Rejected)
	}
}

// TestUncitableEvidenceDoesNotRaiseConfidence pins the other half: a caller
// supplied Level 1 record with an empty ref must not lift a claim to Observed.
func TestUncitableEvidenceDoesNotRaiseConfidence(t *testing.T) {
	st := extract(t, hostileInput())
	for _, r := range st.Requirements {
		for _, e := range r.Evidence {
			if e.Ref == "" {
				t.Fatalf("requirement %s kept an uncitable record %+v", r.ID, e)
			}
		}
		if want := model.ConfidenceFor(r.Evidence); r.Confidence != want {
			t.Errorf("requirement %s confidence %q, want %q derived from surviving evidence",
				r.ID, r.Confidence, want)
		}
	}

	// pruneUnevidenced is the documented chokepoint, so drive it directly: a
	// future builder that hands it a claim propped up by an empty citation must
	// see the claim downgraded, not shipped as observed fact.
	propped := model.NewEngineeringState(model.TaskRef{})
	propped.Requirements = []model.Requirement{{
		ID:         "R1",
		Status:     model.ReqUnknown,
		Confidence: model.Observed,
		Evidence: []model.Evidence{
			{Kind: model.EvidenceCommit, Ref: ""}, // Level 1, cites nothing
			{Kind: model.EvidencePrompt, Ref: sourceOriginalPrompt, LineStart: 1},
		},
	}}
	propped.Assumptions = []model.Assumption{{
		Statement:  "the store is writable",
		Confidence: model.Observed,
		Evidence:   []model.Evidence{{Kind: model.EvidenceFile, Ref: ""}},
	}}
	pruneUnevidenced(propped)

	if len(propped.Requirements) != 1 {
		t.Fatalf("got %d requirements, want the one with a real citation kept", len(propped.Requirements))
	}
	if got := propped.Requirements[0]; got.Confidence != model.Inferred {
		t.Errorf("requirement confidence %q, want %q once the empty citation is discarded",
			got.Confidence, model.Inferred)
	}
	if len(propped.Requirements[0].Evidence) != 1 {
		t.Errorf("evidence = %+v, want only the prompt citation", propped.Requirements[0].Evidence)
	}
	if len(propped.Assumptions) != 0 {
		t.Errorf("assumptions = %+v, want the one held up by nothing dropped", propped.Assumptions)
	}
}

// TestEvidenceIsNotSharedBetweenClaims guards against the quietest failure this
// package can have: two claims handed the same backing array, so that a later
// merge step appending a citation to one silently rewrites the other's.
func TestEvidenceIsNotSharedBetweenClaims(t *testing.T) {
	det := model.NewEngineeringState(model.TaskRef{})
	det.Tests = []model.TestResult{{
		Name:   "TestResumeOnWindows",
		Status: model.TestFailed,
		// The deterministic layer already cited this test. The synthesized
		// citation dedupes against it, which is what leaves spare capacity on
		// the requirement's evidence slice for a later append to land in.
		Evidence: []model.Evidence{{Kind: model.EvidenceTest, Ref: "TestResumeOnWindows", Result: "failed"}},
	}}
	st := extract(t, model.ExtractionInput{
		OriginalPrompt: "Resume must keep working on Windows.",
		Deterministic:  det,
	})

	if len(st.Requirements) != 1 {
		t.Fatalf("got %d requirements, want 1: %+v", len(st.Requirements), st.Requirements)
	}
	req := st.Requirements[0]
	var act model.NextAction
	for _, a := range st.NextActions {
		if strings.HasPrefix(a.Description, "Resolve "+req.ID) {
			act = a
		}
	}
	if act.Description == "" {
		t.Fatalf("no next action for %s: %+v", req.ID, st.NextActions)
	}
	if len(req.Evidence) == 0 || len(act.Evidence) == 0 {
		t.Fatal("both claims must carry evidence for this test to mean anything")
	}
	if &req.Evidence[0] == &act.Evidence[0] {
		t.Fatal("the requirement and its next action share one evidence array")
	}

	// Two downstream layers each add a citation, as a merge would.
	reqGrown := append(req.Evidence, model.Evidence{Kind: model.EvidenceCommit, Ref: "req-side"})
	actGrown := append(act.Evidence, model.Evidence{Kind: model.EvidenceCommit, Ref: "act-side"})
	if got := reqGrown[len(reqGrown)-1].Ref; got != "req-side" {
		t.Errorf("the requirement's own citation became %q: an append to the next action overwrote it", got)
	}
	if got := actGrown[len(actGrown)-1].Ref; got != "act-side" {
		t.Errorf("the next action's own citation became %q", got)
	}
}

// TestReasonFrom covers the causal clause, including the inputs that used to
// mis-slice or crash.
//
// The clause was previously located in a strings.ToLower copy and then used to
// slice the original. Lower-casing can lengthen a string (U+023A becomes a
// three-byte rune), so the offset ran past the original: at best the reason lost
// its first bytes, at worst the slice panicked. Event summaries are free text
// from an external agent and plan §48 forbids anything here being fatal to the
// host agent's work.
func TestReasonFrom(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "Reverted it because the lock was held", "the lock was held"},
		{"cue is case insensitive", "Reverted it BECAUSE the lock was held", "the lock was held"},
		{"no cue", "Reverted the schema change", ""},
		{"trailing punctuation is trimmed", "Failed because the disk was full.", "the disk was full"},
		{
			// One rune whose lower-case form is longer: the old offset dropped
			// the first byte of the answer and nothing reported it.
			name: "a lengthening rune before the cue does not shift the clause",
			in:   "Ⱥ reverted because the lock was held",
			want: "the lock was held",
		},
		{
			// Two of them plus a one-byte tail: the old offset ran off the end.
			name: "a lengthening rune with a short tail does not panic",
			in:   "ȺȺ reverted the daemon because x",
			want: "x",
		},
		{"cue at the very end yields nothing", "Reverted it because", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonFrom(tc.in); got != tc.want {
				t.Errorf("reasonFrom(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExtractSurvivesLengtheningRunes drives the same hazard end to end, since
// reasonFrom is reached from both vocabularies.
func TestExtractSurvivesLengtheningRunes(t *testing.T) {
	events := []model.AgentEvent{
		{Type: model.TurnEnded, Timestamp: base, SessionID: "s1", Agent: model.AgentOpenClaw,
			Summary: "ȺȺ decided on channels because x"},
		{Type: model.ToolUsed, Timestamp: base.Add(time.Second), SessionID: "s1", Agent: model.AgentOpenClaw,
			Summary: "ȾȾ abandoned the daemon because y"},
	}
	st := extract(t, model.ExtractionInput{Events: events})
	if len(st.Decisions) != 1 || st.Decisions[0].Reason != "x" {
		t.Errorf("decisions = %+v, want one reading its reason as %q", st.Decisions, "x")
	}
	if len(st.Rejected) != 1 || st.Rejected[0].Reason != "y" {
		t.Errorf("rejected = %+v, want one reading its reason as %q", st.Rejected, "y")
	}
}
