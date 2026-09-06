package normalize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// fixture loads a testdata stream. A missing fixture is a fatal test-setup
// error, never a skipped test: the fixtures are the specification of the two
// wire formats.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// at parses an RFC3339 instant for expected values.
func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse expected timestamp %q: %v", s, err)
	}
	return ts.UTC()
}

// assertEvents compares whole event slices and reports the first field that
// differs, because a reflect.DeepEqual failure on six events is unreadable.
func assertEvents(t *testing.T, got, want []model.AgentEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count: got %d, want %d\ngot:  %s\nwant: %s",
			len(got), len(want), render(got), render(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("event %d:\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}

func render(in []model.AgentEvent) string {
	var b strings.Builder
	for _, e := range in {
		b.WriteString("\n  ")
		b.WriteString(string(e.Type))
		b.WriteString("@")
		b.WriteString(e.Timestamp.Format(time.RFC3339))
	}
	return b.String()
}

func TestParseStream(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantCount int
		wantFirst string
		wantErr   string
	}{
		{
			name:      "ndjson",
			raw:       "{\"a\":1}\n{\"a\":2}\n",
			wantCount: 2,
			wantFirst: `{"a":1}`,
		},
		{
			name:      "ndjson with blank lines and trailing whitespace",
			raw:       "\n{\"a\":1}\n\n\n{\"a\":2}\n   \n",
			wantCount: 2,
			wantFirst: `{"a":1}`,
		},
		{
			name:      "json array",
			raw:       `[{"a":1},{"a":2}]`,
			wantCount: 2,
			wantFirst: `{"a":1}`,
		},
		{
			name:      "json array with leading whitespace and newlines",
			raw:       "\n  [\n  {\"a\":1},\n  {\"a\":2}\n]\n",
			wantCount: 2,
		},
		{
			name: "pretty printed records are not split on newlines",
			// A shim that pretty-prints its records is still emitting a stream of
			// JSON values; splitting on '\n' would corrupt it.
			raw:       "{\n  \"a\": 1\n}\n{\n  \"a\": 2\n}\n",
			wantCount: 2,
		},
		{
			name:      "empty input is an empty stream, not an error",
			raw:       "",
			wantCount: 0,
		},
		{
			name:      "whitespace only input is an empty stream",
			raw:       "  \n\t\n",
			wantCount: 0,
		},
		{
			name:      "empty array",
			raw:       "[]",
			wantCount: 0,
		},
		{
			// A hook shim whose log was written by a tool that prefixes UTF-8
			// files with a byte order mark (Windows PowerShell's Out-File, several
			// editors) is still emitting NDJSON. Reporting it as corrupt would
			// blame the session for an artifact of how the file was written.
			name:      "leading utf-8 bom is an encoding artifact, not corruption",
			raw:       "\ufeff{\"a\":1}\n{\"a\":2}\n",
			wantCount: 2,
			wantFirst: `{"a":1}`,
		},
		{
			name:      "bom in front of an array",
			raw:       "\ufeff[{\"a\":1},{\"a\":2}]",
			wantCount: 2,
			wantFirst: `{"a":1}`,
		},
		{
			name:      "a file containing nothing but a bom logged nothing",
			raw:       "\ufeff\n",
			wantCount: 0,
		},
		{
			// Only a *leading* mark is an encoding artifact. One in the middle of
			// a stream is corrupt input and must still be reported.
			name:    "a bom between records is still corruption",
			raw:     "{\"a\":1}\n\ufeff{\"a\":2}\n",
			wantErr: "record 1",
		},
		{
			name:    "malformed ndjson names the failing record",
			raw:     "{\"a\":1}\n{\"a\":\n",
			wantErr: "record 1",
		},
		{
			name:    "malformed array",
			raw:     `[{"a":1},`,
			wantErr: "JSON array",
		},
		{
			name:    "not json at all",
			raw:     "this is a log line, not a stream",
			wantErr: "NDJSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseStream([]byte(tc.raw))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %d records", tc.wantErr, len(got))
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.wantCount {
				t.Fatalf("record count: got %d, want %d", len(got), tc.wantCount)
			}
			if tc.wantFirst != "" && string(got[0]) != tc.wantFirst {
				t.Errorf("first record: got %s, want %s", got[0], tc.wantFirst)
			}
		})
	}
}

// TestParseStreamFramingIsMeaningless proves the two framings are
// interchangeable: the same Hermes session written as NDJSON and as a JSON array
// must produce identical events. Framing is a shim implementation detail and
// must not change what the continuity layer believes happened.
func TestParseStreamFramingIsMeaningless(t *testing.T) {
	fromLines, err := Hermes().Normalize(fixture(t, "hermes_main.ndjson"))
	if err != nil {
		t.Fatalf("normalize ndjson: %v", err)
	}
	fromArray, err := Hermes().Normalize(fixture(t, "hermes_main_array.json"))
	if err != nil {
		t.Fatalf("normalize array: %v", err)
	}
	assertEvents(t, fromArray, fromLines)
}

func TestFor(t *testing.T) {
	tests := []struct {
		name    string
		agent   model.AgentKind
		want    model.AgentKind
		wantErr bool
	}{
		{name: "openclaw", agent: model.AgentOpenClaw, want: model.AgentOpenClaw},
		{name: "hermes", agent: model.AgentHermes, want: model.AgentHermes},
		{name: "human has no hook stream", agent: model.AgentHuman, wantErr: true},
		{name: "unknown is a gap, not a format", agent: model.AgentUnknown, wantErr: true},
		{name: "empty", agent: model.AgentKind(""), wantErr: true},
		{name: "never heard of it", agent: model.AgentKind("cursor"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := For(tc.agent)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.agent)
				}
				if got != nil {
					t.Fatalf("expected a nil Normalizer alongside the error, got %T", got)
				}
				// The error has to name the runtimes that do work, or the caller
				// cannot act on it.
				if !strings.Contains(err.Error(), string(model.AgentOpenClaw)) ||
					!strings.Contains(err.Error(), string(model.AgentHermes)) {
					t.Errorf("error does not name the supported runtimes: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Agent() != tc.want {
				t.Errorf("Agent(): got %q, want %q", got.Agent(), tc.want)
			}
		})
	}
}

// TestMappedTypesAreInTheDomainVocabulary checks both mapping tables against the
// frozen contract. A typo here would produce events that fail Validate at
// runtime instead of at build time.
func TestMappedTypesAreInTheDomainVocabulary(t *testing.T) {
	tables := map[string]map[string]model.EventType{
		"openclaw": openClawHooks,
		"hermes":   hermesEvents,
	}
	for host, table := range tables {
		for _, name := range knownNames(table) {
			if !table[name].Valid() {
				t.Errorf("%s hook %q maps to %q, which is not a model.EventType", host, name, table[name])
			}
		}
	}
}

// TestUnexposedLifecycleIsNeverMapped is the plan §9 guard rail expressed as a
// test. These event types exist in the domain, but no host hook may be bent into
// producing them: TaskResumed, HandoffCreated and ConstraintAdded are recorded by
// the continuity CLI when a human resumes, hands off, or introduces a mid-task
// constraint, and SubagentStarted has no Hermes hook at all. If someone later
// "improves" a mapping table by filling these in, this test is the objection.
func TestUnexposedLifecycleIsNeverMapped(t *testing.T) {
	forbidden := []model.EventType{model.TaskResumed, model.HandoffCreated, model.ConstraintAdded}
	tables := map[string]map[string]model.EventType{
		"openclaw": openClawHooks,
		"hermes":   hermesEvents,
	}
	for host, table := range tables {
		for _, name := range knownNames(table) {
			for _, bad := range forbidden {
				if table[name] == bad {
					t.Errorf("%s maps hook %q to %q, which no host hook exposes", host, name, bad)
				}
			}
		}
	}
	for _, name := range knownNames(hermesEvents) {
		if hermesEvents[name] == model.SubagentStarted {
			t.Errorf("hermes maps %q to SubagentStarted, but Hermes exposes no child-agent start hook", name)
		}
	}
	// OpenClaw does expose one, and must keep mapping it.
	found := false
	for _, name := range knownNames(openClawHooks) {
		if openClawHooks[name] == model.SubagentStarted {
			found = true
		}
	}
	if !found {
		t.Error("openclaw no longer maps any hook to SubagentStarted; sub-agent lineage would lose its spawn edge")
	}
}

// TestNormalizeIsDeterministic is a product requirement, not a nicety: the
// handoff receipt hashes state derived from these events, so two runs over the
// same stream must serialize byte-identically.
func TestNormalizeIsDeterministic(t *testing.T) {
	streams := []struct {
		name string
		n    Normalizer
		file string
	}{
		{name: "openclaw main", n: OpenClaw(), file: "openclaw_main.ndjson"},
		{name: "openclaw subagent", n: OpenClaw(), file: "openclaw_subagent.ndjson"},
		{name: "hermes main", n: Hermes(), file: "hermes_main.ndjson"},
		{name: "hermes child", n: Hermes(), file: "hermes_child.ndjson"},
	}
	for _, s := range streams {
		t.Run(s.name, func(t *testing.T) {
			raw := fixture(t, s.file)
			var last []byte
			for i := 0; i < 5; i++ {
				events, err := s.n.Normalize(raw)
				if err != nil {
					t.Fatalf("normalize: %v", err)
				}
				encoded, err := json.Marshal(events)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if last != nil && string(encoded) != string(last) {
					t.Fatalf("run %d differed:\n prev: %s\n this: %s", i, last, encoded)
				}
				last = encoded
			}
		})
	}
}

// TestEmptyStreamProducesNoEventsAndNoError: an agent that logged nothing is a
// real answer. The error case is reserved for "we could not read this", which is
// a different claim (plan §33).
func TestEmptyStreamProducesNoEventsAndNoError(t *testing.T) {
	for _, n := range []Normalizer{OpenClaw(), Hermes()} {
		events, err := n.Normalize([]byte("   \n\n"))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", n.Agent(), err)
		}
		if len(events) != 0 {
			t.Errorf("%s: got %d events from an empty stream", n.Agent(), len(events))
		}
	}
}

// TestWrongRuntimeStreamIsReportedNotSilentlyEmpty covers the mistake an
// integrator will actually make: pointing one normalizer at the other runtime's
// log. Both streams are valid JSON, so nothing fails to parse — the honest
// outcome is an error saying nothing was recognised, never an empty success that
// would be rendered as "this agent did no work".
func TestWrongRuntimeStreamIsReportedNotSilentlyEmpty(t *testing.T) {
	tests := []struct {
		name string
		n    Normalizer
		file string
	}{
		{name: "hermes stream into openclaw normalizer", n: OpenClaw(), file: "hermes_main.ndjson"},
		{name: "openclaw stream into hermes normalizer", n: Hermes(), file: "openclaw_main.ndjson"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := tc.n.Normalize(fixture(t, tc.file))
			if err == nil {
				t.Fatalf("expected an error, got %d events", len(events))
			}
			if events != nil {
				t.Errorf("expected no events alongside the error, got %d", len(events))
			}
			for _, want := range []string{"none of the", "6 records"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestTimestampsNormaliseToUTC covers the one rule both runtimes share and that
// neither fixture exercises: every fixture stamps its events in Zulu, so nothing
// proved that an offset timestamp is actually converted rather than merely
// re-labelled. It matters because ordering across two agents in different zones
// is the whole basis of cross-agent lineage — an OpenClaw shim on a laptop set to
// +05:30 and a Hermes plugin on a UTC runner must interleave correctly.
//
// All four records below describe the same instant, written four ways.
func TestTimestampsNormaliseToUTC(t *testing.T) {
	want := at(t, "2026-03-02T09:00:00Z")
	tests := []struct {
		name string
		n    Normalizer
		raw  string
	}{
		{
			name: "openclaw zulu",
			n:    OpenClaw(),
			raw:  `{"hook":"turn_start","ts":"2026-03-02T09:00:00Z","session":{"id":"s1"},"task":{"id":"t1"}}`,
		},
		{
			name: "openclaw positive offset",
			n:    OpenClaw(),
			raw:  `{"hook":"turn_start","ts":"2026-03-02T14:30:00+05:30","session":{"id":"s1"},"task":{"id":"t1"}}`,
		},
		{
			name: "openclaw negative offset",
			n:    OpenClaw(),
			raw:  `{"hook":"turn_start","ts":"2026-03-02T04:00:00-05:00","session":{"id":"s1"},"task":{"id":"t1"}}`,
		},
		{
			name: "hermes epoch millis",
			n:    Hermes(),
			raw:  `{"event":"PreLLM","time":1772442000000,"agent_session":"s1","entire_task":"t1"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.n.Normalize([]byte(tc.raw))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			ts := got[0].Timestamp
			if !ts.Equal(want) {
				t.Errorf("instant: got %s, want %s", ts.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
			}
			// The instant being right is not enough: the zone must be UTC, or two
			// events at the same moment would serialize differently and break the
			// byte-stability the handoff receipt hashes.
			if loc := ts.Location(); loc != time.UTC {
				t.Errorf("zone: got %v, want UTC", loc)
			}
			if s := ts.Format(time.RFC3339); !strings.HasSuffix(s, "Z") {
				t.Errorf("serialized as %q, which is not UTC", s)
			}
			// And the value must be interchangeable with the expectation literal
			// the rest of the suite compares against.
			if !reflect.DeepEqual(ts, want) {
				t.Errorf("representation differs from an equal UTC instant: %#v vs %#v", ts, want)
			}
		})
	}
}

func TestScalarAttrs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{name: "absent bag", raw: `{}`, want: nil},
		{
			name: "strings, numbers and booleans keep their exact source text",
			raw:  `{"tool_name":"run_tests","exit_code":1,"duration":4.20,"cached":false,"id":12345678901234567890}`,
			want: map[string]string{
				"tool_name": "run_tests",
				"exit_code": "1",
				"duration":  "4.20",
				"cached":    "false",
				"id":        "12345678901234567890",
			},
		},
		{
			name: "nested payloads are dropped rather than inlined",
			raw:  `{"exit_code":1,"diff":{"files":3},"paths":["a","b"]}`,
			want: map[string]string{"exit_code": "1"},
		},
		{
			name: "null carries no claim",
			raw:  `{"exit_code":null,"tool_name":"shell"}`,
			want: map[string]string{"tool_name": "shell"},
		},
		{
			name: "a bag of nothing but payloads yields no attributes",
			raw:  `{"diff":{"files":3}}`,
			want: nil,
		},
		{
			name: "blank keys are dropped",
			raw:  `{"  ":"x","tool_name":"shell"}`,
			want: map[string]string{"tool_name": "shell"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var in map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.raw), &in); err != nil {
				t.Fatalf("bad test input: %v", err)
			}
			got := scalarAttrs(in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("scalarAttrs()\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

func TestHostRole(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		want      model.Role
		wantKnown bool
	}{
		{name: "main", raw: "main", want: model.RoleMain, wantKnown: true},
		{name: "case is not a new semantic", raw: "Test", want: model.RoleTest, wantKnown: true},
		{name: "padded", raw: "  review  ", want: model.RoleReview, wantKnown: true},
		{name: "no role reported is not an unknown role", raw: "", want: "", wantKnown: true},
		{name: "host specific role is not invented into a domain role", raw: "verifier", want: "", wantKnown: false},
		{name: "near miss is still a miss", raw: "testing", want: "", wantKnown: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, known := hostRole(tc.raw)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("hostRole(%q) = (%q, %v), want (%q, %v)", tc.raw, got, known, tc.want, tc.wantKnown)
			}
		})
	}
}
