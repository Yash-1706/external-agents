// Package render produces every human-facing view of an engineering task.
//
// The human handoff and the machine handoff are two projections of one
// EngineeringState (plan §24, Rule 10); this package is the human side. It
// derives no engineering conclusions of its own. If a claim is not carried by
// the state it is not printed, and if the state does not know something the
// view says so out loud: staying silent about a gap would let an incomplete
// recovery read as a complete one, which is the failure mode plan §33 and §47
// exist to prevent.
//
// Two properties are load-bearing rather than cosmetic:
//
//   - Every claim that carries a confidence prints its OBSERVED / INFERRED /
//     UNKNOWN / RECOMMENDED label (plan §12), so a reader can separate a fact
//     from a guess without reading the evidence. A recorded confidence is
//     honoured only up to what its evidence can carry: a claim recorded as
//     OBSERVED with no citation is shown at the weaker rating, and the
//     overstatement is reported rather than quietly corrected.
//   - A degraded Capture always produces a STATE PARTIALLY RECOVERED block
//     naming what was verified and what was not (plan §33, §47). The trigger
//     is this package's own captureDegraded, not model.Capture.Complete, which
//     does not inspect the graph, test-parsing or semantic layers.
//
// Output is plain text with no ANSI escapes so it stays pipeable, and it is
// deterministic: no map iteration reaches the output, no wall clock is read,
// and every collection is ordered before it is rendered. Rendering never
// mutates its input — the state and lineage are copied and normalised first —
// so two runs over the same input are byte-identical.
package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Width is the prose wrap column. 72 leaves room for the quoting and
// indentation a handoff picks up on its way through a terminal, an email or a
// pull-request body without reflowing into a mess.
const Width = 72

// maxInlineEvidence caps how many citations are inlined next to a single claim.
// The full set is always reachable through the EVIDENCE section; inlining all
// of it would spend the next agent's context budget on repetition (plan §49).
// The overflow is announced, never dropped silently.
const maxInlineEvidence = 3

// item is one rendered entry. Glyph is the leading marker, Label is the §12
// confidence label, Text is the claim itself and Notes are supporting lines
// such as a reason or a citation. Raw entries are pre-formatted (the §51 tree)
// and are emitted verbatim without wrapping.
type item struct {
	Glyph string
	Label string
	Text  string
	Notes []string
	Raw   bool
	// Sub marks a sub-heading inside a section, used to break a long block
	// such as the partial-recovery report into groups without inventing a
	// second level of section headers.
	Sub bool
}

func (it item) blank() bool {
	return !it.Raw && !it.Sub && it.Glyph == "" && it.Label == "" && it.Text == "" && len(it.Notes) == 0
}

// section is a CAPS-headed block of items.
type section struct {
	Title string
	Items []item
}

// document is the intermediate form every view is built into. Text and
// Markdown are two renderings of the same document, which is what keeps the
// plain-text and Markdown handoffs from drifting apart (plan §24).
type document struct {
	Title    string
	Subtitle string
	Sections []section
}

// blank is a spacer item.
func blank() item { return item{} }

// subhead is a sub-heading inside a section.
func subhead(text string) item { return item{Sub: true, Text: text} }

func (d *document) add(s section) {
	if len(s.Items) == 0 {
		return
	}
	d.Sections = append(d.Sections, s)
}

// Text renders the document as wrapped plain text.
func (d document) Text() string {
	var b strings.Builder
	if d.Title != "" {
		b.WriteString(d.Title + "\n")
		b.WriteString(rule(d.Title) + "\n")
	}
	if d.Subtitle != "" {
		for _, l := range wrapLines(d.Subtitle, Width) {
			b.WriteString(l + "\n")
		}
	}
	for i, s := range d.Sections {
		if i > 0 || d.Title != "" || d.Subtitle != "" {
			b.WriteString("\n")
		}
		b.WriteString(s.Title + "\n")
		for _, it := range s.Items {
			writeItemText(&b, it)
		}
	}
	return normalizeTrailer(b.String())
}

func writeItemText(b *strings.Builder, it item) {
	if it.Raw {
		for _, l := range strings.Split(strings.TrimRight(it.Text, "\n"), "\n") {
			b.WriteString(l + "\n")
		}
		for _, n := range it.Notes {
			b.WriteString(n + "\n")
		}
		return
	}
	prefix := ""
	if it.Glyph != "" {
		prefix += it.Glyph + " "
	}
	if it.Label != "" {
		prefix += "[" + it.Label + "] "
	}
	indent := ""
	if prefix != "" {
		indent = "  "
	}
	// The first line loses room to the glyph and label; continuation lines only
	// lose the hanging indent. Wrapping both at the narrower width would leave
	// every continuation line short and the block visibly ragged.
	lines := wrapLines2(it.Text, Width-runeLen(prefix), Width-runeLen(indent))
	if len(lines) == 0 {
		lines = []string{""}
	}
	b.WriteString(prefix + lines[0] + "\n")
	for _, l := range lines[1:] {
		b.WriteString(indent + l + "\n")
	}
	noteIndent := indent + "  "
	for _, n := range it.Notes {
		for _, l := range wrapLines(n, Width-runeLen(noteIndent)) {
			b.WriteString(noteIndent + l + "\n")
		}
	}
}

// Markdown renders the same document as Markdown. Prose is not re-wrapped
// because a Markdown consumer reflows it anyway; the content is identical to
// Text by construction.
func (d document) Markdown() string {
	var b strings.Builder
	if d.Title != "" {
		b.WriteString("# " + d.Title + "\n")
	}
	if d.Subtitle != "" {
		b.WriteString("\n" + d.Subtitle + "\n")
	}
	for _, s := range d.Sections {
		b.WriteString("\n## " + s.Title + "\n\n")
		// A single unadorned line is a paragraph; anything else becomes a
		// list, so a section never mixes the two shapes for what is one kind
		// of entry in the plain-text view.
		prose := len(s.Items) == 1 && s.Items[0].plain()
		for _, it := range s.Items {
			writeItemMarkdown(&b, it, prose)
		}
	}
	return normalizeTrailer(b.String())
}

// plain reports whether the item carries no marker of its own.
func (it item) plain() bool {
	return !it.Raw && !it.Sub && it.Glyph == "" && it.Label == "" && len(it.Notes) == 0
}

func writeItemMarkdown(b *strings.Builder, it item, prose bool) {
	switch {
	case it.Raw:
		b.WriteString("```text\n")
		b.WriteString(strings.TrimRight(it.Text, "\n") + "\n")
		for _, n := range it.Notes {
			b.WriteString(n + "\n")
		}
		b.WriteString("```\n")
		return
	case it.blank():
		b.WriteString("\n")
		return
	case it.Sub:
		b.WriteString("**" + it.Text + "**\n")
		return
	case prose:
		b.WriteString(it.Text + "\n")
		return
	}
	line := "-"
	// A plain bullet is what "-" already means in Markdown, so it is dropped
	// rather than doubled. Markers that carry meaning are kept.
	if it.Glyph != "" && it.Glyph != "•" {
		line += " " + it.Glyph
	}
	if it.Label != "" {
		line += " **[" + it.Label + "]**"
	}
	if it.Text != "" {
		line += " " + it.Text
	}
	b.WriteString(line + "\n")
	for _, n := range it.Notes {
		b.WriteString("  - " + n + "\n")
	}
}

// rule draws the underline beneath a document title. Its length tracks the
// title so the header reads as a unit, clamped so it never runs past the wrap
// column or shrinks to a stub.
func rule(title string) string {
	n := runeLen(title)
	if n < 8 {
		n = 8
	}
	if n > Width {
		n = Width
	}
	return strings.Repeat("─", n)
}

func runeLen(s string) int { return len([]rune(s)) }

// wrapLines greedily wraps text to width columns.
func wrapLines(text string, width int) []string {
	return wrapLines2(text, width, width)
}

// wrapLines2 wraps text with one budget for the first line and another for the
// rest, which is what a hanging indent needs.
//
// A single word longer than the budget is never broken: evidence refs, paths
// and commit shas must survive rendering intact or the citation stops being
// usable, and an unusable citation is worse than an overlong line.
func wrapLines2(text string, firstWidth, restWidth int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if firstWidth < 8 {
		firstWidth = 8
	}
	if restWidth < 8 {
		restWidth = 8
	}
	out := make([]string, 0, 4)
	cur := words[0]
	width := firstWidth
	for _, w := range words[1:] {
		if runeLen(cur)+1+runeLen(w) <= width {
			cur += " " + w
			continue
		}
		out = append(out, cur)
		cur = w
		width = restWidth
	}
	return append(out, cur)
}

// normalizeTrailer guarantees exactly one trailing newline, so golden files
// and shell pipelines see a stable end-of-output.
func normalizeTrailer(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// confidenceRank orders the three factual confidences from weakest to
// strongest. Recommended is deliberately absent: it marks a proposal, not a
// degree of belief, so it is never weighed against evidence.
func confidenceRank(c model.Confidence) int {
	switch c {
	case model.Unknown:
		return 1
	case model.Inferred:
		return 2
	case model.Observed:
		return 3
	}
	return 0
}

// claim resolves the §12 marker to print for a claim, plus any caveat the
// marker needs.
//
// A recorded confidence is honoured only up to what its evidence can carry.
// §12 defines OBSERVED as "directly supported by repository/checkpoint/test/
// event evidence", so a claim recorded as OBSERVED with no evidence is not an
// observation whatever the state says, and printing the strong marker would
// put a false signal exactly where the spec requires a reader to tell a fact
// from a guess at a glance — with the citation line underneath already saying
// the claim is unverified. A weaker recorded confidence is never raised: an
// extractor is allowed to be less sure than its evidence permits, only never
// more.
//
// The overstatement is reported rather than quietly corrected. Silently
// relabelling would leave the human view saying UNKNOWN while the machine
// handoff built from the same object still says observed, and those two must
// stay reconcilable (Rule 10, plan §24).
func claim(c model.Confidence, ev []model.Evidence) (string, []string) {
	derived := model.ConfidenceFor(ev)
	// An invalid or absent confidence is derived outright: a blank label would
	// be read as an unqualified fact.
	if !c.Valid() {
		return derived.Label(), nil
	}
	if c == model.Recommended || confidenceRank(c) <= confidenceRank(derived) {
		return c.Label(), nil
	}
	return derived.Label(), []string{fmt.Sprintf(
		"recorded as %s, but the evidence supports no more than %s; shown at the weaker rating (plan §12)",
		c.Label(), derived.Label())}
}

// label is claim without the caveat, for the few places that have nowhere to
// hang one.
func label(c model.Confidence, ev []model.Evidence) string {
	l, _ := claim(c, ev)
	return l
}

// sortedEvidence returns a deterministic, de-duplicated copy. The copy matters:
// model.SortEvidence sorts in place and a view must not reorder its caller's
// state.
func sortedEvidence(ev []model.Evidence) []model.Evidence {
	if len(ev) == 0 {
		return nil
	}
	out := append([]model.Evidence(nil), ev...)
	model.SortEvidence(out)
	return model.DedupeEvidence(out)
}

// evidenceNote renders the citation line hung under a claim. A claim with no
// evidence says so explicitly rather than omitting the line, because an absent
// citation and an unverified claim must not look the same (plan §11).
func evidenceNote(ev []model.Evidence) string {
	sorted := sortedEvidence(ev)
	if len(sorted) == 0 {
		return "evidence: none recorded — this claim is unverified"
	}
	refs := make([]string, 0, maxInlineEvidence)
	for i, e := range sorted {
		if i == maxInlineEvidence {
			break
		}
		refs = append(refs, e.String())
	}
	note := "evidence: " + strings.Join(refs, "; ")
	if extra := len(sorted) - len(refs); extra > 0 {
		note += fmt.Sprintf(" (+%d more)", extra)
	}
	return note
}

// ts formats a timestamp for display. A zero time prints "unknown" rather than
// year 1: a fabricated date is worse than an admitted gap.
func ts(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// shortHash trims a state hash for display while keeping enough of it to be
// distinguishing. The full value stays in the JSON handoff.
func shortHash(h string) string {
	if h == "" {
		return "unknown"
	}
	return h
}

// sortedStrings copies and orders a string slice so it can never leak the
// ordering of whatever map or scan produced it.
func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// normalizeState returns a sorted deep copy. Views are pure: they must not
// reorder or otherwise mutate the state their caller still holds.
func normalizeState(s *model.EngineeringState) *model.EngineeringState {
	c := s.Clone()
	c.Sort()
	return c
}

// normalizeLineage returns a sorted copy. model.Lineage methods sort in place
// and share backing arrays with the caller, so the slices are copied first.
func normalizeLineage(l model.Lineage) model.Lineage {
	out := model.Lineage{Task: l.Task}
	out.Sessions = append([]model.SessionNode(nil), l.Sessions...)
	out.Checkpoints = append([]model.CheckpointNode(nil), l.Checkpoints...)
	out.Handoffs = append([]model.HandoffNode(nil), l.Handoffs...)
	out.Sort()
	return out
}

// missingStateDoc is what every view prints when handed no state at all. It
// deliberately does not read as "nothing happened": a missing input is a gap
// in what we captured, not a finding about the work (plan §48).
func missingStateDoc() document {
	return document{
		Title: "NO ENGINEERING STATE",
		Sections: []section{{
			Title: "STATE UNAVAILABLE",
			Items: []item{{
				Glyph: "?",
				Label: model.Unknown.Label(),
				Text:  "No engineering state was supplied, so nothing can be reported about this task.",
				Notes: []string{
					"This is a missing input, not evidence that no work was done (plan §48).",
					"Safe next action: inspect the repository directly and re-capture the task state.",
				},
			}},
		}},
	}
}
