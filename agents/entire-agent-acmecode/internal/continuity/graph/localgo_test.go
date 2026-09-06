package graph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// writeTree materialises a throwaway Go tree for the parse-failure cases. The
// unparseable file is generated rather than committed so that repository-wide
// gofmt stays clean while the degradation path is still exercised for real.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

const (
	goodSource   = "package sample\n\nfunc Process(id string) error { return nil }\n"
	brokenSource = "package sample\n\n// Deliberately unterminated.\nfunc Broken(id string) error {\n"
)

func names(syms []model.GraphSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Path+":"+s.Name)
	}
	return out
}

// TestLocalGoSearch covers target resolution, the shapes a human writes a
// target in, and the exclusions that keep vendored and generated decoys out of
// the answer.
func TestLocalGoSearch(t *testing.T) {
	ctx := context.Background()
	gc := NewLocalGo(fixtureDir)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// vendor/lib and the nested testdata/generated both declare
			// Process. Exactly one of the three is a real answer.
			name:  "method resolves once, decoys excluded",
			query: "Process",
			want:  []string{"billing/webhook.go:Process"},
		},
		{
			name:  "qualified by receiver",
			query: "WebhookProcessor.Process",
			want:  []string{"billing/webhook.go:Process"},
		},
		{
			name:  "written the way a human writes a pointer method",
			query: "(*WebhookProcessor).Process",
			want:  []string{"billing/webhook.go:Process"},
		},
		{
			name:  "type declaration",
			query: "WebhookProcessor",
			want:  []string{"billing/webhook.go:WebhookProcessor"},
		},
		{
			name:  "qualified by package",
			query: "billing.WebhookProcessor",
			want:  []string{"billing/webhook.go:WebhookProcessor"},
		},
		{
			name:  "unexported package-level func",
			query: "validate",
			want:  []string{"billing/webhook.go:validate"},
		},
		{
			name:  "package-level func in another package",
			query: "Run",
			want:  []string{"retry/scheduler.go:Run"},
		},
		{
			// Absence is a fact the analyser is allowed to state: it is not the
			// same as ErrUnavailable.
			name:  "missing target is an empty answer, not an error",
			query: "NoSuchSymbol",
			want:  []string{},
		},
		{
			name:  "blank query matches nothing",
			query: "   ",
			want:  []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gc.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search(%q) error = %v", tc.query, err)
			}
			if !reflect.DeepEqual(names(got), tc.want) {
				t.Errorf("Search(%q) = %v, want %v", tc.query, names(got), tc.want)
			}
		})
	}
}

// TestLocalGoSearchPopulatesLocation checks that Path and the line range really
// come from the FileSet: without them the evidence citation is worthless.
func TestLocalGoSearchPopulatesLocation(t *testing.T) {
	got, err := NewLocalGo(fixtureDir).Search(context.Background(), "Process")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search returned %d symbols, want 1", len(got))
	}
	want := model.GraphSymbol{
		Name:      "Process",
		Kind:      "method",
		Path:      "billing/webhook.go",
		LineStart: 16,
		LineEnd:   18,
		Signature: "func (p *WebhookProcessor) Process(id string) error",
	}
	if got[0] != want {
		t.Errorf("symbol = %+v, want %+v", got[0], want)
	}
}

func TestLocalGoSearchTypeSignature(t *testing.T) {
	got, err := NewLocalGo(fixtureDir).Search(context.Background(), "WebhookProcessor")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search returned %d symbols, want 1", len(got))
	}
	// The struct body is deliberately not rendered: a dependent breaks against
	// the name and kind, not against the field list.
	if want := "type WebhookProcessor struct"; got[0].Signature != want {
		t.Errorf("signature = %q, want %q", got[0].Signature, want)
	}
	if got[0].Kind != "type" {
		t.Errorf("kind = %q, want %q", got[0].Kind, "type")
	}
}

// TestLocalGoImpact is the core of the verification layer: who calls the target,
// which components therefore depend on it, and which tests to run.
func TestLocalGoImpact(t *testing.T) {
	ctx := context.Background()
	gc := NewLocalGo(fixtureDir)

	tests := []struct {
		name           string
		symbol         string
		wantCallers    []string
		wantDependents []string
		wantTests      []string
		wantFiles      []string
	}{
		{
			// Selector calls from two receivers plus one package-level func.
			// TestDuplicateWebhook also calls Process, but a test is reported
			// as a test, never counted in the blast radius.
			name:           "method with method and package-level callers",
			symbol:         "Process",
			wantCallers:    []string{"billing/service.go:Handle", "retry/scheduler.go:Reschedule", "retry/scheduler.go:Run"},
			wantDependents: []string{"BillingService", "RetryScheduler", "retry"},
			wantTests:      []string{"TestBillingResume", "TestDuplicateWebhook", "TestRetry"},
			wantFiles: []string{
				"billing/service.go", "billing/service_test.go",
				"billing/webhook.go", "billing/webhook_test.go",
				"retry/scheduler.go", "retry/scheduler_test.go",
			},
		},
		{
			// The plain-identifier call shape.
			name:           "unexported func called through a plain ident",
			symbol:         "validate",
			wantCallers:    []string{"billing/webhook.go:Process"},
			wantDependents: []string{"WebhookProcessor"},
			wantTests:      []string{"TestDuplicateWebhook"},
			wantFiles:      []string{"billing/webhook.go", "billing/webhook_test.go"},
		},
		{
			// Only a test calls Handle, so the blast radius is empty while a
			// candidate test still exists. Reporting "1 caller" here would
			// overstate the risk.
			name:           "test-only caller does not become blast radius",
			symbol:         "Handle",
			wantCallers:    []string{},
			wantDependents: []string{},
			wantTests:      []string{"TestBillingResume"},
			wantFiles:      []string{"billing/service.go", "billing/service_test.go"},
		},
		{
			// A type is never "called", so the caller walk finds nothing; the
			// test that constructs it is still a candidate.
			name:           "type target has no callers but keeps its tests",
			symbol:         "RetryScheduler",
			wantCallers:    []string{},
			wantDependents: []string{},
			wantTests:      []string{"TestRetry"},
			wantFiles:      []string{"retry/scheduler.go", "retry/scheduler_test.go"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			imp, err := gc.Impact(ctx, tc.symbol)
			if err != nil {
				t.Fatalf("Impact(%q) error = %v", tc.symbol, err)
			}
			if !reflect.DeepEqual(names(imp.Callers), tc.wantCallers) {
				t.Errorf("callers = %v, want %v", names(imp.Callers), tc.wantCallers)
			}
			if !reflect.DeepEqual(imp.Dependents, tc.wantDependents) {
				t.Errorf("dependents = %v, want %v", imp.Dependents, tc.wantDependents)
			}
			if !reflect.DeepEqual(imp.Tests, tc.wantTests) {
				t.Errorf("tests = %v, want %v", imp.Tests, tc.wantTests)
			}
			if !reflect.DeepEqual(imp.Files, tc.wantFiles) {
				t.Errorf("files = %v, want %v", imp.Files, tc.wantFiles)
			}
			if imp.Target.Name == "" || imp.Target.Path == "" {
				t.Errorf("target = %+v, want a located definition", imp.Target)
			}
		})
	}
}

// TestLocalGoImpactExcludesTestLookalikes guards the go test naming rule: a
// helper called TestingHelperIsNotATest must never be recommended as a test to
// run.
func TestLocalGoImpactExcludesTestLookalikes(t *testing.T) {
	imp, err := NewLocalGo(fixtureDir).Impact(context.Background(), "Process")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	for _, name := range imp.Tests {
		if name == "TestingHelperIsNotATest" {
			t.Fatalf("tests = %v, want the lookalike helper excluded", imp.Tests)
		}
	}
}

func TestIsTestFuncName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "Test", want: true},
		{name: "TestRetry", want: true},
		{name: "Test_retry", want: true},
		{name: "TestingHelperIsNotATest", want: false},
		{name: "Testify", want: false},
		{name: "BenchmarkRetry", want: false},
		{name: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTestFuncName(tc.name); got != tc.want {
				t.Errorf("isTestFuncName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestLocalGoImpactNotFound: an analysed tree that does not contain the symbol
// is ErrNotFound, which is a different fact from ErrUnavailable.
func TestLocalGoImpactNotFound(t *testing.T) {
	_, err := NewLocalGo(fixtureDir).Impact(context.Background(), "NoSuchSymbol")
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Impact error = %v, want ErrNotFound", err)
	}
	if errors.Is(err, model.ErrUnavailable) {
		t.Error("a missing symbol must not be reported as an unavailable Graph")
	}
}

// TestLocalGoDegradation covers the trees where analysis cannot produce an
// answer at all. Every one of them must report ErrUnavailable rather than an
// empty result that would read as "nothing found".
func TestLocalGoDegradation(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		dir           func(t *testing.T) string
		wantAvailable bool
		wantReason    string
	}{
		{
			name:          "directory with no Go sources",
			dir:           func(*testing.T) string { return "testdata/empty" },
			wantAvailable: false,
			wantReason:    "no Go sources",
		},
		{
			name:          "directory that does not exist",
			dir:           func(*testing.T) string { return "testdata/does-not-exist" },
			wantAvailable: false,
			wantReason:    "no Go sources",
		},
		{
			// The file is there, so Available is true, but nothing in it parses:
			// the honest answer is unavailable, never an empty result.
			name: "every Go file fails to parse",
			dir: func(t *testing.T) string {
				return writeTree(t, map[string]string{"broken.go": brokenSource})
			},
			wantAvailable: true,
			wantReason:    "could be parsed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gc := NewLocalGo(tc.dir(t))
			if got := gc.Available(ctx); got != tc.wantAvailable {
				t.Errorf("Available() = %v, want %v", got, tc.wantAvailable)
			}
			_, err := gc.Search(ctx, "Process")
			if !errors.Is(err, model.ErrUnavailable) {
				t.Fatalf("Search error = %v, want ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("Search error = %q, want it to explain %q", err, tc.wantReason)
			}
			if _, err := gc.Impact(ctx, "Process"); !errors.Is(err, model.ErrUnavailable) {
				t.Errorf("Impact error = %v, want ErrUnavailable", err)
			}
		})
	}
}

// TestLocalGoSurvivesUnparseableFile: one file that does not parse must not
// blind the analysis of the files around it (plan §33).
func TestLocalGoSurvivesUnparseableFile(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"broken.go":      brokenSource,
		"good/sample.go": goodSource,
	})
	got, err := NewLocalGo(dir).Search(context.Background(), "Process")
	if err != nil {
		t.Fatalf("Search error = %v, want the broken file to be skipped", err)
	}
	if want := []string{"good/sample.go:Process"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("Search = %v, want %v", names(got), want)
	}
}

// TestLocalGoSemanticDiffUnavailable covers the git degradation path. It never
// touches the project repository: a directory outside any work tree, and a
// directory that does not exist at all.
func TestLocalGoSemanticDiffUnavailable(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		dir        string
		wantReason string
	}{
		{
			name:       "missing directory short-circuits before running git",
			dir:        "testdata/does-not-exist",
			wantReason: "not a readable directory",
		},
		{
			// Either git is absent, the temp dir is not a work tree, or the
			// bogus refs do not resolve. Every one of those is unavailable.
			name: "no usable revisions",
			dir:  t.TempDir(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLocalGo(tc.dir).SemanticDiff(ctx, "refs/entire/no-such-a", "refs/entire/no-such-b")
			if !errors.Is(err, model.ErrUnavailable) {
				t.Fatalf("SemanticDiff error = %v, want ErrUnavailable", err)
			}
			if tc.wantReason != "" && !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("SemanticDiff error = %q, want it to explain %q", err, tc.wantReason)
			}
		})
	}
}

// TestDiffExported exercises the semantic-diff comparison without git, so the
// added/removed/modified classification is covered on its own.
func TestDiffExported(t *testing.T) {
	tests := []struct {
		name   string
		before map[string]string
		after  map[string]string
		want   []model.SemanticChange
	}{
		{
			name:   "added",
			before: map[string]string{},
			after:  map[string]string{"Process": "func Process(id string) error"},
			want: []model.SemanticChange{
				{Symbol: "Process", Path: "billing/webhook.go", Change: "added", Detail: "func Process(id string) error"},
			},
		},
		{
			name:   "removed",
			before: map[string]string{"Process": "func Process(id string) error"},
			after:  map[string]string{},
			want: []model.SemanticChange{
				{Symbol: "Process", Path: "billing/webhook.go", Change: "removed", Detail: "func Process(id string) error"},
			},
		},
		{
			name:   "modified reports both sides",
			before: map[string]string{"Process": "func Process(id string) error"},
			after:  map[string]string{"Process": "func Process(ctx context.Context, id string) error"},
			want: []model.SemanticChange{{
				Symbol: "Process",
				Path:   "billing/webhook.go",
				Change: "modified",
				Detail: "func Process(id string) error -> func Process(ctx context.Context, id string) error",
			}},
		},
		{
			name:   "identical signatures produce nothing",
			before: map[string]string{"Process": "func Process(id string) error"},
			after:  map[string]string{"Process": "func Process(id string) error"},
			want:   []model.SemanticChange{},
		},
		{
			name:   "ordering does not depend on map iteration",
			before: map[string]string{"Zebra": "type Zebra struct", "Alpha": "type Alpha struct"},
			after:  map[string]string{},
			want: []model.SemanticChange{
				{Symbol: "Alpha", Path: "billing/webhook.go", Change: "removed", Detail: "type Alpha struct"},
				{Symbol: "Zebra", Path: "billing/webhook.go", Change: "removed", Detail: "type Zebra struct"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := diffExported("billing/webhook.go", tc.before, tc.after)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("diffExported = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGoPathsAppliesExclusions(t *testing.T) {
	raw := []byte("billing/webhook.go\nvendor/lib/lib.go\ndocs/readme.md\n\ninternal/graph/testdata/proj/x.go\nbilling/webhook.go\n")
	want := []string{"billing/webhook.go"}
	if got := goPaths(raw); !reflect.DeepEqual(got, want) {
		t.Errorf("goPaths = %v, want %v", got, want)
	}
}

// TestLocalGoIsDeterministic: two runs over the same tree must be identical,
// which is what makes a rendered verification reproducible (product rule).
func TestLocalGoIsDeterministic(t *testing.T) {
	ctx := context.Background()
	first, err := NewLocalGo(fixtureDir).Impact(ctx, "Process")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	second, err := NewLocalGo(fixtureDir).Impact(ctx, "Process")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two runs differ:\n%+v\n%+v", first, second)
	}
}

// TestLocalGoRespectsContext: a cancelled context stops the walk instead of
// producing a half-analysed answer that would understate the blast radius.
func TestLocalGoRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewLocalGo(fixtureDir).Search(ctx, "Process"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search error = %v, want context.Canceled", err)
	}
	if NewLocalGo(fixtureDir).Available(ctx) {
		t.Error("Available reported true under a cancelled context")
	}
}

// TestLocalGoImpactAppliesTheTestFileRule pins both halves of the go test rule.
// A func named like a test but declared in production code is production code:
// it is a caller, and it must never be handed to the next worker as a test to
// run, because go test will never run it (plan §16). Symmetrically, a helper
// declared in a _test.go file is test code even though it is not named like a
// test, so it is never counted in the blast radius.
func TestLocalGoImpactAppliesTheTestFileRule(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"prod/prod.go": "package prod\n\n" +
			"func Target() error { return nil }\n\n" +
			"// TestConnection is production code that happens to be named like a test.\n" +
			"func TestConnection() error { return Target() }\n",
		"prod/prod_test.go": "package prod\n\n" +
			"import \"testing\"\n\n" +
			"func helperCall() error { return Target() }\n\n" +
			"func TestReal(t *testing.T) { _ = helperCall() }\n",
	})

	imp, err := NewLocalGo(dir).Impact(context.Background(), "Target")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}

	// TestConnection is a real caller; helperCall is not.
	if want := []string{"prod/prod.go:TestConnection"}; !reflect.DeepEqual(names(imp.Callers), want) {
		t.Errorf("callers = %v, want %v", names(imp.Callers), want)
	}
	if want := []string{"prod"}; !reflect.DeepEqual(imp.Dependents, want) {
		t.Errorf("dependents = %v, want %v", imp.Dependents, want)
	}
	// Only the function go test would actually run may be listed.
	if want := []string{"TestReal"}; !reflect.DeepEqual(imp.Tests, want) {
		t.Errorf("tests = %v, want %v (a production func must never be listed as a test)", imp.Tests, want)
	}
}

// TestLocalGoImpactIgnoresTestNamesOutsideTestFiles is the narrow version of the
// rule above: with no _test.go file in the tree at all, there are no tests to
// recommend, however the production functions happen to be named.
func TestLocalGoImpactIgnoresTestNamesOutsideTestFiles(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"prod/prod.go": "package prod\n\n" +
			"func Target() error { return nil }\n\n" +
			"func TestConnection() error { return Target() }\n",
	})
	imp, err := NewLocalGo(dir).Impact(context.Background(), "Target")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	if len(imp.Tests) != 0 {
		t.Errorf("tests = %v, want none: the tree contains no _test.go file", imp.Tests)
	}
	if len(imp.Callers) != 1 {
		t.Errorf("callers = %v, want the production caller counted", names(imp.Callers))
	}
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "billing/webhook_test.go", want: true},
		{path: "webhook_test.go", want: true},
		{path: "billing/webhook.go", want: false},
		{path: "billing/test.go", want: false},
		{path: "billing/_test.go", want: true},
		{path: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if got := isTestFile(tc.path); got != tc.want {
				t.Errorf("isTestFile(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestLocalGoImpactScopesPlainIdentifierCalls pins Go's own scoping rule on the
// caller walk. Bare Run() resolves in its file's package scope, so a namesake
// in another package is not a caller of it. Names like New, Run and Parse
// repeat across packages in every real tree, and inventing a dependent out of a
// namesake is a wrong answer presented as OBSERVED evidence, not a rounding.
func TestLocalGoImpactScopesPlainIdentifierCalls(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		files          map[string]string
		symbol         string
		wantCallers    []string
		wantDependents []string
	}{
		{
			// Drive's bare Run() resolves in package alpha and cannot reach
			// beta, however identical the two names look.
			name: "plain call in another package is not a caller",
			files: map[string]string{
				"alpha/alpha.go":  "package alpha\n\nfunc Run() error { return nil }\n",
				"alpha/caller.go": "package alpha\n\nfunc Drive() error { return Run() }\n",
				"beta/beta.go":    "package beta\n\nfunc Run() error { return nil }\n",
			},
			symbol:         "beta.Run",
			wantCallers:    []string{},
			wantDependents: []string{},
		},
		{
			// The other half of the rule: suppressing the cross-package match
			// must not suppress the real, same-package one.
			name: "plain call in the same package is a caller",
			files: map[string]string{
				"alpha/alpha.go":  "package alpha\n\nfunc Run() error { return nil }\n",
				"alpha/caller.go": "package alpha\n\nfunc Drive() error { return Run() }\n",
				"beta/beta.go":    "package beta\n\nfunc Run() error { return nil }\n",
			},
			symbol:         "alpha.Run",
			wantCallers:    []string{"alpha/caller.go:Drive"},
			wantDependents: []string{"alpha"},
		},
		{
			// A qualified call is a selector, and a selector is still a caller.
			name: "qualified cross-package call is still a caller",
			files: map[string]string{
				"beta/beta.go": "package beta\n\nfunc Run() error { return nil }\n",
				"gamma/gamma.go": "package gamma\n\n" +
					"import \"example.com/tree/beta\"\n\n" +
					"func Kick() error { return beta.Run() }\n",
			},
			symbol:         "beta.Run",
			wantCallers:    []string{"gamma/gamma.go:Kick"},
			wantDependents: []string{"gamma"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			imp, err := NewLocalGo(writeTree(t, tc.files)).Impact(ctx, tc.symbol)
			if err != nil {
				t.Fatalf("Impact(%q) error = %v", tc.symbol, err)
			}
			if !reflect.DeepEqual(names(imp.Callers), tc.wantCallers) {
				t.Errorf("callers = %v, want %v", names(imp.Callers), tc.wantCallers)
			}
			if !reflect.DeepEqual(imp.Dependents, tc.wantDependents) {
				t.Errorf("dependents = %v, want %v", imp.Dependents, tc.wantDependents)
			}
		})
	}
}

// TestLocalGoImpactSelectorCallsStayNameResolved records a KNOWN LIMITATION
// rather than a desired behaviour, so that it is visible in the test suite
// instead of being discovered in output.
//
// A selector call is matched on the method name alone: beta.Run() is reported
// as a caller of alpha.Run too, because deciding what the qualifier refers to
// needs a type checker this backend deliberately does not run. The limitation
// is disclosed to the reader — Describe() says "name-resolved", and
// VerifyNextAction reports how many definitions matched — and it errs towards
// listing an extra caller rather than hiding a real one, which is the safer
// direction for a blast radius. Narrowing it later should update this test;
// widening it silently should fail it.
func TestLocalGoImpactSelectorCallsStayNameResolved(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nfunc Run() error { return nil }\n",
		"beta/beta.go":   "package beta\n\nfunc Run() error { return nil }\n",
		"gamma/gamma.go": "package gamma\n\n" +
			"import \"example.com/tree/beta\"\n\n" +
			"func Kick() error { return beta.Run() }\n",
	})
	imp, err := NewLocalGo(dir).Impact(context.Background(), "alpha.Run")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	if want := []string{"gamma/gamma.go:Kick"}; !reflect.DeepEqual(names(imp.Callers), want) {
		t.Errorf("callers = %v, want %v (known limitation: selectors are name-resolved)", names(imp.Callers), want)
	}
}

// TestLocalGoImpactPlainCallNeverReachesAMethod: calling a method always needs a
// receiver, so a bare Process() is a call to some function named Process, never
// to WebhookProcessor.Process.
func TestLocalGoImpactPlainCallNeverReachesAMethod(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"svc/svc.go": "package svc\n\n" +
			"type Proc struct{}\n\n" +
			"func (p *Proc) Process(id string) error { return nil }\n\n" +
			"func Process(id string) error { return nil }\n\n" +
			"func Local() error { return Process(\"x\") }\n\n" +
			"func ViaReceiver(p *Proc) error { return p.Process(\"x\") }\n",
	})
	imp, err := NewLocalGo(dir).Impact(context.Background(), "Proc.Process")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	if want := []string{"svc/svc.go:ViaReceiver"}; !reflect.DeepEqual(names(imp.Callers), want) {
		t.Errorf("callers = %v, want %v: a bare Process() calls the func, not the method", names(imp.Callers), want)
	}
}

// TestLocalGoImpactKeepsDotImportedCallers: a dot-import is the one case where a
// plain identifier does resolve outside its own package, so the loose match is
// kept there. Missing a real caller understates a blast radius, which is the
// more dangerous of the two errors.
func TestLocalGoImpactKeepsDotImportedCallers(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"beta/beta.go": "package beta\n\nfunc Run() error { return nil }\n",
		"gamma/gamma.go": "package gamma\n\n" +
			"import . \"example.com/tree/beta\"\n\n" +
			"func Kick() error { return Run() }\n",
	})
	imp, err := NewLocalGo(dir).Impact(context.Background(), "beta.Run")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	if want := []string{"gamma/gamma.go:Kick"}; !reflect.DeepEqual(names(imp.Callers), want) {
		t.Errorf("callers = %v, want %v: a dot-imported plain call is a real caller", names(imp.Callers), want)
	}
}

// TestCallShapes covers the call forms the caller walk has to recognise,
// including the generic and parenthesised wrappers.
func TestCallShapes(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantPlain    bool
		wantSelector bool
	}{
		{name: "plain", body: "Process(id)", wantPlain: true},
		{name: "selector", body: "p.Process(id)", wantSelector: true},
		{name: "parenthesised", body: "(Process)(id)", wantPlain: true},
		{name: "generic instantiation", body: "Process[int](id)", wantPlain: true},
		{name: "generic selector", body: "p.Process[int, string](id)", wantSelector: true},
		{name: "both shapes", body: "Process(id); p.Process(id)", wantPlain: true, wantSelector: true},
		{name: "nested in an argument", body: "log(p.Process(id))", wantSelector: true},
		{name: "different name", body: "Handle(id)"},
		{name: "not a call", body: "_ = Process"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := "package sample\n\nfunc caller() { " + tc.body + " }\n"
			dir := writeTree(t, map[string]string{"sample.go": src})
			fset, files, err := (&localGo{dir: dir}).load(context.Background())
			if err != nil {
				t.Fatalf("load error = %v", err)
			}
			decls := collectDecls(fset, files)
			if len(decls) != 1 || decls[0].body == nil {
				t.Fatalf("collectDecls = %+v, want one func decl", decls)
			}
			plain, selector := callShapes(decls[0].body, "Process")
			if plain != tc.wantPlain || selector != tc.wantSelector {
				t.Errorf("callShapes(%q) = (plain=%v, selector=%v), want (plain=%v, selector=%v)",
					tc.body, plain, selector, tc.wantPlain, tc.wantSelector)
			}
		})
	}
}
