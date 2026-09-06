package semantic

import (
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// TestPromptCandidates covers which shapes of prompt line become requirements
// and, just as importantly, which do not.
func TestPromptCandidates(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   []string // "text|form"
	}{
		{
			name:   "numbered with R prefix",
			prompt: "R1 - Ship the adapter.",
			want:   []string{"Ship the adapter|numbered"},
		},
		{
			name:   "numbered with dot and paren",
			prompt: "1. First thing.\n2) Second thing.",
			want:   []string{"First thing|numbered", "Second thing|numbered"},
		},
		{
			name:   "req prefix with colon",
			prompt: "REQ 4: Persist the lineage.",
			want:   []string{"Persist the lineage|numbered"},
		},
		{
			name:   "bullets",
			prompt: "- Handle drift.\n* Record checkpoints.",
			want:   []string{"Handle drift|bullet", "Record checkpoints|bullet"},
		},
		{
			name:   "prose keeps only the cue sentences",
			prompt: "We looked at the code. The adapter must emit SessionEnded. It was slow.",
			want:   []string{"The adapter must emit SessionEnded|cue:must"},
		},
		{
			name:   "multiword cue",
			prompt: "The renderer needs to cite a checkpoint.",
			want:   []string{"The renderer needs to cite a checkpoint|cue:needs to"},
		},
		{
			name:   "negative cue",
			prompt: "Persist events without copying payloads.",
			want:   []string{"Persist events without copying payloads|cue:without"},
		},
		{
			name:   "source order is preserved across forms",
			prompt: "Add the adapter.\n- Then record checkpoints.\n2. Finally ensure drift is detected.",
			want: []string{
				"Add the adapter|cue:add",
				"Then record checkpoints|bullet",
				"Finally ensure drift is detected|numbered",
			},
		},
		{
			name:   "prose with no cue is not an obligation",
			prompt: "The store already writes JSONL. Everything looks fine.",
			want:   nil,
		},
		{
			// "add" must not fire on "address": a substring match would promote
			// ordinary prose into an obligation nobody stated.
			name:   "cue words match on token boundaries",
			prompt: "The address parser was rewritten.",
			want:   nil,
		},
		{
			name:   "blank prompt yields nothing",
			prompt: "   \n\n",
			want:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range promptCandidates(tc.prompt) {
				got = append(got, c.text+"|"+c.form)
			}
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Errorf("promptCandidates() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequirementsSkipUncorrelatableText(t *testing.T) {
	// Every meaning-bearing token is filtered out of these lines, so nothing
	// about them could ever be correlated or verified. Emitting "R1: ok" as a
	// requirement would be noise wearing an id.
	st := extract(t, model.ExtractionInput{OriginalPrompt: "- ok\n1. the\n- Ship the adapter."})
	if len(st.Requirements) != 1 {
		t.Fatalf("got %d requirements, want only the correlatable one: %+v", len(st.Requirements), st.Requirements)
	}
	got := st.Requirements[0]
	if got.ID != "R1" || got.Description != "Ship the adapter" {
		t.Errorf("got %s %q, want R1 %q (ids stay contiguous after a skip)", got.ID, got.Description, "Ship the adapter")
	}
}

func TestRequirementsDeduplicate(t *testing.T) {
	st := extract(t, model.ExtractionInput{
		OriginalPrompt: "- Ship the adapter.\n- ship the adapter\n- Ship the parser.",
	})
	if len(st.Requirements) != 2 {
		t.Fatalf("got %d requirements, want 2 (a restated obligation is one requirement): %+v",
			len(st.Requirements), st.Requirements)
	}
}

// TestCorrelate drives the correlation rules directly, including the tie-breaks
// that keep the choice independent of input ordering.
func TestCorrelate(t *testing.T) {
	det := func(tests []model.TestResult, files []model.ChangedFile) *model.EngineeringState {
		st := model.NewEngineeringState(model.TaskRef{})
		st.Tests, st.ChangedFiles = tests, files
		return st
	}

	tests := []struct {
		name       string
		text       string
		det        *model.EngineeringState
		wantStatus model.ReqStatus
		wantRef    string
	}{
		{
			name:       "no deterministic state is unknown",
			text:       "Resume must keep working on Windows",
			det:        nil,
			wantStatus: model.ReqUnknown,
		},
		{
			name:       "no overlap is unknown",
			text:       "Document the release process",
			det:        det([]model.TestResult{{Name: "TestResume", Status: model.TestFailed}}, nil),
			wantStatus: model.ReqUnknown,
		},
		{
			name:       "failing test blocks",
			text:       "Resume must keep working on Windows",
			det:        det([]model.TestResult{{Name: "TestResumeOnWindows", Status: model.TestFailed}}, nil),
			wantStatus: model.ReqBlocked,
			wantRef:    "TestResumeOnWindows",
		},
		{
			name: "a passing test is not evidence of completion",
			text: "Resume must keep working on Windows",
			det: det([]model.TestResult{{Name: "TestResumeOnWindows", Status: model.TestPassed}},
				[]model.ChangedFile{{Path: "internal/resume/windows.go", Status: "M"}}),
			// The strongest thing that can be said is "work touched this", never
			// "this is done": only ReqPartial is reachable here.
			wantStatus: model.ReqPartial,
			wantRef:    "internal/resume/windows.go",
		},
		{
			name: "a failing test outranks a changed file",
			text: "Resume must keep working on Windows",
			det: det([]model.TestResult{{Name: "TestResumeOnWindows", Status: model.TestFailed}},
				[]model.ChangedFile{{Path: "internal/resume/windows.go", Status: "M"}}),
			wantStatus: model.ReqBlocked,
			wantRef:    "TestResumeOnWindows",
		},
		{
			name: "strongest overlap wins over slice position",
			text: "Resume must keep working on Windows",
			det: det([]model.TestResult{
				{Name: "TestWindows", Status: model.TestFailed},
				{Name: "TestResumeOnWindows", Status: model.TestFailed},
			}, nil),
			wantStatus: model.ReqBlocked,
			wantRef:    "TestResumeOnWindows",
		},
		{
			name: "equal overlap breaks on name, not on order",
			text: "Resume the session",
			det: det([]model.TestResult{
				{Name: "TestResumeZ", Status: model.TestFailed},
				{Name: "TestResumeA", Status: model.TestFailed},
			}, nil),
			wantStatus: model.ReqBlocked,
			wantRef:    "TestResumeA",
		},
		{
			name: "equal overlap on files breaks on path",
			text: "Fix the handoff renderer",
			det: det(nil, []model.ChangedFile{
				{Path: "z/handoff.go", Status: "M"},
				{Path: "a/handoff.go", Status: "M"},
			}),
			wantStatus: model.ReqPartial,
			wantRef:    "a/handoff.go",
		},
		{
			name:       "path scaffolding alone does not correlate",
			text:       "Keep the internal test helpers tidy",
			det:        det(nil, []model.ChangedFile{{Path: "internal/store/jsonl_test.go", Status: "M"}}),
			wantStatus: model.ReqUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, ev := correlate(keywords(tc.text), tc.det, base)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if status == model.ReqComplete {
				t.Error("correlate must have no path to complete")
			}
			if tc.wantRef == "" {
				if len(ev) != 0 {
					t.Errorf("evidence = %v, want none", ev)
				}
				return
			}
			if len(ev) == 0 || ev[0].Ref != tc.wantRef {
				t.Errorf("evidence = %v, want it to cite %q", ev, tc.wantRef)
			}
		})
	}
}

// TestDecisionAndRejectionCues checks each vocabulary, including the summary
// that is legitimately both a decision and a rejection.
func TestDecisionAndRejectionCues(t *testing.T) {
	tests := []struct {
		name         string
		summary      string
		wantDecision bool
		wantRejected bool
		wantReason   string
	}{
		{"decided", "Decided to use JSONL.", true, false, ""},
		{"chose", "Chose the adapter interface.", true, false, ""},
		{"chosen", "JSONL was chosen.", true, false, ""},
		{"instead of", "Used a file lock instead of a mutex.", true, false, ""},
		{"switched to", "Switched to a channel-based pipeline.", true, false, ""},
		{"went with", "Went with the simpler schema.", true, false, ""},
		{"reverted", "Reverted the SQLite store.", false, true, ""},
		{"rejected", "Rejected the polling approach.", false, true, ""},
		{"does not work", "The polling approach does not work here.", false, true, ""},
		{"broke", "The migration broke resume.", false, true, ""},
		{"abandoned", "Abandoned the daemon.", false, true, ""},
		{"failed because", "Resume failed because the lock was held.", false, true, "the lock was held"},
		{"backed out", "Backed out the schema change.", false, true, ""},
		{
			name:         "both vocabularies on one summary",
			summary:      "Reverted the mutex and went with channels.",
			wantDecision: true,
			wantRejected: true,
		},
		{"neither", "Ran the test suite.", false, false, ""},
		{"empty summary yields nothing", "", false, false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := []model.AgentEvent{{
				Type: model.TurnEnded, Timestamp: base, SessionID: "s1",
				Agent: model.AgentOpenClaw, Summary: tc.summary,
			}}
			dec, rej := decisions(events), rejected(events)
			if got := len(dec) == 1; got != tc.wantDecision {
				t.Errorf("decisions = %v, want a decision: %v", dec, tc.wantDecision)
			}
			if got := len(rej) == 1; got != tc.wantRejected {
				t.Errorf("rejected = %v, want a rejection: %v", rej, tc.wantRejected)
			}
			if tc.wantReason != "" {
				if len(rej) != 1 || rej[0].Reason != tc.wantReason {
					t.Errorf("rejection reason = %v, want %q", rej, tc.wantReason)
				}
			}
		})
	}
}

// TestAssumptionSources checks that every source the extractor may read is
// scanned, that a repeat is recorded once, and that the citation points at the
// source it actually came from.
func TestAssumptionSources(t *testing.T) {
	events := []model.AgentEvent{{
		Type: model.TurnEnded, Timestamp: base, SessionID: "s1", Agent: model.AgentOpenClaw,
		Summary: "Assuming the adapter emits SessionEnded. The run then ended.",
	}}
	got := assumptions(
		"We assume the store is writable.\nNothing unproven here.",
		events,
		[]string{"The reviewer presumably wants JSONL.", "We assume the store is writable."},
		base,
	)

	want := []struct {
		statement string
		kind      model.EvidenceKind
		ref       string
	}{
		{"We assume the store is writable", model.EvidencePrompt, sourceOriginalPrompt},
		{"Assuming the adapter emits SessionEnded", model.EvidenceEvent, string(model.TurnEnded)},
		{"The reviewer presumably wants JSONL", model.EvidencePrompt, refTranscript},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d assumptions, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Statement != w.statement {
			t.Errorf("assumption %d = %q, want %q", i, got[i].Statement, w.statement)
		}
		if len(got[i].Evidence) != 1 {
			t.Fatalf("assumption %d evidence = %v, want exactly one citation", i, got[i].Evidence)
		}
		if e := got[i].Evidence[0]; e.Kind != w.kind || e.Ref != w.ref {
			t.Errorf("assumption %d cites %s:%s, want %s:%s", i, e.Kind, e.Ref, w.kind, w.ref)
		}
		if got[i].Invalidated {
			t.Errorf("assumption %d is marked invalidated; only later evidence can do that", i)
		}
	}
}

// TestSymbolFromTest pins the derivation that fills NextAction.Target, which is
// what the Graph layer resolves. A wrong answer here is not a cosmetic slip: it
// aims impact analysis at something that does not exist, which is the fabricated
// target the function exists to avoid.
func TestSymbolFromTest(t *testing.T) {
	tests := []struct{ name, want string }{
		{"TestResumeOnWindows", "ResumeOnWindows"},
		{"TestResumeOnWindows/windows_path", "ResumeOnWindows"},
		{"internal/store.TestAppend", "Append"},
		{"TestUpgrade/v1.2", "Upgrade"},
		{"test_resume_command", "resume_command"},
		{"Test", ""},
		{"", ""},
		{"BenchmarkAppend", "BenchmarkAppend"},
		// A package-qualified name whose subtest also contains a dot. Anchoring
		// on the last dot misses the qualifier here, and the leading path is
		// then trimmed at its first "/" — returning the directory "internal" as
		// if it were a symbol.
		{"internal/store.TestAppend/case.1", "Append"},
		{"internal/store.TestAppend/v1.2", "Append"},
		{"github.com/x/y/pkg.TestFoo/sub.case", "Foo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := symbolFromTest(tc.name)
			if got != tc.want {
				t.Errorf("symbolFromTest(%q) = %q, want %q", tc.name, got, tc.want)
			}
			if strings.ContainsAny(got, "/\\") {
				t.Errorf("symbolFromTest(%q) = %q, which is a path, not a symbol", tc.name, got)
			}
		})
	}
}

func TestSplitTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"camel case", "TestResumeOnWindows", []string{"test", "resume", "on", "windows"}},
		{"path", "internal/store/jsonl.go", []string{"internal", "store", "jsonl", "go"}},
		{"snake case", "resume_on_windows", []string{"resume", "on", "windows"}},
		{"digits stay attached", "sha256sum", []string{"sha256sum"}},
		{"punctuation only", "-- ,. --", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitTokens(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("splitTokens(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestKeywordsDropNoise(t *testing.T) {
	// Cue words are how a sentence was phrased, not what it is about; matching a
	// file because both say "add" would be a fabricated evidence link.
	got := keywords("The adapter must add events without the internal test payloads")
	want := []string{"adapter", "events", "payloads"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keywords() = %v, want %v (sorted, cue and scaffolding words dropped)", got, want)
	}
}
