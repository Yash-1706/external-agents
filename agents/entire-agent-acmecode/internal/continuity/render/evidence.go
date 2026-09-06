package render

import (
	"fmt"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// EvidenceList renders a citation list. A non-positive limit means "all".
//
// Every entry is prefixed with its §31 priority level, because "commit abc123"
// and "an inference the model made" are not interchangeable support for the
// same claim and a reader must be able to tell them apart at a glance. When
// the limit truncates the list the remainder is announced: silently shortening
// evidence would understate how much support a claim actually has.
func EvidenceList(ev []model.Evidence, limit int) string {
	return evidenceSectionText(evidenceSection(ev, limit))
}

// allEvidence collects every citation this package can render.
//
// model.EngineeringState.AllEvidence walks eleven collections but not
// Constraints or ChangedFiles, so a citation attached only to a mid-task
// constraint or to a changed file is rendered next to its claim and then
// missing from the EVIDENCE index that is supposed to list it. The two are
// appended here; DedupeEvidence and the sort in sortedEvidence keep the result
// stable and free of repeats.
func allEvidence(s *model.EngineeringState) []model.Evidence {
	out := s.AllEvidence()
	for _, c := range s.Constraints {
		out = append(out, c.Evidence...)
	}
	for _, f := range s.ChangedFiles {
		out = append(out, f.Evidence...)
	}
	return model.DedupeEvidence(out)
}

func evidenceSectionText(s section) string {
	d := document{Sections: []section{s}}
	return d.Text()
}

// evidenceSection builds the EVIDENCE block shared by every view.
func evidenceSection(ev []model.Evidence, limit int) section {
	sec := section{Title: "EVIDENCE"}
	sorted := sortedEvidence(ev)
	if len(sorted) == 0 {
		// An empty evidence list is a finding in its own right: without
		// citations nothing here can be rated above UNKNOWN (plan §11).
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no evidence recorded",
			Notes: []string{"A claim with no evidence cannot be rated above UNKNOWN (plan §11)."},
		})
		return sec
	}
	shown := sorted
	if limit > 0 && limit < len(sorted) {
		shown = sorted[:limit]
	}
	for _, e := range shown {
		sec.Items = append(sec.Items, item{
			Glyph: fmt.Sprintf("[L%d]", int(e.Level())),
			Text:  e.String(),
		})
	}
	if extra := len(sorted) - len(shown); extra > 0 {
		sec.Items = append(sec.Items, item{
			Text: fmt.Sprintf("… %d further %s not shown (limit %d).", extra, plural(extra, "citation", "citations"), limit),
		})
	}
	return sec
}
