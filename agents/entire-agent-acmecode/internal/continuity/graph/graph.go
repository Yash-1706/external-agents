// Package graph is the Graph verification layer around continuation
// (plan §35, §36, §37, Step 20).
//
// Graph is not the product. Task state proposes a next action; this package
// asks the code graph what that action would touch and turns the answer into a
// decision — target, blast radius, candidate tests, recommendation. Raw Graph
// output is never displayed as the product (plan §35).
//
// Three backends implement model.GraphClient:
//
//	NewCLI      shells out to an installed Entire Graph binary.
//	NewLocalGo  runs real static analysis over a Go tree with go/ast, so that
//	            verification still happens when Entire is not installed.
//	Unavailable answers every call with model.ErrUnavailable and a reason.
//
// NewLocalGo resolves symbols by name rather than by full type resolution: two
// packages that both declare Process are indistinguishable to it. That is an
// honest approximation of a call graph, and it is why VerifyNextAction reports
// how many definitions matched instead of silently pretending there was one.
//
// Nothing in this package ever implies a check that did not happen. When a
// backend cannot answer, the verification says so explicitly and names the
// reason (plan §48, "Graph unavailable: do not pretend it occurred").
package graph

import (
	"context"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// graphReason lets a backend explain, in words a human can act on, why it
// cannot answer. VerifyNextAction quotes it instead of emitting a generic
// "unavailable", because a reason is what tells the next worker what to fix.
type graphReason interface {
	GraphUnavailableReason() string
}

// unavailableClient is the null backend. Every call reports model.ErrUnavailable
// so a caller can never mistake "we did not look" for "we looked and found
// nothing" (plan §48).
type unavailableClient struct {
	reason string
}

// Unavailable returns a GraphClient that performs no analysis and reports the
// given reason on every call. Wiring code uses it when no Graph backend could
// be resolved; it exists so the absence of Graph is represented explicitly in
// the state rather than by silently missing verification.
func Unavailable(reason string) model.GraphClient {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		// An unexplained gap is still a gap, but it is a much less useful one,
		// so give the reader something concrete to act on.
		reason = "no Graph backend is configured"
	}
	return unavailableClient{reason: reason}
}

// Available always reports false: that is the entire point of this backend.
func (u unavailableClient) Available(context.Context) bool { return false }

func (u unavailableClient) Search(context.Context, string) ([]model.GraphSymbol, error) {
	return nil, u.err()
}

func (u unavailableClient) Impact(context.Context, string) (model.GraphImpact, error) {
	return model.GraphImpact{}, u.err()
}

func (u unavailableClient) SemanticDiff(context.Context, string, string) ([]model.SemanticChange, error) {
	return nil, u.err()
}

// Describe names the backend in output so the user always knows which Graph
// they are (not) talking to.
func (u unavailableClient) Describe() string { return "graph unavailable: " + u.reason }

// GraphUnavailableReason implements graphReason.
func (u unavailableClient) GraphUnavailableReason() string { return u.reason }

func (u unavailableClient) err() error {
	return fmt.Errorf("%w: %s", model.ErrUnavailable, u.reason)
}

// Detect chooses the strongest Graph backend that is actually usable here.
//
// An installed Entire Graph CLI wins, because it is the real thing. Failing
// that, a Go source tree can still be analysed locally. Failing both, the
// result is an explicitly unavailable client carrying the reason — never a
// silently empty one (plan §48).
func Detect(bin, dir string) model.GraphClient {
	if bin != "" {
		if _, err := exec.LookPath(bin); err == nil {
			return NewCLI(bin, dir)
		}
	}
	if hasGoSources(dir) {
		return NewLocalGo(dir)
	}
	var b strings.Builder
	b.WriteString("no Graph backend: ")
	if bin == "" {
		b.WriteString("no Graph CLI was configured")
	} else {
		b.WriteString("CLI " + strconv.Quote(bin) + " is not on PATH")
	}
	b.WriteString(" and no Go sources were found under " + dirLabel(dir))
	return Unavailable(b.String())
}

// dirLabel renders an analysis root for human-readable messages.
func dirLabel(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return "the working directory"
	}
	return strconv.Quote(dir)
}

// skipDir reports whether a directory is excluded from static analysis.
// vendor/ and testdata/ hold code that is not the project's own behaviour, so
// reporting a symbol from them as the target would be a false positive; dot
// directories hold VCS and tool state (plan §35: the answer must be actionable).
func skipDir(name string) bool {
	switch name {
	case "vendor", "testdata", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

// hasGoSources reports whether dir contains at least one Go file that local
// analysis would consider. It applies the same exclusions as the analyser so
// that Detect never selects a backend that would immediately report nothing.
func hasGoSources(dir string) bool {
	root := dir
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	found := false
	// Errors are deliberately swallowed: an unreadable subtree means "no
	// sources here", not "abort".
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if found {
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// sortSymbols orders symbols by source location. Determinism is a product
// requirement: the same tree must produce byte-identical verification output on
// every run and on every machine.
func sortSymbols(in []model.GraphSymbol) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.LineStart != b.LineStart {
			return a.LineStart < b.LineStart
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})
}

// sortImpact normalises an impact set so that output does not depend on the
// order a backend happened to emit.
func sortImpact(imp *model.GraphImpact) {
	sortSymbols(imp.Callers)
	imp.Dependents = sortedUnique(imp.Dependents)
	imp.Tests = sortedUnique(imp.Tests)
	imp.Files = sortedUnique(imp.Files)
}

// sortChanges orders a semantic diff by path, then symbol, then change kind.
func sortChanges(in []model.SemanticChange) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.Change < b.Change
	})
}

// sortedUnique returns the distinct non-empty entries of in, sorted. It always
// returns a non-nil slice so serialized output carries [] rather than null,
// matching the convention model.NewEngineeringState sets.
func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// setToSorted converts a membership set into deterministic slice order. Map
// iteration order must never reach output.
func setToSorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		if k == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// plural picks the singular or plural noun for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
