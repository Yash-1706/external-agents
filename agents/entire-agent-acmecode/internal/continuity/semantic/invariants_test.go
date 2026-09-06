package semantic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// claim is one emitted semantic claim, flattened so the invariants below can be
// checked over every kind at once.
type claim struct {
	kind     string
	label    string
	evidence []model.Evidence
}

func claims(st *model.EngineeringState) []claim {
	var out []claim
	for _, r := range st.Requirements {
		out = append(out, claim{"requirement", r.ID, r.Evidence})
	}
	for _, d := range st.Decisions {
		out = append(out, claim{"decision", d.ID, d.Evidence})
	}
	for _, x := range st.Rejected {
		out = append(out, claim{"rejected", x.ID, x.Evidence})
	}
	for _, a := range st.Assumptions {
		out = append(out, claim{"assumption", a.Statement, a.Evidence})
	}
	for _, a := range st.NextActions {
		out = append(out, claim{"next_action", a.Description, a.Evidence})
	}
	return out
}

type namedInput struct {
	name string
	in   model.ExtractionInput
}

// inputVariants are the shapes an extraction realistically arrives in, used by
// the invariant tests below. Several are engineered to tempt the extractor into
// claiming completion.
func inputVariants() []namedInput {
	// Everything about this one says "finished": the work is described in the
	// past tense, every test passes, and the changed files match the
	// requirements' vocabulary word for word.
	looksDone := model.ExtractionInput{
		Task:           model.TaskRef{ID: "task-2"},
		OriginalPrompt: "Implement the JSONL store.\n- Add a resume command that must restore the session.",
		Events: []model.AgentEvent{{
			Type: model.TurnEnded, Timestamp: base, SessionID: "s1", Agent: model.AgentOpenClaw,
			Summary: "Chose JSONL and finished the resume command; everything is done and all tests pass.",
		}},
		Deterministic: func() *model.EngineeringState {
			st := model.NewEngineeringState(model.TaskRef{ID: "task-2"})
			st.Tests = []model.TestResult{
				{Name: "TestJSONLStore", Status: model.TestPassed},
				{Name: "TestResumeCommand", Status: model.TestPassed},
			}
			st.ChangedFiles = []model.ChangedFile{
				{Path: "internal/store/jsonl.go", Status: "A"},
				{Path: "cmd/resume/main.go", Status: "A"},
			}
			return st
		}(),
		Transcript: []string{"The task is complete."},
	}

	noDeterministic := fixtureInput()
	noDeterministic.Deterministic = nil

	noPrompt := fixtureInput()
	noPrompt.OriginalPrompt = ""

	noEvents := fixtureInput()
	noEvents.Events = nil

	return []namedInput{
		{"full", fixtureInput()},
		{"looks_done", looksDone},
		{"no_deterministic", noDeterministic},
		{"no_prompt", noPrompt},
		{"no_events", noEvents},
		{"empty", model.ExtractionInput{Task: model.TaskRef{ID: "task-3"}}},
		{"hostile", hostileInput()},
	}
}

// hostileInput is input that looks well formed and is quietly not: every detail
// in it is a real shape an upstream adapter or repo inspector can produce, and
// every one of them defeats a naive "does this claim have evidence" check.
//
//   - An event whose Type an adapter never set. AgentEvent.AsEvidence uses the
//     type as its ref, so it yields a citation with an empty ref: length says
//     "evidenced", the reader has nothing to open.
//   - A failing test with no name. It cannot be cited or re-run, yet correlating
//     against it would declare a requirement blocked — and one blocked
//     requirement makes DeriveStatus call the whole task blocked.
//   - A changed file carrying a caller-supplied commit citation with no sha.
//     EvidenceCommit is Level 1, so ConfidenceFor reads it as Observed: an empty
//     string upgrading a guess to a fact.
//   - Summaries and transcript lines containing U+023A/U+023E, whose lower-case
//     forms are a byte longer than the originals.
func hostileInput() model.ExtractionInput {
	det := model.NewEngineeringState(model.TaskRef{ID: "task-4"})
	det.Tests = []model.TestResult{
		{Name: "", Status: model.TestFailed, Files: []string{"internal/resume/windows.go"}},
	}
	det.ChangedFiles = []model.ChangedFile{{
		Path:     "internal/handoff/render.go",
		Status:   "M",
		Evidence: []model.Evidence{{Kind: model.EvidenceCommit, Ref: ""}},
	}}
	return model.ExtractionInput{
		Task:           model.TaskRef{ID: "task-4"},
		OriginalPrompt: "Resume must keep working on Windows.\n- Ensure the handoff cites a checkpoint.",
		Events: []model.AgentEvent{
			{Timestamp: base, SessionID: "s1", Agent: model.AgentOpenClaw,
				Summary: "Decided to keep JSONL."},
			{Type: model.TurnEnded, Timestamp: base.Add(time.Second), SessionID: "s1",
				Agent: model.AgentOpenClaw, Summary: "ȺȺ reverted the daemon because x"},
		},
		Deterministic: det,
		Transcript:    []string{"Presumably ȺȺ still holds."},
	}
}

// TestRequirementsNeverComplete is a correctness test, not a stylistic one.
//
// Keyword overlap shows that work touched the same subject; it is never proof
// that an obligation was met. Plan §32 forbids marking a requirement complete
// without sufficient evidence, so a heuristic must have no path to ReqComplete
// at all, including over input that loudly claims to be finished.
func TestRequirementsNeverComplete(t *testing.T) {
	for _, tc := range inputVariants() {
		t.Run(tc.name, func(t *testing.T) {
			st := extract(t, tc.in)
			for _, r := range st.Requirements {
				if r.Status == model.ReqComplete {
					t.Errorf("requirement %s (%q) was marked complete; a heuristic cannot certify completion",
						r.ID, r.Description)
				}
				if !r.Status.Valid() {
					t.Errorf("requirement %s has invalid status %q", r.ID, r.Status)
				}
			}
			if complete, _ := st.CountRequirements(); complete != 0 {
				t.Errorf("CountRequirements reports %d complete, want 0", complete)
			}
		})
	}
}

// TestEveryClaimCarriesEvidence enforces the other half of the package contract:
// a claim nobody can check is worse than an absent claim, so it is dropped
// rather than emitted bare (plan §11).
func TestEveryClaimCarriesEvidence(t *testing.T) {
	for _, tc := range inputVariants() {
		t.Run(tc.name, func(t *testing.T) {
			st := extract(t, tc.in)
			for _, c := range claims(st) {
				if len(c.evidence) == 0 {
					t.Errorf("%s %q carries no evidence", c.kind, c.label)
					continue
				}
				for i, e := range c.evidence {
					if e.Kind == "" || e.Ref == "" {
						t.Errorf("%s %q evidence[%d] is not a usable citation: %+v", c.kind, c.label, i, e)
					}
				}
			}
		})
	}
}

// TestConfidenceMatchesEvidence checks that no claim reads stronger than the
// evidence under it. model.ConfidenceFor is the single chokepoint for that, so
// this really tests that nothing bypasses it.
func TestConfidenceMatchesEvidence(t *testing.T) {
	for _, tc := range inputVariants() {
		t.Run(tc.name, func(t *testing.T) {
			st := extract(t, tc.in)
			for _, r := range st.Requirements {
				if want := model.ConfidenceFor(r.Evidence); r.Confidence != want {
					t.Errorf("requirement %s confidence %q, want %q", r.ID, r.Confidence, want)
				}
			}
			for _, x := range st.Rejected {
				if want := model.ConfidenceFor(x.Evidence); x.Confidence != want {
					t.Errorf("rejected %s confidence %q, want %q", x.ID, x.Confidence, want)
				}
			}
			for _, a := range st.Assumptions {
				if want := model.ConfidenceFor(a.Evidence); a.Confidence != want {
					t.Errorf("assumption %q confidence %q, want %q", a.Statement, a.Confidence, want)
				}
			}
			// A next action is a proposal, never a statement of fact (plan §12),
			// so it is Recommended however strong its evidence is.
			for _, a := range st.NextActions {
				if a.Confidence != model.Recommended {
					t.Errorf("next action %q confidence %q, want %q", a.Description, a.Confidence, model.Recommended)
				}
				if a.GraphVerified {
					t.Errorf("next action %q claims Graph verification the extractor never performed", a.Description)
				}
			}
		})
	}
}

// TestExtractDeterministic proves the property the whole product rests on: the
// same input produces byte-identical output, and the caller's slice ordering
// does not leak into the result.
func TestExtractDeterministic(t *testing.T) {
	t.Run("repeated runs are identical", func(t *testing.T) {
		a := extract(t, fixtureInput())
		b := extract(t, fixtureInput())
		if a.Hash() != b.Hash() {
			t.Errorf("state hash differs between runs: %s vs %s", a.Hash(), b.Hash())
		}
		ja, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		jb, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(ja) != string(jb) {
			t.Errorf("serialized state differs between runs:\n%s\n%s", ja, jb)
		}
	})

	t.Run("event input order does not matter", func(t *testing.T) {
		shuffled := fixtureInput()
		ev := shuffled.Events
		for i, j := 0, len(ev)-1; i < j; i, j = i+1, j-1 {
			ev[i], ev[j] = ev[j], ev[i]
		}
		want := strings.Join(dump(extract(t, fixtureInput())), "\n")
		got := strings.Join(dump(extract(t, shuffled)), "\n")
		if got != want {
			t.Errorf("reordering the event slice changed the extraction:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("extraction does not mutate its input", func(t *testing.T) {
		in := fixtureInput()
		before := append([]model.AgentEvent(nil), in.Events...)
		extract(t, in)
		for i := range before {
			if in.Events[i].Key() != before[i].Key() {
				t.Errorf("event %d was reordered or rewritten in the caller's slice", i)
			}
		}
	})
}

// countingClock records how often the extractor reads the time.
type countingClock struct {
	n int
	t time.Time
}

func (c *countingClock) Now() time.Time {
	c.n++
	return c.t
}

// TestExtractReadsClockOnce pins "one extraction is one instant". If a later
// change reads the clock per claim, a Clock that advances would stamp claims
// with drifting times and two runs would stop being byte-identical.
func TestExtractReadsClockOnce(t *testing.T) {
	c := &countingClock{t: base}
	if _, err := NewHeuristic(c).Extract(context.Background(), fixtureInput()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if c.n != 1 {
		t.Errorf("clock read %d times, want exactly 1", c.n)
	}
}

// TestExtractDegradesHonestly covers the paths where an input is missing. The
// product requirement is that the state says what it could not see rather than
// filling the hole (plan §33, §48).
func TestExtractDegradesHonestly(t *testing.T) {
	noDeterministic := fixtureInput()
	noDeterministic.Deterministic = nil

	noPrompt := fixtureInput()
	noPrompt.OriginalPrompt = ""

	noEvents := fixtureInput()
	noEvents.Events = nil

	tests := []struct {
		name        string
		in          model.ExtractionInput
		wantMissing []string
		check       func(t *testing.T, st *model.EngineeringState)
	}{
		{
			name:        "no deterministic state",
			in:          noDeterministic,
			wantMissing: []string{"deterministic_state"},
			check: func(t *testing.T, st *model.EngineeringState) {
				if len(st.Requirements) == 0 {
					t.Fatal("requirements should still be extracted from the prompt")
				}
				for _, r := range st.Requirements {
					// Nothing to correlate against means nothing is known about
					// status. Unknown is the honest answer (plan §32).
					if r.Status != model.ReqUnknown {
						t.Errorf("requirement %s status %q, want unknown with no deterministic state", r.ID, r.Status)
					}
					if r.Confidence != model.Inferred {
						t.Errorf("requirement %s confidence %q, want inferred from prompt evidence alone", r.ID, r.Confidence)
					}
				}
				for _, a := range st.NextActions {
					if strings.HasPrefix(a.Description, "Fix failing test") {
						t.Errorf("proposed %q with no test results to justify it", a.Description)
					}
				}
			},
		},
		{
			name:        "no prompt",
			in:          noPrompt,
			wantMissing: []string{sourceOriginalPrompt},
			check: func(t *testing.T, st *model.EngineeringState) {
				if len(st.Requirements) != 0 {
					t.Errorf("invented %d requirements without a prompt", len(st.Requirements))
				}
				// The event stream is still readable, so decisions survive.
				if len(st.Decisions) == 0 {
					t.Error("decisions should still come from events when the prompt is missing")
				}
			},
		},
		{
			name:        "no events",
			in:          noEvents,
			wantMissing: []string{"agent_events"},
			check: func(t *testing.T, st *model.EngineeringState) {
				if len(st.Decisions) != 0 || len(st.Rejected) != 0 {
					t.Errorf("invented %d decisions and %d rejections with no events",
						len(st.Decisions), len(st.Rejected))
				}
				if len(st.Requirements) == 0 {
					t.Error("requirements should still be extracted from the prompt")
				}
			},
		},
		{
			name:        "nothing at all",
			in:          model.ExtractionInput{Task: model.TaskRef{ID: "task-empty"}},
			wantMissing: []string{sourceOriginalPrompt, "agent_events", "deterministic_state"},
			check: func(t *testing.T, st *model.EngineeringState) {
				if n := len(claims(st)); n != 0 {
					t.Errorf("produced %d claims from no input at all", n)
				}
				// Empty, not nil: the serialized state must contain no nulls for
				// a consumer to read it back safely.
				b, err := json.Marshal(st)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if strings.Contains(string(b), "null") {
					t.Errorf("serialized empty state contains null: %s", b)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := extract(t, tc.in)
			if !st.Capture.SemanticExtraction {
				t.Error("Capture.SemanticExtraction is false although extraction ran")
			}
			for _, want := range tc.wantMissing {
				found := false
				for _, m := range st.Capture.Missing {
					if m == want {
						found = true
					}
				}
				if !found {
					t.Errorf("Capture.Missing = %v, want it to name %q", st.Capture.Missing, want)
				}
			}
			if len(st.Capture.Notes) < len(tc.wantMissing) {
				t.Errorf("Capture.Notes = %v, want an explanation for each of %v", st.Capture.Notes, tc.wantMissing)
			}
			tc.check(t, st)
		})
	}
}

func TestExtractHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHeuristic(testClock()).Extract(ctx, fixtureInput()); err == nil {
		t.Fatal("Extract on a cancelled context returned no error")
	}
}

func TestNewHeuristicDefaults(t *testing.T) {
	e := NewHeuristic(nil) // a nil clock must not panic; it falls back to SystemClock
	if !e.Available(context.Background()) {
		t.Error("the heuristic extractor is never unavailable: it needs nothing external")
	}
	if !strings.Contains(e.Describe(), "no model") {
		t.Errorf("Describe() = %q; it must make clear that no model was consulted", e.Describe())
	}
	if _, err := e.Extract(context.Background(), fixtureInput()); err != nil {
		t.Fatalf("Extract with the default clock: %v", err)
	}
}
