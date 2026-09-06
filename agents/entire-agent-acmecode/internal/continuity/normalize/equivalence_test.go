package normalize

import (
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// mainSessionEvents is the session that testdata/openclaw_main.ndjson and
// testdata/hermes_main.ndjson both describe: the same six lifecycle points, on
// the same task, at the same instants — recorded by two runtimes that agree on
// nothing about how to write them down.
//
// One expectation serves both fixtures, parameterised only by which runtime
// produced the stream. That is the whole assertion of plan §29 reduced to a
// function signature: after normalization the agent is the only thing left that
// remembers which host was involved.
func mainSessionEvents(t *testing.T, agent model.AgentKind) []model.AgentEvent {
	t.Helper()
	const (
		task    = "task_subscription_pause"
		session = "sess-main"
	)
	return []model.AgentEvent{
		{
			Type: model.SessionStarted, Timestamp: at(t, "2026-03-02T09:00:00Z"),
			TaskID: task, SessionID: session, Agent: agent, Role: model.RoleMain,
			Summary: "Resume work on subscription pause",
			Attrs:   map[string]string{"repo": "acme/billing", "branch": "feature/subscription-pause"},
		},
		{
			Type: model.TurnStarted, Timestamp: at(t, "2026-03-02T09:00:05Z"),
			TaskID: task, SessionID: session, Agent: agent, Role: model.RoleMain,
		},
		{
			Type: model.ToolUsed, Timestamp: at(t, "2026-03-02T09:00:09Z"),
			TaskID: task, SessionID: session, Agent: agent, Role: model.RoleMain,
			PayloadRef: "artifact://runs/42",
			Summary:    "go test ./billing/... failed: TestPauseResumesBilling",
			Attrs:      map[string]string{"tool_name": "run_tests"},
		},
		{
			Type: model.TurnEnded, Timestamp: at(t, "2026-03-02T09:00:20Z"),
			TaskID: task, SessionID: session, Agent: agent, Role: model.RoleMain,
			Summary: "Proposed a proration fix",
		},
		{
			Type: model.CheckpointCreated, Timestamp: at(t, "2026-03-02T09:02:00Z"),
			TaskID: task, SessionID: session, Agent: agent, Role: model.RoleMain,
			PayloadRef: "ck_pre_noon",
			Summary:    "Pre-noon stable checkpoint",
			Attrs:      map[string]string{"checkpoint_id": "ck_pre_noon"},
		},
		{
			Type: model.SessionInterrupted, Timestamp: at(t, "2026-03-02T09:03:30Z"),
			TaskID: task, SessionID: session, Agent: agent, Role: model.RoleMain,
			Summary: "Operator interrupted the session",
		},
	}
}

// TestTwoRuntimesOneEventVocabulary is the reason this package exists.
//
// The two fixtures are structurally equivalent recordings of one session, but
// they share no field names, no lifecycle names, and not even a time encoding:
// OpenClaw writes {"hook":"session_interrupt","ts":"2026-03-02T09:03:30Z"} where
// Hermes writes {"event":"SessionAborted","time":1772442210000}. If the
// continuity abstraction were secretly agent-specific, that difference would
// survive normalization and leak into task state, lineage and handoff.
//
// It does not. After Normalize, the only field that still differs is Agent —
// which must differ, because provenance is evidence and the product refuses to
// forget which runtime made a claim (plan §26, §31).
func TestTwoRuntimesOneEventVocabulary(t *testing.T) {
	fromOpenClaw, err := OpenClaw().Normalize(fixture(t, "openclaw_main.ndjson"))
	if err != nil {
		t.Fatalf("normalize openclaw: %v", err)
	}
	fromHermes, err := Hermes().Normalize(fixture(t, "hermes_main.ndjson"))
	if err != nil {
		t.Fatalf("normalize hermes: %v", err)
	}

	// Each runtime independently matches the shared expectation.
	assertEvents(t, fromOpenClaw, mainSessionEvents(t, model.AgentOpenClaw))
	assertEvents(t, fromHermes, mainSessionEvents(t, model.AgentHermes))

	if len(fromOpenClaw) != len(fromHermes) {
		t.Fatalf("event counts differ: openclaw %d, hermes %d", len(fromOpenClaw), len(fromHermes))
	}

	// And they match each other, compared directly rather than through the
	// literal above, so the test still holds if the fixtures are rewritten.
	for i := range fromOpenClaw {
		oc, hm := fromOpenClaw[i], fromHermes[i]
		if oc.Agent != model.AgentOpenClaw || hm.Agent != model.AgentHermes {
			t.Errorf("event %d: provenance was lost: openclaw=%q hermes=%q", i, oc.Agent, hm.Agent)
		}
		// Project out the one field that is allowed to differ, then require
		// everything else — type, instant, identity, lineage, role, payload
		// reference, summary and attributes — to be identical.
		oc.Agent, hm.Agent = "", ""
		if oc.Key() != hm.Key() {
			t.Errorf("event %d: identities differ across runtimes:\n openclaw: %s\n hermes:   %s", i, oc.Key(), hm.Key())
		}
		if !eventsEqual(oc, hm) {
			t.Errorf("event %d survived normalization with a host-specific difference:\n openclaw: %+v\n hermes:   %+v", i, oc, hm)
		}
	}

	// Stated once more as the vocabulary itself, which is what downstream code
	// actually consumes.
	wantTypes := []model.EventType{
		model.SessionStarted, model.TurnStarted, model.ToolUsed,
		model.TurnEnded, model.CheckpointCreated, model.SessionInterrupted,
	}
	for i, want := range wantTypes {
		if fromOpenClaw[i].Type != want || fromHermes[i].Type != want {
			t.Errorf("event %d: got openclaw=%q hermes=%q, want %q",
				i, fromOpenClaw[i].Type, fromHermes[i].Type, want)
		}
	}
}

// eventsEqual compares two events field by field, treating timestamps with
// time.Time.Equal so that two identical instants carrying different monotonic or
// location metadata still compare equal.
func eventsEqual(a, b model.AgentEvent) bool {
	if a.Type != b.Type || !a.Timestamp.Equal(b.Timestamp) {
		return false
	}
	if a.TaskID != b.TaskID || a.SessionID != b.SessionID || a.ParentSessionID != b.ParentSessionID {
		return false
	}
	if a.Agent != b.Agent || a.Role != b.Role {
		return false
	}
	if a.PayloadRef != b.PayloadRef || a.Summary != b.Summary {
		return false
	}
	if len(a.Attrs) != len(b.Attrs) {
		return false
	}
	for k, v := range a.Attrs {
		if b.Attrs[k] != v {
			return false
		}
	}
	return true
}

// TestCrossAgentLineageSharesOneTask is the §26 case: two runtimes working the
// same task must land on the same TaskID, because that identifier is the only
// thing that lets a Hermes session continue what an OpenClaw session started.
func TestCrossAgentLineageSharesOneTask(t *testing.T) {
	streams := []struct {
		n    Normalizer
		file string
	}{
		{n: OpenClaw(), file: "openclaw_main.ndjson"},
		{n: OpenClaw(), file: "openclaw_subagent.ndjson"},
		{n: Hermes(), file: "hermes_main.ndjson"},
		{n: Hermes(), file: "hermes_child.ndjson"},
	}
	const wantTask = "task_subscription_pause"
	agents := map[model.AgentKind]bool{}
	for _, s := range streams {
		events, err := s.n.Normalize(fixture(t, s.file))
		if err != nil {
			t.Fatalf("normalize %s: %v", s.file, err)
		}
		if len(events) == 0 {
			t.Fatalf("%s produced no events", s.file)
		}
		for i, ev := range events {
			if ev.TaskID != wantTask {
				t.Errorf("%s event %d: TaskID %q, want %q", s.file, i, ev.TaskID, wantTask)
			}
			agents[ev.Agent] = true
		}
	}
	if len(agents) != 2 {
		t.Errorf("expected events from both runtimes on one task, got %d distinct agents", len(agents))
	}
}
