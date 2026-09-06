package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// fixtureDir is the fake Go tree the local analyser tests read with go/parser.
// It lives under testdata so the go tool never compiles it.
const fixtureDir = "testdata/proj"

// TestUnavailableNeverClaimsAnything is the honesty guard for plan §48: an
// unavailable Graph must never report itself as available, must never resolve a
// target, and must return ErrUnavailable from every call.
func TestUnavailableNeverClaimsAnything(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{
			name:       "explicit reason is preserved",
			reason:     "entire graph is not installed",
			wantReason: "entire graph is not installed",
		},
		{
			name:       "blank reason still explains itself",
			reason:     "   ",
			wantReason: "no Graph backend is configured",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gc := Unavailable(tc.reason)

			if gc.Available(ctx) {
				t.Fatal("Available reported true for an unavailable Graph")
			}
			if got := reasonOf(gc); got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
			if !strings.Contains(gc.Describe(), tc.wantReason) {
				t.Errorf("Describe() = %q, want it to carry the reason", gc.Describe())
			}

			if _, err := gc.Search(ctx, "Process"); !errors.Is(err, model.ErrUnavailable) {
				t.Errorf("Search error = %v, want ErrUnavailable", err)
			}
			if _, err := gc.Impact(ctx, "Process"); !errors.Is(err, model.ErrUnavailable) {
				t.Errorf("Impact error = %v, want ErrUnavailable", err)
			}
			if _, err := gc.SemanticDiff(ctx, "a", "b"); !errors.Is(err, model.ErrUnavailable) {
				t.Errorf("SemanticDiff error = %v, want ErrUnavailable", err)
			}

			// The verification built on top of it must carry the same honesty:
			// nothing was checked, and it says so.
			v := VerifyNextAction(ctx, gc, model.NextAction{Target: "Process"})
			if v.Available {
				t.Error("Verification.Available = true for an unavailable Graph")
			}
			if v.Resolved {
				t.Error("Verification.Resolved = true for an unavailable Graph")
			}
			if v.Confidence != model.Unknown {
				t.Errorf("Confidence = %q, want %q", v.Confidence, model.Unknown)
			}
			if len(v.Evidence) != 0 {
				t.Errorf("Evidence = %v, want none when nothing was analysed", v.Evidence)
			}
			if !strings.Contains(v.Reason, tc.wantReason) {
				t.Errorf("Verification.Reason = %q, want it to quote %q", v.Reason, tc.wantReason)
			}
		})
	}
}

// TestDetect covers backend selection, including the case where nothing is
// usable and the reason has to name both failures.
func TestDetect(t *testing.T) {
	tests := []struct {
		name         string
		bin          string
		dir          string
		wantDescribe string
		wantReason   []string
	}{
		{
			// "go" is on PATH wherever these tests run, which is enough to
			// exercise CLI selection without executing anything.
			name:         "cli wins when the binary resolves",
			bin:          "go",
			dir:          fixtureDir,
			wantDescribe: "entire graph CLI (go)",
		},
		{
			name:         "falls back to local analysis when no cli is configured",
			bin:          "",
			dir:          fixtureDir,
			wantDescribe: "local Go static analysis",
		},
		{
			name:         "falls back to local analysis when the cli is missing",
			bin:          "entire-graph-that-does-not-exist",
			dir:          fixtureDir,
			wantDescribe: "local Go static analysis",
		},
		{
			name:         "unavailable names both failures",
			bin:          "entire-graph-that-does-not-exist",
			dir:          "testdata/empty",
			wantDescribe: "graph unavailable",
			wantReason: []string{
				"entire-graph-that-does-not-exist",
				"not on PATH",
				"no Go sources",
			},
		},
		{
			name:         "unavailable says when no cli was configured at all",
			bin:          "",
			dir:          "testdata/empty",
			wantDescribe: "graph unavailable",
			wantReason:   []string{"no Graph CLI was configured"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gc := Detect(tc.bin, tc.dir)
			if !strings.Contains(gc.Describe(), tc.wantDescribe) {
				t.Fatalf("Describe() = %q, want it to contain %q", gc.Describe(), tc.wantDescribe)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(gc.Describe(), want) {
					t.Errorf("Describe() = %q, want it to contain %q", gc.Describe(), want)
				}
			}
		})
	}
}

// TestSortedUniqueIsDeterministic guards the rule that map iteration order must
// never reach output.
func TestSortedUniqueIsDeterministic(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty stays non-nil", in: nil, want: []string{}},
		{name: "drops blanks and duplicates", in: []string{"b", "", "a", "b"}, want: []string{"a", "b"}},
		{name: "sorts", in: []string{"retry", "BillingService"}, want: []string{"BillingService", "retry"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedUnique(tc.in)
			if got == nil {
				t.Fatal("sortedUnique returned nil; output must serialize as [] not null")
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("sortedUnique(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSetToSortedIgnoresInsertionOrder(t *testing.T) {
	a := setToSorted(map[string]bool{"retry": true, "BillingService": true, "": true})
	b := setToSorted(map[string]bool{"BillingService": true, "": true, "retry": true})
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("setToSorted is order dependent: %v vs %v", a, b)
	}
	if want := "BillingService,retry"; strings.Join(a, ",") != want {
		t.Errorf("setToSorted = %v, want %q", a, want)
	}
}
