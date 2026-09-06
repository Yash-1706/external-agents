package semantic

import (
	"context"

	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// base is the single instant every fixture is anchored to. Nothing in these
// tests may read the wall clock; that is the same rule the production code
// follows (plan §33).
var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testClock() model.Clock { return &model.FixedClock{Current: base, Step: time.Second} }

// fixturePrompt is written the way a task actually arrives: a sentence of
// intent, a numbered list, a bullet, and a closing paragraph containing one
// assumption and one sentence that is not an obligation at all.
//
// The prompt's own "R1"/"R2" labels deliberately do not line up with the
// extracted ids. Generated ids are positional (R1..Rn in source order) so that
// the same prompt yields the same ids everywhere; they are not inherited from
// whatever the author happened to call their bullets.
const fixturePrompt = `Add resumable task state so a second agent can continue this work.

R1 - The extractor must never mark a requirement complete without evidence.
R2 - Resume must keep working on Windows.
3. Store events without copying raw tool payloads.
- Ensure the handoff cites a checkpoint for every claim.

We assume the local store is writable, so nothing here needs to be atomic.
Nothing in this paragraph is an obligation.`

func fixtureEvents() []model.AgentEvent {
	ev := func(offset time.Duration, typ model.EventType, session, summary string) model.AgentEvent {
		return model.AgentEvent{
			Type:      typ,
			Timestamp: base.Add(offset),
			TaskID:    "task-1",
			SessionID: session,
			Agent:     model.AgentOpenClaw,
			Summary:   summary,
		}
	}
	checkpoint := ev(3*time.Minute, model.CheckpointCreated, "s1", "")
	checkpoint.Attrs = map[string]string{"checkpoint_id": "cp-7"}
	return []model.AgentEvent{
		// Out of chronological order on purpose: extraction must sort.
		ev(4*time.Minute, model.TurnEnded, "s2",
			"Switched to merging semantic output under deterministic facts, which does not work with the old schema."),
		ev(0, model.TurnEnded, "s1", "Decided to store events as JSONL instead of SQLite."),
		ev(time.Minute, model.ToolUsed, "s1",
			"Reverted the SQLite store because it broke resume on Windows."),
		ev(2*time.Minute, model.TurnEnded, "s1", "Assuming the adapter always emits SessionEnded."),
		checkpoint,
		// An exact replay of the first decision event: DedupeEvents must drop it
		// so a resumed session does not double-count its own history.
		ev(0, model.TurnEnded, "s1", "Decided to store events as JSONL instead of SQLite."),
	}
}

func fixtureDeterministic() *model.EngineeringState {
	st := model.NewEngineeringState(model.TaskRef{ID: "task-1"})
	st.Tests = []model.TestResult{
		{
			Name:     "TestResumeOnWindows",
			Status:   model.TestFailed,
			Command:  "go test ./internal/adapter",
			RanAt:    base.Add(10 * time.Minute),
			Evidence: []model.Evidence{{Kind: model.EvidenceCommit, Ref: "abc1234"}},
		},
		{Name: "TestHandoffRendersCitations", Status: model.TestPassed},
	}
	st.ChangedFiles = []model.ChangedFile{
		{Path: "internal/store/jsonl.go", Status: "M"},
		{Path: "internal/handoff/render.go", Status: "M"},
	}
	return st
}

func fixtureInput() model.ExtractionInput {
	return model.ExtractionInput{
		Task:           model.TaskRef{ID: "task-1", Title: "resumable task state"},
		OriginalPrompt: fixturePrompt,
		Events:         fixtureEvents(),
		Deterministic:  fixtureDeterministic(),
		Transcript: []string{
			"The reviewer presumably wants the JSONL format kept.",
			"This transcript line states nothing unproven.",
		},
	}
}

func extract(t *testing.T, in model.ExtractionInput) *model.EngineeringState {
	t.Helper()
	st, err := NewHeuristic(testClock()).Extract(context.Background(), in)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return st
}

func evRender(e model.Evidence) string {
	s := string(e.Kind) + ":" + e.Ref
	if e.Path != "" && e.Path != e.Ref {
		s += "@" + e.Path
	}
	if e.SessionID != "" {
		s += "~" + e.SessionID
	}
	if e.LineStart > 0 {
		s += fmt.Sprintf("#%d", e.LineStart)
	}
	if e.Result != "" {
		s += "(" + e.Result + ")"
	}
	return s
}

func evList(ev []model.Evidence) string {
	parts := make([]string, 0, len(ev))
	for _, e := range ev {
		parts = append(parts, evRender(e))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// dump renders the extracted state as one line per claim. A golden comparison on
// this catches a changed status, a lost citation or a reordered slice, which is
// what determinism actually means here.
func dump(st *model.EngineeringState) []string {
	var out []string
	out = append(out, "status "+string(st.Status))
	for _, r := range st.Requirements {
		out = append(out, fmt.Sprintf("REQ %s %s %s src=%s %q ev=%s",
			r.ID, r.Status, r.Confidence, r.Source, r.Description, evList(r.Evidence)))
	}
	for _, d := range st.Decisions {
		out = append(out, fmt.Sprintf("DEC %s %q reason=%q session=%s cp=%s ev=%s",
			d.ID, d.Decision, d.Reason, d.SessionID, d.CheckpointID, evList(d.Evidence)))
	}
	for _, x := range st.Rejected {
		out = append(out, fmt.Sprintf("REJ %s %s %q reason=%q ev=%s",
			x.ID, x.Confidence, x.Approach, x.Reason, evList(x.Evidence)))
	}
	for _, a := range st.Assumptions {
		out = append(out, fmt.Sprintf("ASM %s %q ev=%s", a.Confidence, a.Statement, evList(a.Evidence)))
	}
	for _, a := range st.NextActions {
		out = append(out, fmt.Sprintf("ACT %d %s target=%s %q ev=%s",
			a.Priority, a.Confidence, a.Target, a.Description, evList(a.Evidence)))
	}
	out = append(out, fmt.Sprintf("CAP semantic=%v missing=%v notes=%d",
		st.Capture.SemanticExtraction, st.Capture.Missing, len(st.Capture.Notes)))
	return out
}

// TestExtractGolden pins the whole extraction over the fixture.
//
// Read the want block as the product promise: R3 is blocked and says which test
// blocks it, R4/R5/R6 are partial and name the file that suggests progress, R1
// and R2 are honestly unknown, the replayed event is counted once, one event
// legitimately produces both a decision and a rejection, and nothing anywhere is
// complete. Every line carries a citation.
func TestExtractGolden(t *testing.T) {
	want := []string{
		// Status stays at the constructor default: an Extractor does not get to
		// declare where the task stands (Rule 6).
		`status new`,
		`REQ R1 unknown inferred src=original_prompt "Add resumable task state so a second agent can continue this work" ev=[prompt:original_prompt#1]`,
		`REQ R2 unknown inferred src=original_prompt "The extractor must never mark a requirement complete without evidence" ev=[prompt:original_prompt#3]`,
		`REQ R3 blocked observed src=original_prompt "Resume must keep working on Windows" ev=[commit:abc1234 test:TestResumeOnWindows(failed) prompt:original_prompt#4]`,
		`REQ R4 partial observed src=original_prompt "Store events without copying raw tool payloads" ev=[file:internal/store/jsonl.go prompt:original_prompt#5]`,
		`REQ R5 partial observed src=original_prompt "Ensure the handoff cites a checkpoint for every claim" ev=[file:internal/handoff/render.go prompt:original_prompt#6]`,
		`REQ R6 partial observed src=original_prompt "We assume the local store is writable, so nothing here needs to be atomic" ev=[file:internal/store/jsonl.go prompt:original_prompt#8]`,
		`DEC D1 "Decided to store events as JSONL instead of SQLite" reason="" session=s1 cp= ev=[event:TurnEnded~s1]`,
		`DEC D2 "Switched to merging semantic output under deterministic facts, which does not work with the old schema" reason="" session=s2 cp= ev=[event:TurnEnded~s2]`,
		`REJ X1 inferred "Reverted the SQLite store because it broke resume on Windows" reason="it broke resume on Windows" ev=[event:ToolUsed~s1]`,
		// Same event as D2: it records both a choice and what that choice broke.
		`REJ X2 inferred "Switched to merging semantic output under deterministic facts, which does not work with the old schema" reason="" ev=[event:TurnEnded~s2]`,
		`ASM inferred "We assume the local store is writable, so nothing here needs to be atomic" ev=[prompt:original_prompt#8]`,
		`ASM inferred "Assuming the adapter always emits SessionEnded" ev=[event:TurnEnded~s1]`,
		`ASM inferred "The reviewer presumably wants the JSONL format kept" ev=[prompt:transcript#1]`,
		`ACT 0 recommended target=ResumeOnWindows "Fix failing test TestResumeOnWindows" ev=[commit:abc1234 test:TestResumeOnWindows(failed)]`,
		`ACT 1 recommended target= "Resolve R1: Add resumable task state so a second agent can continue this work" ev=[prompt:original_prompt#1]`,
		`ACT 2 recommended target= "Resolve R2: The extractor must never mark a requirement complete without evidence" ev=[prompt:original_prompt#3]`,
		`ACT 3 recommended target=ResumeOnWindows "Resolve R3: Resume must keep working on Windows" ev=[commit:abc1234 test:TestResumeOnWindows(failed) prompt:original_prompt#4]`,
		`ACT 4 recommended target=internal/store/jsonl.go "Resolve R4: Store events without copying raw tool payloads" ev=[file:internal/store/jsonl.go prompt:original_prompt#5]`,
		`ACT 5 recommended target=internal/handoff/render.go "Resolve R5: Ensure the handoff cites a checkpoint for every claim" ev=[file:internal/handoff/render.go prompt:original_prompt#6]`,
		`ACT 6 recommended target=internal/store/jsonl.go "Resolve R6: We assume the local store is writable, so nothing here needs to be atomic" ev=[file:internal/store/jsonl.go prompt:original_prompt#8]`,
		`CAP semantic=true missing=[] notes=0`,
	}
	got := dump(extract(t, fixtureInput()))
	for i := 0; i < len(got) || i < len(want); i++ {
		var g, w string
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			t.Errorf("line %d:\n got: %s\nwant: %s", i, g, w)
		}
	}
}
