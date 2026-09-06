package render

import (
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// handoffEvidenceLimit caps the citation list in a handoff. A handoff is a
// compact package meant to be pasted into another agent's context (plan §19,
// §49); the full citation list stays available through Status and the JSON
// handoff, and the truncation is always announced.
const handoffEvidenceLimit = 12

// Snapshot renders the plan §18 task snapshot: what this task is, why it
// exists, what happened, where it stands, what remains and what should happen
// next, with the evidence behind each answer.
func Snapshot(s *model.EngineeringState) string {
	if s == nil {
		return missingStateDoc().Text()
	}
	st := normalizeState(s)
	d := document{}
	for _, sec := range snapshotSections(st) {
		d.add(sec)
	}
	d.add(evidenceSection(allEvidence(st), 0))
	return d.Text()
}

// Status renders the `entire task status` view: the §18 snapshot plus the
// cross-agent lineage context that says who has worked on the task and what
// checkpoint a resume would start from.
func Status(s *model.EngineeringState, l model.Lineage) string {
	if s == nil {
		return missingStateDoc().Text()
	}
	st := normalizeState(s)
	ln := normalizeLineage(l)
	d := document{}
	for _, sec := range snapshotSections(st) {
		d.add(sec)
	}
	d.add(lineageSummarySection(ln))
	d.add(evidenceSection(allEvidence(st), 0))
	return d.Text()
}

// Handoff renders the plan §19 human handoff: the task state compressed into
// something another developer or agent can act on immediately.
func Handoff(s *model.EngineeringState, l model.Lineage) string {
	if s == nil {
		return missingStateDoc().Text()
	}
	return handoffDocument(normalizeState(s), normalizeLineage(l)).Text()
}

// HandoffMarkdown renders the same handoff as Markdown. It is built from the
// same document as Handoff, from the same state: the human and machine views
// are projections of one object and are never assembled separately (plan §24,
// Rule 10).
func HandoffMarkdown(s *model.EngineeringState, l model.Lineage) string {
	if s == nil {
		return missingStateDoc().Markdown()
	}
	return handoffDocument(normalizeState(s), normalizeLineage(l)).Markdown()
}

// snapshotSections builds the §18 body shared by Snapshot and Status. The
// STATE PARTIALLY RECOVERED block sits immediately after STATUS, before any
// claim is made, so a reader learns the state is degraded before they read
// anything that might be affected by the gap (plan §33, §47).
func snapshotSections(s *model.EngineeringState) []section {
	out := []section{
		taskSection(s),
		intentSection(s),
		statusSection(s),
	}
	if partial, ok := partialCaptureSection(s.Capture); ok {
		out = append(out, partial)
	}
	out = append(out,
		requirementsSection(s),
		// Plan §18 asks "What has happened?". CompletedWork and InProgress are
		// the state's answer to it, and omitting them left their citations
		// stranded in the EVIDENCE block with no claim to support.
		workSection("WORK COMPLETED", "✓", s.CompletedWork),
		workSection("WORK IN PROGRESS", "⚠", s.InProgress),
		constraintsSection(s),
		decisionsSection(s),
		supersededDecisionsSection(s),
		rejectedSection(s),
		failedAttemptsSection(s),
		assumptionsSection(s),
		risksSection(s),
		filesSection(s),
		testsSection(s),
		nextActionsSection(s),
	)
	return out
}

func handoffDocument(s *model.EngineeringState, l model.Lineage) document {
	title := firstNonEmpty(s.Task.Title, s.Task.ID)
	if title == "" {
		title = "untitled task"
	}
	d := document{
		Title:    strings.ToUpper(title),
		Subtitle: "Task " + firstNonEmpty(s.Task.ID, "(unidentified)"),
	}
	d.add(intentSectionNamed(s, "ORIGINAL INTENT"))
	d.add(statusSection(s))
	if partial, ok := partialCaptureSection(s.Capture); ok {
		d.add(partial)
	}
	d.add(handoffCompletedSection(s))
	d.add(handoffIncompleteSection(s))
	d.add(handoffFailedSection(s))
	d.add(constraintsSection(s))
	d.add(decisionsSectionNamed(s, "IMPORTANT DECISIONS"))
	d.add(rejectedSection(s))
	d.add(risksSection(s))
	d.add(filesSection(s))
	d.add(nextActionsSection(s))
	d.add(handoffCheckpointSection(l))
	d.add(lineageSummarySection(l))
	d.add(stateHashSection(s))
	d.add(evidenceSection(allEvidence(s), handoffEvidenceLimit))
	return d
}

func taskSection(s *model.EngineeringState) section {
	id := firstNonEmpty(s.Task.ID, "(unidentified task)")
	title := firstNonEmpty(s.Task.Title, "(untitled task)")
	return section{Title: "TASK", Items: []item{{Text: id + " — " + title}}}
}

func intentSection(s *model.EngineeringState) section {
	return intentSectionNamed(s, "INTENT")
}

func intentSectionNamed(s *model.EngineeringState, title string) section {
	sec := section{Title: title}
	if intent := strings.TrimSpace(s.Task.OriginalIntent); intent != "" {
		sec.Items = append(sec.Items, item{Text: intent})
		return sec
	}
	// Reconstructing intent from a diff is exactly the fabrication this product
	// exists to avoid, so the absence is reported instead (plan §33).
	sec.Items = append(sec.Items, item{
		Glyph: "?",
		Label: model.Unknown.Label(),
		Text:  "the original intent was not captured",
		Notes: []string{"Do not infer it from the diff; ask the originating human or agent."},
	})
	return sec
}

func statusSection(s *model.EngineeringState) section {
	sec := section{Title: "STATUS"}
	sec.Items = append(sec.Items, item{
		Text: "recorded: " + firstNonEmpty(string(s.Status), "(unrecorded)"),
	})

	// DeriveStatus recomputes the status from the evidence alone. When it
	// disagrees with the stored value the disagreement is surfaced rather than
	// resolved silently: the evidence-backed value is the defensible one and
	// the reader is entitled to see both (plan §17, §31).
	derived := s.DeriveStatus()
	if derived != s.Status {
		sec.Items = append(sec.Items, item{
			Text:  "derived from evidence: " + string(derived),
			Notes: []string{"The recorded status and the evidence disagree; trust the derived value (plan §17)."},
		})
	}

	complete, total := s.CountRequirements()
	if total > 0 {
		sec.Items = append(sec.Items, item{
			Text: fmt.Sprintf("%d / %d requirements complete", complete, total),
		})
	} else {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no requirements recorded, so completeness cannot be judged",
		})
	}

	sec.Items = append(sec.Items, testSummaryItem(s))
	sec.Items = append(sec.Items, repoItem(s))
	return sec
}

func testSummaryItem(s *model.EngineeringState) item {
	if len(s.Tests) == 0 {
		if !s.Capture.TestResultsParsed {
			return item{
				Glyph: "?",
				Label: model.Unknown.Label(),
				Text:  "test results were not captured; passing cannot be claimed",
			}
		}
		return item{
			Label: model.Observed.Label(),
			Text:  "no test results recorded",
		}
	}
	var passed, failed, skipped, unknown int
	for _, t := range s.Tests {
		switch t.Status {
		case model.TestPassed:
			passed++
		case model.TestFailed:
			failed++
		case model.TestSkipped:
			skipped++
		default:
			unknown++
		}
	}
	txt := fmt.Sprintf("tests: %d passed, %d failed, %d skipped, %d unknown", passed, failed, skipped, unknown)
	glyph := "✓"
	if failed > 0 {
		glyph = "✗"
	} else if skipped > 0 || unknown > 0 {
		glyph = "⚠"
	}
	return item{Glyph: glyph, Label: model.Observed.Label(), Text: txt}
}

func repoItem(s *model.EngineeringState) item {
	r := s.Repo
	if r.Repo == "" && r.Branch == "" && r.CommitSHA == "" {
		return item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "repository state was not captured",
		}
	}
	parts := []string{"repo:", firstNonEmpty(r.Repo, "(unnamed)")}
	parts = append(parts, "branch", firstNonEmpty(r.Branch, "(unknown)"))
	parts = append(parts, "@", firstNonEmpty(r.CommitSHA, "(unknown commit)"))
	if r.Dirty {
		// A dirty tree means the recorded commit does not describe what is on
		// disk, which changes what a resume can safely assume (plan §23).
		parts = append(parts, "(working tree dirty)")
	}
	return item{Label: model.Observed.Label(), Text: strings.Join(parts, " ")}
}

func requirementsSection(s *model.EngineeringState) section {
	sec := section{Title: "REQUIREMENTS"}
	if len(s.Requirements) == 0 {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no requirements recorded",
			Notes: []string{"Requirements were not extracted, so nothing here says the task is done (plan §33)."},
		})
		return sec
	}
	complete, total := s.CountRequirements()
	sec.Items = append(sec.Items, item{Text: fmt.Sprintf("%d / %d complete", complete, total)})
	for _, r := range s.Requirements {
		sec.Items = append(sec.Items, requirementItem(r))
	}
	return sec
}

func requirementItem(r model.Requirement) item {
	lbl, caveats := claim(r.Confidence, r.Evidence)
	it := item{
		Glyph: r.Status.Glyph(),
		Label: lbl,
		Text:  strings.TrimSpace(firstNonEmpty(r.ID, "") + " " + r.Description),
	}
	if src := strings.TrimSpace(r.Source); src != "" {
		it.Notes = append(it.Notes, "source: "+src)
	}
	if r.SupersededBy != "" {
		// Superseded requirements are annotated, never rewritten (plan §41).
		it.Notes = append(it.Notes, "changed by constraint: "+r.SupersededBy)
	}
	it.Notes = append(it.Notes, caveats...)
	it.Notes = append(it.Notes, evidenceNote(r.Evidence))
	return it
}

func constraintsSection(s *model.EngineeringState) section {
	sec := section{Title: "CONSTRAINTS ADDED MID-TASK"}
	if len(s.Constraints) == 0 {
		return sec
	}
	// Constraints are additive: they annotate which requirements and
	// assumptions they affect and never overwrite the original intent
	// (plan §41).
	sec.Items = append(sec.Items, item{
		Text: "These arrived after the task started. They add to the original intent; they do not replace it (plan §41).",
	})
	for _, c := range s.Constraints {
		it := item{
			Glyph: "•",
			Label: label("", c.Evidence),
			Text:  strings.TrimSpace(firstNonEmpty(c.ID, "") + " " + c.Text),
		}
		if src := strings.TrimSpace(c.Source); src != "" {
			it.Notes = append(it.Notes, "source: "+src)
		}
		if affected := sortedStrings(c.AffectedRequirements); len(affected) > 0 {
			it.Notes = append(it.Notes, "affects requirements: "+strings.Join(affected, ", "))
		}
		if changed := sortedStrings(c.ChangedAssumptions); len(changed) > 0 {
			it.Notes = append(it.Notes, "invalidates assumptions: "+strings.Join(changed, "; "))
		}
		it.Notes = append(it.Notes, "added: "+ts(c.AddedAt))
		it.Notes = append(it.Notes, evidenceNote(c.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func decisionsSection(s *model.EngineeringState) section {
	return decisionsSectionNamed(s, "DECISIONS")
}

func decisionsSectionNamed(s *model.EngineeringState, title string) section {
	sec := section{Title: title}
	for _, d := range s.ActiveDecisions() {
		sec.Items = append(sec.Items, decisionItem(d))
	}
	return sec
}

// supersededDecisionsSection keeps replaced decisions visible. Plan §32
// requires superseded decisions to be retained rather than deleted, so a later
// reader can see that a question was already settled once and why the answer
// changed.
func supersededDecisionsSection(s *model.EngineeringState) section {
	sec := section{Title: "SUPERSEDED DECISIONS"}
	for _, d := range s.Decisions {
		if d.Active() {
			continue
		}
		it := decisionItem(d)
		it.Notes = append([]string{"superseded by: " + d.SupersededBy}, it.Notes...)
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func decisionItem(d model.Decision) item {
	// Decision carries no Confidence field, so it is derived from the evidence
	// through the §12 chokepoint rather than assumed to be a fact.
	it := item{
		Glyph: "•",
		Label: label("", d.Evidence),
		Text:  strings.TrimSpace(firstNonEmpty(d.ID, "") + " " + d.Decision),
	}
	if reason := strings.TrimSpace(d.Reason); reason != "" {
		it.Notes = append(it.Notes, "why: "+reason)
	} else {
		it.Notes = append(it.Notes, "why: not recorded")
	}
	if where := decisionOrigin(d); where != "" {
		it.Notes = append(it.Notes, where)
	}
	it.Notes = append(it.Notes, evidenceNote(d.Evidence))
	return it
}

func decisionOrigin(d model.Decision) string {
	var parts []string
	if d.SessionID != "" {
		parts = append(parts, "session "+d.SessionID)
	}
	if d.CheckpointID != "" {
		parts = append(parts, "checkpoint "+d.CheckpointID)
	}
	if !d.MadeAt.IsZero() {
		parts = append(parts, "at "+ts(d.MadeAt))
	}
	if len(parts) == 0 {
		return ""
	}
	return "made in: " + strings.Join(parts, ", ")
}

func rejectedSection(s *model.EngineeringState) section {
	sec := section{Title: "REJECTED APPROACHES"}
	if len(s.Rejected) == 0 {
		return sec
	}
	// Recording these is what stops the next worker rediscovering the same
	// dead end (plan §15).
	for _, r := range s.Rejected {
		lbl, caveats := claim(r.Confidence, r.Evidence)
		it := item{
			Glyph: "✗",
			Label: lbl,
			Text:  strings.TrimSpace(firstNonEmpty(r.ID, "") + " " + r.Approach),
		}
		if reason := strings.TrimSpace(r.Reason); reason != "" {
			it.Notes = append(it.Notes, "why not: "+reason)
		} else {
			it.Notes = append(it.Notes, "why not: not recorded")
		}
		it.Notes = append(it.Notes, caveats...)
		it.Notes = append(it.Notes, evidenceNote(r.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func failedAttemptsSection(s *model.EngineeringState) section {
	sec := section{Title: "FAILED ATTEMPTS"}
	for _, f := range s.FailedAttempts {
		it := item{
			Glyph: "✗",
			Label: label("", f.Evidence),
			Text:  f.Description,
		}
		if f.SessionID != "" {
			it.Notes = append(it.Notes, "session: "+f.SessionID)
		}
		if !f.OccurredAt.IsZero() {
			it.Notes = append(it.Notes, "at: "+ts(f.OccurredAt))
		}
		it.Notes = append(it.Notes, evidenceNote(f.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func assumptionsSection(s *model.EngineeringState) section {
	sec := section{Title: "ASSUMPTIONS"}
	for _, a := range s.Assumptions {
		glyph := "•"
		if a.Invalidated {
			glyph = "✗"
		}
		lbl, caveats := claim(a.Confidence, a.Evidence)
		it := item{
			Glyph: glyph,
			Label: lbl,
			Text:  a.Statement,
		}
		if a.Invalidated {
			// Retained rather than removed: a later reader needs to know the
			// task once relied on this (plan §32).
			it.Notes = append(it.Notes, "invalidated by later evidence — do not rely on this")
		}
		it.Notes = append(it.Notes, caveats...)
		it.Notes = append(it.Notes, evidenceNote(a.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func risksSection(s *model.EngineeringState) section {
	sec := section{Title: "RISKS"}
	for _, r := range s.Risks {
		lbl, caveats := claim(r.Confidence, r.Evidence)
		it := item{
			Glyph: "⚠",
			Label: lbl,
			Text:  r.Description,
		}
		if sev := strings.TrimSpace(r.Severity); sev != "" {
			it.Notes = append(it.Notes, "severity: "+sev)
		}
		it.Notes = append(it.Notes, caveats...)
		it.Notes = append(it.Notes, evidenceNote(r.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func filesSection(s *model.EngineeringState) section {
	sec := section{Title: "FILES"}
	if len(s.ChangedFiles) == 0 {
		// "No files changed" and "we could not look" are different claims and
		// must not render identically (plan §33).
		if s.Capture.GitAvailable {
			sec.Items = append(sec.Items, item{
				Label: model.Observed.Label(),
				Text:  "no files changed",
			})
		} else {
			sec.Items = append(sec.Items, item{
				Glyph: "?",
				Label: model.Unknown.Label(),
				Text:  "changed files were not captured (git state unavailable)",
			})
		}
		return sec
	}
	// The file list is one claim with one provenance, so the §12 label belongs
	// on the list rather than repeated per row. With git available the diff is
	// a Level 1 repository fact; without it the list came from something
	// weaker and must not be presented as a verified diff (plan §33).
	count := fmt.Sprintf("%d %s changed", len(s.ChangedFiles), plural(len(s.ChangedFiles), "file", "files"))
	if s.Capture.GitAvailable {
		sec.Items = append(sec.Items, item{Label: model.Observed.Label(), Text: count})
	} else {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  count + ", but git state was unavailable — this list was not verified against the repository",
		})
	}
	for _, f := range s.ChangedFiles {
		it := item{
			Glyph: firstNonEmpty(f.Status, "?"),
			Text:  f.Path,
		}
		if f.Insert > 0 || f.Delete > 0 {
			it.Text += fmt.Sprintf(" +%d -%d", f.Insert, f.Delete)
		}
		// Only cited when the state actually carries a citation. The list's own
		// provenance is stated above, so a "none recorded" line under every row
		// would be noise; a citation that exists must never be dropped, which
		// is what happened before.
		if len(f.Evidence) > 0 {
			it.Notes = append(it.Notes, evidenceNote(f.Evidence))
		}
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func testsSection(s *model.EngineeringState) section {
	sec := section{Title: "TESTS"}
	if len(s.Tests) == 0 {
		sec.Items = append(sec.Items, testSummaryItem(s))
		return sec
	}
	for _, t := range s.Tests {
		it := item{
			Glyph: testGlyph(t.Status),
			Label: label("", t.Evidence),
			Text:  t.Name + " — " + string(t.Status),
		}
		if t.Package != "" {
			it.Notes = append(it.Notes, "package: "+t.Package)
		}
		if t.Command != "" {
			it.Notes = append(it.Notes, "command: "+t.Command)
		}
		if !t.RanAt.IsZero() {
			it.Notes = append(it.Notes, "ran at: "+ts(t.RanAt))
		}
		if files := sortedStrings(t.Files); len(files) > 0 {
			it.Notes = append(it.Notes, "files: "+strings.Join(files, ", "))
		}
		it.Notes = append(it.Notes, evidenceNote(t.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func testGlyph(st model.TestStatus) string {
	switch st {
	case model.TestPassed:
		return "✓"
	case model.TestFailed:
		return "✗"
	case model.TestSkipped:
		return "⚠"
	}
	return "?"
}

func nextActionsSection(s *model.EngineeringState) section {
	sec := section{Title: "NEXT ACTION"}
	if len(s.NextActions) == 0 {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no next action was recorded",
			Notes: []string{"Safe next action: inspect the repository and confirm with a human before continuing (plan §47)."},
		})
		return sec
	}
	for _, a := range s.NextActions {
		// A next action is a proposal, never a statement of fact, so it prints
		// RECOMMENDED however strong its supporting evidence is (plan §12).
		// Deriving the label from evidence here would let a proposal be shown
		// as an observation, which is the one direction the label must never
		// overstate in.
		it := item{
			Glyph: "→",
			Label: model.Recommended.Label(),
			Text:  a.Description,
		}
		if r := strings.TrimSpace(a.Rationale); r != "" {
			it.Notes = append(it.Notes, "why: "+r)
		}
		if a.Target != "" {
			it.Notes = append(it.Notes, "target: "+a.Target)
		}
		it.Notes = append(it.Notes, fmt.Sprintf("priority: %d", a.Priority))
		it.Notes = append(it.Notes, graphNote(a.GraphVerified, s.Capture.GraphAvailable))
		if tests := sortedStrings(a.SuggestedTests); len(tests) > 0 {
			it.Notes = append(it.Notes, "suggested tests: "+strings.Join(tests, ", "))
		}
		it.Notes = append(it.Notes, evidenceNote(a.Evidence))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

// graphNote reports whether Graph impact analysis actually backs an action.
//
// Plan §48 is explicit: when the graph is unavailable, mark the verification
// unavailable and do not pretend it occurred. A stale GraphVerified flag
// carried over from an earlier capture would otherwise print "impact analysis
// backs this action" in the same document whose capture block says the graph
// was unreachable — a self-contradiction, and on the side that overstates.
func graphNote(verified, available bool) string {
	switch {
	case verified && available:
		return "graph verified: yes — impact analysis backs this action"
	case verified && !available:
		return "graph verified: recorded as yes, but the graph was unavailable during this capture, so the impact analysis could not be confirmed (plan §48)"
	default:
		// An unverified action is still a guess about blast radius, and saying
		// so is cheaper than a wrong edit (plan §35).
		return "graph verified: no — blast radius is unconfirmed"
	}
}

func handoffCompletedSection(s *model.EngineeringState) section {
	sec := section{Title: "COMPLETED"}
	for _, r := range s.Requirements {
		if r.Status == model.ReqComplete {
			sec.Items = append(sec.Items, requirementItem(r))
		}
	}
	for _, w := range s.CompletedWork {
		sec.Items = append(sec.Items, workItem("✓", w))
	}
	if len(sec.Items) == 0 {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "nothing is recorded as complete",
		})
	}
	return sec
}

func handoffIncompleteSection(s *model.EngineeringState) section {
	sec := section{Title: "INCOMPLETE"}
	for _, r := range s.Outstanding() {
		sec.Items = append(sec.Items, requirementItem(r))
	}
	for _, w := range s.InProgress {
		sec.Items = append(sec.Items, workItem("⚠", w))
	}
	if len(sec.Items) == 0 {
		// "Everything is done" is only defensible when there were requirements
		// to finish. With none recorded, an empty INCOMPLETE section means we
		// do not know what was outstanding, not that nothing was.
		if len(s.Requirements) == 0 {
			sec.Items = append(sec.Items, item{
				Glyph: "?",
				Label: model.Unknown.Label(),
				Text:  "no requirements were recorded, so nothing can be called outstanding",
			})
		} else {
			sec.Items = append(sec.Items, item{
				Glyph: "✓",
				Label: model.Observed.Label(),
				Text:  "every recorded requirement is complete",
			})
		}
	}
	return sec
}

func handoffFailedSection(s *model.EngineeringState) section {
	sec := section{Title: "FAILED"}
	for _, t := range s.FailingTests() {
		it := item{
			Glyph: "✗",
			Label: label("", t.Evidence),
			Text:  t.Name,
		}
		if t.Command != "" {
			it.Notes = append(it.Notes, "command: "+t.Command)
		}
		it.Notes = append(it.Notes, evidenceNote(t.Evidence))
		sec.Items = append(sec.Items, it)
	}
	sec.Items = append(sec.Items, failedAttemptsSection(s).Items...)
	if len(sec.Items) == 0 {
		// The section is kept even when empty. A handoff that simply omits
		// FAILED reads as "nothing failed"; what we can actually support is
		// "nothing failing was recorded", and only when tests were parsed.
		if s.Capture.TestResultsParsed {
			sec.Items = append(sec.Items, item{
				Label: model.Observed.Label(),
				Text:  "no failing tests or failed attempts recorded",
			})
		} else {
			sec.Items = append(sec.Items, item{
				Glyph: "?",
				Label: model.Unknown.Label(),
				Text:  "no failures recorded, but test results were never parsed — absence of failure is not evidence of success",
			})
		}
	}
	return sec
}

// workSection renders a list of work items. An empty list yields an empty
// section, which document.add drops: the snapshot already reports the absence
// of recorded work through REQUIREMENTS and STATUS, and an extra "nothing
// recorded" line per empty list would dilute the warnings that matter.
func workSection(title, glyph string, items []model.WorkItem) section {
	sec := section{Title: title}
	for _, w := range items {
		sec.Items = append(sec.Items, workItem(glyph, w))
	}
	return sec
}

func workItem(glyph string, w model.WorkItem) item {
	lbl, caveats := claim(w.Confidence, w.Evidence)
	it := item{
		Glyph: glyph,
		Label: lbl,
		Text:  w.Description,
	}
	if w.SessionID != "" {
		it.Notes = append(it.Notes, "session: "+w.SessionID)
	}
	it.Notes = append(it.Notes, caveats...)
	it.Notes = append(it.Notes, evidenceNote(w.Evidence))
	return it
}

func handoffCheckpointSection(l model.Lineage) section {
	sec := section{Title: "CHECKPOINT"}
	cp, ok := l.LatestCheckpoint()
	if !ok {
		// No checkpoint means a resume cannot be verified against a known-good
		// state, and the reader must be told rather than left to assume one
		// exists (plan §48).
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no checkpoint recorded for this task",
			Notes: []string{"A resume cannot be verified against a checkpoint; re-verify from the repository."},
		})
		return sec
	}
	// The checkpoint record itself is a Level 2 observation, so it is labelled
	// OBSERVED — but "a checkpoint exists" and "a resume can be verified
	// against it" are different claims. A checkpoint with no commit sha cannot
	// be tied to a repository state and one with no state hash cannot be
	// compared against anything, so each gap is named here instead of being
	// rendered as an absent line. Leaving them out is what would let an
	// unverifiable checkpoint read as a safe resume point (plan §22, §48).
	it := item{
		Glyph: "✓",
		Label: model.Observed.Label(),
		Text:  cp.CheckpointID + " — " + firstNonEmpty(cp.Label, "(unlabelled)"),
	}
	if cp.CommitSHA != "" {
		it.Notes = append(it.Notes, "commit: "+cp.CommitSHA)
	} else {
		it.Glyph = "⚠"
		it.Notes = append(it.Notes, "commit: not recorded — this checkpoint cannot be tied to a repository state")
	}
	if cp.Branch != "" {
		it.Notes = append(it.Notes, "branch: "+cp.Branch)
	}
	it.Notes = append(it.Notes, "created: "+ts(cp.CreatedAt))
	if cp.StateHash != "" {
		it.Notes = append(it.Notes, "state hash: "+cp.StateHash)
	} else {
		it.Glyph = "⚠"
		it.Notes = append(it.Notes, "state hash: not recorded — a resume cannot be verified against this checkpoint (plan §48)")
	}
	// A checkpoint whose session is not in the lineage is still the latest one
	// recorded, but the reader is entitled to know its provenance is broken
	// before they resume from it.
	if _, ok := l.Session(cp.SessionID); !ok {
		it.Glyph = "⚠"
		if cp.SessionID == "" {
			it.Notes = append(it.Notes, "session: not recorded — this checkpoint is not attributable to a session")
		} else {
			it.Notes = append(it.Notes, "session: "+cp.SessionID+" is not in the recorded lineage")
		}
	}
	sec.Items = append(sec.Items, it)
	return sec
}

func stateHashSection(s *model.EngineeringState) section {
	// The hash is what lets a receiver prove the handoff they read is the
	// handoff that was produced (plan §25).
	return section{Title: "STATE HASH", Items: []item{{Text: shortHash(s.Hash())}}}
}
