package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/redact"
)

// Format names a wire format rather than a runtime.
//
// The original design keyed the parser off the agent alone — For(agent) — which
// encoded the assumption that each runtime emits exactly one, stable format.
// That assumption does not survive contact with a host that ships a new
// transcript format while its existing users keep producing the old one: both
// are live at the same time, and one of them may not even name the same runtime.
// Format is the dimension that was missing.
type Format string

const (
	// FormatOpenClawV1 is the OpenClaw hook envelope: {"hook": ..., "ts": ...}.
	FormatOpenClawV1 Format = "openclaw/v1"
	// FormatHermesV1 is the Hermes event envelope: {"event": ..., "time": <ms>}.
	FormatHermesV1 Format = "hermes/v1"
	// FormatLifecycleV2 is the newer flat lifecycle JSONL:
	// {"timestamp": <RFC3339>, "event": ..., "session_id": ...}.
	//
	// Unlike the two above it is not tied to a runtime: it carries the agent's
	// own name inside its session_started record, so any host can emit it.
	FormatLifecycleV2 Format = "lifecycle/v2"
	// FormatAuto asks for per-record detection, which is the only setting that
	// reads a stream containing more than one format.
	FormatAuto Format = "auto"
)

// Formats lists the concrete formats this build can read.
func Formats() []Format {
	return []Format{FormatOpenClawV1, FormatHermesV1, FormatLifecycleV2}
}

// Options controls a Stream decode.
type Options struct {
	// Format forces one format. Empty or FormatAuto detects per record, which
	// is what lets a partially-migrated fleet — old and new records interleaved
	// in one file — decode correctly.
	Format Format

	// TaskID anchors every event to a task. It wins over anything derived from
	// the stream, and is the only way to anchor a transcript that does not name
	// its repository.
	TaskID string

	// Repo and Branch are the fallback anchor for a stream whose records carry
	// no task id and no session_started header — an incomplete transcript. They
	// are normally filled from the working repository by the caller.
	Repo   string
	Branch string
}

// Report is the honest account of what a decode actually did. It exists because
// a count of accepted events alone cannot distinguish a clean read from one that
// silently dropped a third of the file (plan §33, §47).
type Report struct {
	// Total is the number of records the stream framing yielded.
	Total int
	// Accepted is the number that produced a normalized lifecycle event.
	Accepted int
	// Unknown is the number carrying a lifecycle name this build does not map.
	// They are retained as model.EventUnknown wherever they could be anchored.
	Unknown int
	// Ignored is the number recognised but deliberately carrying no continuity
	// meaning, such as a token-usage record. Distinct from Unknown: we know
	// exactly what these are and chose not to model them.
	Ignored int
	// Malformed is the number that could not be read at all — truncated lines,
	// invalid JSON, or records failing the domain contract. They never abort the
	// decode; an incomplete transcript must still produce a partial result.
	Malformed int
	// Anchorless counts records that were understood but could not be tied to a
	// task, and so could not be emitted.
	Anchorless int

	// Formats lists the formats actually seen, sorted.
	Formats []string
	// UnknownKinds lists the distinct unmapped lifecycle names, sorted.
	UnknownKinds []string
	// Notes explain every degradation in words a reader can act on.
	Notes []string
}

// Complete reports whether every record in the stream was understood and used.
func (r Report) Complete() bool {
	return r.Unknown == 0 && r.Malformed == 0 && r.Anchorless == 0
}

// Summary renders the report as a short block. It always states the outcome,
// including when everything was clean, so that a caller printing it never has
// to decide whether silence means success.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d record(s): %d event(s)", r.Total, r.Accepted)
	if r.Ignored > 0 {
		fmt.Fprintf(&b, ", %d ignored", r.Ignored)
	}
	if r.Unknown > 0 {
		fmt.Fprintf(&b, ", %d unrecognised", r.Unknown)
	}
	if r.Malformed > 0 {
		fmt.Fprintf(&b, ", %d unreadable", r.Malformed)
	}
	if r.Anchorless > 0 {
		fmt.Fprintf(&b, ", %d unanchored", r.Anchorless)
	}
	if len(r.Formats) > 0 {
		fmt.Fprintf(&b, " [%s]", strings.Join(r.Formats, ", "))
	}
	b.WriteString("\n")
	if r.Complete() {
		b.WriteString("Every record was understood.\n")
		return b.String()
	}
	b.WriteString("This transcript was read incompletely; the task timeline is partial.\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  ? %s\n", n)
	}
	return b.String()
}

// Result is a decoded stream plus the account of how it went.
type Result struct {
	Events []model.AgentEvent
	Report Report
}

// decoder maps records of one format. Implementations may hold per-stream state
// (the lifecycle/v2 decoder must, to correlate a tool result with its call and
// to carry the anchor derived from the session header), so a fresh one is built
// for each Stream call.
type decoder interface {
	format() Format
	mapRecord(index int, rec json.RawMessage, opts Options) (mapped, error)
}

// newDecoder builds a decoder for a format.
func newDecoder(f Format) (decoder, error) {
	switch f {
	case FormatOpenClawV1:
		return legacyDecoder{f: FormatOpenClawV1, m: openClawNormalizer{}.mapRecord}, nil
	case FormatHermesV1:
		return legacyDecoder{f: FormatHermesV1, m: hermesNormalizer{}.mapRecord}, nil
	case FormatLifecycleV2:
		return newLifecycleV2(), nil
	}
	return nil, fmt.Errorf("normalize: unknown format %q; this build reads %v", f, Formats())
}

// legacyDecoder adapts the stateless per-runtime mappers to the decoder
// interface. The v1 mappers are reused verbatim rather than reimplemented: the
// framing, redaction, validation and skip policy are shared by construction, so
// supporting a second format adds a mapping, not a pipeline.
type legacyDecoder struct {
	f Format
	m recordMapper
}

func (d legacyDecoder) format() Format { return d.f }

func (d legacyDecoder) mapRecord(index int, rec json.RawMessage, _ Options) (mapped, error) {
	return d.m(index, rec)
}

// sniff is the minimal set of fields format detection looks at.
type sniff struct {
	Hook         string          `json:"hook"`
	Event        string          `json:"event"`
	TS           string          `json:"ts"`
	Timestamp    string          `json:"timestamp"`
	Time         json.RawMessage `json:"time"`
	SessionID    string          `json:"session_id"`
	AgentSession string          `json:"agent_session"`
	Session      json.RawMessage `json:"session"`
}

// DetectFormat identifies the format of a single record.
//
// Detection is per record rather than per file on purpose: during a rollout the
// same task can be fed by one shim that has been upgraded and one that has not,
// and a file-level guess would misread half of it.
func DetectFormat(rec json.RawMessage) (Format, bool) {
	var s sniff
	if err := json.Unmarshal(rec, &s); err != nil {
		return "", false
	}
	switch {
	case strings.TrimSpace(s.Hook) != "":
		// Only the OpenClaw envelope has a "hook" discriminator.
		return FormatOpenClawV1, true
	case strings.TrimSpace(s.Event) != "":
		// Both remaining formats key on "event"; the timestamp encoding and the
		// session field name separate them. Hermes carries epoch millis in
		// "time" and names its session "agent_session"; lifecycle/v2 carries an
		// RFC3339 "timestamp" and names its session "session_id".
		if strings.TrimSpace(s.AgentSession) != "" || len(bytes.TrimSpace(s.Time)) > 0 {
			return FormatHermesV1, true
		}
		if strings.TrimSpace(s.Timestamp) != "" || strings.TrimSpace(s.SessionID) != "" {
			return FormatLifecycleV2, true
		}
	}
	return "", false
}

// Stream decodes a transcript into normalized events.
//
// It differs from Normalizer.Normalize in policy, not in mapping — both run the
// same per-format mappers. Normalize is strict: it fails a stream it cannot read,
// which is the right answer when a caller has explicitly named a runtime and
// wants to know it pointed at the wrong file. Stream is tolerant, because the
// curveball requirement is that an incomplete transcript yields a partial result
// rather than a discarded session, and that an unknown event never takes the
// session down with it. Nothing is dropped quietly: everything Stream could not
// use is counted and explained in the Report.
func Stream(raw []byte, opts Options) (Result, error) {
	records, framingNotes, err := parseStreamTolerant(raw)
	if err != nil {
		// A stream that is not JSON in any framing is not a partial transcript,
		// it is the wrong file. That is worth failing on.
		return Result{}, err
	}

	res := Result{Report: Report{Total: len(records), Notes: framingNotes}}
	res.Report.Malformed += len(framingNotes)

	forced := opts.Format
	if forced == "" {
		forced = FormatAuto
	}

	decoders := map[Format]decoder{}
	seenFormats := map[Format]bool{}
	unknownKinds := map[string]bool{}

	for i, rec := range records {
		f := forced
		if forced == FormatAuto {
			detected, ok := DetectFormat(rec)
			if !ok {
				res.Report.Malformed++
				res.Report.Notes = append(res.Report.Notes, fmt.Sprintf(
					"record %d does not match any format this build reads (%v); it was skipped", i, Formats()))
				continue
			}
			f = detected
		}

		d, ok := decoders[f]
		if !ok {
			built, derr := newDecoder(f)
			if derr != nil {
				return Result{}, derr
			}
			decoders[f], d = built, built
		}
		seenFormats[f] = true

		m, merr := d.mapRecord(i, rec, opts)
		if merr != nil {
			// A record the mapper rejects is unreadable, not fatal. Reporting it
			// and continuing is the whole difference between "partial result"
			// and "discarded session".
			res.Report.Malformed++
			res.Report.Notes = append(res.Report.Notes, fmt.Sprintf("record %d: %s", i, redact.Text(merr.Error())))
			continue
		}

		switch {
		case m.ok:
			res.Events = append(res.Events, m.event)
			res.Report.Accepted++
		case m.ignored:
			res.Report.Ignored++
		default:
			res.Report.Unknown++
			if m.name != "" {
				unknownKinds[m.name] = true
			}
			// Retain the record as a first-class unknown event so the timeline
			// shows that something happened here, without inventing a lifecycle
			// semantic the host never exposed (plan §9).
			ev, anchored := unknownEvent(m, f, opts)
			if !anchored {
				res.Report.Anchorless++
				res.Report.Notes = append(res.Report.Notes, fmt.Sprintf(
					"record %d (%s) could not be tied to a task or session and was not stored",
					i, redact.Text(m.name)))
				continue
			}
			res.Events = append(res.Events, ev)
		}
	}

	for f := range seenFormats {
		res.Report.Formats = append(res.Report.Formats, string(f))
	}
	sort.Strings(res.Report.Formats)
	for k := range unknownKinds {
		res.Report.UnknownKinds = append(res.Report.UnknownKinds, redact.Text(k))
	}
	sort.Strings(res.Report.UnknownKinds)

	if len(res.Report.UnknownKinds) > 0 {
		res.Report.Notes = append(res.Report.Notes, fmt.Sprintf(
			"lifecycle names this build does not map: %s", strings.Join(res.Report.UnknownKinds, ", ")))
	}
	return res, nil
}

// unknownEvent builds the retained record for an unmapped lifecycle name. It
// reports false when the record cannot be anchored to a task and session, in
// which case there is nothing meaningful to store and the gap is reported
// instead.
func unknownEvent(m mapped, f Format, opts Options) (model.AgentEvent, bool) {
	ev := m.event
	ev.Type = model.EventUnknown
	if ev.TaskID == "" {
		ev.TaskID = opts.TaskID
	}
	if ev.SessionID == "" || ev.TaskID == "" || ev.Timestamp.IsZero() {
		return model.AgentEvent{}, false
	}
	if ev.Agent == "" {
		ev.Agent = model.AgentUnknown
	}
	ev.Attrs = setAttr(ev.Attrs, model.AttrRawKind, m.name)
	ev.Attrs = setAttr(ev.Attrs, "format", string(f))
	if ev.Summary == "" {
		ev.Summary = "unrecognised " + string(f) + " record: " + m.name
	}
	if err := ev.Validate(); err != nil {
		return model.AgentEvent{}, false
	}
	return ev, true
}

// parseStreamTolerant splits a stream into records without letting one bad
// record cost the rest of the file.
//
// The strict parser is tried first, so a well-formed stream is read exactly as
// it always was. Only when that fails does the stream get re-read line by line,
// which is the framing that can actually be resynchronised: a JSONL transcript
// truncated mid-write — an agent killed partway through a session, which is
// precisely the case this product exists for — loses its last line and no more.
func parseStreamTolerant(raw []byte) ([]json.RawMessage, []string, error) {
	if records, err := ParseStream(raw); err == nil {
		return records, nil, nil
	}

	trimmed := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(raw), utf8BOM))
	if len(trimmed) == 0 {
		return nil, nil, nil
	}
	if trimmed[0] == '[' {
		// An array is a single JSON value: a truncated one cannot be split into
		// intact records, so there is nothing honest to recover.
		return nil, nil, fmt.Errorf("normalize: the stream begins as a JSON array but is not valid JSON; it cannot be partially recovered")
	}

	var (
		out   []json.RawMessage
		notes []string
	)
	for n, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimSuffix(line, []byte("\r")))
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			notes = append(notes, fmt.Sprintf(
				"line %d is not valid JSON and was skipped; the transcript looks truncated or damaged", n+1))
			continue
		}
		out = append(out, json.RawMessage(line))
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("normalize: no readable JSON record in the stream (%d unreadable line(s))", len(notes))
	}
	return out, notes, nil
}

// Gaps renders a one-line account of what the decode could not use, for callers
// that need to append it to a sentence rather than print a block. It returns
// "none" when the read was clean, so a caller can always say something rather
// than leaving a reader to infer completeness from silence.
func (r Report) Gaps() string {
	var parts []string
	if r.Unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unrecognised record(s)", r.Unknown))
	}
	if r.Malformed > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable record(s)", r.Malformed))
	}
	if r.Anchorless > 0 {
		parts = append(parts, fmt.Sprintf("%d unanchored record(s)", r.Anchorless))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
