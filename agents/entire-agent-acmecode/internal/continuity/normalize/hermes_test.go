package normalize

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func TestHermesNormalizeMainSession(t *testing.T) {
	got, err := Hermes().Normalize(fixture(t, "hermes_main.ndjson"))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertEvents(t, got, mainSessionEvents(t, model.AgentHermes))
}

// TestHermesChildAgent covers the three things the Hermes child stream exercises
// that the main stream does not: a role the domain does not define, a tool name
// that has to be dug out of the free-form fields bag, and a note that arrived
// carrying a live bearer token.
func TestHermesChildAgent(t *testing.T) {
	got, err := Hermes().Normalize(fixture(t, "hermes_child.ndjson"))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	const (
		task   = "task_subscription_pause"
		child  = "hermes-child-7"
		parent = "hermes-main"
	)
	want := []model.AgentEvent{
		{
			Type: model.SessionStarted, Timestamp: at(t, "2026-03-02T09:01:00Z"),
			TaskID: task, SessionID: child, ParentSessionID: parent,
			Agent: model.AgentHermes,
			// "verifier" is not a model.Role, so Role stays empty and the host's
			// own word is preserved verbatim instead.
			Summary: "Verification sub-agent started",
			Attrs:   map[string]string{"raw_role": "verifier"},
		},
		{
			Type: model.ToolUsed, Timestamp: at(t, "2026-03-02T09:01:10Z"),
			TaskID: task, SessionID: child, ParentSessionID: parent,
			Agent:      model.AgentHermes,
			PayloadRef: "artifact://hermes/runs/7",
			// The header value is gone; the command, the endpoint and therefore
			// the evidence of what was verified all survive.
			Summary: `curl -H "Authorization: [REDACTED]" https://api.example.com/v1/pause`,
			Attrs: map[string]string{
				"raw_role": "verifier",
				"status":   "200",
				// The host's own field is kept as it was sent, and the normalized
				// tool_name is added alongside it. Rewriting the host's key would
				// discard what it actually said.
				"tool":      "http_probe",
				"tool_name": "http_probe",
			},
		},
		{
			Type: model.SubagentEnded, Timestamp: at(t, "2026-03-02T09:01:40Z"),
			TaskID: task, SessionID: child, ParentSessionID: parent,
			Agent:   model.AgentHermes,
			Summary: "pause endpoint returns 200 for an active subscription",
			Attrs: map[string]string{
				"raw_role":  "verifier",
				"exit_code": "0",
				"api_key":   "[REDACTED]",
			},
		},
	}
	assertEvents(t, got, want)
}

// TestHermesSkipsUnknownEvents: the Hermes plugin surface is larger than the
// part we map, and a plausible-sounding name is the most dangerous kind of
// unknown. "ChildAgentStarted" does not exist in Hermes; if it ever appears in a
// stream it must be skipped rather than trusted, because a SubagentStarted the
// host never emitted would put a fabricated spawn edge into the lineage.
func TestHermesSkipsUnknownEvents(t *testing.T) {
	raw := strings.Join([]string{
		`{"event":"SessionStart","time":1772442000000,"agent_session":"s1","entire_task":"t1"}`,
		`{"event":"ContextInjected","time":1772442001000,"agent_session":"s1","entire_task":"t1"}`,
		`{"event":"ChildAgentStarted","time":1772442002000,"agent_session":"s2","delegated_from":"s1","entire_task":"t1"}`,
		`{"event":"TokenBudgetWarning","time":1772442003000,"agent_session":"s1","entire_task":"t1"}`,
		`{"event":"SessionEnd","time":1772442004000,"agent_session":"s1","entire_task":"t1"}`,
	}, "\n")

	got, err := Hermes().Normalize([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []model.EventType{model.SessionStarted, model.SessionEnded}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %s", len(got), len(want), render(got))
	}
	for i := range want {
		if got[i].Type != want[i] {
			t.Errorf("event %d: got %q, want %q", i, got[i].Type, want[i])
		}
	}
	for _, ev := range got {
		if ev.Type == model.SubagentStarted {
			t.Error("a SubagentStarted was synthesised for Hermes, which exposes no such hook")
		}
	}
}

func TestHermesErrors(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantErrs []string
	}{
		{
			name:     "missing time",
			raw:      `{"event":"PreLLM","agent_session":"s1","entire_task":"t1"}`,
			wantErrs: []string{"record 0", "PreLLM", "missing time"},
		},
		{
			name: "epoch zero is a missing stamp, not an instant in 1970",
			raw:  `{"event":"PreLLM","time":0,"agent_session":"s1","entire_task":"t1"}`,
			// Validate would accept 1970-01-01; it is this package's job to
			// refuse it, because an invented ordering corrupts every downstream
			// conclusion drawn from event order.
			wantErrs: []string{"record 0", "not a real instant"},
		},
		{
			name:     "negative time",
			raw:      `{"event":"PreLLM","time":-5,"agent_session":"s1","entire_task":"t1"}`,
			wantErrs: []string{"not a real instant"},
		},
		{
			name:     "fractional millis are not epoch millis",
			raw:      `{"event":"PreLLM","time":1772442005000.5,"agent_session":"s1","entire_task":"t1"}`,
			wantErrs: []string{"record 0", "not epoch milliseconds"},
		},
		{
			name:     "unparseable time",
			raw:      `{"event":"PreLLM","time":"yesterday","agent_session":"s1","entire_task":"t1"}`,
			wantErrs: []string{"record 0"},
		},
		{
			name:     "missing task id",
			raw:      `{"event":"SessionStart","time":1772442000000,"agent_session":"s1"}`,
			wantErrs: []string{"record 0", "SessionStart", "task_id is required"},
		},
		{
			name:     "missing session id",
			raw:      `{"event":"SessionStart","time":1772442000000,"entire_task":"t1"}`,
			wantErrs: []string{"session_id is required"},
		},
		{
			name: "a child delegated from itself",
			raw: `{"event":"ChildAgentFinished","time":1772442000000,"agent_session":"s1",` +
				`"delegated_from":"s1","entire_task":"t1"}`,
			wantErrs: []string{"cannot be its own parent"},
		},
		{
			name:     "record that is not an envelope",
			raw:      `{"event":{"name":"SessionStart"},"time":1772442000000}`,
			wantErrs: []string{"record 0", "cannot unmarshal"},
		},
		{
			name: "nothing recognised at all",
			raw: `{"event":"ContextInjected","time":1772442000000}` + "\n" +
				`{"event":"TokenBudgetWarning","time":1772442001000}`,
			wantErrs: []string{"none of the 2 records", "TokenBudgetWarning"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := Hermes().Normalize([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected an error, got %d events", len(events))
			}
			if events != nil {
				t.Errorf("expected no events alongside the error, got %d", len(events))
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestHermesAcceptsQuotedMillis: some shims quote large integers to keep them out
// of a JSON float. That is a serialization detail, not a different claim about
// when the event happened, so it must normalize to the same instant.
func TestHermesAcceptsQuotedMillis(t *testing.T) {
	raw := `{"event":"PreLLM","time":"1772442005000","agent_session":"s1","entire_task":"t1"}`
	got, err := Hermes().Normalize([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if want := at(t, "2026-03-02T09:00:05Z"); !got[0].Timestamp.Equal(want) {
		t.Errorf("timestamp: got %s, want %s", got[0].Timestamp, want)
	}
	// time.Time.Location never returns nil — it reports UTC for a zero location —
	// so this is asserted directly rather than guarded by a condition that is
	// always true and would have hidden the check.
	if loc := got[0].Timestamp.Location(); loc != time.UTC {
		t.Errorf("timestamp was not normalized to UTC: %s (%v)", got[0].Timestamp, loc)
	}
}

// TestHermesToolName documents where a tool name may come from, and what happens
// when it is absent: the event is still emitted, without a fabricated label.
func TestHermesToolName(t *testing.T) {
	tests := []struct {
		name   string
		fields string
		want   string
	}{
		{name: "explicit tool_name", fields: `{"tool_name":"http_probe"}`, want: "http_probe"},
		{name: "tool is the fallback", fields: `{"tool":"shell"}`, want: "shell"},
		{name: "tool_name wins", fields: `{"tool":"shell","tool_name":"http_probe"}`, want: "http_probe"},
		{name: "neither is present", fields: `{"status":200}`, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"event":"ToolInvoked","time":1772442000000,"agent_session":"s1","entire_task":"t1",` +
				`"payload_ref":"artifact://runs/1","fields":` + tc.fields + `}`
			got, err := Hermes().Normalize([]byte(raw))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if v := got[0].Attr(attrToolName); v != tc.want {
				t.Errorf("tool_name: got %q, want %q", v, tc.want)
			}
			if got[0].PayloadRef != "artifact://runs/1" {
				t.Errorf("PayloadRef: got %q, want the artifact reference", got[0].PayloadRef)
			}
		})
	}
}

// TestHermesCheckpointID mirrors the OpenClaw fallback so that both runtimes put
// the checkpoint id in the same place. Downstream code reads checkpoint lineage
// without knowing which agent wrote it, which only works if this holds.
func TestHermesCheckpointID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "explicit field wins over the payload ref",
			raw: `{"event":"CheckpointWritten","time":1772442000000,"agent_session":"s1","entire_task":"t1",` +
				`"payload_ref":"artifact://ck/9","fields":{"checkpoint_id":"ck_pre_noon"}}`,
			want: map[string]string{"checkpoint_id": "ck_pre_noon"},
		},
		{
			name: "payload ref is the checkpoint when no field names it",
			raw: `{"event":"CheckpointWritten","time":1772442000000,"agent_session":"s1","entire_task":"t1",` +
				`"payload_ref":"ck_pre_noon"}`,
			want: map[string]string{"checkpoint_id": "ck_pre_noon"},
		},
		{
			name: "no id anywhere leaves the attribute absent",
			raw:  `{"event":"CheckpointWritten","time":1772442000000,"agent_session":"s1","entire_task":"t1"}`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Hermes().Normalize([]byte(tc.raw))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if !reflect.DeepEqual(got[0].Attrs, tc.want) {
				t.Errorf("attrs: got %#v, want %#v", got[0].Attrs, tc.want)
			}
		})
	}
}

// TestHermesNeverInlinesPayloads is the §34 contract on the Hermes side: a
// fields bag carrying structured tool output is reduced to scalars, and the
// structure stays behind the reference.
func TestHermesNeverInlinesPayloads(t *testing.T) {
	raw := `{"event":"ToolInvoked","time":1772442000000,"agent_session":"s1","entire_task":"t1",` +
		`"payload_ref":"artifact://runs/1","note":"probe finished",` +
		`"fields":{"tool_name":"http_probe","response":{"headers":{"authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.abc"}},` +
		`"status":200}}`
	got, err := Hermes().Normalize([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := map[string]string{"tool_name": "http_probe", "status": "200"}
	if !reflect.DeepEqual(got[0].Attrs, want) {
		t.Errorf("attrs: got %#v, want %#v", got[0].Attrs, want)
	}
	for k, v := range got[0].Attrs {
		if strings.Contains(v, "eyJhbGciOiJIUzI1NiJ9") {
			t.Errorf("attribute %q leaked a token: %q", k, v)
		}
	}
}
