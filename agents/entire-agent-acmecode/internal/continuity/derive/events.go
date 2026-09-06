package derive

import (
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// deriveSessions rebuilds session lineage from the event stream.
//
// Sessions are reconstructed from lifecycle events only; a sub-agent is just a
// session with a parent, so SubagentStarted/SubagentEnded are treated exactly
// like their session-level counterparts (plan §9).
//
// A session that is referenced by ordinary events but never announced by a
// start event is still recorded, because dropping it would hide work that
// demonstrably happened. It is reported as an incomplete timeline instead
// (plan §48, "agent event missing").
func deriveSessions(events []model.AgentEvent) ([]model.SessionNode, []string) {
	order := make([]string, 0, len(events))
	byID := make(map[string]*model.SessionNode, len(events))
	// announced records sessions we saw an explicit start event for. The rest
	// were only inferred from surrounding activity.
	announced := make(map[string]bool, len(events))

	// touch returns the node for a session, creating it from the first event
	// that mentions it. Events arrive chronologically, so that first event also
	// gives the earliest moment the session is known to have existed.
	touch := func(ev model.AgentEvent) *model.SessionNode {
		n, ok := byID[ev.SessionID]
		if !ok {
			n = &model.SessionNode{
				SessionID: ev.SessionID,
				Agent:     ev.Agent,
				Role:      ev.Role,
				StartedAt: ev.Timestamp,
			}
			byID[ev.SessionID] = n
			order = append(order, ev.SessionID)
		}
		// Parent, agent and role are filled from whichever event first supplies
		// them; none of them is ever overwritten with a weaker value.
		if n.ParentSessionID == "" && ev.ParentSessionID != "" {
			n.ParentSessionID = ev.ParentSessionID
		}
		if (n.Agent == "" || n.Agent == model.AgentUnknown) && ev.Agent != "" {
			n.Agent = ev.Agent
		}
		if n.Role == "" && ev.Role != "" {
			n.Role = ev.Role
		}
		return n
	}

	for _, ev := range events {
		n := touch(ev)
		switch ev.Type {
		case model.SessionStarted, model.SubagentStarted:
			announced[ev.SessionID] = true
		case model.SessionEnded, model.SubagentEnded:
			n.EndedAt = ev.Timestamp
		case model.SessionInterrupted:
			// An interrupted session gets no EndedAt on purpose: its captured
			// state is incomplete, not final (see model.SessionNode).
			n.Interrupted = true
		}
	}

	out := make([]model.SessionNode, 0, len(order))
	var notes []string
	for _, id := range order {
		out = append(out, *byID[id])
		if !announced[id] {
			notes = append(notes, fmt.Sprintf(
				"session %q was observed without a start event; its lineage is incomplete", id))
		}
	}
	return out, notes
}

// sessionEvidence cites each session as Level 2 evidence. model.SessionNode
// carries no Evidence field of its own, so the citation lives on the state
// (plan §31).
func sessionEvidence(sessions []model.SessionNode) []model.Evidence {
	out := make([]model.Evidence, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, model.Evidence{
			Kind:       model.EvidenceSession,
			Ref:        s.SessionID,
			Result:     sessionOutcome(s),
			Detail:     s.Agent.Display(),
			SessionID:  s.SessionID,
			CapturedAt: s.StartedAt,
		})
	}
	return out
}

// sessionOutcome names how the session stopped, so a reader can tell a clean
// finish from one that was cut off mid-flight.
func sessionOutcome(s model.SessionNode) string {
	switch {
	case s.Interrupted:
		return "interrupted"
	case !s.EndedAt.IsZero():
		return "ended"
	default:
		return "active"
	}
}

// deriveCheckpoints reads checkpoint lineage out of CheckpointCreated events.
//
// A checkpoint event that does not name its checkpoint cannot be cited later,
// and an uncitable checkpoint is not evidence. Such an event is reported as a
// gap rather than recorded as a checkpoint, so that Capture.CheckpointAvailable
// keeps meaning "there is a checkpoint you can actually resume from"
// (plan §48, "checkpoint unavailable").
func deriveCheckpoints(events []model.AgentEvent) ([]model.CheckpointNode, []string) {
	out := make([]model.CheckpointNode, 0, len(events))
	seen := make(map[string]bool, len(events))
	var unidentified int

	for _, ev := range events {
		if ev.Type != model.CheckpointCreated {
			continue
		}
		id := strings.TrimSpace(ev.Attr(attrCheckpointID))
		if id == "" {
			unidentified++
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true

		label := ev.Attr(attrCheckpointLabel)
		if label == "" {
			// The summary is the adapter's redaction-safe description, which is
			// the closest thing to a label we are allowed to persist (§34).
			label = ev.Summary
		}
		out = append(out, model.CheckpointNode{
			CheckpointID: id,
			SessionID:    ev.SessionID,
			Agent:        ev.Agent,
			Label:        label,
			CommitSHA:    ev.Attr(attrCommitSHA),
			Branch:       ev.Attr(attrBranch),
			CreatedAt:    ev.Timestamp,
			StateHash:    ev.Attr(attrStateHash),
		})
	}

	var notes []string
	if unidentified > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d checkpoint event(s) carried no %s and cannot be cited or resumed from",
			unidentified, attrCheckpointID))
	}
	return out, notes
}

// checkpointEvidence cites each checkpoint as Level 2 evidence.
func checkpointEvidence(checkpoints []model.CheckpointNode) []model.Evidence {
	out := make([]model.Evidence, 0, len(checkpoints))
	for _, c := range checkpoints {
		out = append(out, model.Evidence{
			Kind:       model.EvidenceCheckpoint,
			Ref:        c.CheckpointID,
			Detail:     c.Label,
			SessionID:  c.SessionID,
			CapturedAt: c.CreatedAt,
		})
	}
	return out
}

// deriveTests records test outcomes that an adapter reported in structured
// form. Only ToolUsed and TurnEnded events carry them (plan §30, "test
// command/result when structured").
//
// Nothing here parses test output: if an adapter ran a test command but could
// not say which test produced which result, that is reported as a gap. Turning
// unstructured output into a claim of "tests pass" is precisely the fabrication
// plan §16 forbids.
func deriveTests(events []model.AgentEvent) ([]model.TestResult, []string) {
	order := make([]string, 0, len(events))
	byName := make(map[string]*model.TestResult, len(events))
	var unnamed, unrecognised int
	// notes about retained failures, plus a guard so a test reported
	// inconclusively several times is only mentioned once.
	var retained []string
	reported := make(map[string]bool, len(events))

	for _, ev := range events {
		if ev.Type != model.ToolUsed && ev.Type != model.TurnEnded {
			continue
		}
		name := strings.TrimSpace(ev.Attr(attrTestName))
		command := ev.Attr(attrTestCommand)
		if name == "" {
			if strings.TrimSpace(command) != "" {
				unnamed++
			}
			continue
		}

		status, ok := parseTestStatus(ev.Attr(attrTestStatus))
		if !ok {
			unrecognised++
		}
		evidence := model.Evidence{
			Kind:       model.EvidenceTest,
			Ref:        name,
			Result:     string(status),
			Detail:     command,
			SessionID:  ev.SessionID,
			CapturedAt: ev.Timestamp,
		}
		next := model.TestResult{
			Name:     name,
			Status:   status,
			Command:  command,
			Package:  ev.Attr(attrTestPackage),
			RanAt:    ev.Timestamp,
			Evidence: []model.Evidence{evidence},
		}

		cur, ok := byName[name]
		if !ok {
			stored := next
			byName[name] = &stored
			order = append(order, name)
			continue
		}
		// Plan §32: a failing test stays failed until a later result proves
		// otherwise, and only a run that actually executed the test proves
		// anything. A later result therefore supersedes the stored one only
		// when it is at least as conclusive (see testStrength).
		//
		// The superseded run stays cited. model.Evidence.Key ignores Result and
		// CapturedAt, so two runs of the same test share a key; de-duplicating
		// here would erase the failure the current pass replaced. Byte-identical
		// event replays were already removed by selectEvents upstream.
		history := cur.Evidence
		if testStrength(status) >= testStrength(cur.Status) {
			*cur = next
		} else if cur.Status == model.TestFailed && !reported[name] {
			// Silently downgrading a failure to "skipped" would empty
			// EngineeringState.FailingTests and hand the next worker a clean
			// bill of health for a test that never passed.
			reported[name] = true
			retained = append(retained, fmt.Sprintf(
				"test %q was later reported as %q, which does not prove the recorded failure was resolved; it is still recorded as failed",
				name, status))
		}
		cur.Evidence = append(history, evidence)
	}

	out := make([]model.TestResult, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}

	var notes []string
	if unnamed > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d event(s) reported a test command with no %s; those runs have no recorded outcome",
			unnamed, attrTestName))
	}
	if unrecognised > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d test result(s) carried no recognised %s and are recorded as unknown",
			unrecognised, attrTestStatus))
	}
	notes = append(notes, retained...)
	return out, notes
}

// testStrength ranks how much a recorded outcome proves about the test.
//
// A pass or a failure is the test having run to a verdict. A skip is the test
// not having run, which says nothing about whether an earlier failure was
// fixed. An unknown status says less still. Merging on this rank is what keeps
// a failure from being cleared by a run that never exercised the code
// (plan §16, §32).
func testStrength(s model.TestStatus) int {
	switch s {
	case model.TestPassed, model.TestFailed:
		return 2
	case model.TestSkipped:
		return 1
	default:
		return 0
	}
}

// parseTestStatus maps an adapter's status string onto model.TestStatus. It
// reports false when the value is missing or unrecognised, in which case the
// result is unknown rather than optimistically passed (plan §12).
func parseTestStatus(raw string) (model.TestStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "passed", "pass", "ok":
		return model.TestPassed, true
	case "failed", "fail":
		return model.TestFailed, true
	case "skipped", "skip":
		return model.TestSkipped, true
	case "unknown":
		return model.TestUnknown, true
	}
	return model.TestUnknown, false
}

// deriveConstraints records requirements injected mid-task, such as the noon
// curveball (plan §41).
//
// Constraints are additive and are never dropped. Which requirements a
// constraint affects and which assumptions it invalidates is interpretation, so
// those fields are left for the semantic layer to fill.
func deriveConstraints(events []model.AgentEvent) ([]model.Constraint, []string) {
	out := make([]model.Constraint, 0, len(events))
	var notes []string

	// Identifiers the adapters supplied are reserved up front so a generated id
	// can never collide with an explicit one that appears later in the stream.
	taken := make(map[string]bool, len(events))
	for _, ev := range events {
		if ev.Type == model.ConstraintAdded {
			if id := model.NormalizeID(ev.Attr(attrConstraintID)); id != "" {
				taken[id] = true
			}
		}
	}
	generated := 0
	// used tracks which ids have actually been handed out, so an adapter that
	// reuses one is reported rather than quietly producing two constraints that
	// answer to the same identifier.
	used := make(map[string]bool, len(events))
	collided := make(map[string]bool, len(events))

	for _, ev := range events {
		if ev.Type != model.ConstraintAdded {
			continue
		}
		id := model.NormalizeID(ev.Attr(attrConstraintID))
		if id != "" && used[id] && !collided[id] {
			collided[id] = true
			notes = append(notes, fmt.Sprintf(
				"constraint id %s was supplied by more than one event; every one is kept, but the id no longer identifies a single constraint", id))
		}
		if id == "" {
			// Numbered by position in the chronologically sorted stream, so the
			// same input always yields the same identifiers.
			for {
				generated++
				id = fmt.Sprintf("C%d", generated)
				if !taken[id] {
					taken[id] = true
					break
				}
			}
		}
		used[id] = true
		text := strings.TrimSpace(ev.Attr(attrConstraintText))
		if text == "" {
			text = strings.TrimSpace(ev.Summary)
		}

		source := strings.TrimSpace(ev.Attr(attrConstraintSource))
		if source == "" {
			// We still know which session delivered it, and that is a fact
			// rather than a guess about who authored it.
			source = "session:" + ev.SessionID
		}

		if text == "" {
			// The constraint is kept even though its text is missing. A
			// requirement we know changed but cannot quote is a gap the next
			// worker must be told about; silently discarding it would present
			// stale intent as current (plan §41).
			notes = append(notes, fmt.Sprintf(
				"constraint %s was recorded without text; inspect session %q for what changed", id, ev.SessionID))
		}

		out = append(out, model.Constraint{
			ID:      id,
			Text:    text,
			Source:  source,
			AddedAt: ev.Timestamp,
			// A constraint arrives as an agent statement, which is Level 3
			// evidence: there is no repository artifact behind it (plan §31).
			Evidence: []model.Evidence{ev.AsEvidence()},
		})
	}
	return out, notes
}

// countUnknownEvents counts records retained under model.EventUnknown: ones a
// host emitted whose lifecycle name this build does not map.
func countUnknownEvents(events []model.AgentEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == model.EventUnknown {
			n++
		}
	}
	return n
}

// originalIntentFrom recovers the task's original intent from the event stream.
//
// Original intent is the one field a later worker cannot reconstruct from the
// repository: the diff shows what changed, never what was asked for. When a host
// states it, capturing it here is the difference between a resumable task and
// one whose purpose has to be guessed at (plan §41, §49).
//
// A checkpoint's explicit intent wins over the opening prompt, because it is the
// host's considered statement of the goal rather than the first thing typed.
// Both are direct quotes, never a summary: an inferred intent would be exactly
// the fabrication this product exists to remove.
func originalIntentFrom(events []model.AgentEvent) string {
	for _, e := range events {
		if e.Type == model.CheckpointCreated {
			if intent := strings.TrimSpace(e.Attr("intent")); intent != "" {
				return intent
			}
		}
	}
	for _, e := range events {
		// The first turn of a session is the request that started it.
		if e.Type == model.TurnStarted {
			if s := strings.TrimSpace(e.Summary); s != "" {
				return s
			}
		}
	}
	return ""
}

// OriginalPrompt returns the request that started the task: the text of the
// first turn a host recorded.
//
// It is deliberately separate from the task's original intent. A checkpoint's
// stated intent is the better one-line answer to "what is this task for", but
// the prompt is where the obligations live — "should be rejected if expired",
// "add tests" — and plan §13 extracts requirements from the prompt. Feeding a
// polished intent line to requirement extraction loses most of them.
func OriginalPrompt(events []model.AgentEvent) string {
	sorted := append([]model.AgentEvent(nil), events...)
	model.SortEvents(sorted)
	for _, e := range sorted {
		if e.Type == model.TurnStarted {
			if s := strings.TrimSpace(e.Summary); s != "" {
				return s
			}
		}
	}
	return ""
}
