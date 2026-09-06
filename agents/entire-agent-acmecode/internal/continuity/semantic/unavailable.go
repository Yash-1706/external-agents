package semantic

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// unavailable is the Extractor used when semantic extraction cannot run at all:
// no model backend configured, a policy that forbids sending the prompt out, a
// transport that is down.
type unavailable struct {
	reason string
}

// Unavailable returns an Extractor that refuses to extract and says why.
//
// It exists so that "we could not analyse this" is representable. The tempting
// alternative — return an empty state — is a lie: an empty state is
// indistinguishable from "analysed, found nothing", and the caller would go on
// to render a handoff that silently omits every requirement and decision. Plan
// §48 and Rule 8 require the opposite: fail visibly, let the caller record the
// gap in Capture, and never let it be fatal to the host agent's own work.
func Unavailable(reason string) model.Extractor {
	r := strings.TrimSpace(reason)
	if r == "" {
		// An unexplained gap is still a gap, but the reader deserves a phrase
		// they can act on rather than an empty one.
		r = "no reason recorded"
	}
	return unavailable{reason: r}
}

// Available reports false, so callers can degrade before doing any work.
func (u unavailable) Available(context.Context) bool { return false }

// Extract returns an error wrapping model.ErrUnavailable, which is the sentinel
// every port uses to mean "the backing system is not reachable" as opposed to
// "the thing does not exist" (model.ErrNotFound).
func (u unavailable) Extract(context.Context, model.ExtractionInput) (*model.EngineeringState, error) {
	return nil, fmt.Errorf("semantic extraction unavailable: %s: %w", u.reason, model.ErrUnavailable)
}

// Describe carries the reason into any output that names the extractor, so the
// user sees why the semantic half of the state is missing.
func (u unavailable) Describe() string {
	return "semantic extraction unavailable: " + u.reason
}
