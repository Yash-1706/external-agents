package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// fakeGraph is a GraphClient whose every answer is dictated by the test, so the
// decision logic can be exercised independently of any backend.
type fakeGraph struct {
	available bool
	symbols   []model.GraphSymbol
	searchErr error
	impact    model.GraphImpact
	impactErr error
	describe  string
}

func (f fakeGraph) Available(context.Context) bool { return f.available }

func (f fakeGraph) Search(context.Context, string) ([]model.GraphSymbol, error) {
	return f.symbols, f.searchErr
}

func (f fakeGraph) Impact(context.Context, string) (model.GraphImpact, error) {
	return f.impact, f.impactErr
}

func (f fakeGraph) SemanticDiff(context.Context, string, string) ([]model.SemanticChange, error) {
	return nil, nil
}

func (f fakeGraph) Describe() string {
	if f.describe == "" {
		return "fake graph"
	}
	return f.describe
}

var processSymbol = model.GraphSymbol{
	Name:      "Process",
	Kind:      "method",
	Path:      "billing/webhook.go",
	LineStart: 16,
	LineEnd:   18,
	Signature: "func (p *WebhookProcessor) Process(id string) error",
}

// TestVerifyNextAction walks the decision table of plan §35, with the
// degradation paths of §48 alongside the happy one.
func TestVerifyNextAction(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		client         model.GraphClient
		action         model.NextAction
		wantAvailable  bool
		wantResolved   bool
		wantConfidence model.Confidence
		wantRecommend  string
		wantReason     string
		wantEvidence   int
	}{
		{
			name:           "no client configured",
			client:         nil,
			action:         model.NextAction{Target: "Process"},
			wantConfidence: model.Unknown,
			wantRecommend:  "Graph verification was unavailable",
			wantReason:     "no Graph client is configured",
		},
		{
			name:           "unavailable backend quotes its own reason",
			client:         Unavailable("entire graph is not activated for this repository"),
			action:         model.NextAction{Target: "Process"},
			wantConfidence: model.Unknown,
			wantRecommend:  "Graph verification was unavailable",
			wantReason:     "entire graph is not activated for this repository",
		},
		{
			name:           "unavailable backend without a reason falls back to its description",
			client:         fakeGraph{available: false, describe: "entire graph CLI (entire)"},
			action:         model.NextAction{Target: "Process"},
			wantConfidence: model.Unknown,
			wantRecommend:  "Graph verification was unavailable",
			wantReason:     "entire graph CLI (entire)",
		},
		{
			// An action with nothing to verify must not look verified.
			name:           "next action carries no target",
			client:         fakeGraph{available: true, symbols: []model.GraphSymbol{processSymbol}},
			action:         model.NextAction{Description: "tidy the retry path"},
			wantConfidence: model.Unknown,
			wantRecommend:  "This next action names no target",
			wantReason:     "records no target",
		},
		{
			name:           "search fails for a reason of its own",
			client:         fakeGraph{available: true, searchErr: errors.New("index is rebuilding")},
			action:         model.NextAction{Target: "Process"},
			wantConfidence: model.Unknown,
			wantRecommend:  "Graph verification was unavailable",
			wantReason:     "index is rebuilding",
		},
		{
			name:           "search reports the unavailability itself",
			client:         fakeGraph{available: true, searchErr: model.ErrUnavailable},
			action:         model.NextAction{Target: "Process"},
			wantConfidence: model.Unknown,
			wantRecommend:  "Graph verification was unavailable",
			wantReason:     "capability unavailable",
		},
		{
			// Graph ran and the target is not there: available, unresolved, and
			// the reader is told to go look rather than to proceed.
			name:           "target does not resolve",
			client:         fakeGraph{available: true, symbols: []model.GraphSymbol{}},
			action:         model.NextAction{Target: "Process"},
			wantAvailable:  true,
			wantConfidence: model.Unknown,
			wantRecommend:  "Do not assume Process exists",
			wantReason:     "found no definition",
		},
		{
			name: "resolved but impact analysis fails",
			client: fakeGraph{
				available: true,
				symbols:   []model.GraphSymbol{processSymbol},
				impactErr: errors.New("call index missing"),
			},
			action:         model.NextAction{Target: "Process"},
			wantAvailable:  true,
			wantResolved:   true,
			wantConfidence: model.Observed,
			wantRecommend:  "blast radius is unknown",
			wantReason:     "call index missing",
			wantEvidence:   1,
		},
		{
			name: "resolved with tests recommends running them",
			client: fakeGraph{
				available: true,
				symbols:   []model.GraphSymbol{processSymbol},
				impact: model.GraphImpact{
					Target:     processSymbol,
					Callers:    []model.GraphSymbol{{Name: "Handle", Path: "billing/service.go", LineStart: 9}},
					Dependents: []string{"BillingService"},
					Tests:      []string{"TestRetry", "TestDuplicateWebhook", "TestBillingResume"},
				},
			},
			action:         model.NextAction{Target: "Process"},
			wantAvailable:  true,
			wantResolved:   true,
			wantConfidence: model.Observed,
			wantRecommend:  "Proceed, but run the 3 affected tests after changing Process.",
			wantEvidence:   5, // definition + 1 caller + 3 candidate tests
		},
		{
			// Callers but no test: the honest advice is not "proceed".
			name: "resolved with callers and no covering test",
			client: fakeGraph{
				available: true,
				symbols:   []model.GraphSymbol{processSymbol},
				impact: model.GraphImpact{
					Target: processSymbol,
					Callers: []model.GraphSymbol{
						{Name: "Handle", Path: "billing/service.go", LineStart: 9},
						{Name: "Run", Path: "retry/scheduler.go", LineStart: 17},
					},
					Dependents: []string{"BillingService", "retry"},
				},
			},
			action:         model.NextAction{Target: "Process"},
			wantAvailable:  true,
			wantResolved:   true,
			wantConfidence: model.Observed,
			wantRecommend:  "2 callers depend on Process and Graph found no test covering it",
			wantEvidence:   3,
		},
		{
			// Nothing found must be stated as "nothing visible here", not as a
			// guarantee of safety.
			name: "resolved with an empty blast radius",
			client: fakeGraph{
				available: true,
				symbols:   []model.GraphSymbol{processSymbol},
				impact:    model.GraphImpact{Target: processSymbol},
			},
			action:         model.NextAction{Target: "Process"},
			wantAvailable:  true,
			wantResolved:   true,
			wantConfidence: model.Observed,
			wantRecommend:  "A caller outside the analysed tree would not appear here.",
			wantEvidence:   1,
		},
		{
			// Name resolution is ambiguous by construction, so say so.
			name: "several definitions match the target",
			client: fakeGraph{
				available: true,
				symbols: []model.GraphSymbol{
					processSymbol,
					{Name: "Process", Kind: "func", Path: "retry/process.go", LineStart: 4, LineEnd: 6},
				},
				impact: model.GraphImpact{Target: processSymbol, Tests: []string{"TestRetry"}},
			},
			action:         model.NextAction{Target: "Process"},
			wantAvailable:  true,
			wantResolved:   true,
			wantConfidence: model.Observed,
			wantRecommend:  "Proceed, but run the 1 affected test after changing Process.",
			wantReason:     "resolved 2 definitions",
			wantEvidence:   2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := VerifyNextAction(ctx, tc.client, tc.action)

			if v.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", v.Available, tc.wantAvailable)
			}
			if v.Resolved != tc.wantResolved {
				t.Errorf("Resolved = %v, want %v", v.Resolved, tc.wantResolved)
			}
			if v.Confidence != tc.wantConfidence {
				t.Errorf("Confidence = %q, want %q", v.Confidence, tc.wantConfidence)
			}
			if !strings.Contains(v.Recommendation, tc.wantRecommend) {
				t.Errorf("Recommendation = %q, want it to contain %q", v.Recommendation, tc.wantRecommend)
			}
			if tc.wantReason != "" && !strings.Contains(v.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", v.Reason, tc.wantReason)
			}
			if len(v.Evidence) != tc.wantEvidence {
				t.Errorf("Evidence count = %d, want %d (%+v)", len(v.Evidence), tc.wantEvidence, v.Evidence)
			}
			if !v.Available {
				// Whatever went wrong, an unverified action must never render
				// as if the check happened (plan §48).
				out := v.Render()
				if !strings.Contains(out, "UNAVAILABLE") {
					t.Errorf("Render() = %q, want it to say UNAVAILABLE", out)
				}
				if strings.Contains(out, "no callers found") {
					t.Errorf("Render() = %q, must not imply an analysed empty impact", out)
				}
				if v.Reason == "" {
					t.Error("Reason is empty; an unavailable verification must explain itself")
				}
			}
		})
	}
}

// TestVerificationEvidenceIsCited checks that every claim in a verification
// points at a concrete artifact (plan §11), and that a candidate test is never
// presented as a test result (plan §16).
func TestVerificationEvidenceIsCited(t *testing.T) {
	v := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impact: model.GraphImpact{
			Target:  processSymbol,
			Callers: []model.GraphSymbol{{Name: "Handle", Path: "billing/service.go", LineStart: 9, LineEnd: 11}},
			Tests:   []string{"TestBillingResume"},
		},
	}, model.NextAction{Target: "Process"})

	byResult := map[string]model.Evidence{}
	for _, e := range v.Evidence {
		if e.Kind != model.EvidenceGraph {
			t.Errorf("evidence kind = %q, want %q", e.Kind, model.EvidenceGraph)
		}
		if e.Ref == "" {
			t.Errorf("evidence %+v has no ref", e)
		}
		byResult[e.Result] = e
	}

	def, ok := byResult["definition"]
	if !ok {
		t.Fatal("no definition evidence recorded")
	}
	if def.Path != "billing/webhook.go" || def.LineStart != 16 || def.LineEnd != 18 {
		t.Errorf("definition evidence = %+v, want the file and line range", def)
	}
	if caller, ok := byResult["caller"]; !ok || caller.Path != "billing/service.go" {
		t.Errorf("caller evidence = %+v, want billing/service.go", caller)
	}
	test, ok := byResult["candidate-test"]
	if !ok {
		t.Fatal("no candidate-test evidence recorded")
	}
	if !strings.Contains(test.Detail, "not run") {
		t.Errorf("candidate test evidence = %+v, want it to say the test was not run", test)
	}
	// Graph evidence is repository-level, so a verified action is observed.
	if got := model.ConfidenceFor(v.Evidence); got != model.Observed {
		t.Errorf("ConfidenceFor(evidence) = %q, want %q", got, model.Observed)
	}
}

// TestRenderVerifiedBlock pins the plan §35 output exactly.
func TestRenderVerifiedBlock(t *testing.T) {
	v := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impact: model.GraphImpact{
			Target: processSymbol,
			Callers: []model.GraphSymbol{
				{Name: "Handle", Path: "billing/service.go", LineStart: 9},
				{Name: "Reschedule", Path: "retry/scheduler.go", LineStart: 11},
				{Name: "Run", Path: "retry/scheduler.go", LineStart: 17},
			},
			// Deliberately unsorted: Render must not depend on backend order.
			Dependents: []string{"retry", "BillingService", "RetryScheduler"},
			Tests:      []string{"TestRetry", "TestBillingResume", "TestDuplicateWebhook"},
		},
	}, model.NextAction{Target: "WebhookProcessor.Process"})

	want := strings.Join([]string{
		"NEXT ACTION VALIDATION",
		"",
		"Target:",
		"WebhookProcessor.Process",
		"",
		"Definition:",
		"billing/webhook.go:16-18",
		"",
		"Impact:",
		"• BillingService",
		"• RetryScheduler",
		"• retry",
		"• 3 callers",
		"",
		"Relevant tests:",
		"• TestBillingResume",
		"• TestDuplicateWebhook",
		"• TestRetry",
		"",
		"Recommendation:",
		"Proceed, but run the 3 affected tests after changing Process.",
		"",
		"Confidence:",
		"OBSERVED",
		"",
	}, "\n")

	if got := v.Render(); got != want {
		t.Errorf("Render() =\n%s\nwant:\n%s", got, want)
	}
	if got := v.Render(); got != want {
		t.Error("Render() is not stable across calls")
	}
}

// TestRenderUnavailableBlock is the §48 guard in rendered form: the block must
// say the check did not happen and must not present empty sections that could
// be read as "nothing is affected".
func TestRenderUnavailableBlock(t *testing.T) {
	v := VerifyNextAction(context.Background(), Unavailable("entire graph is not installed"), model.NextAction{Target: "Process"})

	want := strings.Join([]string{
		"NEXT ACTION VALIDATION",
		"",
		"Target:",
		"Process",
		"",
		"Graph verification:",
		"UNAVAILABLE — entire graph is not installed",
		"",
		"Impact:",
		"• unknown — Graph did not run",
		"",
		"Relevant tests:",
		"• unknown — Graph did not run",
		"",
		"Recommendation:",
		"Graph verification was unavailable, so this action has not been checked against the code graph. Confirm the target, its callers and its tests yourself before editing.",
		"",
		"Confidence:",
		"UNKNOWN",
		"",
	}, "\n")

	if got := v.Render(); got != want {
		t.Errorf("Render() =\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderWithoutTarget(t *testing.T) {
	v := VerifyNextAction(context.Background(), Unavailable("x"), model.NextAction{Description: "keep going"})
	if !strings.Contains(v.Render(), "Target:\n(none recorded)") {
		t.Errorf("Render() = %q, want it to say no target was recorded", v.Render())
	}
}

// TestRenderUnresolvedBlock covers the middle ground: Graph ran, the target is
// not in the tree, and the sections must say "not resolved" rather than "none".
func TestRenderUnresolvedBlock(t *testing.T) {
	v := VerifyNextAction(context.Background(),
		fakeGraph{available: true, symbols: []model.GraphSymbol{}},
		model.NextAction{Target: "Process"})

	out := v.Render()
	for _, want := range []string{
		"Definition:\nnot resolved",
		"• unknown — target not resolved",
		"Confidence:\nUNKNOWN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render() =\n%s\nwant it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "UNAVAILABLE") {
		t.Error("Render() must not say UNAVAILABLE when Graph did run")
	}
}

// TestVerifyNextActionWithLocalGo is the end-to-end §35 moment: a real next
// action, verified by real static analysis over the fixture tree.
func TestVerifyNextActionWithLocalGo(t *testing.T) {
	gc := NewLocalGo(fixtureDir)
	action := model.NextAction{
		Description: "make webhook processing idempotent",
		Target:      "Process",
		Priority:    1,
		Confidence:  model.Recommended,
	}

	v := VerifyNextAction(context.Background(), gc, action)

	want := strings.Join([]string{
		"NEXT ACTION VALIDATION",
		"",
		"Target:",
		"Process",
		"",
		"Definition:",
		"billing/webhook.go:16-18",
		"",
		"Impact:",
		"• BillingService",
		"• RetryScheduler",
		"• retry",
		"• 3 callers",
		"",
		"Relevant tests:",
		"• TestBillingResume",
		"• TestDuplicateWebhook",
		"• TestRetry",
		"",
		"Recommendation:",
		"Proceed, but run the 3 affected tests after changing Process.",
		"",
		"Confidence:",
		"OBSERVED",
		"",
	}, "\n")

	if got := v.Render(); got != want {
		t.Fatalf("Render() =\n%s\nwant:\n%s", got, want)
	}
	if v.Reason != "" {
		t.Errorf("Reason = %q, want empty for a clean verification", v.Reason)
	}

	// The same tree must verify identically on a second run.
	if second := VerifyNextAction(context.Background(), NewLocalGo(fixtureDir), action); second.Render() != want {
		t.Error("verification is not reproducible across runs")
	}
}

func TestLocation(t *testing.T) {
	tests := []struct {
		name string
		sym  model.GraphSymbol
		want string
	}{
		{name: "range", sym: model.GraphSymbol{Path: "a.go", LineStart: 3, LineEnd: 9}, want: "a.go:3-9"},
		{name: "single line", sym: model.GraphSymbol{Path: "a.go", LineStart: 3, LineEnd: 3}, want: "a.go:3"},
		{name: "no lines", sym: model.GraphSymbol{Path: "a.go"}, want: "a.go"},
		{name: "nothing at all", sym: model.GraphSymbol{}, want: "an unknown location"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := location(tc.sym); got != tc.want {
				t.Errorf("location = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoinReason(t *testing.T) {
	tests := []struct {
		name           string
		existing, next string
		want           string
	}{
		{name: "first", existing: "", next: "b", want: "b"},
		{name: "second is empty", existing: "a", next: "", want: "a"},
		{name: "both", existing: "a", next: "b", want: "a; b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinReason(tc.existing, tc.next); got != tc.want {
				t.Errorf("joinReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifyImpactAnalysesTheResolvedDefinition guards the seam between Search
// and Impact. The target disambiguated which Run was meant; handing Impact the
// bare declaration name instead would throw that away and let a name-resolving
// backend answer about a different Run — printing one symbol's definition above
// another symbol's blast radius, with nothing to tell the reader.
func TestVerifyImpactAnalysesTheResolvedDefinition(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"alpha/alpha.go":  "package alpha\n\nfunc Run() error { return nil }\n",
		"alpha/caller.go": "package alpha\n\nfunc Drive() error { return Run() }\n",
		"beta/beta.go":    "package beta\n\nfunc Run() error { return nil }\n",
	})

	v := VerifyNextAction(context.Background(), NewLocalGo(dir), model.NextAction{Target: "beta.Run"})

	if !v.Available || !v.Resolved {
		t.Fatalf("Available = %v, Resolved = %v, want both true", v.Available, v.Resolved)
	}
	if v.Symbol.Path != "beta/beta.go" {
		t.Fatalf("Symbol.Path = %q, want beta/beta.go", v.Symbol.Path)
	}
	if v.Impact.Target.Path != v.Symbol.Path {
		t.Errorf("Impact.Target.Path = %q, want the resolved definition %q", v.Impact.Target.Path, v.Symbol.Path)
	}
	// alpha.Drive calls alpha.Run, not beta.Run. Attributing it here would
	// invent a dependent out of a namesake.
	if len(v.Impact.Callers) != 0 {
		t.Errorf("callers = %v, want none: nothing calls beta.Run", names(v.Impact.Callers))
	}
	if len(v.Impact.Dependents) != 0 {
		t.Errorf("dependents = %v, want none", v.Impact.Dependents)
	}
	if out := v.Render(); strings.Contains(out, "• alpha") {
		t.Errorf("Render() =\n%s\nmust not attribute alpha's callers to beta.Run", out)
	}
	// The same tree, resolved through the other package, still reports its own
	// caller: the fix must not simply suppress impact for qualified targets.
	other := VerifyNextAction(context.Background(), NewLocalGo(dir), model.NextAction{Target: "alpha.Run"})
	if len(other.Impact.Callers) != 1 {
		t.Errorf("alpha.Run callers = %v, want Drive", names(other.Impact.Callers))
	}
}

// TestVerifyReportsADefinitionMismatch: a backend is still free to answer about
// a different declaration than Search returned. When it does, the verification
// says so instead of pairing one definition with another one's blast radius.
func TestVerifyReportsADefinitionMismatch(t *testing.T) {
	elsewhere := model.GraphSymbol{Name: "Process", Path: "retry/process.go", LineStart: 4, LineEnd: 6}
	v := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impact:    model.GraphImpact{Target: elsewhere, Tests: []string{"TestRetry"}},
	}, model.NextAction{Target: "Process"})

	if !strings.Contains(v.Reason, "retry/process.go:4-6") || !strings.Contains(v.Reason, "billing/webhook.go:16-18") {
		t.Errorf("Reason = %q, want it to name both the analysed and the reported definition", v.Reason)
	}
	if !strings.Contains(v.Render(), "Note:") {
		t.Errorf("Render() =\n%s\nwant the mismatch surfaced as a note", v.Render())
	}
	// An agreeing backend must not be annotated.
	agreeing := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impact:    model.GraphImpact{Target: processSymbol, Tests: []string{"TestRetry"}},
	}, model.NextAction{Target: "Process"})
	if agreeing.Reason != "" {
		t.Errorf("Reason = %q, want empty when the backend agrees", agreeing.Reason)
	}
}

// TestRenderDoesNotInventAnEmptyBlastRadius is the §48 guard for the middle
// failure: the definition resolved but impact analysis did not run. Printing
// "no callers found in the analysed source tree" over an analysis that never
// happened is the same lie as omitting the section.
func TestRenderDoesNotInventAnEmptyBlastRadius(t *testing.T) {
	v := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impactErr: errors.New("call index missing"),
	}, model.NextAction{Target: "Process"})

	want := strings.Join([]string{
		"NEXT ACTION VALIDATION",
		"",
		"Target:",
		"Process",
		"",
		"Definition:",
		"billing/webhook.go:16-18",
		"",
		"Impact:",
		"• unknown — impact analysis did not run",
		"",
		"Relevant tests:",
		"• unknown — impact analysis did not run",
		"",
		"Note:",
		"impact analysis did not run: call index missing",
		"",
		"Recommendation:",
		"Proceed with care: Process was located at billing/webhook.go:16-18, but its blast radius is unknown because impact analysis did not run. Check its callers and tests yourself before editing.",
		"",
		"Confidence:",
		"OBSERVED",
		"",
	}, "\n")

	got := v.Render()
	if got != want {
		t.Errorf("Render() =\n%s\nwant:\n%s", got, want)
	}
	for _, forbidden := range []string{"no callers found", "none found"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("Render() =\n%s\nmust not contain %q when impact analysis did not run", got, forbidden)
		}
	}
}

// TestRenderStillReportsAGenuinelyEmptyBlastRadius is the other half of the
// guard above: when impact analysis did run and found nothing, that is a real
// finding and must keep reading as one.
func TestRenderStillReportsAGenuinelyEmptyBlastRadius(t *testing.T) {
	v := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impact:    model.GraphImpact{Target: processSymbol},
	}, model.NextAction{Target: "Process"})

	out := v.Render()
	for _, want := range []string{
		"• no callers found in the analysed source tree",
		"• none found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render() =\n%s\nwant it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "impact analysis did not run") {
		t.Errorf("Render() =\n%s\nmust not claim impact analysis failed when it ran", out)
	}
}

func TestDifferentDefinition(t *testing.T) {
	tests := []struct {
		name string
		a, b model.GraphSymbol
		want bool
	}{
		{
			name: "same file and line agree",
			a:    model.GraphSymbol{Path: "a.go", LineStart: 4},
			b:    model.GraphSymbol{Path: "a.go", LineStart: 4},
		},
		{
			name: "different files disagree",
			a:    model.GraphSymbol{Path: "a.go", LineStart: 4},
			b:    model.GraphSymbol{Path: "b.go", LineStart: 4},
			want: true,
		},
		{
			// A func and a method of the same name can share one file, so an
			// equal path is not on its own proof of agreement.
			name: "same file, different declaration",
			a:    model.GraphSymbol{Path: "a.go", LineStart: 4},
			b:    model.GraphSymbol{Path: "a.go", LineStart: 40},
			want: true,
		},
		{
			// A backend that does not report lines must not be accused.
			name: "missing line numbers are not a disagreement",
			a:    model.GraphSymbol{Path: "a.go", LineStart: 4},
			b:    model.GraphSymbol{Path: "a.go"},
		},
		{
			name: "missing paths are not a disagreement",
			a:    model.GraphSymbol{Path: "a.go", LineStart: 4},
			b:    model.GraphSymbol{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := differentDefinition(tc.a, tc.b); got != tc.want {
				t.Errorf("differentDefinition(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestVerifyReportsAMismatchWithinOneFile is the narrow case the path check
// alone would miss: the backend answered about the other Process in the same
// file.
func TestVerifyReportsAMismatchWithinOneFile(t *testing.T) {
	v := VerifyNextAction(context.Background(), fakeGraph{
		available: true,
		symbols:   []model.GraphSymbol{processSymbol},
		impact: model.GraphImpact{
			Target: model.GraphSymbol{Name: "Process", Path: "billing/webhook.go", LineStart: 40, LineEnd: 44},
			Tests:  []string{"TestRetry"},
		},
	}, model.NextAction{Target: "Process"})

	if !strings.Contains(v.Reason, "billing/webhook.go:40-44") {
		t.Errorf("Reason = %q, want it to name the declaration the impact answered for", v.Reason)
	}
}
