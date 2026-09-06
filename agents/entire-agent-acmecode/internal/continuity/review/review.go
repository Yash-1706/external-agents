// Package review finds the points in a task where a machine cannot responsibly
// decide, and says what question a human has to answer.
//
// The rest of the system is careful never to overclaim: a requirement with weak
// evidence stays UNKNOWN, a next action stays RECOMMENDED. That caution is
// correct, and on its own it is also useless — a reader is handed a page of
// uncertainty with no indication of which uncertainty is theirs to resolve.
//
// This package closes that gap. It converts "we could not establish this" into
// "you need to decide this, and here is why it is blocked" — which is the
// difference between a report and a workflow.
//
// It deliberately does not decide anything itself. Every item is a question,
// with the evidence that raised it attached, so the answer is a human judgement
// recorded through `task decide` or `task reject` and evidenced like any other
// claim (plan §12, §14, §15).
package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Kind classifies why a human is needed. It exists so the caller can sort and
// filter, and so the phrasing of each question stays consistent.
type Kind string

const (
	// KindBlocked is work stopped by something that failed.
	KindBlocked Kind = "blocked"
	// KindUnverifiable is a requirement nothing in the repository speaks to,
	// so neither "done" nor "not done" can be established.
	KindUnverifiable Kind = "unverifiable"
	// KindUndecided is a question the source material raises and never answers.
	KindUndecided Kind = "undecided"
	// KindUnverifiedPlan is a recommended action whose blast radius was never
	// checked, so acting on it is a risk somebody has to accept.
	KindUnverifiedPlan Kind = "unverified-plan"
)

// Item is one thing a human has to resolve.
type Item struct {
	Kind Kind
	// Ref names what the item is about: a requirement id, a test, an action.
	Ref string
	// Question is what the human is being asked, phrased so it can be answered.
	Question string
	// Because states what made this a question rather than a settled fact.
	Because string
	// Suggest is the command that records the answer, so the workflow closes
	// rather than ending in advice.
	Suggest  string
	Evidence []model.Evidence
}

// Priority orders the kinds: something broken outranks something merely
// unproven, and an unproven fact outranks an unchecked plan.
func (k Kind) Priority() int {
	switch k {
	case KindBlocked:
		return 0
	case KindUndecided:
		return 1
	case KindUnverifiable:
		return 2
	case KindUnverifiedPlan:
		return 3
	}
	return 4
}

// Find returns everything in the state that needs a human, most urgent first.
//
// An empty result is a real answer, not a failure: it means nothing in the task
// currently requires judgement — though it does not mean the task is finished.
func Find(s *model.EngineeringState) []Item {
	if s == nil {
		return nil
	}
	var items []Item

	// 1. A failing test blocks whatever it covers. The machine can see that it
	//    failed; it cannot decide whether to fix, waive or reinterpret it.
	for _, t := range s.FailingTests() {
		items = append(items, Item{
			Kind:     KindBlocked,
			Ref:      t.Name,
			Question: fmt.Sprintf("%s is failing. Fix it, or record why the current behaviour is acceptable?", t.Name),
			Because:  "a failing test is observed fact, but what to do about it is a judgement",
			Suggest:  fmt.Sprintf("task decide --decision \"...\" --reason \"...\" (or task reject if the approach itself is wrong)"),
			Evidence: t.Evidence,
		})
	}

	// 2. A requirement blocked or unresolved with nothing in the repository
	//    speaking to it. We cannot call it done and we cannot call it undone.
	for _, r := range s.Requirements {
		if r.Status == model.ReqComplete {
			continue
		}
		level, ok := model.StrongestLevel(r.Evidence)
		verifiable := ok && level <= model.LevelCheckpoint
		switch {
		case r.Status == model.ReqBlocked:
			items = append(items, Item{
				Kind:     KindBlocked,
				Ref:      r.ID,
				Question: fmt.Sprintf("%s is blocked: %s. What unblocks it?", r.ID, r.Description),
				Because:  "the evidence shows this cannot proceed as planned",
				Suggest:  "task decide --decision \"...\" --reason \"...\"",
				Evidence: r.Evidence,
			})
		case !verifiable:
			items = append(items, Item{
				Kind: KindUnverifiable,
				Ref:  r.ID,
				Question: fmt.Sprintf("%s (%s) — is this actually done? Nothing in the repository proves it either way.",
					r.ID, r.Description),
				Because: "the only evidence is a stated requirement; no commit, file or test speaks to it, " +
					"and a rule-based extractor may not certify completion (plan §32)",
				Suggest:  "confirm against the code, then record the outcome with task decide",
				Evidence: r.Evidence,
			})
		}
	}

	// 3. A question the source material asks and never answers. These are the
	//    most valuable ones: somebody already knew a decision was needed.
	for _, q := range openQuestions(s) {
		items = append(items, Item{
			Kind:     KindUndecided,
			Ref:      "open question",
			Question: q.text,
			Because:  "raised in " + q.source + " and never resolved",
			Suggest:  "task decide --decision \"...\" --reason \"...\"",
			Evidence: q.evidence,
		})
	}

	// 4. A recommended action nobody checked the blast radius of. Acting on it
	//    is a risk, and accepting a risk is a human act.
	if action, ok := s.PrimaryNextAction(); ok && !action.GraphVerified {
		items = append(items, Item{
			Kind:     KindUnverifiedPlan,
			Ref:      "next action",
			Question: fmt.Sprintf("Proceed with %q without a verified blast radius?", action.Description),
			Because:  "Graph did not confirm what this change touches, so the risk is unquantified",
			Suggest:  "entire graph impact --symbol <target> --repo <module root>",
			Evidence: action.Evidence,
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind.Priority() != items[j].Kind.Priority() {
			return items[i].Kind.Priority() < items[j].Kind.Priority()
		}
		return items[i].Ref < items[j].Ref
	})
	return items
}

// openQuestion is a question captured from the source material.
type openQuestion struct {
	text     string
	source   string
	evidence []model.Evidence
}

// openQuestions collects questions the inputs raised and nobody answered.
//
// Two sources feed this. A checkpoint may carry open questions explicitly — the
// agent that wrote it knew something was unresolved. And the task's own intent
// may pose one, which is what a PRD does when it defers a decision to
// engineering. Both are direct quotes; neither is inferred.
func openQuestions(s *model.EngineeringState) []openQuestion {
	var out []openQuestion
	seen := map[string]bool{}

	add := func(text, source string, ev []model.Evidence) {
		t := strings.TrimSpace(text)
		if t == "" || seen[strings.ToLower(t)] {
			return
		}
		// A question already answered by a recorded decision is not open.
		for _, d := range s.ActiveDecisions() {
			if overlaps(d.Decision, t) || overlaps(d.Reason, t) {
				return
			}
		}
		seen[strings.ToLower(t)] = true
		out = append(out, openQuestion{text: t, source: source, evidence: ev})
	}

	for _, ev := range s.Evidence {
		if q := strings.TrimSpace(ev.Detail); q != "" && ev.Kind == model.EvidenceCheckpoint && strings.Contains(q, "?") {
			add(q, "checkpoint "+ev.Ref, []model.Evidence{ev})
		}
	}
	// Sentences, not lines — and the newlines are flattened first.
	//
	// A spec states its open question inside a paragraph, and a hard-wrapped
	// document splits that sentence across lines: "…which error should the
	// customer\nsee?". Scanning line by line therefore finds neither half,
	// which is exactly how a PRD's one explicit request for an engineering
	// decision went unnoticed.
	// Heading lines are dropped before flattening, or a section title glues
	// itself to the first sentence beneath it and the question is quoted back
	// to the reader as "## Open question for engineering When a coupon is...".
	var prose []string
	for _, line := range strings.Split(s.Task.OriginalIntent, "\n") {
		if t := strings.TrimSpace(line); !strings.HasPrefix(t, "#") {
			prose = append(prose, t)
		}
	}
	flat := strings.Join(strings.Fields(strings.Join(prose, " ")), " ")
	for _, sentence := range splitSentences(flat) {
		q := strings.TrimSpace(sentence)
		if !strings.HasSuffix(q, "?") || len(q) < 15 {
			continue
		}
		add(q, "the task's stated intent", []model.Evidence{{
			Kind: model.EvidencePrompt, Ref: "original_prompt", Detail: q,
		}})
	}
	return out
}

// overlaps reports whether a recorded decision plausibly answers a question. It
// is deliberately loose: showing a question that was in fact answered wastes a
// reader's time, whereas hiding an open one loses a decision.
func overlaps(decision, question string) bool {
	d := strings.ToLower(decision)
	if strings.TrimSpace(d) == "" {
		return false
	}
	words := 0
	for _, w := range strings.Fields(strings.ToLower(question)) {
		w = strings.Trim(w, ".,?;:\"'()")
		if len(w) < 5 {
			continue
		}
		if strings.Contains(d, w) {
			words++
		}
	}
	return words >= 3
}

// Render prints the items as a block a reader can work through top to bottom.
func Render(items []Item) string {
	var b strings.Builder
	if len(items) == 0 {
		b.WriteString("HUMAN JUDGEMENT REQUIRED\n")
		b.WriteString("  Nothing currently needs a decision.\n")
		b.WriteString("  This is not the same as the task being finished — it means every\n")
		b.WriteString("  open item is one the evidence can still settle on its own.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "HUMAN JUDGEMENT REQUIRED — %d item(s)\n", len(items))
	b.WriteString("These are the points a machine cannot responsibly decide.\n\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, strings.ToUpper(string(it.Kind)), it.Ref)
		fmt.Fprintf(&b, "     %s\n", it.Question)
		fmt.Fprintf(&b, "     why you: %s\n", it.Because)
		if len(it.Evidence) > 0 {
			refs := make([]string, 0, len(it.Evidence))
			for _, e := range it.Evidence {
				refs = append(refs, e.String())
			}
			fmt.Fprintf(&b, "     evidence: %s\n", strings.Join(refs, "; "))
		}
		fmt.Fprintf(&b, "     record it: %s\n\n", it.Suggest)
	}
	return b.String()
}

// splitSentences breaks a line after terminal punctuation, keeping the mark so
// a question can still be recognised as one.
func splitSentences(line string) []string {
	var out []string
	start := 0
	runes := []rune(line)
	for i, r := range runes {
		if r != '.' && r != '?' && r != '!' {
			continue
		}
		// Only break when the next rune is a space or the line ends, so a
		// decimal or an abbreviation does not split a sentence in half.
		if i+1 < len(runes) && runes[i+1] != ' ' {
			continue
		}
		out = append(out, strings.TrimSpace(string(runes[start:i+1])))
		start = i + 1
	}
	if start < len(runes) {
		out = append(out, strings.TrimSpace(string(runes[start:])))
	}
	return out
}
