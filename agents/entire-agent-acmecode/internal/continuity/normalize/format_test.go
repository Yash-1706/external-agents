package normalize

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// The Track 3 curveball requires four cases to be covered: the original format,
// the new format, unknown events, and incomplete input. Each has a test below,
// plus the two that actually decide whether the design is right — a stream
// carrying both formats at once, and proof that the original format decodes
// identically to how it did before the format dimension existed.

const demoSession = "btw-track3-demo-001"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

func eventsOfType(evs []model.AgentEvent, typ model.EventType) []model.AgentEvent {
	var out []model.AgentEvent
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------- new format

func TestLifecycleV2Fixture(t *testing.T) {
	res, err := Stream(readFixture(t, "lifecycle_v2_acmecode.jsonl"), Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if res.Report.Total != 17 {
		t.Errorf("Total = %d, want 17", res.Report.Total)
	}
	// Every record is used except the usage line, which is recognised and
	// deliberately not modelled.
	if res.Report.Accepted != 16 {
		t.Errorf("Accepted = %d, want 16", res.Report.Accepted)
	}
	if res.Report.Ignored != 1 {
		t.Errorf("Ignored = %d, want 1 (the usage record)", res.Report.Ignored)
	}
	if res.Report.Unknown != 0 || res.Report.Malformed != 0 {
		t.Errorf("a complete fixture reported gaps: %+v", res.Report)
	}
	if !res.Report.Complete() {
		t.Errorf("Complete() = false for a fully understood stream:\n%s", res.Report.Summary())
	}
	if want := []string{string(FormatLifecycleV2)}; !reflect.DeepEqual(res.Report.Formats, want) {
		t.Errorf("Formats = %v, want %v", res.Report.Formats, want)
	}

	// The format names its own runtime, and that name must survive rather than
	// being flattened to "unknown".
	for _, e := range res.Events {
		if e.Agent != model.AgentKind("acmecode") {
			t.Fatalf("event %s carries agent %q, want %q", e.Type, e.Agent, "acmecode")
		}
	}

	// The format carries no task id at all, so the anchor must be derived — and
	// derived identically for every record of the session.
	wantTask := model.NewTaskID(
		"github.com/example/checkout-service", "feature/add-coupon-validation", demoSession)
	for _, e := range res.Events {
		if e.TaskID != wantTask {
			t.Fatalf("event %s anchored to %q, want %q", e.Type, e.TaskID, wantTask)
		}
	}

	// Lifecycle mapping.
	for _, tc := range []struct {
		typ  model.EventType
		want int
	}{
		{model.SessionStarted, 1},
		{model.SessionEnded, 1},
		{model.TurnStarted, 1},
		{model.TurnEnded, 2},
		{model.CheckpointCreated, 1},
		{model.ToolUsed, 10},
	} {
		if got := len(eventsOfType(res.Events, tc.typ)); got != tc.want {
			t.Errorf("%s count = %d, want %d", tc.typ, got, tc.want)
		}
	}

	// The opening prompt is the original intent, which nothing downstream can
	// reconstruct if it is lost here.
	prompt := eventsOfType(res.Events, model.TurnStarted)[0]
	if !strings.Contains(prompt.Summary, "Add coupon validation to checkout") {
		t.Errorf("user_prompt summary lost the intent: %q", prompt.Summary)
	}

	cp := eventsOfType(res.Events, model.CheckpointCreated)[0]
	if cp.Attr("checkpoint_id") != "cp-001" {
		t.Errorf("checkpoint_id = %q, want cp-001", cp.Attr("checkpoint_id"))
	}
	if cp.Attr("commit_sha") != "8d34f70c1e9fd62c1b5dc4fbbbf5013db2817ae1" {
		t.Errorf("commit_sha = %q", cp.Attr("commit_sha"))
	}
	if !strings.Contains(cp.Attr("open_questions"), "precedence") {
		t.Errorf("open questions were dropped: %q", cp.Attr("open_questions"))
	}
}

// TestLifecycleV2CorrelatesToolResults is the load-bearing one: the format
// splits a command across a tool_call and a tool_result correlated by call_id,
// so a test outcome is only knowable by remembering the call. Getting this
// wrong would mean a handoff that cannot say whether the tests pass.
func TestLifecycleV2CorrelatesToolResults(t *testing.T) {
	res, err := Stream(readFixture(t, "lifecycle_v2_acmecode.jsonl"), Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var results []model.AgentEvent
	for _, e := range res.Events {
		if e.Attr("test_status") != "" {
			results = append(results, e)
		}
	}
	if len(results) != 2 {
		t.Fatalf("found %d structured test results, want 2", len(results))
	}

	for i, want := range []model.TestStatus{model.TestFailed, model.TestPassed} {
		got := results[i]
		if got.Attr("test_status") != string(want) {
			t.Errorf("result %d status = %q, want %q", i, got.Attr("test_status"), want)
		}
		if got.Attr("test_name") != "apply_coupon.test.ts" {
			t.Errorf("result %d name = %q, want apply_coupon.test.ts", i, got.Attr("test_name"))
		}
		if got.Attr("test_command") != "npm test -- apply_coupon.test.ts" {
			t.Errorf("result %d command = %q", i, got.Attr("test_command"))
		}
	}

	// Both results must carry the SAME test name, otherwise the merge rule that
	// lets a later pass resolve an earlier failure can never match them up.
	if results[0].Attr("test_name") != results[1].Attr("test_name") {
		t.Error("the failing and passing runs of one test got different names")
	}
	if !results[1].Timestamp.After(results[0].Timestamp) {
		t.Error("the passing result is not ordered after the failing one")
	}
}

// ----------------------------------------------------------- original format

// TestOriginalFormatsStillDecode is the regression guarantee. The v1 fixtures
// must decode through the new format-aware path exactly as they did through the
// runtime-keyed path, without the caller naming a format.
func TestOriginalFormatsStillDecode(t *testing.T) {
	tests := []struct {
		fixture string
		agent   model.AgentKind
		format  Format
	}{
		{"openclaw_main.ndjson", model.AgentOpenClaw, FormatOpenClawV1},
		{"openclaw_subagent.ndjson", model.AgentOpenClaw, FormatOpenClawV1},
		{"hermes_main.ndjson", model.AgentHermes, FormatHermesV1},
		{"hermes_child.ndjson", model.AgentHermes, FormatHermesV1},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			raw := readFixture(t, tt.fixture)

			// The pre-curveball path, untouched.
			norm, err := For(tt.agent)
			if err != nil {
				t.Fatalf("For(%s): %v", tt.agent, err)
			}
			legacy, err := norm.Normalize(raw)
			if err != nil {
				t.Fatalf("legacy Normalize: %v", err)
			}

			// The new path, with no format named: detection must find it.
			res, err := Stream(raw, Options{})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if !reflect.DeepEqual(res.Events, legacy) {
				t.Fatalf("Stream produced different events than the original path\nstream: %+v\nlegacy: %+v", res.Events, legacy)
			}
			if want := []string{string(tt.format)}; !reflect.DeepEqual(res.Report.Formats, want) {
				t.Errorf("detected %v, want %v", res.Report.Formats, want)
			}
			if !res.Report.Complete() {
				t.Errorf("a known-good v1 fixture reported gaps:\n%s", res.Report.Summary())
			}
		})
	}
}

// ---------------------------------------------------------- mixed transcripts

// TestMixedFormatStream is the case a partially-migrated fleet actually
// produces: some shims upgraded, some not, appending to one log. A file-level
// format guess would misread half of it.
func TestMixedFormatStream(t *testing.T) {
	mixed := strings.Join([]string{
		strings.TrimSpace(string(readFixture(t, "openclaw_main.ndjson"))),
		strings.TrimSpace(string(readFixture(t, "lifecycle_v2_acmecode.jsonl"))),
	}, "\n")

	res, err := Stream([]byte(mixed), Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	want := []string{string(FormatLifecycleV2), string(FormatOpenClawV1)}
	if !reflect.DeepEqual(res.Report.Formats, want) {
		t.Fatalf("Formats = %v, want %v", res.Report.Formats, want)
	}

	var sawOpenClaw, sawAcme bool
	for _, e := range res.Events {
		switch e.Agent {
		case model.AgentOpenClaw:
			sawOpenClaw = true
		case model.AgentKind("acmecode"):
			sawAcme = true
		}
	}
	if !sawOpenClaw || !sawAcme {
		t.Errorf("mixed stream lost a runtime: openclaw=%v acmecode=%v", sawOpenClaw, sawAcme)
	}
	if res.Report.Malformed != 0 {
		t.Errorf("mixed stream reported %d unreadable records:\n%s", res.Report.Malformed, res.Report.Summary())
	}
}

// ------------------------------------------------------------ unknown events

// TestUnknownEventsAreRetainedNotDropped covers the curveball requirement that
// unknown events must not crash the integration, and the stronger property this
// build commits to: they must not vanish either. A timeline missing records it
// could not read still renders as a complete timeline, and the reader has no
// way to tell.
func TestUnknownEventsAreRetainedNotDropped(t *testing.T) {
	stream := strings.Join([]string{
		`{"timestamp":"2026-09-06T09:00:00Z","event":"session_started","session_id":"s1","agent":{"name":"AcmeCode"},"repository":"github.com/example/svc","branch":"main"}`,
		`{"timestamp":"2026-09-06T09:00:05Z","event":"quantum_entanglement_started","session_id":"s1","weirdness":9}`,
		`{"timestamp":"2026-09-06T09:00:09Z","event":"telemetry_flushed","session_id":"s1"}`,
		`{"timestamp":"2026-09-06T09:00:10Z","event":"session_ended","session_id":"s1","status":"completed"}`,
	}, "\n")

	res, err := Stream([]byte(stream), Options{})
	if err != nil {
		t.Fatalf("an unknown event took the whole stream down: %v", err)
	}
	if res.Report.Unknown != 2 {
		t.Errorf("Unknown = %d, want 2", res.Report.Unknown)
	}
	if res.Report.Complete() {
		t.Error("a stream containing unmapped lifecycle names reported itself as complete")
	}

	unknown := eventsOfType(res.Events, model.EventUnknown)
	if len(unknown) != 2 {
		t.Fatalf("retained %d unknown events, want 2", len(unknown))
	}
	// The host's own name for the record is preserved, so a reader can see what
	// arrived without this build having invented a meaning for it.
	kinds := []string{unknown[0].Attr(model.AttrRawKind), unknown[1].Attr(model.AttrRawKind)}
	for _, want := range []string{"quantum_entanglement_started", "telemetry_flushed"} {
		if !contains(kinds, want) {
			t.Errorf("raw kind %q was not preserved, got %v", want, kinds)
		}
	}
	// They are still fully-formed domain events: anchored, ordered and storable.
	for _, e := range unknown {
		if err := e.Validate(); err != nil {
			t.Errorf("retained unknown event is not storable: %v", err)
		}
		if e.SessionID != "s1" || e.TaskID == "" {
			t.Errorf("retained unknown event lost its anchor: %+v", e)
		}
	}
	if !strings.Contains(res.Report.Summary(), "quantum_entanglement_started") {
		t.Errorf("the report does not name what it could not read:\n%s", res.Report.Summary())
	}
}

// TestUnknownEventsInOriginalFormat proves the tolerance is a property of the
// engine, not of one format.
func TestUnknownEventsInOriginalFormat(t *testing.T) {
	res, err := Stream(readFixture(t, "unknown_events.ndjson"), Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Report.Unknown == 0 {
		t.Fatalf("no unknown records reported for a fixture built of them:\n%s", res.Report.Summary())
	}
	if res.Report.Complete() {
		t.Error("a stream of unmapped names reported itself as complete")
	}
}

// ---------------------------------------------------------- incomplete input

// TestIncompleteTranscriptProducesPartialResult covers the requirement that an
// incomplete transcript must produce a partial result rather than a corrupted
// or discarded session. The realistic cause is an agent killed mid-write, which
// leaves the final JSONL line truncated — exactly the situation this product
// exists to recover from.
func TestIncompleteTranscriptProducesPartialResult(t *testing.T) {
	full := strings.TrimSpace(string(readFixture(t, "lifecycle_v2_acmecode.jsonl")))
	lines := strings.Split(full, "\n")

	// Keep the first eight records intact and truncate the ninth mid-object.
	truncated := strings.Join(lines[:8], "\n") + "\n" + lines[8][:len(lines[8])/2]

	res, err := Stream([]byte(truncated), Options{})
	if err != nil {
		t.Fatalf("a truncated transcript was discarded instead of partially read: %v", err)
	}
	if len(res.Events) == 0 {
		t.Fatal("a truncated transcript produced no events at all")
	}
	if res.Report.Malformed == 0 {
		t.Error("the truncated line was not reported as unreadable")
	}
	if res.Report.Complete() {
		t.Error("a truncated transcript reported itself as complete")
	}
	if !strings.Contains(res.Report.Summary(), "partial") {
		t.Errorf("the report does not tell the reader the timeline is partial:\n%s", res.Report.Summary())
	}

	// The surviving records must be intact and correctly anchored: a partial
	// read must not mean a corrupted one.
	wantTask := model.NewTaskID(
		"github.com/example/checkout-service", "feature/add-coupon-validation", demoSession)
	for _, e := range res.Events {
		if e.TaskID != wantTask {
			t.Errorf("surviving event %s lost its anchor: %q", e.Type, e.TaskID)
		}
		if err := e.Validate(); err != nil {
			t.Errorf("surviving event is malformed: %v", err)
		}
	}
	if len(eventsOfType(res.Events, model.SessionStarted)) != 1 {
		t.Error("the session header did not survive truncation")
	}
}

// TestHeadlessTranscriptStillAnchors covers the other kind of incomplete input:
// a transcript that begins mid-session, so the session_started header carrying
// the repository is missing entirely.
func TestHeadlessTranscriptStillAnchors(t *testing.T) {
	full := strings.TrimSpace(string(readFixture(t, "lifecycle_v2_acmecode.jsonl")))
	headless := strings.Join(strings.Split(full, "\n")[1:], "\n")

	// With no header and no caller fallback there is nothing to anchor to, and
	// saying so is better than inventing a task.
	bare, err := Stream([]byte(headless), Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if bare.Report.Accepted != 0 {
		t.Errorf("events were anchored with no repository available: %d accepted", bare.Report.Accepted)
	}
	if bare.Report.Malformed == 0 {
		t.Error("unanchorable records were not reported")
	}

	// Given the repository the caller is ingesting into, the same transcript
	// becomes usable — which is the difference between a partial result and a
	// discarded session.
	withRepo, err := Stream([]byte(headless), Options{
		Repo:   "github.com/example/checkout-service",
		Branch: "feature/add-coupon-validation",
	})
	if err != nil {
		t.Fatalf("Stream with fallback: %v", err)
	}
	if withRepo.Report.Accepted == 0 {
		t.Fatalf("a headless transcript stayed unusable with a repository supplied:\n%s", withRepo.Report.Summary())
	}
	wantTask := model.NewTaskID(
		"github.com/example/checkout-service", "feature/add-coupon-validation", demoSession)
	for _, e := range withRepo.Events {
		if e.TaskID != wantTask {
			t.Errorf("event %s anchored to %q, want the same task as the full transcript", e.Type, e.TaskID)
		}
	}
}

// ------------------------------------------------------------------- general

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name   string
		record string
		want   Format
		ok     bool
	}{
		{"openclaw", `{"hook":"session_start","ts":"2026-09-06T09:00:00Z"}`, FormatOpenClawV1, true},
		{"hermes", `{"event":"SessionStart","time":1757142000000,"agent_session":"s1"}`, FormatHermesV1, true},
		{"lifecycle v2", `{"event":"session_started","timestamp":"2026-09-06T09:00:00Z","session_id":"s1"}`, FormatLifecycleV2, true},
		{"not a record", `{"hello":"world"}`, "", false},
		{"not json", `{`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DetectFormat([]byte(tt.record))
			if ok != tt.ok || got != tt.want {
				t.Errorf("DetectFormat = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestStreamIsDeterministic(t *testing.T) {
	raw := readFixture(t, "lifecycle_v2_acmecode.jsonl")
	first, err := Stream(raw, Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := Stream(raw, Options{})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("Stream is not deterministic on run %d", i)
		}
	}
}

// TestForcedFormatOverridesDetection covers the escape hatch: a caller who
// knows the format must be able to say so, for a stream detection would guess
// wrong on.
func TestForcedFormatOverridesDetection(t *testing.T) {
	raw := readFixture(t, "lifecycle_v2_acmecode.jsonl")
	res, err := Stream(raw, Options{Format: FormatHermesV1})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Report.Accepted != 0 {
		t.Errorf("forcing the wrong format still accepted %d events", res.Report.Accepted)
	}
	if res.Report.Complete() {
		t.Error("forcing the wrong format reported a complete read")
	}
}

// TestExplicitTaskIDWins covers ingestion into a task the caller already knows,
// which must override anything derivable from the stream.
func TestExplicitTaskIDWins(t *testing.T) {
	res, err := Stream(readFixture(t, "lifecycle_v2_acmecode.jsonl"), Options{TaskID: "task_chosen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, e := range res.Events {
		if e.TaskID != "task_chosen" {
			t.Fatalf("event %s anchored to %q, want the caller's task", e.Type, e.TaskID)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestNewDecoderIsExhaustiveOverFormat pins the closed-set property Entire
// Graph flagged: every Format variant must have an arm, and the one variant
// that is valid-but-not-decodable must say so rather than being reported as
// unknown. A new format added without an arm fails here instead of falling out
// of the switch at runtime.
func TestNewDecoderIsExhaustiveOverFormat(t *testing.T) {
	for _, f := range Formats() {
		d, err := newDecoder(f)
		if err != nil {
			t.Errorf("newDecoder(%q) = error %v, want a decoder", f, err)
			continue
		}
		if d.format() != f {
			t.Errorf("newDecoder(%q) built a decoder for %q", f, d.format())
		}
	}

	// Auto is valid but unresolved; the error must name that, not "unknown".
	_, err := newDecoder(FormatAuto)
	if err == nil {
		t.Fatal("newDecoder(auto) returned a decoder; auto is not a concrete format")
	}
	if strings.Contains(err.Error(), "unknown format") {
		t.Errorf("auto reported as unknown, which is a wrong answer to a valid input: %v", err)
	}
	if !strings.Contains(err.Error(), "DetectFormat") {
		t.Errorf("the error does not say how to resolve auto: %v", err)
	}

	if _, err := newDecoder(Format("nope/v9")); err == nil {
		t.Error("an unknown format was accepted")
	}
}
