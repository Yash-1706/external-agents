package render

import (
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// captureInput pairs a Capture flag with the name a human would recognise it
// by. The list is a fixed slice rather than a map so the rendered order is
// identical on every run and on every machine.
type captureInput struct {
	name string
	ok   bool
}

// captureInputs enumerates the capture surface in a fixed, meaningful order:
// the deterministic repository facts first (Level 1 evidence, plan §31), then
// the checkpoint and event history, then the model-assisted layer last,
// because that is the order in which a reader should trust them.
func captureInputs(c model.Capture) []captureInput {
	return []captureInput{
		{"git repository state", c.GitAvailable},
		{"test results", c.TestResultsParsed},
		{"entire checkpoints", c.CheckpointAvailable},
		{"agent events", c.EventsAvailable},
		{"agent transcript", c.TranscriptAvailable},
		{"entire graph", c.GraphAvailable},
		{"semantic extraction", c.SemanticExtraction},
	}
}

// captureDegraded reports whether anything about the capture is worth warning
// about.
//
// It deliberately does not use model.Capture.Complete. Complete inspects only
// git, checkpoints, events, transcript and Missing; it ignores GraphAvailable,
// TestResultsParsed, SemanticExtraction and Notes. A state whose graph and
// semantic-extraction layers were unreachable therefore reports Complete, and
// keying the §33/§47 block on it would let that state render as a full
// recovery — which is exactly the silence plan §33 and §47 exist to prevent,
// and which plan §48 forbids outright for the graph ("Explicitly mark Graph
// verification unavailable. Do not pretend it occurred."). Notes matter for
// the same reason: they are the only human-readable record of why capture
// degraded, and this block is the only place they are ever printed.
func captureDegraded(c model.Capture) bool {
	for _, in := range captureInputs(c) {
		if !in.ok {
			return true
		}
	}
	return len(c.Missing) > 0 || len(c.Notes) > 0
}

// partialCaptureSection renders the STATE PARTIALLY RECOVERED block from plan
// §33 and §47, or returns false when every capture input was available.
//
// This block is a correctness requirement, not decoration. When an external
// agent did not expose everything, the product must name what it verified and
// what it could not, so the reader is never handed a partial recovery dressed
// up as a full one.
func partialCaptureSection(c model.Capture) (section, bool) {
	if !captureDegraded(c) {
		return section{}, false
	}
	sec := section{Title: "STATE PARTIALLY RECOVERED"}
	sec.Items = append(sec.Items, item{
		Text: "Capture was incomplete. Everything below is reported at the confidence its evidence supports, and the gaps are named rather than filled in (plan §33, §47).",
	})

	inputs := captureInputs(c)

	var verified, unavailable []item
	for _, in := range inputs {
		if in.ok {
			verified = append(verified, item{Glyph: "✓", Text: in.name})
		} else {
			unavailable = append(unavailable, item{Glyph: "✗", Text: in.name})
		}
	}

	sec.Items = append(sec.Items, blank(), subhead("Verified:"))
	if len(verified) == 0 {
		sec.Items = append(sec.Items, item{Glyph: "✗", Text: "nothing — no capture input was available"})
	} else {
		sec.Items = append(sec.Items, verified...)
	}

	sec.Items = append(sec.Items, blank(), subhead("Unavailable:"))
	if len(unavailable) == 0 {
		sec.Items = append(sec.Items, item{Glyph: "✓", Text: "none — every capture input was reachable"})
	} else {
		sec.Items = append(sec.Items, unavailable...)
	}

	// Capture.Missing names the specific inputs the builder expected and did
	// not get. Printing them verbatim is what lets a reader tell which piece of
	// context is absent, instead of only learning that "something" was.
	//
	// The heading is emitted even when Missing is empty. Plan §47 pins the
	// shape of this block as Verified / Unavailable / Unknown / Safe next
	// action, and a reader scanning for what is unknown must get an answer
	// rather than a heading that silently disappeared — "nothing further was
	// recorded as missing" and "we did not look" are different statements.
	sec.Items = append(sec.Items, blank(), subhead("Unknown:"))
	if missing := sortedStrings(c.Missing); len(missing) > 0 {
		for _, m := range missing {
			sec.Items = append(sec.Items, item{Glyph: "?", Label: model.Unknown.Label(), Text: m})
		}
	} else {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "nothing beyond the unavailable inputs above was recorded as missing — whatever those inputs would have carried is unknown, not absent",
		})
	}

	if notes := sortedStrings(c.Notes); len(notes) > 0 {
		sec.Items = append(sec.Items, blank(), subhead("Notes:"))
		for _, n := range notes {
			sec.Items = append(sec.Items, item{Glyph: "•", Text: n})
		}
	}

	// Plan §47 requires the degraded view to end on a safe action rather than
	// on a guess about what the missing context contained.
	sec.Items = append(sec.Items, blank(), subhead("Safe next action:"))
	sec.Items = append(sec.Items, item{
		Glyph: "→",
		Label: model.Recommended.Label(),
		Text:  "Inspect the current repository, and ask for human confirmation before relying on anything above that is not marked OBSERVED.",
	})
	return sec, true
}
