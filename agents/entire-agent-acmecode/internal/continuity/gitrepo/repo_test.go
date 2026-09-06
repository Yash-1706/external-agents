package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// The integration tests below drive the real git CLI, because the whole point of
// this package is that its facts come from git rather than from a model of git.
// They build throwaway repositories in t.TempDir() and skip when git is absent.

// fixture is a throwaway repository plus the helpers to mutate it.
type fixture struct {
	t   *testing.T
	dir string
	bin string
}

// newFixture creates an initialised repository with a deterministic branch name.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	bin := requireGit(t)
	isolateGit(t)

	f := &fixture{t: t, dir: t.TempDir(), bin: bin}
	f.git("init")
	return f
}

// requireGit resolves the git binary or skips: an environment without git is a
// legitimate one, and this package's contract there is ErrUnavailable, which
// TestUnavailableOutsideRepository covers without needing git at all.
func requireGit(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed; skipping gitrepo CLI integration test")
	}
	return bin
}

// isolateGit blanks the system and global configuration for both the fixture's
// git calls and the Repo's own, so results do not depend on whatever identity,
// default branch or autocrlf setting the developer's machine happens to carry.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-gitconfig"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// git runs one git command in the fixture. Identity and formatting are forced
// with -c flags so the tests pass on a machine with no global git identity.
func (f *fixture) git(args ...string) string {
	f.t.Helper()
	base := []string{
		"-c", "user.name=Continuity Test",
		"-c", "user.email=continuity@example.invalid",
		"-c", "commit.gpgsign=false",
		"-c", "core.autocrlf=false",
		"-c", "init.defaultBranch=main",
	}
	cmd := exec.Command(f.bin, append(base, args...)...)
	cmd.Dir = f.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	abs := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", rel, err)
	}
}

func (f *fixture) remove(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.dir, filepath.FromSlash(rel))); err != nil {
		f.t.Fatalf("remove %s: %v", rel, err)
	}
}

// commit stages everything and commits, returning the new commit SHA.
func (f *fixture) commit(msg string) string {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

func (f *fixture) repo() *Repo { return New(f.dir) }

func TestAvailable(t *testing.T) {
	f := newFixture(t)
	if !f.repo().Available(context.Background()) {
		t.Fatal("Available = false inside an initialised repository")
	}
}

// A directory that is not a repository must degrade to ErrUnavailable on every
// method, and must never answer with a zero value that reads as a fact: an
// empty changed-file list would say "nothing changed", which we do not know.
// This test needs no git: without git installed the contract is identical.
func TestUnavailableOutsideRepository(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := New(dir)
	ctx := context.Background()

	if r.Available(ctx) {
		t.Skip("temporary directory is unexpectedly inside a git repository")
	}

	t.Run("Head", func(t *testing.T) {
		state, err := r.Head(ctx)
		if !errors.Is(err, model.ErrUnavailable) {
			t.Fatalf("Head error = %v, want ErrUnavailable", err)
		}
		if state != (model.RepoState{}) {
			t.Fatalf("Head returned %+v alongside an error; must not look like a fact", state)
		}
	})

	t.Run("ChangedFiles working tree", func(t *testing.T) {
		files, err := r.ChangedFiles(ctx, "")
		if !errors.Is(err, model.ErrUnavailable) {
			t.Fatalf("ChangedFiles error = %v, want ErrUnavailable", err)
		}
		if files != nil {
			t.Fatalf("ChangedFiles returned %v; an empty list would claim nothing changed", files)
		}
	})

	t.Run("ChangedFiles base ref", func(t *testing.T) {
		if _, err := r.ChangedFiles(ctx, "HEAD~1"); !errors.Is(err, model.ErrUnavailable) {
			t.Fatalf("ChangedFiles error = %v, want ErrUnavailable", err)
		}
	})

	t.Run("FileHash of a file that exists on disk", func(t *testing.T) {
		// The bytes are readable, but outside a repository this package has no
		// working tree to speak for, so it reports unavailable rather than a
		// hash that would imply repository provenance.
		got, err := r.FileHash(ctx, "present.txt")
		if !errors.Is(err, model.ErrUnavailable) {
			t.Fatalf("FileHash error = %v, want ErrUnavailable", err)
		}
		if got != "" {
			t.Fatalf("FileHash = %q alongside an error", got)
		}
	})

	t.Run("Exists", func(t *testing.T) {
		if r.Exists(ctx, "present.txt") {
			t.Fatal("Exists = true outside a repository; the port has no error channel, so it must answer false")
		}
	})
}

func TestHeadCleanRepository(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "one\n")
	sha := f.commit("initial")

	state, err := f.repo().Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if state.Branch != "main" {
		t.Errorf("Branch = %q, want main", state.Branch)
	}
	if state.CommitSHA != sha {
		t.Errorf("CommitSHA = %q, want %q", state.CommitSHA, sha)
	}
	if state.Dirty {
		t.Error("Dirty = true on a freshly committed tree")
	}
	if !strings.HasPrefix(state.TreeHash, "sha256:") {
		t.Errorf("TreeHash = %q, want a sha256: prefix", state.TreeHash)
	}
	// No origin remote: identity falls back to the directory name rather than
	// being left empty, because model.NewTaskID hashes this value.
	if want := filepath.Base(f.dir); state.Repo != want {
		t.Errorf("Repo = %q, want the directory name %q", state.Repo, want)
	}
	// ObservedAt is owned by the caller's Clock; stamping it here would make the
	// output non-deterministic.
	if !state.ObservedAt.IsZero() {
		t.Errorf("ObservedAt = %v, want the zero time", state.ObservedAt)
	}
}

// TreeHash must move when the working tree moves, including for changes git
// never recorded in the index. Drift detection is exactly this comparison
// (plan Step 19), so a hash that ignored uncommitted edits would report a tree
// as unchanged while an agent was actively editing it.
func TestHeadTreeHashTracksDirtyContent(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "one\n")
	f.commit("initial")
	ctx := context.Background()

	clean, err := f.repo().Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	steps := []struct {
		name       string
		mutate     func()
		wantDirty  bool
		wantChange bool // hash must differ from the clean-tree hash
	}{
		{
			name:       "uncommitted edit to a tracked file",
			mutate:     func() { f.write("tracked.txt", "one\ntwo\n") },
			wantDirty:  true,
			wantChange: true,
		},
		{
			name:       "reverting the edit restores the original fingerprint",
			mutate:     func() { f.write("tracked.txt", "one\n") },
			wantDirty:  false,
			wantChange: false,
		},
		{
			name:       "a new untracked file changes the fingerprint",
			mutate:     func() { f.write("scratch.txt", "notes\n") },
			wantDirty:  true,
			wantChange: true,
		},
		{
			name:       "removing the untracked file restores it again",
			mutate:     func() { f.remove("scratch.txt") },
			wantDirty:  false,
			wantChange: false,
		},
		{
			name:       "deleting a tracked file changes the fingerprint",
			mutate:     func() { f.remove("tracked.txt") },
			wantDirty:  true,
			wantChange: true,
		},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate()
			state, err := f.repo().Head(ctx)
			if err != nil {
				t.Fatalf("Head: %v", err)
			}
			if state.Dirty != tc.wantDirty {
				t.Errorf("Dirty = %v, want %v", state.Dirty, tc.wantDirty)
			}
			if changed := state.TreeHash != clean.TreeHash; changed != tc.wantChange {
				t.Errorf("TreeHash changed = %v, want %v (clean %s, got %s)",
					changed, tc.wantChange, clean.TreeHash, state.TreeHash)
			}
		})
	}
}

// Two reads of the same tree, through two different Repo values, must produce
// byte-identical output. Determinism is a product requirement, not a nicety.
func TestHeadIsDeterministic(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a\n")
	f.write("nested/b.txt", "b\n")
	f.commit("initial")
	f.write("a.txt", "a changed\n")
	f.write("untracked.txt", "u\n")
	ctx := context.Background()

	first, err := f.repo().Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	second, err := New(f.dir).Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if first != second {
		t.Fatalf("Head is not deterministic:\nfirst  %+v\nsecond %+v", first, second)
	}
}

func TestHeadRepoNameFromOrigin(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"scp style github", "git@github.com:acme/widget.git", "acme/widget"},
		{"https github", "https://github.com/acme/widget.git", "acme/widget"},
		{"credential bearing url is normalised without the secret", "https://tok:s3cr3t@github.com/acme/widget.git", "acme/widget"},
		{"nested gitlab groups", "ssh://git@gitlab.com/group/sub/widget.git", "sub/widget"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write("a.txt", "a\n")
			f.commit("initial")
			f.git("config", "remote.origin.url", tc.url)

			state, err := f.repo().Head(context.Background())
			if err != nil {
				t.Fatalf("Head: %v", err)
			}
			if state.Repo != tc.want {
				t.Fatalf("Repo = %q, want %q", state.Repo, tc.want)
			}
		})
	}
}

// A repository with no commits is a real repository. Head must report what it
// knows (branch, dirtiness, fingerprint) and leave CommitSHA empty rather than
// failing or inventing a SHA.
func TestHeadUnbornRepository(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	state, err := f.repo().Head(ctx)
	if err != nil {
		t.Fatalf("Head on an unborn repository: %v", err)
	}
	if state.CommitSHA != "" {
		t.Errorf("CommitSHA = %q, want empty before the first commit", state.CommitSHA)
	}
	if state.Branch != "main" {
		t.Errorf("Branch = %q, want main", state.Branch)
	}
	if state.Dirty {
		t.Error("Dirty = true in an empty repository")
	}
	if state.TreeHash == "" {
		t.Error("TreeHash is empty; an empty tree still has a fingerprint")
	}

	f.write("new.txt", "content\n")
	dirty, err := f.repo().Head(ctx)
	if err != nil {
		t.Fatalf("Head after adding a file: %v", err)
	}
	if !dirty.Dirty {
		t.Error("Dirty = false with an untracked file present")
	}
	if dirty.TreeHash == state.TreeHash {
		t.Error("TreeHash did not move when an untracked file appeared")
	}
}

// A detached HEAD has no branch. Reporting git's literal "HEAD" would read
// downstream as a branch called HEAD, so the honest answer is empty.
func TestHeadDetached(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a\n")
	sha := f.commit("initial")
	f.git("checkout", "--detach")

	state, err := f.repo().Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if state.Branch != "" {
		t.Errorf("Branch = %q, want empty on a detached HEAD", state.Branch)
	}
	if state.CommitSHA != sha {
		t.Errorf("CommitSHA = %q, want %q", state.CommitSHA, sha)
	}
}

func TestChangedFilesWorkingTree(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "one\n")
	f.write("doomed.txt", "bye\n")
	f.commit("initial")

	f.write("tracked.txt", "one\ntwo\n") // modified
	f.remove("doomed.txt")               // deleted
	f.write("untracked.txt", "new\n")    // untracked
	f.write("sub/deep.txt", "deep\n")    // untracked inside a new directory

	got, err := f.repo().ChangedFiles(context.Background(), "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}

	want := []model.ChangedFile{
		{Path: "doomed.txt", Status: "D"},
		{Path: "sub/deep.txt", Status: "A"},
		{Path: "tracked.txt", Status: "M"},
		{Path: "untracked.txt", Status: "A"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changed files, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		// Sorted by path, so position is part of the contract.
		if got[i].Path != want[i].Path || got[i].Status != want[i].Status {
			t.Errorf("entry %d = {%s %s}, want {%s %s}", i, got[i].Status, got[i].Path, want[i].Status, want[i].Path)
		}
		// git status carries no line counts; claiming zero changes as a fact
		// would be fabrication, so the fields stay unset.
		if got[i].Insert != 0 || got[i].Delete != 0 {
			t.Errorf("entry %d has line counts %d/%d; git status cannot know them", i, got[i].Insert, got[i].Delete)
		}
		if len(got[i].Evidence) != 1 || got[i].Evidence[0].Kind != model.EvidenceFile {
			t.Errorf("entry %d carries %v, want one file evidence record", i, got[i].Evidence)
		}
	}
}

func TestChangedFilesAgainstBaseRef(t *testing.T) {
	f := newFixture(t)
	f.write("keep.txt", "one\ntwo\nthree\n")
	f.write("gone.txt", "bye\n")
	base := f.commit("base")

	f.write("keep.txt", "one\ntwo\nthree\nfour\nfive\n")
	f.write("added.txt", "fresh\n")
	f.remove("gone.txt")
	f.commit("second")

	got, err := f.repo().ChangedFiles(context.Background(), base)
	if err != nil {
		t.Fatalf("ChangedFiles(%s): %v", base, err)
	}

	want := []model.ChangedFile{
		{Path: "added.txt", Status: "A", Insert: 1, Delete: 0},
		{Path: "gone.txt", Status: "D", Insert: 0, Delete: 1},
		{Path: "keep.txt", Status: "M", Insert: 2, Delete: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changed files, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Path != want[i].Path || got[i].Status != want[i].Status {
			t.Errorf("entry %d = {%s %s}, want {%s %s}", i, got[i].Status, got[i].Path, want[i].Status, want[i].Path)
		}
		if got[i].Insert != want[i].Insert || got[i].Delete != want[i].Delete {
			t.Errorf("entry %d counts = +%d/-%d, want +%d/-%d",
				i, got[i].Insert, got[i].Delete, want[i].Insert, want[i].Delete)
		}
		if len(got[i].Evidence) != 1 || !strings.Contains(got[i].Evidence[0].Detail, base) {
			t.Errorf("entry %d evidence = %+v, want a citation naming the base ref", i, got[i].Evidence)
		}
	}
}

// An unresolvable base ref is a failure of the request, not of the repository:
// it must not be dressed up as ErrUnavailable and must not return a list.
func TestChangedFilesUnknownBaseRef(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a\n")
	f.commit("initial")

	got, err := f.repo().ChangedFiles(context.Background(), "no-such-ref")
	if err == nil {
		t.Fatalf("ChangedFiles against a missing ref returned %+v and no error", got)
	}
	if errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("error = %v; the repository is available, only the ref is not", err)
	}
	if got != nil {
		t.Fatalf("ChangedFiles returned %+v alongside an error", got)
	}
}

func TestFileHash(t *testing.T) {
	f := newFixture(t)
	const content = "hello continuity\n"
	f.write("tracked.txt", content)
	f.write("sub/nested.txt", "nested\n")
	f.commit("initial")
	ctx := context.Background()

	sum := sha256.Sum256([]byte(content))
	want := "sha256:" + hex.EncodeToString(sum[:])

	t.Run("hashes working tree bytes", func(t *testing.T) {
		got, err := f.repo().FileHash(ctx, "tracked.txt")
		if err != nil {
			t.Fatalf("FileHash: %v", err)
		}
		if got != want {
			t.Fatalf("FileHash = %q, want %q", got, want)
		}
	})

	t.Run("uncommitted edits change the hash", func(t *testing.T) {
		f.write("tracked.txt", "edited\n")
		got, err := f.repo().FileHash(ctx, "tracked.txt")
		if err != nil {
			t.Fatalf("FileHash: %v", err)
		}
		if got == want {
			t.Fatal("FileHash ignored the working-tree edit")
		}
		f.write("tracked.txt", content)
	})

	t.Run("relative paths resolve against the repository root", func(t *testing.T) {
		// A Repo constructed inside a subdirectory must answer the same
		// repository-relative paths git itself reports.
		got, err := New(filepath.Join(f.dir, "sub")).FileHash(ctx, "tracked.txt")
		if err != nil {
			t.Fatalf("FileHash from a subdirectory: %v", err)
		}
		if got != want {
			t.Fatalf("FileHash = %q, want %q", got, want)
		}
	})

	t.Run("missing path is ErrNotFound, not ErrUnavailable", func(t *testing.T) {
		got, err := f.repo().FileHash(ctx, "no/such/file.txt")
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if errors.Is(err, model.ErrUnavailable) {
			t.Fatalf("error = %v; the repository worked, the file is simply absent", err)
		}
		if got != "" {
			t.Fatalf("FileHash = %q alongside an error", got)
		}
	})

	t.Run("directory has no file content", func(t *testing.T) {
		if _, err := f.repo().FileHash(ctx, "sub"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})
}

func TestExists(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "one\n")
	f.write("sub/nested.txt", "nested\n")
	f.commit("initial")
	f.write("untracked.txt", "u\n")
	ctx := context.Background()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"committed file", "tracked.txt", true},
		{"untracked file is still in the working tree", "untracked.txt", true},
		{"nested path", "sub/nested.txt", true},
		{"directory", "sub", true},
		{"missing path", "nope.txt", false},
		{"missing nested path", "sub/nope.txt", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.repo().Exists(ctx, tc.path); got != tc.want {
				t.Fatalf("Exists(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// A cancelled context is a statement about the caller, not about the
// repository, so it must surface as the context error rather than as
// ErrUnavailable — otherwise a caller would record a permanent capture gap for
// a transient timeout.
func TestCancelledContext(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a\n")
	f.commit("initial")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := New(f.dir) // fresh Repo: nothing cached from an earlier call
	state, err := r.Head(ctx)
	if err == nil {
		t.Fatalf("Head with a cancelled context returned %+v and no error", state)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("error = %v; cancellation must not be reported as an unavailable repository", err)
	}
	if r.Available(ctx) {
		t.Error("Available = true with a cancelled context")
	}
}

// Repo is documented as safe for concurrent use; drift detection calls Exists
// once per relevant file and may well do so in parallel. Run under -race.
func TestConcurrentUse(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "one\n")
	f.commit("initial")
	ctx := context.Background()

	r := f.repo()
	const workers = 8
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			if !r.Available(ctx) {
				errs <- errors.New("Available = false")
				return
			}
			if !r.Exists(ctx, "tracked.txt") {
				errs <- errors.New("Exists = false")
				return
			}
			_, err := r.Head(ctx)
			errs <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent use: %v", err)
		}
	}
}

// A Repo constructed inside a subdirectory speaks for the whole repository, as
// New documents. This is not cosmetic: "git ls-files" lists only the current
// directory's subtree, so a fingerprint taken from a subdirectory would be blind
// to every file outside it and drift detection would call a moved tree
// unchanged — the one property the tree hash exists to provide.
func TestHeadFromSubdirectoryCoversTheWholeRepository(t *testing.T) {
	f := newFixture(t)
	f.write("top.txt", "one\n")
	f.write("sub/nested.txt", "nested\n")
	f.commit("initial")
	ctx := context.Background()
	sub := filepath.Join(f.dir, "sub")

	fromRoot, err := f.repo().Head(ctx)
	if err != nil {
		t.Fatalf("Head from the root: %v", err)
	}
	fromSub, err := New(sub).Head(ctx)
	if err != nil {
		t.Fatalf("Head from a subdirectory: %v", err)
	}
	if fromRoot != fromSub {
		t.Fatalf("Head depends on the directory the Repo was constructed in:\nroot %+v\nsub  %+v", fromRoot, fromSub)
	}

	t.Run("committed change outside the subdirectory moves the fingerprint", func(t *testing.T) {
		f.write("top.txt", "one\ntwo\n")
		f.commit("second")

		after, err := New(sub).Head(ctx)
		if err != nil {
			t.Fatalf("Head from a subdirectory: %v", err)
		}
		if after.TreeHash == fromSub.TreeHash {
			t.Fatal("TreeHash from a subdirectory ignored a committed change elsewhere in the tree; drift would go unreported")
		}
	})

	t.Run("uncommitted change outside the subdirectory moves the fingerprint", func(t *testing.T) {
		before, err := New(sub).Head(ctx)
		if err != nil {
			t.Fatalf("Head from a subdirectory: %v", err)
		}
		f.write("top.txt", "one\ntwo\nthree\n")

		after, err := New(sub).Head(ctx)
		if err != nil {
			t.Fatalf("Head from a subdirectory: %v", err)
		}
		if after.TreeHash == before.TreeHash {
			t.Fatal("TreeHash from a subdirectory ignored an uncommitted edit elsewhere in the tree")
		}
		if !after.Dirty {
			t.Error("Dirty = false with an uncommitted edit outside the subdirectory")
		}
	})
}

// diff.relative is a real user setting that makes "git diff" restrict itself to
// the current directory and print paths relative to it. Answers must not change
// because of it: a caller asking a subdirectory Repo for changed files is asking
// about the repository, and quietly dropping the files outside would understate
// the change set without saying so.
func TestChangedFilesFromSubdirectoryIgnoresRelativeConfig(t *testing.T) {
	f := newFixture(t)
	f.write("top.txt", "one\n")
	f.write("sub/nested.txt", "nested\n")
	base := f.commit("initial")
	f.git("config", "diff.relative", "true")
	f.git("config", "status.relativePaths", "true")

	f.write("top.txt", "one\ntwo\n")
	f.write("sub/nested.txt", "changed\n")
	ctx := context.Background()
	sub := New(filepath.Join(f.dir, "sub"))

	t.Run("working tree", func(t *testing.T) {
		got, err := sub.ChangedFiles(ctx, "")
		if err != nil {
			t.Fatalf("ChangedFiles: %v", err)
		}
		assertPaths(t, got, "sub/nested.txt", "top.txt")
	})

	f.commit("second")

	t.Run("against a base ref", func(t *testing.T) {
		got, err := sub.ChangedFiles(ctx, base)
		if err != nil {
			t.Fatalf("ChangedFiles(%s): %v", base, err)
		}
		assertPaths(t, got, "sub/nested.txt", "top.txt")
	})
}

// assertPaths checks the exact, ordered set of reported paths.
func assertPaths(t *testing.T, got []model.ChangedFile, want ...string) {
	t.Helper()
	paths := make([]string, 0, len(got))
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
}

// GIT_DIR and its relatives point git at one specific repository. This process
// runs inside another agent's session, which may itself have been started from a
// git hook that exports them. Honouring them would make every method describe a
// repository the caller never named, which is a fabricated fact rather than a
// missing one.
func TestEnvironmentRepositoryPointersAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("mine.txt", "mine\n")
	sha := f.commit("initial")
	f.git("config", "remote.origin.url", "git@github.com:acme/mine.git")

	// A second, unrelated repository, as a hook's environment would name.
	other := &fixture{t: t, dir: t.TempDir(), bin: f.bin}
	other.git("init")
	other.write("theirs.txt", "theirs\n")
	other.commit("initial")
	other.git("config", "remote.origin.url", "git@github.com:someone/elsewhere.git")

	t.Setenv("GIT_DIR", filepath.Join(other.dir, ".git"))
	t.Setenv("GIT_WORK_TREE", other.dir)

	state, err := New(f.dir).Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if state.Repo != "acme/mine" {
		t.Errorf("Repo = %q, want acme/mine: the environment redirected the answer at another repository", state.Repo)
	}
	if state.CommitSHA != sha {
		t.Errorf("CommitSHA = %q, want %q", state.CommitSHA, sha)
	}

	files, err := New(f.dir).ChangedFiles(context.Background(), "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("ChangedFiles = %+v, want none: this repository is clean", files)
	}
	if !New(f.dir).Exists(context.Background(), "mine.txt") {
		t.Error("Exists(mine.txt) = false; the environment redirected the working tree")
	}
}

// A rename detected in the working tree rather than in the index carries its
// original path in an extra record. Reading that record as a status entry of its
// own manufactures a changed file, with a path that is a fragment of a real one,
// out of nothing — the exact fabrication this package must never commit.
func TestChangedFilesWorktreeRenameInventsNothing(t *testing.T) {
	f := newFixture(t)
	const body = "line one\nline two\nline three\nline four\n"
	f.write("original-name.txt", body)
	f.commit("initial")

	f.remove("original-name.txt")
	f.write("renamed-name.txt", body)
	// An intent-to-add is what makes git pair the two as a working tree rename
	// rather than reporting a deletion plus an untracked file.
	f.git("add", "-N", "renamed-name.txt")

	got, err := f.repo().ChangedFiles(context.Background(), "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}

	real := map[string]bool{"original-name.txt": true, "renamed-name.txt": true}
	for _, c := range got {
		if !real[c.Path] {
			t.Errorf("ChangedFiles reported %q, which is not a path git named: %+v", c.Path, got)
		}
	}
	if len(got) == 0 {
		t.Fatalf("ChangedFiles reported nothing after a rename")
	}

	// The same desync would corrupt the fingerprint, so check it moved for the
	// right reason: back out the rename and the original value must return.
	renamed, err := f.repo().Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	f.git("reset")
	f.remove("renamed-name.txt")
	f.write("original-name.txt", body)
	restored, err := f.repo().Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if restored.TreeHash == renamed.TreeHash {
		t.Error("TreeHash did not move for a working tree rename")
	}
	if restored.Dirty {
		t.Errorf("Dirty = true after undoing the rename: %+v", restored)
	}
}
