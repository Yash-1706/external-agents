package graph

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// localGo is the fallback Graph backend: genuine static analysis of a Go tree
// using go/parser and go/ast. It exists because verification must still happen
// when Entire Graph is not installed — an unverified next action is exactly the
// failure mode this product is built to remove (plan §35).
//
// Its resolution is by identifier name, not by full type checking. A selector
// call p.Process() is attributed to any declaration named Process, because
// deciding what p is would need a type checker. Go's own scoping rule is
// applied where it is free: a plain identifier Process() resolves in its file's
// package scope, so it is only attributed to a package-level func in that same
// package. Callers of this package must not present its output as type-exact;
// VerifyNextAction reports the number of matched definitions so an ambiguous
// name is visible rather than hidden.
type localGo struct {
	dir string
}

// NewLocalGo returns a GraphClient that analyses the Go sources under dir.
func NewLocalGo(dir string) model.GraphClient { return &localGo{dir: dir} }

// Available reports whether there is anything here to analyse.
func (g *localGo) Available(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	return hasGoSources(g.dir)
}

// Describe names the backend, including the fact that it resolves by name, so
// that a reader of the verification knows the strength of the claim.
func (g *localGo) Describe() string {
	return fmt.Sprintf("local Go static analysis (go/ast, name-resolved) over %s", dirLabel(g.dir))
}

// root returns the analysis root, defaulting to the working directory.
func (g *localGo) root() string {
	if strings.TrimSpace(g.dir) == "" {
		return "."
	}
	return g.dir
}

// sourceFile is one successfully parsed Go file from the analysed tree.
type sourceFile struct {
	// path is slash-separated and relative to the analysis root, so that a
	// Windows run and a Linux run of the same tree produce identical output.
	path string
	file *ast.File
}

// decl is a declaration located in the analysed tree, with the context Impact
// needs in order to name the components that depend on it.
type decl struct {
	sym model.GraphSymbol
	// recv is the receiver type name for a method, empty otherwise. It is what
	// makes "BillingService" a dependent rather than just "package billing".
	recv string
	pkg  string
	// dir is the slash-separated directory holding the declaration. Together
	// with pkg it identifies the package scope a plain identifier resolves in.
	dir string
	// dotImported records that the declaring file dot-imports another package,
	// which is the one case where a plain identifier does resolve outside its
	// own package. Such a file keeps the loose name match: missing a real
	// caller understates a blast radius, which is the more dangerous error.
	dotImported bool
	// body is the function body to scan for calls; nil for type declarations.
	body *ast.BlockStmt
}

// load walks the analysis root and parses every Go file it will consider.
//
// A file that does not parse is skipped, never fatal: one broken file must not
// blind the whole analysis (plan §33, degrade gracefully). Files are returned
// in path order so every downstream result is deterministic.
func (g *localGo) load(ctx context.Context) (*token.FileSet, []sourceFile, error) {
	root := g.root()
	fset := token.NewFileSet()
	var files []sourceFile
	skipped := 0

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// An unreadable entry removes that subtree from the analysis; it
			// does not end the walk.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// The root itself is analysed even when it is called "testdata":
			// exclusions apply to directories found underneath it.
			if p != root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			skipped++
			return nil
		}
		files = append(files, sourceFile{path: relSlash(root, p), file: parsed})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		if skipped > 0 {
			return nil, nil, fmt.Errorf("%w: none of the %d Go file(s) under %s could be parsed",
				model.ErrUnavailable, skipped, dirLabel(g.dir))
		}
		return nil, nil, fmt.Errorf("%w: no Go sources under %s", model.ErrUnavailable, dirLabel(g.dir))
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].path < files[j].path })
	return fset, files, nil
}

// Search finds func, method and type declarations matching query.
//
// An empty result with a nil error means the tree was analysed and the target
// is not in it. That is a fact worth acting on, and it is deliberately distinct
// from model.ErrUnavailable, which means no analysis happened at all.
func (g *localGo) Search(ctx context.Context, query string) ([]model.GraphSymbol, error) {
	fset, files, err := g.load(ctx)
	if err != nil {
		return nil, err
	}
	out := []model.GraphSymbol{}
	for _, d := range collectDecls(fset, files) {
		if d.matches(query) {
			out = append(out, d.sym)
		}
	}
	return out, nil
}

// Impact reports who calls the symbol, which components therefore depend on it,
// and which tests are candidates to exercise the change (plan §35).
//
// Nothing declared in a _test.go file is counted as a caller — neither a Test
// function nor a helper it calls: "7 callers" must mean seven places production
// behaviour changes, or the number is not a blast radius at all. Symmetrically,
// only a _test.go file can contribute a candidate test, so a production func
// named like a test is a caller and never a test to run.
func (g *localGo) Impact(ctx context.Context, symbol string) (model.GraphImpact, error) {
	fset, files, err := g.load(ctx)
	if err != nil {
		return model.GraphImpact{}, err
	}
	decls := collectDecls(fset, files)

	var target *decl
	for i := range decls {
		if decls[i].matches(symbol) {
			target = &decls[i]
			break
		}
	}
	if target == nil {
		// The analysis worked and the symbol is simply absent: ErrNotFound, not
		// ErrUnavailable.
		return model.GraphImpact{}, fmt.Errorf("graph: no definition of %q under %s: %w",
			symbol, dirLabel(g.dir), model.ErrNotFound)
	}

	name := target.sym.Name
	callers := []model.GraphSymbol{}
	dependents := map[string]bool{}
	fileSet := map[string]bool{target.sym.Path: true}
	// referenced is the set of identifiers whose presence in a file makes that
	// file's tests candidates: the target plus everything that calls it.
	referenced := map[string]bool{name: true}

	for _, d := range decls {
		if d.body == nil || d.sameDeclAs(*target) {
			continue
		}
		plain, selector := shapesFor(*target, d.body, name)
		if !d.reaches(*target, plain, selector) {
			continue
		}
		if isTestFile(d.sym.Path) {
			// Nothing declared in a _test.go file is blast radius, whether it is
			// a Test function or a helper it calls: changing the target changes
			// how the tests exercise it, not how production behaves. "3 callers"
			// has to mean three places production behaviour changes or it is not
			// a blast radius. The declaration is still recorded as a reference so
			// the test file it lives in becomes a candidate below.
			referenced[d.sym.Name] = true
			continue
		}
		callers = append(callers, d.sym)
		referenced[d.sym.Name] = true
		fileSet[d.sym.Path] = true
		if d.recv != "" {
			dependents[d.recv] = true
		} else if d.pkg != "" {
			dependents[d.pkg] = true
		}
	}

	tests := map[string]bool{}
	for _, sf := range files {
		// Only a _test.go file can declare a test. A func named TestConnection
		// in production code is ordinary code that go test will never run, and
		// listing it here would tell the next worker to run something that is
		// not a test (plan §16: never present a check this product has not
		// seen happen).
		if !isTestFile(sf.path) || !referencesAny(sf.file, referenced) {
			continue
		}
		for _, d := range sf.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTestFuncName(fn.Name.Name) {
				continue
			}
			tests[fn.Name.Name] = true
			fileSet[sf.path] = true
		}
	}

	return model.GraphImpact{
		Target:     target.sym,
		Callers:    callers,
		Dependents: setToSorted(dependents),
		Tests:      setToSorted(tests),
		Files:      setToSorted(fileSet),
	}, nil
}

// SemanticDiff compares the exported declaration set of every changed Go file
// between two revisions (plan §36, moment 3: final semantic-diff analysis).
//
// It reads both sides through "git show <ref>:<path>" so the comparison is
// against committed history rather than whatever happens to be in the working
// tree. Without a usable git it returns model.ErrUnavailable: an empty diff
// would read as "nothing changed", which is precisely the lie §48 forbids.
func (g *localGo) SemanticDiff(ctx context.Context, fromRef, toRef string) ([]model.SemanticChange, error) {
	if err := g.gitUsable(ctx); err != nil {
		return nil, err
	}
	out, err := g.git(ctx, "diff", "--name-only", fromRef, toRef)
	if err != nil {
		return nil, fmt.Errorf("%w: git could not compare %q and %q under %s",
			model.ErrUnavailable, fromRef, toRef, dirLabel(g.dir))
	}

	changes := []model.SemanticChange{}
	for _, path := range goPaths(out) {
		before, okBefore := g.exportedAt(ctx, fromRef, path)
		after, okAfter := g.exportedAt(ctx, toRef, path)
		if !okBefore && !okAfter {
			// Neither side parsed: report nothing rather than inventing a
			// wholesale rewrite from two failures.
			continue
		}
		changes = append(changes, diffExported(path, before, after)...)
	}
	sortChanges(changes)
	return changes, nil
}

// goPaths extracts the Go source paths from git's newline-separated output,
// applying the same directory exclusions as the local analyser.
func goPaths(out []byte) []string {
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(strings.ReplaceAll(line, "\\", "/"))
		if p == "" || !strings.HasSuffix(p, ".go") {
			continue
		}
		excluded := false
		for _, seg := range strings.Split(filepath.ToSlash(filepath.Dir(p)), "/") {
			if skipDir(seg) {
				excluded = true
				break
			}
		}
		if !excluded {
			paths = append(paths, p)
		}
	}
	return sortedUnique(paths)
}

// exportedAt returns the exported declarations of one path at one revision.
// The bool reports whether the file could be read and parsed at all; a missing
// path is simply an empty side, which is how an added or deleted file appears.
func (g *localGo) exportedAt(ctx context.Context, ref, path string) (map[string]string, bool) {
	src, err := g.git(ctx, "show", ref+":"+path)
	if err != nil {
		return map[string]string{}, false
	}
	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if perr != nil {
		// Same rule as the walk: one unparseable revision of one file is a gap,
		// not a failure of the whole diff.
		return map[string]string{}, false
	}
	return exportedDecls(fset, file), true
}

// exportedDecls maps each exported declaration to a signature string. Methods
// are keyed as Receiver.Method, and a method on an unexported type is excluded
// because it is not part of what another package can depend on.
func exportedDecls(fset *token.FileSet, file *ast.File) map[string]string {
	out := map[string]string{}
	for _, d := range file.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			name := n.Name.Name
			if !ast.IsExported(name) {
				continue
			}
			if n.Recv != nil && len(n.Recv.List) > 0 {
				recv := receiverType(n.Recv.List[0].Type)
				if !ast.IsExported(recv) {
					continue
				}
				name = recv + "." + name
			}
			out[name] = funcSignature(fset, n)
		case *ast.GenDecl:
			for _, spec := range n.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(s.Name.Name) {
						out[s.Name.Name] = typeSignature(fset, s)
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if !ast.IsExported(id.Name) {
							continue
						}
						sig := n.Tok.String() + " " + id.Name
						if s.Type != nil {
							sig += " " + exprText(fset, s.Type)
						}
						out[id.Name] = sig
					}
				}
			}
		}
	}
	return out
}

// diffExported turns two declaration sets into semantic changes.
func diffExported(path string, before, after map[string]string) []model.SemanticChange {
	names := map[string]bool{}
	for n := range before {
		names[n] = true
	}
	for n := range after {
		names[n] = true
	}
	out := []model.SemanticChange{}
	for _, n := range setToSorted(names) {
		b, hadBefore := before[n]
		a, hadAfter := after[n]
		switch {
		case !hadBefore && hadAfter:
			out = append(out, model.SemanticChange{Symbol: n, Path: path, Change: "added", Detail: a})
		case hadBefore && !hadAfter:
			out = append(out, model.SemanticChange{Symbol: n, Path: path, Change: "removed", Detail: b})
		case b != a:
			out = append(out, model.SemanticChange{Symbol: n, Path: path, Change: "modified", Detail: b + " -> " + a})
		}
	}
	return out
}

// gitUsable reports whether git can be used against the analysis root, without
// mutating anything.
func (g *localGo) gitUsable(ctx context.Context) error {
	root := g.root()
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return fmt.Errorf("%w: %s is not a readable directory", model.ErrUnavailable, dirLabel(g.dir))
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("%w: git is not on PATH, so a semantic diff cannot be computed", model.ErrUnavailable)
	}
	if _, err := g.git(ctx, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%w: %s is not inside a usable git work tree", model.ErrUnavailable, dirLabel(g.dir))
	}
	return nil
}

// git runs one read-only git command and returns its stdout. stderr is dropped:
// tool output must not be copied into state or rendered output (plan §34).
func (g *localGo) git(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-C", g.root()}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// collectDecls returns every func, method and type declaration in the parsed
// files, ordered by source location.
func collectDecls(fset *token.FileSet, files []sourceFile) []decl {
	var out []decl
	for _, sf := range files {
		pkg := ""
		if sf.file.Name != nil {
			pkg = sf.file.Name.Name
		}
		dir := path.Dir(sf.path)
		dotImported := hasDotImport(sf.file)
		for _, d := range sf.file.Decls {
			switch n := d.(type) {
			case *ast.FuncDecl:
				kind, recv := "func", ""
				if n.Recv != nil && len(n.Recv.List) > 0 {
					kind = "method"
					recv = receiverType(n.Recv.List[0].Type)
				}
				out = append(out, decl{
					sym: model.GraphSymbol{
						Name:      n.Name.Name,
						Kind:      kind,
						Path:      sf.path,
						LineStart: line(fset, n.Pos()),
						LineEnd:   line(fset, n.End()),
						Signature: funcSignature(fset, n),
					},
					recv:        recv,
					pkg:         pkg,
					dir:         dir,
					dotImported: dotImported,
					body:        n.Body,
				})
			case *ast.GenDecl:
				if n.Tok != token.TYPE {
					continue
				}
				for _, spec := range n.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					out = append(out, decl{
						sym: model.GraphSymbol{
							Name:      ts.Name.Name,
							Kind:      "type",
							Path:      sf.path,
							LineStart: line(fset, ts.Pos()),
							LineEnd:   line(fset, ts.End()),
							Signature: typeSignature(fset, ts),
						},
						pkg:         pkg,
						dir:         dir,
						dotImported: dotImported,
					})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].sym, out[j].sym
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.LineStart != b.LineStart {
			return a.LineStart < b.LineStart
		}
		return a.Name < b.Name
	})
	return out
}

// matches reports whether the declaration answers the query.
//
// A next action names its target the way a human writes it, so a method answers
// both "Process" and "WebhookProcessor.Process", and a package-level symbol
// also answers "billing.WebhookProcessor".
func (d decl) matches(query string) bool {
	q := normalizeQuery(query)
	if q == "" {
		return false
	}
	if q == d.sym.Name {
		return true
	}
	if d.recv != "" && q == d.recv+"."+d.sym.Name {
		return true
	}
	return d.pkg != "" && q == d.pkg+"."+d.sym.Name
}

// sameDeclAs reports whether two declarations are the same source declaration,
// which is how a recursive function is kept out of its own caller list.
func (d decl) sameDeclAs(other decl) bool {
	return d.sym.Path == other.sym.Path && d.sym.LineStart == other.sym.LineStart && d.sym.Name == other.sym.Name
}

// isTestFile reports whether a path is a Go test file. It is the first half of
// the go test rule: a function is only a test if it is declared in a _test.go
// file AND named for a test.
func isTestFile(path string) bool { return strings.HasSuffix(path, "_test.go") }

// normalizeQuery strips the punctuation a human writes around a symbol, so that
// "(*WebhookProcessor).Process", "*WebhookProcessor.Process" and
// "WebhookProcessor.Process" all resolve to the same target.
func normalizeQuery(q string) string {
	return strings.NewReplacer("(", "", ")", "", "*", "", " ", "", "\t", "").Replace(strings.TrimSpace(q))
}

// isTestFuncName applies the go test naming rule: "Test" followed by nothing or
// by a non-lowercase rune. It keeps helpers such as TestingHelper out of the
// candidate test list.
func isTestFuncName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	rest := []rune(name[len("Test"):])
	return len(rest) == 0 || !unicode.IsLower(rest[0])
}

// callShapes reports how a body calls the named identifier: through a plain
// identifier, through a selector, or both. The distinction is what lets the
// caller walk apply Go's scoping rule instead of matching on the name alone.
func callShapes(body *ast.BlockStmt, name string) (plain, selector bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		if plain && selector {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := unwrapCallee(call.Fun).(type) {
		case *ast.Ident:
			if fun.Name == name {
				plain = true
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == name {
				selector = true
			}
		}
		return true
	})
	return plain, selector
}

// reaches reports whether a call in d could actually be a call to target,
// given the shapes the call was written in.
//
// A selector call (p.Process(), billing.Process()) is matched on name alone:
// deciding what p is would need full type resolution, and this analyser is
// honest about not having it. A plain identifier is different — Go resolves it
// in the file's own package scope, so it can only reach a package-level func
// declared in that same package. Attributing alpha's unqualified Run() to
// beta.Run is not an approximation, it is a wrong answer, and names like New,
// Run and Parse repeat across packages in every real tree.
func (d decl) reaches(target decl, plain, selector bool) bool {
	if selector {
		return true
	}
	if !plain {
		return false
	}
	if d.dotImported {
		// The identifier may legitimately resolve into a dot-imported package.
		return true
	}
	// A method is never reachable through a bare identifier: calling one always
	// needs a receiver, which makes the callee a selector.
	return target.recv == "" && d.dir == target.dir && d.pkg == target.pkg
}

// hasDotImport reports whether a file dot-imports any package.
func hasDotImport(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Name != nil && imp.Name.Name == "." {
			return true
		}
	}
	return false
}

// unwrapCallee strips the parenthesis and generic-instantiation wrappers from a
// call target, leaving the identifier or selector underneath.
func unwrapCallee(fun ast.Expr) ast.Expr {
	for {
		switch f := fun.(type) {
		case *ast.ParenExpr:
			fun = f.X
		case *ast.IndexExpr:
			fun = f.X
		case *ast.IndexListExpr:
			fun = f.X
		default:
			return fun
		}
	}
}

// referencesAny reports whether the file mentions any of the given identifiers.
// It is deliberately broad: a test file that names a symbol is a candidate to
// exercise it, and over-listing a candidate test is far cheaper than telling
// the next worker that no test covers the change.
func referencesAny(file *ast.File, names map[string]bool) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && names[id.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}

// receiverType returns the bare type name of a method receiver, unwrapping the
// pointer and generic instantiation forms.
func receiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverType(t.X)
	case *ast.IndexExpr:
		return receiverType(t.X)
	case *ast.IndexListExpr:
		return receiverType(t.X)
	case *ast.ParenExpr:
		return receiverType(t.X)
	}
	return ""
}

// funcSignature renders a one-line signature such as
// "func (p *WebhookProcessor) Process(id string) error".
func funcSignature(fset *token.FileSet, fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		// go/printer cannot print an *ast.Field, so the receiver is assembled
		// from its name and type to match the form go doc uses.
		f := fn.Recv.List[0]
		recv := ""
		if len(f.Names) > 0 && f.Names[0].Name != "" {
			recv = f.Names[0].Name + " "
		}
		b.WriteString("(" + recv + exprText(fset, f.Type) + ") ")
	}
	b.WriteString(fn.Name.Name)
	b.WriteString(strings.TrimPrefix(exprText(fset, fn.Type), "func"))
	return collapseSpace(b.String())
}

// typeSignature renders a type declaration without its body: the shape of a
// struct is not what a dependent breaks against, its name and kind are.
func typeSignature(fset *token.FileSet, ts *ast.TypeSpec) string {
	var kind string
	switch ts.Type.(type) {
	case *ast.StructType:
		kind = "struct"
	case *ast.InterfaceType:
		kind = "interface"
	default:
		kind = exprText(fset, ts.Type)
	}
	sig := "type " + ts.Name.Name
	if ts.Assign.IsValid() {
		sig += " ="
	}
	return collapseSpace(sig + " " + kind)
}

// exprText prints an AST node on one line.
func exprText(fset *token.FileSet, node any) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, node); err != nil {
		return ""
	}
	return collapseSpace(b.String())
}

// collapseSpace folds every run of whitespace into a single space so that a
// declaration split across source lines still renders identically every run.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// line returns the 1-based line of a position, or 0 when it is unknown.
func line(fset *token.FileSet, pos token.Pos) int {
	if !pos.IsValid() {
		return 0
	}
	return fset.Position(pos).Line
}

// relSlash renders a path relative to the analysis root using forward slashes,
// so output does not depend on the host operating system.
func relSlash(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		rel = p
	}
	return filepath.ToSlash(rel)
}

// shapesFor picks the right reference test for the kind of thing target is.
//
// A function is reached by being called, and callShapes looks for exactly that.
// A type is never called — it is constructed, embedded, or named in a field, a
// parameter or a return. Asking callShapes about a type therefore always
// answered "no callers", and the recommendation that followed read as "the
// blast radius is its own definition", which for a widely-used struct is the
// most dangerous possible wrong answer: it invites an edit to something half
// the package depends on.
func shapesFor(target decl, body *ast.BlockStmt, name string) (plain, selector bool) {
	if isTypeDecl(target) {
		return referenceShapes(body, name)
	}
	return callShapes(body, name)
}

// isTypeDecl reports whether a declaration is a type rather than a function.
// Type declarations carry no body, which is the same signal the walk above uses.
func isTypeDecl(d decl) bool {
	switch strings.ToLower(d.sym.Kind) {
	case "type", "struct", "interface":
		return true
	}
	return false
}

// referenceShapes reports how a body mentions a type: as a bare identifier, or
// through a selector. It deliberately matches any mention rather than only
// constructions, because &T{}, var x T, func(T), []T and struct embedding are
// all ways of depending on T, and a blast radius that counted only one of them
// would understate the risk — the error this analyser must not make.
func referenceShapes(body *ast.BlockStmt, name string) (plain, selector bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		if plain && selector {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel != nil && x.Sel.Name == name {
				selector = true
			}
		case *ast.Ident:
			if x.Name == name {
				plain = true
			}
		}
		return true
	})
	return plain, selector
}
