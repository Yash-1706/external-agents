package drift

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// TestSummaryDriftAcceptanceBlock is the plan §46 acceptance test: a human edits
// a relevant file after the checkpoint, and the resume output has to say what the
// history claimed, what the repository says now, and that revalidation is
// required. The golden text is exact because the output is a product surface.
func TestSummaryDriftAcceptanceBlock(t *testing.T) {
	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{changed("internal/webhooks.go", "M", "sha256:aaaa")}
	s.Decisions = []model.Decision{{
		ID:       "D1",
		Decision: "retry queue is per tenant",
		Evidence: []model.Evidence{fileEv("internal/webhooks.go", "")},
	}}
	repo := &fakeRepo{
		head:  model.RepoState{CommitSHA: nextSHA},
		files: map[string]string{"internal/webhooks.go": "sha256:bbbb"},
	}

	report, err := Detect(context.Background(), s, repo)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}

	want := strings.Join([]string{
		"STALE DECISION",
		"Repository drift detected. A historical assumption may no longer hold.",
		"Checked 1 file against the current repository.",
		"",
		"Historical state:",
		"  HEAD [commit] " + histSHA,
		"  internal/webhooks.go [decision] retry queue is per tenant",
		"  internal/webhooks.go [content] sha256:aaaa",
		"",
		"Current state:",
		"  HEAD [commit] " + nextSHA + " (repository HEAD moved since the historical state was captured)",
		"  internal/webhooks.go [decision] the file this decision cites changed; the decision is unverified (decision D1 may no longer hold)",
		"  internal/webhooks.go [content] sha256:bbbb (file content changed since the historical state was captured)",
		"",
		"Revalidation required. Historical state is context, not authority: verify these claims against the current repository before making changes.",
	}, "\n")

	if got := report.Summary(); got != want {
		t.Errorf("Summary mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSummaryPerOutcome(t *testing.T) {
	tests := []struct {
		name        string
		report      Report
		wantBanner  string
		wantSubstr  []string
		wantMissing []string
	}{
		{
			name:       "matches does not demand revalidation",
			report:     Report{Outcome: Matches, CheckedFiles: 4},
			wantBanner: "STATE MATCHES",
			wantSubstr: []string{
				"No meaningful change since the historical state was captured.",
				"Checked 4 files against the current repository.",
				"Revalidation not required",
				"context, not authority",
			},
			wantMissing: []string{"Revalidation required.", "Historical state:", "Notes:"},
		},
		{
			name: "unknown never reads as a match",
			report: Report{
				Outcome:              Unknown,
				RevalidationRequired: true,
				Notes:                []string{"Repository inspection is unavailable."},
			},
			wantBanner: "DRIFT UNKNOWN",
			wantSubstr: []string{
				"Drift could not be determined.",
				"Checked 0 files against the current repository.",
				"Notes:\n  - Repository inspection is unavailable.",
				"Revalidation required.",
			},
			wantMissing: []string{"No meaningful change", "STATE MATCHES"},
		},
		{
			name: "conflict names the absent file on both sides",
			report: Report{
				Outcome:              Conflict,
				RevalidationRequired: true,
				CheckedFiles:         1,
				Findings: []Finding{{
					Path:       "internal/gone.go",
					Kind:       kindMissing,
					Historical: "sha256:eeee",
					Current:    "absent from the working tree",
					Detail:     "a file cited by historical evidence is absent from the working tree",
				}},
			},
			wantBanner: "CONFLICT",
			wantSubstr: []string{
				"Current code contradicts historical task state.",
				"Historical state:\n  internal/gone.go [missing] sha256:eeee",
				"Current state:\n  internal/gone.go [missing] absent from the working tree (a file cited",
			},
		},
		{
			name: "unrecorded values render as uncertainty, not as blanks",
			report: Report{
				Outcome:              Drifted,
				RevalidationRequired: true,
				CheckedFiles:         1,
				Findings:             []Finding{{Path: "a.go", Kind: kindContent, Detail: "changed"}},
			},
			wantBanner: "STATE DRIFTED",
			wantSubstr: []string{
				"  a.go [content] not recorded",
				"  a.go [content] unknown (changed)",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.report.Summary()
			if !strings.HasPrefix(got, tc.wantBanner+"\n") {
				t.Errorf("summary does not open with %q:\n%s", tc.wantBanner, got)
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("summary missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.wantMissing {
				if strings.Contains(got, unwanted) {
					t.Errorf("summary should not contain %q:\n%s", unwanted, got)
				}
			}
			if strings.HasSuffix(got, "\n") {
				t.Error("summary must not end with a newline")
			}
		})
	}
}

// TestSummaryUnavailableRepositoryIsHonest walks the §48 degradation end to end:
// with no repository to read, the rendered block must say so rather than imply a
// verified resume.
func TestSummaryUnavailableRepositoryIsHonest(t *testing.T) {
	report, err := Detect(context.Background(), newState(histSHA), nil)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	got := report.Summary()
	for _, want := range []string{"DRIFT UNKNOWN", "Notes:", "Repository inspection is unavailable", "Revalidation required."} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}
