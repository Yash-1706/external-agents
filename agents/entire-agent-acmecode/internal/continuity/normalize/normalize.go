// Package normalize turns the raw hook envelopes emitted by host agent runtimes
// into the one normalized event vocabulary the rest of the system speaks:
// []model.AgentEvent (plan §9).
//
// This is the layer that earns the claim in plan §29. OpenClaw and Hermes expose
// deliberately different hook surfaces — different names, different envelope
// shapes, different time encodings — and neither of them gets its own task-state
// engine. Both are mapped here, once, into model.AgentEvent, and everything
// downstream (lineage, deterministic state, handoff) is written against that one
// type. If a third runtime shows up tomorrow, it adds a file to this package and
// changes nothing else. That is what "the continuity abstraction is
// agent-independent" has to mean in code.
//
// Three rules govern the mapping, and all three are honesty rules rather than
// convenience ones:
//
//   - A lifecycle name this build does not map is skipped, never guessed at.
//     Plan §9 is explicit that an adapter must not invent a lifecycle semantic
//     its host does not expose, so an unrecognised hook produces no event rather
//     than a plausible-looking one. Hermes, for instance, exposes no child-agent
//     *start* hook, so no Hermes stream ever yields a SubagentStarted.
//
//   - A stream in which nothing at all was recognised is an error. Silently
//     returning zero events would let a caller present "no agent activity" when
//     the truth is "this normalizer could not read that stream".
//
//   - Tool payloads are never inlined. An event keeps the tool's name and a
//     PayloadRef pointing at the durable artifact, and every string that does
//     survive goes through package redact first (plan §34).
package normalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/redact"
)

// Attribute keys this package sets on the events it emits. They match the names
// the domain contract documents on model.AgentEvent.Attrs.
const (
	attrToolName     = "tool_name"
	attrCheckpointID = "checkpoint_id"
	// attrRawRole preserves a session role the host reported that is not one of
	// model's roles. Dropping it would quietly discard what the host actually
	// said; promoting it to a model.Role would invent a role the domain does not
	// define. Keeping the raw string as an attribute does neither.
	attrRawRole = "raw_role"
)

// utf8BOM is the byte order mark some writers prefix to a UTF-8 file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Normalizer converts one host runtime's raw event stream into normalized
// events. Implementations are stateless and safe for concurrent use.
type Normalizer interface {
	// Normalize parses a whole stream (NDJSON or a JSON array) and returns the
	// events it recognised, in stream order. Records whose lifecycle name this
	// build does not map are skipped; an error is returned when the stream could
	// not be parsed, when a recognised record cannot produce a valid event, or
	// when nothing in the stream was recognised at all.
	Normalize(raw []byte) ([]model.AgentEvent, error)
	// Agent reports which runtime this normalizer reads.
	Agent() model.AgentKind
}

// OpenClaw returns the Normalizer for OpenClaw hook envelopes.
func OpenClaw() Normalizer { return openClawNormalizer{} }

// Hermes returns the Normalizer for Hermes event envelopes.
func Hermes() Normalizer { return hermesNormalizer{} }

// For returns the Normalizer for a runtime, or an error naming the runtimes that
// actually have one. It deliberately refuses model.AgentHuman and
// model.AgentUnknown: a human does not emit hook envelopes, and "unknown" is a
// gap to be reported, not a format to be parsed.
func For(agent model.AgentKind) (Normalizer, error) {
	switch agent {
	case model.AgentOpenClaw:
		return OpenClaw(), nil
	case model.AgentHermes:
		return Hermes(), nil
	}
	return nil, fmt.Errorf("normalize: no normalizer for agent %q: only %q and %q emit hook envelopes",
		agent, model.AgentOpenClaw, model.AgentHermes)
}

// ParseStream splits a raw hook stream into its individual JSON records.
//
// Both accepted framings exist in the wild for good reasons, so both are
// supported and neither is preferred: a hook shim appending to a log writes
// NDJSON (one record per line, crash-safe, no rewriting), while a shim that
// buffers a session and flushes once writes a JSON array. The framing carries no
// meaning, so the same session in either framing must normalize to byte-identical
// events; the fixtures in testdata assert exactly that.
//
// Whitespace-separated and pretty-printed records are handled too, because the
// stream is decoded as a sequence of JSON values rather than split on newlines,
// and a leading UTF-8 byte order mark is skipped. An empty or all-whitespace
// input returns no records and no error: an empty stream is a real, honest
// answer ("this session logged nothing"), whereas malformed JSON is not and is
// reported with the index of the offending record.
func ParseStream(raw []byte) ([]json.RawMessage, error) {
	// The BOM is an encoding artifact, not content. Several ordinary writers
	// emit one — Windows PowerShell's Out-File and a number of editors — so a
	// hook shim's log can arrive with it, and encoding/json rejects it as an
	// invalid character. Stripping it cannot change how any stream that parses
	// today is read, because JSON can never legitimately begin with U+FEFF; it
	// only stops a perfectly good log being reported as corrupt.
	trimmed := bytes.TrimSpace(raw)
	trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, utf8BOM))
	if len(trimmed) == 0 {
		return nil, nil
	}

	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("normalize: parse JSON array stream: %w", err)
		}
		out := make([]json.RawMessage, 0, len(arr))
		for _, rec := range arr {
			if len(bytes.TrimSpace(rec)) == 0 {
				continue
			}
			out = append(out, rec)
		}
		return out, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var out []json.RawMessage
	for {
		var rec json.RawMessage
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The index is the record we were about to read, which is the one the
			// operator has to go and look at.
			return nil, fmt.Errorf("normalize: parse NDJSON stream: record %d: %w", len(out), err)
		}
		if len(bytes.TrimSpace(rec)) == 0 {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// mapped is the result of interpreting a single raw record.
type mapped struct {
	event model.AgentEvent
	// name is the host's own lifecycle name for the record. It is kept even when
	// the record is skipped, so a stream where nothing was recognised can report
	// what it actually contained instead of just failing.
	name string
	ok   bool
	// ignored marks a record this build recognises and deliberately does not
	// model, such as a token-usage line. It is reported separately from an
	// unrecognised name because the two mean different things to a reader:
	// "we know what that is and it carries no continuity meaning" versus
	// "we could not read that, so your timeline has a hole in it".
	ignored bool
}

// recordMapper interprets one raw record for a particular host runtime.
type recordMapper func(index int, rec json.RawMessage) (mapped, error)

// normalizeStream is the shared skeleton both runtimes use. Keeping the
// skip/error policy here rather than in each adapter is what stops the two
// runtimes drifting apart on how honestly they degrade (plan §29).
func normalizeStream(agent model.AgentKind, raw []byte, mapRecord recordMapper) ([]model.AgentEvent, error) {
	records, err := ParseStream(raw)
	if err != nil {
		return nil, err
	}

	out := make([]model.AgentEvent, 0, len(records))
	skipped := 0
	lastSkipped := ""
	for i, rec := range records {
		m, err := mapRecord(i, rec)
		if err != nil {
			return nil, err
		}
		if !m.ok {
			// Not an error: a host lifecycle point we do not map is a gap in our
			// coverage, not corrupt input, and inventing an event for it is the
			// one thing plan §9 forbids outright.
			skipped++
			lastSkipped = m.name
			continue
		}
		out = append(out, m.event)
	}

	if skipped > 0 && skipped == len(records) {
		return nil, fmt.Errorf(
			"normalize: %s: none of the %d records carried a lifecycle name this build maps (last was %q); "+
				"the stream is probably not %s hook output, or the shim is emitting names we do not know (plan §9)",
			agent, len(records), redact.Text(lastSkipped), agent)
	}
	return out, nil
}

// finalize applies the invariants that hold for every runtime and every record:
// nothing credential-shaped survives into a persisted event (plan §34), and
// nothing that fails the frozen domain contract is emitted at all (plan §29).
func finalize(ev model.AgentEvent, agent model.AgentKind, index int, hostName string) (model.AgentEvent, error) {
	ev.Agent = agent
	// Identifiers are deliberately not redacted: task, session and parent ids are
	// the lineage itself, and a redacted session id would sever the cross-agent
	// chain the product exists to preserve. Free text and attribute values are.
	ev.Summary = redact.Text(strings.TrimSpace(ev.Summary))
	ev.PayloadRef = redact.Text(strings.TrimSpace(ev.PayloadRef))
	ev.Attrs = redact.Map(ev.Attrs)
	if len(ev.Attrs) == 0 {
		// Keep the "no attributes" case as nil so serialized events omit the key
		// entirely; an empty object and a missing one must not both appear.
		ev.Attrs = nil
	}
	ev.Timestamp = ev.Timestamp.UTC()

	if err := ev.Validate(); err != nil {
		return model.AgentEvent{}, fmt.Errorf("normalize: %s: record %d (%s): %w", agent, index, hostName, err)
	}
	return ev, nil
}

// hostRole maps a host-reported session role onto model.Role. The second result
// reports whether the domain recognises it; an unrecognised, non-empty role is
// the caller's cue to preserve the raw string as an attribute rather than to
// guess (see attrRawRole).
func hostRole(raw string) (model.Role, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		// The host reported no role. That is not an unknown role, it is no claim
		// at all, so there is nothing to preserve.
		return "", true
	}
	switch model.Role(trimmed) {
	case model.RoleMain, model.RolePlanning, model.RoleCoding, model.RoleTest,
		model.RoleReview, model.RoleResearch, model.RoleBackground, model.RoleHuman:
		return model.Role(trimmed), true
	}
	return "", false
}

// scalarAttrs flattens a host metadata bag into the scalar Attrs map the domain
// contract defines.
//
// Nested objects and arrays are dropped on purpose. Attrs is documented as "a
// small set of scalar, adapter-specific fields"; a nested value is a payload,
// and plan §34 says a payload is referenced through PayloadRef, never copied
// into task state. A shim that needs to convey structure must write it to an
// artifact and pass the ref.
func scalarAttrs(in map[string]json.RawMessage) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		// Iteration order cannot reach the output: every entry is converted
		// independently and the result is a map.
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		s, ok := scalarString(v)
		if !ok {
			continue
		}
		out[key] = s
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scalarString renders one JSON value as a string, reporting false for values
// that are not scalars. Numbers and booleans keep their exact source text rather
// than being re-formatted, which keeps normalization byte-stable: 1.50 stays
// 1.50 and a 20-digit id does not go through a float.
func scalarString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	switch trimmed[0] {
	case '{', '[':
		return "", false
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	}
	return trimmed, true
}

// setAttr adds an attribute unless the value is empty, allocating the map only
// when there is something to store. An attribute with an empty value would claim
// the host reported something when it did not.
func setAttr(m map[string]string, key, value string) map[string]string {
	value = strings.TrimSpace(value)
	if value == "" {
		return m
	}
	if m == nil {
		m = make(map[string]string, 1)
	}
	m[key] = value
	return m
}

// parseRFC3339 reads an OpenClaw timestamp and normalizes it to UTC.
func parseRFC3339(raw string, agent model.AgentKind, index int, hostName string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("normalize: %s: record %d (%s): missing ts", agent, index, hostName)
	}
	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("normalize: %s: record %d (%s): ts %q is not RFC3339: %w",
			agent, index, hostName, trimmed, err)
	}
	return t.UTC(), nil
}

// parseEpochMillis reads a Hermes timestamp and normalizes it to UTC.
//
// A missing or non-positive value is rejected rather than being turned into
// 1970-01-01. An event the host never stamped is a capture gap, and dating it to
// the epoch would manufacture an ordering the host never asserted (plan §33).
func parseEpochMillis(raw json.Number, agent model.AgentKind, index int, hostName string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw.String())
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("normalize: %s: record %d (%s): missing time", agent, index, hostName)
	}
	ms, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("normalize: %s: record %d (%s): time %q is not epoch milliseconds: %w",
			agent, index, hostName, trimmed, err)
	}
	if ms <= 0 {
		return time.Time{}, fmt.Errorf("normalize: %s: record %d (%s): time %d is not a real instant",
			agent, index, hostName, ms)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// lowerIndex builds the case-insensitive lookup table used for host lifecycle
// names. The declared maps keep each host's own spelling so the wire format is
// readable in the source; a shim that changes the case of a name has not
// invented a new lifecycle semantic, so it should still resolve.
func lowerIndex(in map[string]model.EventType) map[string]model.EventType {
	out := make(map[string]model.EventType, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

// knownNames lists a host's mapped lifecycle names in sorted order, for error
// messages and documentation. Sorted, because map order must never reach output.
func knownNames(in map[string]model.EventType) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
