// Package gitrepo implements model.RepoInspector on top of the real git CLI.
//
// Everything this package returns is Level 1 evidence in the plan §31 hierarchy:
// facts read straight out of the repository, which outrank any model inference
// layered on top of them (plan §30, "Deterministic"). Two properties therefore
// matter more here than convenience:
//
//   - Determinism. Results derive only from git's own output, are sorted
//     explicitly, and never depend on map iteration or the wall clock. Two runs
//     over the same tree are byte-identical.
//   - Honesty. When git is missing or the directory is not a repository, every
//     method reports model.ErrUnavailable rather than a zero value a caller
//     could mistake for "clean repo, nothing changed" (plan §33).
package gitrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Repo inspects one git working tree through the git command line.
//
// The zero value is not usable; construct one with New. A Repo is safe for
// concurrent use: it holds no mutable state beyond a cached repository root.
type Repo struct {
	dir string

	mu sync.Mutex
	// root caches the resolved repository top level. Only successful lookups are
	// cached: a directory that is not a repository today may become one later
	// (git init), and caching that failure would make us keep reporting
	// ErrUnavailable for a repository that now exists.
	root string
}

// New returns a Repo that inspects the git working tree containing dir. dir may
// be any directory inside the repository; paths are resolved against the
// repository top level, not against dir, so callers get the same answers from a
// subdirectory as from the root.
func New(dir string) *Repo { return &Repo{dir: dir} }

// Compile-time proof that Repo satisfies the frozen port.
var _ model.RepoInspector = (*Repo)(nil)

// Available reports whether git is installed and dir is inside a repository.
// It is the cheapest way for a caller to decide whether to record a capture gap
// (model.Capture.GitAvailable) before doing any real work.
func (r *Repo) Available(ctx context.Context) bool {
	_, err := r.repoRoot(ctx)
	return err == nil
}

// Head returns the deterministic git-derived facts about the current checkout.
//
// RepoState.ObservedAt is deliberately left zero: this package has no
// model.Clock, and stamping time.Now() here would break the determinism
// requirement. The caller that owns a Clock stamps it.
//
// On any error the zero RepoState is returned rather than a partially filled
// one, so a caller that ignores the error cannot read half-truths as facts.
func (r *Repo) Head(ctx context.Context) (model.RepoState, error) {
	root, err := r.repoRoot(ctx)
	if err != nil {
		return model.RepoState{}, err
	}

	dirty, err := r.statusEntries(ctx)
	if err != nil {
		return model.RepoState{}, err
	}

	tree, err := r.treeHash(ctx, root, dirty)
	if err != nil {
		return model.RepoState{}, err
	}

	return model.RepoState{
		Repo:      r.repoName(ctx, root),
		Branch:    r.branch(ctx),
		CommitSHA: r.headSHA(ctx),
		Dirty:     len(dirty) > 0,
		TreeHash:  tree,
	}, nil
}

// ChangedFiles lists the files touched relative to baseRef.
//
// An empty baseRef means "working tree versus HEAD" and is answered from
// git status, which reports staged, unstaged and untracked paths but carries no
// line counts; Insert and Delete stay zero there because git status does not
// know them, and inventing a count would be fabrication. A non-empty baseRef is
// answered from the merge-base diff (baseRef...HEAD) where --numstat does supply
// real counts.
//
// The result is sorted by path so repeated calls and serialized state are
// byte-identical.
func (r *Repo) ChangedFiles(ctx context.Context, baseRef string) ([]model.ChangedFile, error) {
	if _, err := r.repoRoot(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(baseRef) == "" {
		return r.workingTreeChanges(ctx)
	}
	return r.diffChanges(ctx, strings.TrimSpace(baseRef))
}

func (r *Repo) workingTreeChanges(ctx context.Context) ([]model.ChangedFile, error) {
	entries, err := r.statusEntries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.ChangedFile, 0, len(entries))
	for _, e := range entries {
		out = append(out, model.ChangedFile{
			Path:     e.Path,
			Status:   worktreeStatus(e.Code),
			Evidence: []model.Evidence{fileEvidence(e, "working tree")},
		})
	}
	sortChangedFiles(out)
	return out, nil
}

func (r *Repo) diffChanges(ctx context.Context, baseRef string) ([]model.ChangedFile, error) {
	spec := baseRef + "...HEAD"

	nameOut, err := r.git(ctx, "diff", "--name-status", "-z", spec)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: git diff --name-status %s: %w", spec, err)
	}
	entries := parseNameStatusZ(nameOut)

	// --numstat is a second call rather than a combined one because git cannot
	// emit both formats in a single machine-readable stream. A failure here is
	// not fatal: the file list is the fact, the line counts are detail.
	counts := map[string]numstat{}
	if numOut, err := r.git(ctx, "diff", "--numstat", "-z", spec); err == nil {
		counts = parseNumstatZ(numOut)
	}

	out := make([]model.ChangedFile, 0, len(entries))
	for _, e := range entries {
		cf := model.ChangedFile{
			Path:     e.Path,
			Status:   diffStatus(e.Code),
			Evidence: []model.Evidence{fileEvidence(e, spec)},
		}
		if n, ok := counts[e.Path]; ok {
			cf.Insert = n.Insert
			cf.Delete = n.Delete
		}
		out = append(out, cf)
	}
	sortChangedFiles(out)
	return out, nil
}

// FileHash returns "sha256:<hex>" over the working-tree bytes of path.
//
// It hashes what is on disk rather than the git blob, because the bytes on disk
// are what the next agent will actually read; index content can differ from the
// working tree (staged edits, autocrlf). Relative paths are resolved against the
// repository top level, matching the paths git itself reports.
//
// Returns model.ErrNotFound when the path is absent (or is a directory, which
// holds no file content), and model.ErrUnavailable when the repository is not
// usable at all — the two are distinct so callers can degrade differently.
func (r *Repo) FileHash(ctx context.Context, path string) (string, error) {
	abs, err := r.resolve(ctx, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("gitrepo: %s: %w", path, model.ErrNotFound)
		}
		return "", fmt.Errorf("gitrepo: stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("gitrepo: %s is a directory: %w", path, model.ErrNotFound)
	}
	sum, err := hashFile(abs)
	if err != nil {
		return "", fmt.Errorf("gitrepo: hash %s: %w", path, err)
	}
	return sum, nil
}

// Exists reports whether path is present in the working tree.
//
// The port gives Exists no error channel, so an unusable repository is reported
// as false. That is the only answer a bool can carry, and it is why callers that
// must distinguish "absent" from "cannot tell" have to consult Available first.
func (r *Repo) Exists(ctx context.Context, path string) bool {
	abs, err := r.resolve(ctx, path)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}

// treeHash fingerprints everything the task can currently see: the staged index
// listing plus the content of every dirty path.
//
// The dirty half is what makes drift detection work. "git ls-files -s" only
// changes when the index changes, so a plain uncommitted edit would leave the
// fingerprint identical and resume would happily restore state against a tree
// that had moved underneath it (plan Step 19, "Do not blindly restore stale
// state"). Lines are sorted before hashing so the digest does not depend on
// git's listing order.
func (r *Repo) treeHash(ctx context.Context, root string, dirty []statusEntry) (string, error) {
	out, err := r.git(ctx, "ls-files", "-s", "-z")
	if err != nil {
		return "", fmt.Errorf("gitrepo: git ls-files: %w", err)
	}

	records := splitZ(out)
	lines := make([]string, 0, len(records)+len(dirty))
	for _, rec := range records {
		lines = append(lines, hashLine("index", rec))
	}
	for _, e := range dirty {
		abs := filepath.Join(root, filepath.FromSlash(e.Path))
		lines = append(lines, hashLine("dirty", e.Code, e.Path, e.Orig, contentDigest(abs)))
	}
	sort.Strings(lines)

	h := sha256.New()
	for _, l := range lines {
		_, _ = io.WriteString(h, l)
		_, _ = h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// statusEntries returns every path git considers dirty: staged, unstaged and
// untracked. --untracked-files=all expands untracked directories to individual
// files so a brand-new file always shows up as itself in both ChangedFiles and
// the tree hash.
func (r *Repo) statusEntries(ctx context.Context) ([]statusEntry, error) {
	out, err := r.git(ctx, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("gitrepo: git status: %w", err)
	}
	return parseStatusZ(out), nil
}

// branch returns the checked-out branch, or "" when HEAD is detached. An empty
// branch is the honest answer there: a detached HEAD has no branch, and echoing
// git's literal "HEAD" would read downstream as a branch named HEAD.
func (r *Repo) branch(ctx context.Context) string {
	out, err := r.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// headSHA returns the commit HEAD points at, or "" in a repository with no
// commits yet. --verify --quiet makes the unborn case a clean empty answer
// rather than an error we would have to guess about.
func (r *Repo) headSHA(ctx context.Context) string {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// repoName derives "owner/repo" from the origin remote, falling back to the
// directory name of the repository root when there is no origin (a local-only
// repository is still a real repository and still needs a stable identity —
// model.NewTaskID hashes this value).
func (r *Repo) repoName(ctx context.Context, root string) string {
	if out, err := r.git(ctx, "config", "--get", "remote.origin.url"); err == nil {
		if name := normalizeRemote(string(out)); name != "" {
			return name
		}
	}
	return baseName(root)
}

// resolve maps a caller-supplied path onto an absolute working-tree path,
// returning ErrUnavailable when there is no working tree to resolve against.
func (r *Repo) resolve(ctx context.Context, path string) (string, error) {
	root, err := r.repoRoot(ctx)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(root, filepath.FromSlash(path)), nil
}

// repoRoot resolves and caches the repository top level. It is the single place
// that decides "this is not usable", so every method degrades the same way.
func (r *Repo) repoRoot(ctx context.Context) (string, error) {
	r.mu.Lock()
	cached := r.root
	r.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	out, err := r.gitIn(ctx, r.dir, "rev-parse", "--show-toplevel")
	if err != nil {
		// A cancelled or expired context is not a statement about the
		// repository, so it must not be reported as ErrUnavailable.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("gitrepo: %s: %w", r.dir, ctxErr)
		}
		if errors.Is(err, model.ErrUnavailable) {
			return "", err
		}
		return "", fmt.Errorf("gitrepo: %s is not a git repository: %w", r.dir, model.ErrUnavailable)
	}

	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("gitrepo: %s has no repository root: %w", r.dir, model.ErrUnavailable)
	}
	root = filepath.FromSlash(root)

	r.mu.Lock()
	r.root = root
	r.mu.Unlock()
	return root, nil
}

// git runs one git command from the repository top level and returns its stdout.
//
// Running from the top level rather than from r.dir is load-bearing, not tidiness.
// Several git commands are scoped to the current directory: "git ls-files" lists
// only the cwd subtree and prints cwd-relative paths, and "git diff" does the same
// whenever the user has set diff.relative. A Repo constructed in a subdirectory
// would then fingerprint only that subdirectory, so an edit elsewhere in the tree
// would leave TreeHash unchanged and drift detection would report a moved tree as
// unchanged. Anchoring every command at the top level makes the whole repository
// the subject of every answer, exactly as New documents.
func (r *Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	root, err := r.repoRoot(ctx)
	if err != nil {
		return nil, err
	}
	return r.gitIn(ctx, root, args...)
}

// gitIn runs one git command in an explicit directory. Only repoRoot uses a
// directory other than the top level, because it is the call that discovers it.
//
// exec.CommandContext is used throughout so a hung git (a remote that wants
// credentials, a lock held by another process) dies with the caller's context
// instead of stalling the host agent (plan §33: never a hard dependency).
func (r *Repo) gitIn(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin, err := gitExecutable()
	if err != nil {
		return nil, fmt.Errorf("gitrepo: git executable not found: %w", model.ErrUnavailable)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

var (
	gitOnce sync.Once
	gitBin  string
	gitErr  error
)

// gitExecutable resolves the git binary once per process. The lookup is stable
// for the process lifetime, and repeating it on every call would make the
// per-file methods (Exists, FileHash) needlessly expensive.
func gitExecutable() (string, error) {
	gitOnce.Do(func() { gitBin, gitErr = exec.LookPath("git") })
	return gitBin, gitErr
}

// gitRepoPointerEnv are the environment variables that redirect git at a
// specific repository, index or object store. The continuity layer runs inside
// another agent's session, and that session may itself have been launched from a
// git hook or a "git -c ... run" wrapper, which exports these. Inheriting them
// would make every command answer about a different repository than the one the
// caller named — a fabricated fact rather than a missing one, which is the one
// failure mode this package must not have (plan §33). They are dropped so that
// discovery starts from the directory the caller actually passed to New.
var gitRepoPointerEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
}

// gitEnv keeps the inherited environment (PATH, HOME and any repository access
// configuration the user relies on) but drops the variables that would point git
// at another repository, and pins the parts that would otherwise make git
// non-deterministic or blocking.
func gitEnv() []string {
	src := os.Environ()
	env := make([]string, 0, len(src)+3)
	for _, kv := range src {
		if isRepoPointer(kv) {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		// Never stop to ask a human for credentials: this runs inside another
		// agent's session where nobody can answer the prompt.
		"GIT_TERMINAL_PROMPT=0",
		// Read-only inspection must not take the index lock, so we cannot
		// deadlock against the agent's own git commands.
		"GIT_OPTIONAL_LOCKS=0",
		// Stable, parseable messages regardless of the user's locale.
		"LC_ALL=C",
	)
}

// isRepoPointer reports whether an environment entry names one of the
// repository-pointing variables. Names are compared case-insensitively because
// the Windows environment is case-insensitive; entries whose name is empty (the
// "=C:=C:\dir" per-drive entries Windows keeps) are left alone.
func isRepoPointer(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i <= 0 {
		return false
	}
	name := kv[:i]
	for _, p := range gitRepoPointerEnv {
		if strings.EqualFold(name, p) {
			return true
		}
	}
	return false
}

// hashFile streams a file into sha256 so a large artifact never has to be held
// in memory in full.
func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// contentDigest is the tree-hash view of one dirty path. A dirty path that is
// gone (deleted or a directory git listed) still has to contribute something
// stable, and it must be a marker rather than an empty string so that "deleted"
// and "empty file" never collide in the fingerprint.
func contentDigest(abs string) string {
	sum, err := hashFile(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent"
		}
		return "unreadable"
	}
	return sum
}

// fileEvidence cites the file itself, which is Level 1 repository evidence
// (plan §31). detail records where the observation came from so a later reader
// can reproduce it.
func fileEvidence(e statusEntry, detail string) model.Evidence {
	if e.Orig != "" {
		detail = detail + "; renamed from " + e.Orig
	}
	return model.Evidence{
		Kind:   model.EvidenceFile,
		Ref:    e.Path,
		Path:   e.Path,
		Detail: detail,
	}
}

// sortChangedFiles imposes a total order so output never depends on git's
// listing order. Path is unique per result set; Status breaks any tie defensively.
func sortChangedFiles(in []model.ChangedFile) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Path != in[j].Path {
			return in[i].Path < in[j].Path
		}
		return in[i].Status < in[j].Status
	})
}

// hashLine joins fields with NUL so that no combination of path and status can
// be confused with another one that happens to contain a space.
func hashLine(fields ...string) string { return strings.Join(fields, "\x00") }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max]
	}
	return s
}
