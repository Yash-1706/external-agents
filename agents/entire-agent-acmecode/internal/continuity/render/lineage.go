package render

import (
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Lineage renders the plan §51 task-lineage tree: who has worked on this task,
// in what order, and which checkpoints each session left behind.
//
// The tree is grouped by agent because the thing worth seeing at a glance is
// that the task crossed runtimes — a task carried from OpenClaw to Hermes to a
// human is the observable proof of cross-agent continuity (plan §44). Groups
// are ordered by first appearance and sessions chronologically, so the same
// lineage always draws the same tree.
func Lineage(l model.Lineage) string {
	ln := normalizeLineage(l)
	d := document{}
	d.add(lineageTreeSection(ln))
	d.add(lineageHandoffSection(ln))
	d.add(lineageSummarySection(ln))
	return d.Text()
}

// agentGroup is one branch of the §51 tree: a runtime, the sessions it ran and
// any checkpoints attributed to it that no known session claims.
type agentGroup struct {
	title    string
	sessions []model.SessionNode
	orphans  []model.CheckpointNode
}

func lineageTreeSection(l model.Lineage) section {
	sec := section{Title: "LINEAGE"}
	root := firstNonEmpty(l.Task.ID, "(unidentified task)")
	if t := strings.TrimSpace(l.Task.Title); t != "" {
		root += "  " + t
	}

	groups, bySession := agentGroups(l)
	if len(groups) == 0 {
		sec.Items = append(sec.Items, item{Raw: true, Text: root})
		// An empty lineage is a capture gap, not a finding. Rendering a bare
		// root with no comment would invite the reader to conclude that
		// nothing happened (plan §48).
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no sessions or checkpoints are recorded for this task",
			Notes: []string{"Its lineage cannot be reconstructed; this is a missing input, not evidence that no work happened (plan §48)."},
		})
		return sec
	}

	lines := []string{root}
	for i, g := range groups {
		lines = append(lines, "│")
		head, cont := treeBranch(i == len(groups)-1)
		lines = append(lines, head+g.title)
		lines = append(lines, groupLines(cont, g, bySession)...)
	}
	sec.Items = append(sec.Items, item{Raw: true, Text: strings.Join(lines, "\n")})
	return sec
}

// agentGroups partitions the lineage by agent runtime. Maps are used only for
// lookup; every slice that reaches the output is built by walking the already
// sorted lineage slices, so map iteration order can never leak into the tree.
func agentGroups(l model.Lineage) ([]agentGroup, map[string][]model.CheckpointNode) {
	agents := l.Agents()
	present := make(map[model.AgentKind]bool, len(agents))
	for _, a := range agents {
		present[a] = true
	}
	known := make(map[string]bool, len(l.Sessions))
	for _, s := range l.Sessions {
		known[s.SessionID] = true
	}

	bySession := make(map[string][]model.CheckpointNode, len(l.Checkpoints))
	var orphans []model.CheckpointNode
	for _, c := range l.Checkpoints {
		if c.SessionID != "" && known[c.SessionID] {
			bySession[c.SessionID] = append(bySession[c.SessionID], c)
			continue
		}
		orphans = append(orphans, c)
	}

	groups := make([]agentGroup, 0, len(agents)+1)
	for _, a := range agents {
		g := agentGroup{title: agentName(a)}
		for _, s := range l.Sessions {
			if s.Agent == a {
				g.sessions = append(g.sessions, s)
			}
		}
		for _, c := range orphans {
			if c.Agent == a {
				g.orphans = append(g.orphans, c)
			}
		}
		groups = append(groups, g)
	}

	// A checkpoint whose session is unknown is still real history and is never
	// dropped from the tree; it is shown where the reader can see that its
	// attribution is missing.
	var unattributed []model.CheckpointNode
	for _, c := range orphans {
		if c.Agent != "" && present[c.Agent] {
			continue
		}
		unattributed = append(unattributed, c)
	}
	if len(unattributed) > 0 {
		groups = append(groups, agentGroup{
			title:   "(checkpoints not attributable to a recorded session)",
			orphans: unattributed,
		})
	}
	return groups, bySession
}

func groupLines(cont string, g agentGroup, bySession map[string][]model.CheckpointNode) []string {
	total := len(g.sessions) + len(g.orphans)
	if total == 0 {
		return []string{cont + "└── (no sessions recorded)"}
	}
	out := make([]string, 0, total*2)
	idx := 0
	for _, s := range g.sessions {
		idx++
		head, sub := treeBranch(idx == total)
		out = append(out, cont+head+sessionLine(s))
		cps := bySession[s.SessionID]
		if len(cps) == 0 {
			// Saying so is the point: a session with no checkpoint left
			// nothing a later agent can resume from (plan §48).
			out = append(out, cont+sub+"└── (no checkpoint)")
			continue
		}
		for j, c := range cps {
			h2, _ := treeBranch(j == len(cps)-1)
			out = append(out, cont+sub+h2+checkpointLine(c))
		}
	}
	for _, c := range g.orphans {
		idx++
		head, _ := treeBranch(idx == total)
		out = append(out, cont+head+checkpointLine(c)+"  "+orphanNote(c))
	}
	return out
}

// orphanNote explains why a checkpoint could not be hung under a session.
// "no session was recorded" and "a session was recorded but is not in this
// lineage" are different gaps, and the recorded id is the thread a reader
// would pull to find the missing session, so it is never discarded.
func orphanNote(c model.CheckpointNode) string {
	if c.SessionID == "" {
		return "(no session recorded)"
	}
	return "(session " + c.SessionID + " is not in the recorded lineage)"
}

// treeBranch returns the connector for a child and the indent its own children
// must carry.
func treeBranch(last bool) (head, cont string) {
	if last {
		return "└── ", "    "
	}
	return "├── ", "│   "
}

func sessionLine(s model.SessionNode) string {
	role := firstNonEmpty(string(s.Role), "unknown role")
	end := ts(s.EndedAt)
	switch {
	case s.Interrupted:
		// An interrupted session's capture is incomplete by definition; the
		// tree marks it so its state is not read as final (plan §33).
		end = "interrupted"
	case s.EndedAt.IsZero():
		end = "active"
	}
	return fmt.Sprintf("%s  [%s]  %s → %s",
		firstNonEmpty(s.SessionID, "(unidentified session)"), role, ts(s.StartedAt), end)
}

func checkpointLine(c model.CheckpointNode) string {
	parts := []string{firstNonEmpty(c.CheckpointID, "(unidentified checkpoint)")}
	if c.Label != "" {
		parts = append(parts, `"`+c.Label+`"`)
	}
	if c.CommitSHA != "" {
		// Abbreviated in the tree only, to keep the drawing readable. The full
		// sha stays in the status view and in every evidence citation, so
		// nothing that has to be resolvable is shortened.
		parts = append(parts, "@"+shortSHA(c.CommitSHA))
	}
	parts = append(parts, ts(c.CreatedAt))
	return strings.Join(parts, "  ")
}

// shortSHA abbreviates a commit to the conventional git prefix length.
func shortSHA(sha string) string {
	r := []rune(sha)
	if len(r) <= 7 {
		return sha
	}
	return string(r[:7])
}

func lineageHandoffSection(l model.Lineage) section {
	sec := section{Title: "HANDOFFS"}
	for _, h := range l.Handoffs {
		it := item{
			Glyph: "•",
			Text: fmt.Sprintf("%s  %s → %s",
				firstNonEmpty(h.ID, "(unidentified handoff)"),
				agentLabel(h.FromAgent, h.FromSessionID),
				agentLabel(h.ToAgent, h.ToSessionID)),
		}
		if h.SourceCheckpoint != "" {
			it.Notes = append(it.Notes, "source checkpoint: "+h.SourceCheckpoint)
		} else {
			it.Notes = append(it.Notes, "source checkpoint: none — this handoff is not anchored to a checkpoint")
		}
		it.Notes = append(it.Notes, "state hash: "+shortHash(h.StateHash))
		it.Notes = append(it.Notes, fmt.Sprintf("outstanding: %d %s", h.Outstanding, plural(h.Outstanding, "requirement", "requirements")))
		it.Notes = append(it.Notes, fmt.Sprintf("critical failures: %d", h.CriticalFailures))
		it.Notes = append(it.Notes, "created: "+ts(h.CreatedAt))
		sec.Items = append(sec.Items, it)
	}
	return sec
}

// agentName is the display name for a runtime.
//
// Which runtimes touched a task is the one fact the §51 tree exists to show, so
// two different third-party runtimes must never draw two branches with the same
// title — that reads as one runtime appearing twice. model.AgentKind.Display
// now names any well-formed runtime under its own reported name, so the common
// case needs nothing special here. What remains is the two cases Display cannot
// speak for: an absent agent is reported as absent rather than as the
// AgentUnknown runtime, and a value too malformed to be an identity at all
// keeps its raw text beside the fallback so the reader can see what arrived.
func agentName(a model.AgentKind) string {
	if a == "" {
		return "(agent not recorded)"
	}
	if a.Valid() {
		return a.Display()
	}
	return a.Display() + " (" + string(a) + ")"
}

// agentLabel names one end of a handoff. The session id is included because a
// runtime name alone does not identify which session picked the task up.
func agentLabel(a model.AgentKind, session string) string {
	name := agentName(a)
	if session == "" {
		return name
	}
	return name + " (" + session + ")"
}

// lineageSummarySection is the compact lineage line embedded in the status and
// handoff views, where the full §51 tree would be too much.
func lineageSummarySection(l model.Lineage) section {
	sec := section{Title: "LINEAGE SUMMARY"}
	if len(l.Sessions) == 0 && len(l.Checkpoints) == 0 && len(l.Handoffs) == 0 {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "no lineage recorded for this task",
			Notes: []string{"Nothing establishes who worked on it or what a resume would start from (plan §48)."},
		})
		return sec
	}

	agents := l.Agents()
	if len(agents) > 0 {
		names := make([]string, 0, len(agents))
		for _, a := range agents {
			names = append(names, agentName(a))
		}
		it := item{Label: model.Observed.Label(), Text: "agents: " + strings.Join(names, " → ")}
		if len(agents) > 1 {
			// More than one runtime on one task is the observable proof of
			// cross-agent continuity, and is worth stating (plan §44).
			it.Notes = append(it.Notes, fmt.Sprintf("this task has crossed %d agent runtimes", len(agents)))
		}
		sec.Items = append(sec.Items, it)
	}

	interrupted := 0
	for _, s := range l.Sessions {
		if s.Interrupted {
			interrupted++
		}
	}
	sessions := fmt.Sprintf("sessions: %d", len(l.Sessions))
	if interrupted > 0 {
		sessions += fmt.Sprintf(" (%d interrupted — their capture is incomplete)", interrupted)
	}
	sec.Items = append(sec.Items, item{Label: model.Observed.Label(), Text: sessions})

	if cp, ok := l.LatestCheckpoint(); ok {
		sec.Items = append(sec.Items, item{
			Label: model.Observed.Label(),
			Text:  fmt.Sprintf("checkpoints: %d — latest %s (%s)", len(l.Checkpoints), cp.CheckpointID, ts(cp.CreatedAt)),
		})
	} else {
		sec.Items = append(sec.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "checkpoints: none — a resume cannot be verified against a checkpoint (plan §48)",
		})
	}

	sec.Items = append(sec.Items, item{
		Label: model.Observed.Label(),
		Text:  fmt.Sprintf("handoffs: %d", len(l.Handoffs)),
	})
	return sec
}

// Receipt renders the plan §25 handoff receipt: the small, checkable record
// that a specific state was transferred from one worker to another.
//
// The state hash is the load-bearing field. Without it the receipt says a
// handoff happened but not which state was handed over, so an absent hash is
// reported as a defect rather than left blank.
func Receipt(h model.HandoffNode) string {
	d := document{Title: "HANDOFF CREATED"}
	d.add(section{Title: "HANDOFF", Items: []item{{Text: firstNonEmpty(h.ID, "(unidentified handoff)")}}})
	d.add(section{Title: "TASK", Items: []item{{Text: firstNonEmpty(h.TaskID, "(unidentified task)")}}})
	d.add(section{Title: "FROM", Items: []item{{Text: agentLabel(h.FromAgent, h.FromSessionID)}}})
	d.add(section{Title: "TO", Items: []item{{Text: agentLabel(h.ToAgent, h.ToSessionID)}}})

	cp := section{Title: "SOURCE CHECKPOINT"}
	if h.SourceCheckpoint != "" {
		cp.Items = append(cp.Items, item{Label: model.Observed.Label(), Text: h.SourceCheckpoint})
	} else {
		cp.Items = append(cp.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "none — this handoff is not anchored to a checkpoint (plan §48)",
		})
	}
	d.add(cp)

	hash := section{Title: "STATE HASH"}
	if h.StateHash != "" {
		hash.Items = append(hash.Items, item{Text: h.StateHash})
	} else {
		hash.Items = append(hash.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "not recorded — this receipt cannot prove which state was handed over",
		})
	}
	d.add(hash)

	d.add(section{Title: "OUTSTANDING", Items: []item{{
		Text: fmt.Sprintf("%d %s not yet complete", h.Outstanding, plural(h.Outstanding, "requirement", "requirements")),
	}}})
	d.add(section{Title: "CRITICAL FAILURES", Items: []item{{
		Text: fmt.Sprintf("%d failing %s", h.CriticalFailures, plural(h.CriticalFailures, "test", "tests")),
	}}})

	next := section{Title: "NEXT ACTION"}
	if strings.TrimSpace(h.NextAction) != "" {
		next.Items = append(next.Items, item{
			Glyph: "→",
			Label: model.Recommended.Label(),
			Text:  strings.TrimSpace(h.NextAction),
		})
	} else {
		next.Items = append(next.Items, item{
			Glyph: "?",
			Label: model.Unknown.Label(),
			Text:  "not recorded — inspect the repository and confirm with a human (plan §47)",
		})
	}
	d.add(next)

	d.add(section{Title: "CREATED", Items: []item{{Text: ts(h.CreatedAt)}}})
	return d.Text()
}
