package normalize

import (
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// TestOpenClawNormalizeMainSession asserts the whole event slice, not a few
// fields: a silently added attribute or a summary that survived redaction is
// exactly the kind of regression this package exists to prevent.
//
// The expectation lives in equivalence_test.go because the Hermes fixture
// describes the same session and shares it — one expectation, two runtimes,
// which is the claim of plan §29.
func TestOpenClawNormalizeMainSession(t *testing.T) {
	got, err := OpenClaw().Normalize(fixture(t, "openclaw_main.ndjson"))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertEvents(t, got, mainSessionEvents(t, model.AgentOpenClaw))
}

// TestOpenClawSubagentLineageAndRedaction covers the sub-agent path, where the
// envelope describes the child session and points at its parent (plan §8), and
// the redaction path, where a shell command that carried a live credential
// reaches us in a summary.
func TestOpenClawSubagentLineageAndRedaction(t *testing.T) {
	got, err := OpenClaw().Normalize(fixture(t, "openclaw_subagent.ndjson"))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	const (
		task   = "task_subscription_pause"
		child  = "sess-test"
		parent = "sess-main"
	)
	want := []model.AgentEvent{
		{
			Type: model.SubagentStarted, Timestamp: at(t, "2026-03-02T09:01:00Z"),
			TaskID: task, SessionID: child, ParentSessionID: parent,
			Agent: model.AgentOpenClaw, Role: model.RoleTest,
			Summary: "Delegated billing test verification",
		},
		{
			Type: model.ToolUsed, Timestamp: at(t, "2026-03-02T09:01:10Z"),
			TaskID: task, SessionID: child, ParentSessionID: parent,
			Agent: model.AgentOpenClaw, Role: model.RoleTest,
			PayloadRef: "artifact://runs/43",
			// The secret is gone; the surrounding evidence (which command, what
			// exit status) survives, which is what makes the redaction usable
			// rather than merely safe.
			Summary: "ran with AWS_SECRET_ACCESS_KEY=[REDACTED], exit 1",
			Attrs:   map[string]string{"tool_name": "shell"},
		},
		{
			Type: model.SubagentEnded, Timestamp: at(t, "2026-03-02T09:01:40Z"),
			TaskID: task, SessionID: child, ParentSessionID: parent,
			Agent: model.AgentOpenClaw, Role: model.RoleTest,
			Summary: "2 of 3 billing tests still fail",
			Attrs: map[string]string{
				"exit_code": "1",
				// The key names a credential, so the value goes regardless of
				// what it looks like.
				"github_token": "[REDACTED]",
				// "diff" was a nested object in meta and was dropped: a payload
				// is referenced, never copied (plan §34).
			},
		},
	}
	assertEvents(t, got, want)
}

// TestOpenClawSkipsUnknownHooks: OpenClaw exposes lifecycle points we do not
// model (context compaction, plan mode, permission prompts). Skipping them is
// the honest behaviour; mapping them onto the nearest-looking event type would
// put semantics into the lineage that the host never asserted (plan §9).
func TestOpenClawSkipsUnknownHooks(t *testing.T) {
	got, err := OpenClaw().Normalize(fixture(t, "unknown_events.ndjson"))
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
	// Order is preserved across the skipped records, so the surviving events
	// still bracket the session correctly.
	if !got[0].Timestamp.Before(got[1].Timestamp) {
		t.Errorf("skipping records reordered the stream: %s", render(got))
	}
}

func TestOpenClawErrors(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantErrs []string
	}{
		{
			name:     "missing task id cannot be guessed",
			raw:      `{"hook":"session_start","ts":"2026-03-02T09:00:00Z","session":{"id":"s1","role":"main"}}`,
			wantErrs: []string{"record 0", "session_start", "task_id is required"},
		},
		{
			name:     "missing session id",
			raw:      `{"hook":"turn_start","ts":"2026-03-02T09:00:00Z","task":{"id":"t1"}}`,
			wantErrs: []string{"record 0", "turn_start", "session_id is required"},
		},
		{
			name: "a session that is its own parent is a broken shim, not a lineage",
			raw: `{"hook":"subagent_start","ts":"2026-03-02T09:00:00Z","session":{"id":"s1","parent":"s1"},` +
				`"task":{"id":"t1"}}`,
			wantErrs: []string{"record 0", "cannot be its own parent"},
		},
		{
			name:     "missing timestamp",
			raw:      `{"hook":"turn_start","session":{"id":"s1"},"task":{"id":"t1"}}`,
			wantErrs: []string{"record 0", "missing ts"},
		},
		{
			name:     "timestamp that is not RFC3339",
			raw:      `{"hook":"turn_start","ts":"2 March 2026","session":{"id":"s1"},"task":{"id":"t1"}}`,
			wantErrs: []string{"record 0", "not RFC3339"},
		},
		{
			name: "the failing record is named by its index",
			raw: `{"hook":"session_start","ts":"2026-03-02T09:00:00Z","session":{"id":"s1"},"task":{"id":"t1"}}` + "\n" +
				`{"hook":"turn_start","ts":"2026-03-02T09:00:01Z","session":{"id":"s1"},"task":{"id":"t1"}}` + "\n" +
				`{"hook":"turn_end","ts":"nope","session":{"id":"s1"},"task":{"id":"t1"}}`,
			wantErrs: []string{"record 2", "turn_end"},
		},
		{
			name:     "a record that is not an envelope is corrupt input, not an unmapped hook",
			raw:      `{"hook":["session_start"],"ts":"2026-03-02T09:00:00Z"}`,
			wantErrs: []string{"record 0", "cannot unmarshal"},
		},
		{
			name:     "every record unrecognised",
			raw:      `{"hook":"memory_compacted","ts":"2026-03-02T09:00:00Z"}`,
			wantErrs: []string{"none of the 1 records", "memory_compacted"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := OpenClaw().Normalize([]byte(tc.raw))
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

// TestOpenClawCheckpointIDFallback documents the two places a checkpoint id can
// come from and which one wins. An explicit meta entry is the host naming the
// checkpoint; the ref is our reading of a generic field, so it only applies when
// the host said nothing.
func TestOpenClawCheckpointIDFallback(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "explicit meta wins",
			raw: `{"hook":"checkpoint","ts":"2026-03-02T09:00:00Z","session":{"id":"s1"},"task":{"id":"t1"},` +
				`"tool":{"ref":"artifact://ck/9"},"meta":{"checkpoint_id":"ck_pre_noon"}}`,
			want: map[string]string{"checkpoint_id": "ck_pre_noon"},
		},
		{
			name: "ref is used when the host did not name the checkpoint",
			raw: `{"hook":"checkpoint","ts":"2026-03-02T09:00:00Z","session":{"id":"s1"},"task":{"id":"t1"},` +
				`"tool":{"ref":"ck_from_ref"}}`,
			want: map[string]string{"checkpoint_id": "ck_from_ref"},
		},
		{
			name: "no id anywhere leaves the attribute absent rather than empty",
			raw:  `{"hook":"checkpoint","ts":"2026-03-02T09:00:00Z","session":{"id":"s1"},"task":{"id":"t1"}}`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OpenClaw().Normalize([]byte(tc.raw))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			if !reflect.DeepEqual(got[0].Attrs, tc.want) {
				t.Errorf("attrs: got %#v, want %#v", got[0].Attrs, tc.want)
			}
		})
	}
}

// TestOpenClawUnknownRoleIsPreservedNotInvented: the host called the session
// something our domain does not define. We neither drop what it said nor promote
// it to a model.Role.
func TestOpenClawUnknownRoleIsPreservedNotInvented(t *testing.T) {
	raw := `{"hook":"session_start","ts":"2026-03-02T09:00:00Z","session":{"id":"s1","role":"orchestrator"},` +
		`"task":{"id":"t1"}}`
	got, err := OpenClaw().Normalize([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got[0].Role != "" {
		t.Errorf("Role: got %q, want empty; an undefined role must not be promoted", got[0].Role)
	}
	if v := got[0].Attr(attrRawRole); v != "orchestrator" {
		t.Errorf("raw_role: got %q, want %q", v, "orchestrator")
	}
}

// TestOpenClawNeverInlinesToolPayloads is the §34 contract at the adapter
// boundary: whatever the shim put in the envelope, an event keeps only the tool
// name and a reference.
func TestOpenClawNeverInlinesToolPayloads(t *testing.T) {
	raw := `{"hook":"tool_use","ts":"2026-03-02T09:00:00Z","session":{"id":"s1"},"task":{"id":"t1"},` +
		`"tool":{"name":"shell","ref":"artifact://runs/9"},` +
		`"meta":{"stdout":{"lines":["AKIAIOSFODNN7EXAMPLE","done"]},"argv":["aws","s3","ls"]}}`
	got, err := OpenClaw().Normalize([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := map[string]string{"tool_name": "shell"}
	if !reflect.DeepEqual(got[0].Attrs, want) {
		t.Errorf("attrs: got %#v, want %#v (payload structures must not be copied into state)", got[0].Attrs, want)
	}
	if got[0].PayloadRef != "artifact://runs/9" {
		t.Errorf("PayloadRef: got %q, want the artifact reference", got[0].PayloadRef)
	}
}
