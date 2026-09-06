package semantic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func TestUnavailable(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantInText string
	}{
		{
			name:       "reason is carried through",
			reason:     "no model backend configured",
			wantInText: "no model backend configured",
		},
		{
			name:       "reason is trimmed",
			reason:     "  transport timed out  ",
			wantInText: "transport timed out",
		},
		{
			// An unexplained gap is still a gap; the reader gets a phrase they
			// can act on rather than a dangling colon.
			name:       "empty reason still says something",
			reason:     "",
			wantInText: "no reason recorded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := Unavailable(tc.reason)

			if e.Available(context.Background()) {
				t.Error("Available() = true for an unavailable extractor")
			}

			st, err := e.Extract(context.Background(), model.ExtractionInput{})
			if err == nil {
				t.Fatal("Extract returned no error; an empty state would be indistinguishable from 'analysed, found nothing'")
			}
			// Callers switch on this sentinel to record the gap in Capture and
			// carry on: unavailability is never fatal (plan §48, Rule 8).
			if !errors.Is(err, model.ErrUnavailable) {
				t.Errorf("error %v does not wrap model.ErrUnavailable", err)
			}
			if errors.Is(err, model.ErrNotFound) {
				t.Error("unavailability must not be reported as not-found: the thing exists, the capability does not")
			}
			if st != nil {
				t.Errorf("Extract returned a state (%+v) alongside an error", st)
			}
			if !strings.Contains(err.Error(), tc.wantInText) {
				t.Errorf("error %q does not explain the gap (%q)", err, tc.wantInText)
			}
			if !strings.Contains(e.Describe(), tc.wantInText) {
				t.Errorf("Describe() = %q, want it to name the reason %q", e.Describe(), tc.wantInText)
			}
		})
	}
}

// TestUnavailableSatisfiesExtractor is a compile-time check that both
// constructors return the same port, so a caller can swap one for the other
// without knowing which it holds.
func TestUnavailableSatisfiesExtractor(t *testing.T) {
	var extractors = []model.Extractor{
		NewHeuristic(testClock()),
		Unavailable("not configured"),
	}
	for _, e := range extractors {
		if e.Describe() == "" {
			t.Error("Describe() is empty; output must always name the backing implementation")
		}
	}
}
