package drift

import (
	"fmt"
	"strings"
)

// Summary renders the drift block described in plan §23 and §46: what the
// historical state claimed, what the repository says now, and whether the next
// worker has to revalidate before acting.
//
// The rendering is deterministic - findings and notes are already ordered by
// Detect - so two runs over the same inputs produce byte-identical text. The
// returned string carries no trailing newline.
//
// Plan §46 also asks the resume output to restate the historical next action.
// That line is not rendered here because a Report deliberately carries only the
// comparison, not the state; the caller that owns the state prints it.
func (r Report) Summary() string {
	var b strings.Builder

	b.WriteString(banner(r.Outcome))
	b.WriteString("\n")
	b.WriteString(headline(r.Outcome))
	b.WriteString("\n")
	fmt.Fprintf(&b, "Checked %d %s against the current repository.\n",
		r.CheckedFiles, plural(r.CheckedFiles, "file", "files"))

	if len(r.Findings) > 0 {
		b.WriteString("\nHistorical state:\n")
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "  %s [%s] %s\n", label(f), f.Kind, valueOr(f.Historical, "not recorded"))
		}
		b.WriteString("\nCurrent state:\n")
		for _, f := range r.Findings {
			line := fmt.Sprintf("  %s [%s] %s", label(f), f.Kind, valueOr(f.Current, "unknown"))
			if f.Detail != "" {
				line += " (" + f.Detail + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	if len(r.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range r.Notes {
			b.WriteString("  - " + n + "\n")
		}
	}

	b.WriteString("\n")
	if r.RevalidationRequired {
		// Plan §22: the receiving agent is told, every time, not to trust the
		// history over the repository.
		b.WriteString("Revalidation required. Historical state is context, not authority: verify these claims against the current repository before making changes.")
	} else {
		b.WriteString("Revalidation not required for the checks above. Historical state is still context, not authority: verify anything it does not cover.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// banner is the plan §23 headline for each outcome.
func banner(o Outcome) string {
	switch o {
	case Matches:
		return "STATE MATCHES"
	case Drifted:
		return "STATE DRIFTED"
	case StaleDecision:
		return "STALE DECISION"
	case Conflict:
		return "CONFLICT"
	case Unknown:
		return "DRIFT UNKNOWN"
	}
	return "DRIFT UNKNOWN"
}

// headline is the one-line meaning of each outcome, worded as plan §23 and §46
// word them so the resume output matches the acceptance test.
func headline(o Outcome) string {
	switch o {
	case Matches:
		return "No meaningful change since the historical state was captured."
	case Drifted:
		return "Repository drift detected. Code changed after the historical state was captured."
	case StaleDecision:
		return "Repository drift detected. A historical assumption may no longer hold."
	case Conflict:
		return "Repository drift detected. Current code contradicts historical task state."
	case Unknown:
		return "Drift could not be determined. The historical state was not verified against the repository."
	}
	return "Drift could not be determined. The historical state was not verified against the repository."
}

// label names the subject of a finding. A finding with no path is repository
// wide, which today means the HEAD comparison.
func label(f Finding) string {
	if f.Path != "" {
		return f.Path
	}
	if f.Kind == kindCommit {
		return "HEAD"
	}
	return "repository"
}

func valueOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
