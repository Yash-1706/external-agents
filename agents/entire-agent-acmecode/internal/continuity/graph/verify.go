package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Verification is the plan §35 NEXT ACTION VALIDATION result: what the code
// graph could establish about a proposed next action, and what the reader
// should therefore do. It is a decision, not a dump of Graph output.
//
// Available and Resolved are separate facts on purpose. Available=false means
// no analysis ran at all; Available=true with Resolved=false means the tree was
// analysed and the target is not in it. Collapsing the two would let "we did
// not look" be read as "there is nothing there" (plan §48).
type Verification struct {
	// Target is the symbol the next action names, verbatim.
	Target string
	// Resolved reports whether Graph located a definition for Target.
	Resolved bool
	// Symbol is the located definition. When more than one matched, it is the
	// first in source order and Reason says how many there were.
	Symbol model.GraphSymbol
	// Impact is the blast radius Graph reported. It is meaningful only when
	// Resolved is true.
	Impact model.GraphImpact
	// Recommendation is the sentence a human or agent acts on.
	Recommendation string
	// Confidence is the standing of this verification, derived through
	// model.ConfidenceFor so an unverified action can never read as observed.
	Confidence model.Confidence
	// Available reports whether Graph verification actually ran.
	Available bool
	// Reason explains any gap or caveat: why verification did not run, why the
	// target did not resolve, or which part of the analysis was incomplete. It
	// is empty only when the verification is clean.
	Reason string
	// Evidence cites the concrete artifacts behind this verification: the
	// definition's file and lines, each caller's location, and each candidate
	// test (plan §11).
	Evidence []model.Evidence

	// impactFailed records that impact analysis was attempted and could not
	// answer, which is a different fact from an impact set that came back
	// empty. Without it, Render would print "no callers found in the analysed
	// source tree" over an analysis that never ran — an empty section reads as
	// "nothing is affected", which is exactly the §48 failure this package
	// exists to prevent.
	//
	// It is unexported so the exported API stays exactly as specified, and it
	// is phrased as a failure rather than as a success so that the zero value
	// of a hand-built Verification still renders its Impact section normally.
	impactFailed bool
}

// VerifyNextAction runs the plan §35 verification for one proposed next action:
// resolve the target, analyse its impact, list the candidate tests, and turn
// all of it into a single recommendation.
//
// It never returns an error. A failure to verify is itself a result the reader
// must see, so every failure path produces a Verification that says what was
// not checked (plan §48, Rule 8: the integration is never fatal).
//
// Evidence records are left without CapturedAt: this function takes no
// model.Clock, and inventing a timestamp here would break the determinism the
// rest of the system depends on. The caller stamps them if it needs to.
func VerifyNextAction(ctx context.Context, gc model.GraphClient, action model.NextAction) Verification {
	v := Verification{Target: strings.TrimSpace(action.Target), Evidence: []model.Evidence{}}

	switch {
	case v.Target == "":
		// Nothing was checked here either, but the fix is different: the action
		// itself is underspecified, so say that rather than blame the backend.
		v = unverified(v, "the next action records no target, so there was nothing for Graph to resolve")
		v.Recommendation = "This next action names no target, so it could not be checked against the code graph. Name the symbol or file it operates on, then verify it before editing."
		return v
	case gc == nil:
		return unverified(v, "no Graph client is configured")
	case !gc.Available(ctx):
		return unverified(v, reasonOf(gc))
	}

	syms, err := gc.Search(ctx, v.Target)
	if err != nil {
		if errors.Is(err, model.ErrUnavailable) {
			return unverified(v, err.Error())
		}
		// The backend was reachable but could not answer. Nothing was
		// established either way, so this stays an unverified action.
		return unverified(v, fmt.Sprintf("Graph search for %q failed: %v", v.Target, err))
	}

	v.Available = true
	if len(syms) == 0 {
		v.Reason = fmt.Sprintf("Graph analysed the source tree and found no definition of %q", v.Target)
		v.Recommendation = fmt.Sprintf("Do not assume %s exists: Graph could not resolve it. Locate the definition yourself before editing, and correct the target if it has been renamed or moved.", v.Target)
		v.Confidence = model.ConfidenceFor(v.Evidence)
		return v
	}

	v.Resolved = true
	v.Symbol = syms[0]
	v.Evidence = append(v.Evidence, definitionEvidence(v.Symbol))
	if len(syms) > 1 {
		// Name resolution is ambiguous by construction; say so rather than let
		// the first match pass as the only one.
		v.Reason = fmt.Sprintf("Graph resolved %d definitions for %q; the one at %s was analysed",
			len(syms), v.Target, location(v.Symbol))
	}

	// Impact is asked about the target exactly as the action wrote it, not about
	// the bare declaration name. v.Target is the string that resolved to
	// v.Symbol, so reusing it keeps the blast radius on the same declaration the
	// Definition section reports. Passing v.Symbol.Name instead would discard
	// the qualification that disambiguated the search: a target of "beta.Run"
	// would resolve to beta's Run and then be handed the bare "Run", which a
	// name-resolving backend answers with whichever Run it sees first.
	imp, err := gc.Impact(ctx, v.Target)
	if err != nil {
		v.impactFailed = true
		v.Reason = joinReason(v.Reason, fmt.Sprintf("impact analysis did not run: %v", err))
		v.Recommendation = fmt.Sprintf("Proceed with care: %s was located at %s, but its blast radius is unknown because impact analysis did not run. Check its callers and tests yourself before editing.",
			v.Symbol.Name, location(v.Symbol))
		// The definition really was found, so that much is observed.
		v.Confidence = model.ConfidenceFor(v.Evidence)
		return v
	}

	sortImpact(&imp)
	v.Impact = imp
	// A backend that resolves by name can still answer about a different
	// declaration than the one Search returned. Say so when the two disagree:
	// the alternative is presenting one symbol's definition next to another
	// symbol's blast radius, with nothing to warn the reader.
	if differentDefinition(v.Symbol, imp.Target) {
		v.Reason = joinReason(v.Reason, fmt.Sprintf(
			"the impact analysis answered for the definition at %s, not the one at %s",
			location(imp.Target), location(v.Symbol)))
	}
	for _, c := range imp.Callers {
		v.Evidence = append(v.Evidence, callerEvidence(v.Symbol.Name, c))
	}
	for _, t := range imp.Tests {
		v.Evidence = append(v.Evidence, testEvidence(v.Symbol.Name, t))
	}
	model.SortEvidence(v.Evidence)
	v.Evidence = model.DedupeEvidence(v.Evidence)
	v.Confidence = model.ConfidenceFor(v.Evidence)
	v.Recommendation = recommend(v)
	return v
}

// unverified fills in the shape every "this was not checked" result must have.
func unverified(v Verification, reason string) Verification {
	v.Available = false
	v.Resolved = false
	v.Impact = model.GraphImpact{}
	v.Evidence = []model.Evidence{}
	v.Reason = strings.TrimSpace(reason)
	if v.Reason == "" {
		v.Reason = "the Graph backend did not report a reason"
	}
	v.Recommendation = "Graph verification was unavailable, so this action has not been checked against the code graph. Confirm the target, its callers and its tests yourself before editing."
	// No evidence means no claim: model.ConfidenceFor is the chokepoint that
	// keeps an unverified action from reading as an observed one.
	v.Confidence = model.ConfidenceFor(v.Evidence)
	return v
}

// reasonOf extracts the backend's own explanation for being unavailable.
func reasonOf(gc model.GraphClient) string {
	if r, ok := gc.(graphReason); ok {
		if s := strings.TrimSpace(r.GraphUnavailableReason()); s != "" {
			return s
		}
	}
	return fmt.Sprintf("the Graph backend (%s) reported that it is not available", gc.Describe())
}

// recommend produces the single actionable sentence from plan §35.
func recommend(v Verification) string {
	name := v.Symbol.Name
	if name == "" {
		name = v.Target
	}
	tests := len(v.Impact.Tests)
	callers := len(v.Impact.Callers)
	switch {
	case tests > 0:
		return fmt.Sprintf("Proceed, but run the %d affected %s after changing %s.", tests, plural(tests, "test", "tests"), name)
	case callers > 0:
		return fmt.Sprintf("Proceed with care: %d %s depend on %s and Graph found no test covering it. Identify or add a test before changing it.",
			callers, plural(callers, "caller", "callers"), name)
	default:
		return fmt.Sprintf("Proceed: Graph found no callers and no covering tests for %s, so the visible blast radius is its own definition. A caller outside the analysed tree would not appear here.", name)
	}
}

// definitionEvidence cites the located definition. Graph results are Level 1
// evidence because they are computed from the source tree itself (plan §31).
func definitionEvidence(sym model.GraphSymbol) model.Evidence {
	detail := sym.Signature
	if detail == "" {
		detail = strings.TrimSpace(sym.Kind + " " + sym.Name)
	}
	return model.Evidence{
		Kind:      model.EvidenceGraph,
		Ref:       sym.Name,
		Path:      sym.Path,
		LineStart: sym.LineStart,
		LineEnd:   sym.LineEnd,
		Result:    "definition",
		Detail:    detail,
	}
}

// callerEvidence cites one caller of the target.
func callerEvidence(target string, caller model.GraphSymbol) model.Evidence {
	return model.Evidence{
		Kind:      model.EvidenceGraph,
		Ref:       caller.Name,
		Path:      caller.Path,
		LineStart: caller.LineStart,
		LineEnd:   caller.LineEnd,
		Result:    "caller",
		Detail:    fmt.Sprintf("calls %s", target),
	}
}

// testEvidence cites one candidate test. Result deliberately says "candidate":
// Graph identified the test, it did not run it, and a test result this product
// has not seen must never be presented as one it has (plan §16).
func testEvidence(target, test string) model.Evidence {
	return model.Evidence{
		Kind:   model.EvidenceGraph,
		Ref:    test,
		Result: "candidate-test",
		Detail: fmt.Sprintf("exercises %s or one of its callers; not run by this verification", target),
	}
}

// differentDefinition reports whether two located symbols are certainly not the
// same declaration.
//
// It compares only the fields both sides actually filled in, so a backend that
// omits line numbers is never accused of a mismatch it did not make. Two
// declarations of one name can share a file — a func and a method, say — which
// is why an equal path is not on its own proof of agreement.
func differentDefinition(a, b model.GraphSymbol) bool {
	if a.Path == "" || b.Path == "" {
		return false
	}
	if a.Path != b.Path {
		return true
	}
	return a.LineStart > 0 && b.LineStart > 0 && a.LineStart != b.LineStart
}

// location renders a symbol as path:start-end.
func location(sym model.GraphSymbol) string {
	if sym.Path == "" {
		return "an unknown location"
	}
	switch {
	case sym.LineStart <= 0:
		return sym.Path
	case sym.LineEnd > sym.LineStart:
		return fmt.Sprintf("%s:%d-%d", sym.Path, sym.LineStart, sym.LineEnd)
	default:
		return fmt.Sprintf("%s:%d", sym.Path, sym.LineStart)
	}
}

// joinReason appends a second caveat to an existing one.
func joinReason(existing, extra string) string {
	if existing == "" {
		return extra
	}
	if extra == "" {
		return existing
	}
	return existing + "; " + extra
}

// Render prints the NEXT ACTION VALIDATION block from plan §35.
//
// Output is deterministic: list order is sorted here rather than trusted from
// the backend, so the same verification renders byte-identically every run.
// When verification did not run, the impact and test sections say so instead of
// being omitted — an empty section would read as "nothing is affected", which
// is the failure §48 exists to prevent.
func (v Verification) Render() string {
	var b strings.Builder
	b.WriteString("NEXT ACTION VALIDATION\n\n")

	b.WriteString("Target:\n")
	if v.Target == "" {
		b.WriteString("(none recorded)\n")
	} else {
		b.WriteString(v.Target + "\n")
	}

	if !v.Available {
		b.WriteString("\nGraph verification:\nUNAVAILABLE — " + v.Reason + "\n")
		b.WriteString("\nImpact:\n• unknown — Graph did not run\n")
		b.WriteString("\nRelevant tests:\n• unknown — Graph did not run\n")
	} else {
		b.WriteString("\nDefinition:\n")
		if v.Resolved {
			b.WriteString(location(v.Symbol) + "\n")
		} else {
			b.WriteString("not resolved\n")
		}

		b.WriteString("\nImpact:\n")
		dependents := append([]string(nil), v.Impact.Dependents...)
		sort.Strings(dependents)
		for _, d := range dependents {
			b.WriteString("• " + d + "\n")
		}
		if n := len(v.Impact.Callers); n > 0 {
			fmt.Fprintf(&b, "• %d %s\n", n, plural(n, "caller", "callers"))
		}
		if len(dependents) == 0 && len(v.Impact.Callers) == 0 {
			// "no callers found" is a claim about an analysis that ran. When it
			// did not, the only honest line is that the answer is unknown.
			switch {
			case !v.Resolved:
				b.WriteString("• unknown — target not resolved\n")
			case v.impactFailed:
				b.WriteString("• unknown — impact analysis did not run\n")
			default:
				b.WriteString("• no callers found in the analysed source tree\n")
			}
		}

		b.WriteString("\nRelevant tests:\n")
		tests := append([]string(nil), v.Impact.Tests...)
		sort.Strings(tests)
		for _, t := range tests {
			b.WriteString("• " + t + "\n")
		}
		if len(tests) == 0 {
			switch {
			case !v.Resolved:
				b.WriteString("• unknown — target not resolved\n")
			case v.impactFailed:
				b.WriteString("• unknown — impact analysis did not run\n")
			default:
				b.WriteString("• none found\n")
			}
		}

		if v.Reason != "" {
			b.WriteString("\nNote:\n" + v.Reason + "\n")
		}
	}

	b.WriteString("\nRecommendation:\n" + v.Recommendation + "\n")
	b.WriteString("\nConfidence:\n" + v.Confidence.Label() + "\n")
	return b.String()
}
