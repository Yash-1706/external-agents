// Package store implements model.Store on top of an ordinary directory tree.
//
// The store is an index over Entire history, never a replacement for it
// (plan §5): everything durable enough to matter is also written to a
// checkpoint, so losing this directory must never lose an engineering
// conclusion.
//
// Layout under Root():
//
//	tasks/<task_id>/task.json
//	tasks/<task_id>/events.ndjson              append-only, one AgentEvent per line
//	tasks/<task_id>/sessions/<session_id>.json
//	tasks/<task_id>/checkpoints/<cp_id>.json
//	tasks/<task_id>/state.json
//	handoffs/<handoff_id>.json
//	index/session/<session_id>                 owning task id
//	index/repobranch/<sha256(repo\x00branch)>  task id
//
// Three properties are load-bearing:
//
//   - Determinism. No wall clock is read directly and no map iteration order
//     reaches a file, so two runs over the same inputs produce byte-identical
//     trees. Lineage folding is order-independent as well, because adapters
//     replay events and do not promise ordering (plan §29).
//   - Atomicity. Every file lands through a temp file in its own directory
//     followed by os.Rename, so a crash can never leave a half-written task or a
//     torn line in the event log.
//   - Identity safety. Task and session ids arrive from external agents and are
//     used as path segments, so they are validated rather than rewritten; see
//     validID.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Names of the fixed files and directories in the layout.
const (
	tasksDirName       = "tasks"
	handoffsDirName    = "handoffs"
	indexDirName       = "index"
	sessionIndexName   = "session"
	repoBranchIdxName  = "repobranch"
	sessionsDirName    = "sessions"
	checkpointsDirName = "checkpoints"
	taskFileName       = "task.json"
	eventsFileName     = "events.ndjson"
	stateFileName      = "state.json"
)

// FS is a filesystem-backed model.Store.
//
// A single mutex serialises every operation. Several methods touch more than
// one file (task.json plus two index entries, an event line plus a session
// node), and those groups are only consistent if no second goroutine interleaves
// with them. The mutex is in-process only: os.Rename keeps each individual file
// intact for a concurrent reader in another process, but this store does not
// attempt cross-process transactions.
type FS struct {
	mu    sync.Mutex
	root  string
	clock model.Clock
}

var _ model.Store = (*FS)(nil)

// NewFS opens, creating if necessary, a store rooted at root.
//
// clock is required rather than optional. Falling back to time.Now() when a
// caller forgets to pass one would make stored timestamps irreproducible and
// break every golden fixture downstream, so an absent clock is an error instead
// of a silent default.
func NewFS(root string, clock model.Clock) (*FS, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("store: root directory is required")
	}
	if clock == nil {
		return nil, errors.New("store: clock is required, determinism depends on it")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store: resolve root %q: %w", root, err)
	}
	f := &FS{root: abs, clock: clock}
	for _, d := range []string{
		f.root,
		filepath.Join(f.root, tasksDirName),
		filepath.Join(f.root, handoffsDirName),
		filepath.Join(f.root, indexDirName, sessionIndexName),
		filepath.Join(f.root, indexDirName, repoBranchIdxName),
	} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return nil, fmt.Errorf("store: create %q: %w", d, err)
		}
	}
	return f, nil
}

// Root returns the absolute directory the store lives in. Callers surface it in
// output so a reader always knows which store a claim came from.
func (f *FS) Root() string { return f.root }

// taskDir returns the directory for a task, rejecting an id that cannot safely
// become a path segment.
func (f *FS) taskDir(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", fmt.Errorf("store: task id: %w", err)
	}
	return join(f.root, tasksDirName, id)
}

func (f *FS) sessionIndexPath(sessionID string) (string, error) {
	if err := validID(sessionID); err != nil {
		return "", fmt.Errorf("store: session id: %w", err)
	}
	return join(f.root, indexDirName, sessionIndexName, sessionID)
}

func (f *FS) repoBranchIndexPath(repo, branch string) string {
	return filepath.Join(f.root, indexDirName, repoBranchIdxName, repoBranchKey(repo, branch))
}

// CreateTask records a new task. It fails if the task already exists: a task id
// is derived from repo, branch and root session (model.NewTaskID), so a repeat
// create means the caller has lost track of an existing task, and overwriting it
// would discard that task's recorded history.
func (f *FS) CreateTask(ctx context.Context, t model.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := f.taskDir(t.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, taskFileName)); err == nil {
		return fmt.Errorf("store: task %q already exists", t.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: stat task %q: %w", t.ID, err)
	}
	if t.RootSessionID != "" {
		if err := validID(t.RootSessionID); err != nil {
			return fmt.Errorf("store: root session id: %w", err)
		}
		// Checked before anything is written so a session already owned by
		// another task cannot leave a half-created task behind.
		if err := f.checkSessionOwner(t.RootSessionID, t.ID); err != nil {
			return err
		}
	}

	// One clock read per call, used for both fields, so the number of ticks a
	// FixedClock advances is a function of the call sequence alone.
	now := f.clock.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}

	for _, d := range []string{dir, filepath.Join(dir, sessionsDirName), filepath.Join(dir, checkpointsDirName)} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return fmt.Errorf("store: create %q: %w", d, err)
		}
	}
	// task.json lands before the indexes. If an index write then fails the task
	// is still there and still resolvable by id; the reverse order would leave an
	// index pointing at a task that does not exist, which reads as corruption.
	if err := writeJSON(filepath.Join(dir, taskFileName), t); err != nil {
		return err
	}
	if t.RootSessionID != "" {
		if err := f.bindSession(t.RootSessionID, t.ID); err != nil {
			return err
		}
	}
	return f.bindRepoBranch(t.Repo, t.Branch, t.ID)
}

// UpdateTask replaces the stored task and stamps UpdatedAt from the clock.
//
// CreatedAt is taken from the stored task and cannot be rewritten: when a task
// was first seen is a fact about the past, and a caller round-tripping a
// partially filled Task must not be able to erase it.
//
// Repo, Branch and RootSessionID are carried forward for the same reason when
// the caller leaves them empty. model.NewTaskID derives the task id from exactly
// those three values, so they are what the id means; blanking them would also
// take the repo+branch index down with them (see the drift handling below) and
// leave the task permanently unreachable from a bare "resume". An empty field is
// a caller who did not state it, never a caller asking for it to be cleared;
// a genuine rename supplies the new value and still moves the index.
//
// The store deliberately does not enforce model.CanTransition here. Status is
// derived from evidence by the packages above (model.EngineeringState.DeriveStatus);
// making persistence the transition gate would mean a legitimately derived
// status could not be recorded at all, which fails worse than it protects.
func (f *FS) UpdateTask(ctx context.Context, t model.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	prev, err := f.loadTask(t.ID)
	if err != nil {
		return err
	}
	if t.Repo == "" {
		t.Repo = prev.Repo
	}
	if t.Branch == "" {
		t.Branch = prev.Branch
	}
	if t.RootSessionID == "" {
		t.RootSessionID = prev.RootSessionID
	}
	if t.RootSessionID != "" && t.RootSessionID != prev.RootSessionID {
		if err := validID(t.RootSessionID); err != nil {
			return fmt.Errorf("store: root session id: %w", err)
		}
		if err := f.checkSessionOwner(t.RootSessionID, t.ID); err != nil {
			return err
		}
	}

	t.CreatedAt = prev.CreatedAt
	t.UpdatedAt = f.clock.Now()

	dir, err := f.taskDir(t.ID)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, taskFileName), t); err != nil {
		return err
	}

	// A branch rename is a real event in this product (plan §46 covers repository
	// drift), so the repo+branch index follows the task rather than pinning it to
	// where it started.
	if prev.Repo != t.Repo || prev.Branch != t.Branch {
		if err := f.unbindRepoBranch(prev.Repo, prev.Branch, t.ID); err != nil {
			return err
		}
	}
	if err := f.bindRepoBranch(t.Repo, t.Branch, t.ID); err != nil {
		return err
	}
	if t.RootSessionID != "" {
		// The previous root session keeps its binding. A session that worked on
		// this task always belongs to this task, so dropping the old entry would
		// make already-recorded work unresolvable.
		return f.bindSession(t.RootSessionID, t.ID)
	}
	return nil
}

// Task returns the task with the given id, or model.ErrNotFound.
func (f *FS) Task(ctx context.Context, id string) (model.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return model.Task{}, err
	}
	return f.loadTask(id)
}

// loadTask reads one task. The caller must hold f.mu.
func (f *FS) loadTask(id string) (model.Task, error) {
	dir, err := f.taskDir(id)
	if err != nil {
		return model.Task{}, err
	}
	var t model.Task
	if err := readJSON(filepath.Join(dir, taskFileName), &t); err != nil {
		return model.Task{}, fmt.Errorf("store: task %q: %w", id, err)
	}
	return t, nil
}

// Tasks lists every task, oldest first.
//
// A directory under tasks/ with no task.json is reported as an error rather than
// skipped. Silently omitting it would hand the caller a list that looks complete
// and is not, and "never present incomplete context as complete" is the rule the
// whole product rests on.
func (f *FS) Tasks(ctx context.Context) ([]model.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dir := filepath.Join(f.root, tasksDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []model.Task{}, nil
		}
		return nil, fmt.Errorf("store: read dir %q: %w", dir, err)
	}
	out := make([]model.Task, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := f.loadTask(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ResolveTask finds the task a caller is talking about.
//
// With a ref it tries the task id first and then the session index, which is
// what lets a fresh agent session say "I am session X" and land on the task its
// predecessor was working on (plan §8). With no ref it falls back to the
// repo+branch pair, the "resume whatever is happening on this branch" path.
//
// A ref that cannot be a path segment is rejected outright rather than reported
// as ErrNotFound: refusing to look is not the same as looking and finding
// nothing, and collapsing the two would let a traversal attempt read as an
// ordinary miss.
func (f *FS) ResolveTask(ctx context.Context, ref, repo, branch string) (model.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return model.Task{}, err
	}

	if ref != "" {
		if err := validID(ref); err != nil {
			return model.Task{}, fmt.Errorf("store: task reference: %w", err)
		}
		t, err := f.loadTask(ref)
		switch {
		case err == nil:
			return t, nil
		case !errors.Is(err, model.ErrNotFound):
			return model.Task{}, err
		}

		owner, err := f.sessionOwner(ref)
		if err != nil {
			return model.Task{}, err
		}
		if owner == "" {
			return model.Task{}, fmt.Errorf("store: no task for reference %q: %w", ref, model.ErrNotFound)
		}
		t, err = f.loadTask(owner)
		if err != nil {
			// A dangling index entry still leaves the caller with no task, so
			// the error keeps whatever sentinel loadTask produced. The message
			// names the damage rather than implying the session never existed,
			// because "your store is inconsistent" and "you asked about a
			// session nobody has seen" call for different responses.
			return model.Task{}, fmt.Errorf("store: session %q indexes task %q which is unreadable: %w", ref, owner, err)
		}
		return t, nil
	}

	if repo == "" || branch == "" {
		return model.Task{}, errors.New("store: resolve needs a task or session reference, or both a repo and a branch")
	}
	b, err := os.ReadFile(f.repoBranchIndexPath(repo, branch))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return model.Task{}, fmt.Errorf("store: no task for %s@%s: %w", repo, branch, model.ErrNotFound)
		}
		return model.Task{}, fmt.Errorf("store: read repo/branch index: %w", err)
	}
	id := strings.TrimSpace(string(b))
	t, err := f.loadTask(id)
	if err != nil {
		return model.Task{}, fmt.Errorf("store: %s@%s indexes task %q which is unreadable: %w", repo, branch, id, err)
	}
	return t, nil
}

// sessionOwner returns the task id that owns a session, or "" when the session
// is not indexed. The caller must hold f.mu.
func (f *FS) sessionOwner(sessionID string) (string, error) {
	p, err := f.sessionIndexPath(sessionID)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("store: read session index %q: %w", sessionID, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// checkSessionOwner fails when the session is already bound to a different task.
//
// A session belongs to exactly one task for its whole life. Re-pointing it would
// make the lineage claim that one agent session worked on two separate tasks,
// which is a fabricated relationship, so the conflict is surfaced rather than
// overwritten.
func (f *FS) checkSessionOwner(sessionID, taskID string) error {
	owner, err := f.sessionOwner(sessionID)
	if err != nil {
		return err
	}
	if owner != "" && owner != taskID {
		return fmt.Errorf("store: session %q already belongs to task %q, refusing to rebind to %q", sessionID, owner, taskID)
	}
	return nil
}

// bindSession records which task owns a session. The caller must hold f.mu.
func (f *FS) bindSession(sessionID, taskID string) error {
	if err := f.checkSessionOwner(sessionID, taskID); err != nil {
		return err
	}
	p, err := f.sessionIndexPath(sessionID)
	if err != nil {
		return err
	}
	return writeAtomic(p, []byte(taskID+"\n"))
}

// bindRepoBranch points the repo+branch index at a task.
//
// The index holds exactly one task id, so the most recently created or updated
// task on a branch is the one a bare "resume" finds. Earlier tasks on the same
// branch stay reachable by task id and by any of their session ids, so nothing
// is lost, only de-prioritised.
//
// An empty repo or branch is not indexed at all: hashing two empty strings would
// produce a real key that a caller passing no repo and no branch could match,
// and resolving a task from no information is exactly the kind of confident
// wrong answer this product exists to avoid.
func (f *FS) bindRepoBranch(repo, branch, taskID string) error {
	if repo == "" || branch == "" {
		return nil
	}
	return writeAtomic(f.repoBranchIndexPath(repo, branch), []byte(taskID+"\n"))
}

// unbindRepoBranch drops an index entry, but only while it still points at the
// given task, so a task moving off a branch cannot delete a newer task's entry.
func (f *FS) unbindRepoBranch(repo, branch, taskID string) error {
	if repo == "" || branch == "" {
		return nil
	}
	p := f.repoBranchIndexPath(repo, branch)
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("store: read repo/branch index: %w", err)
	}
	if strings.TrimSpace(string(b)) != taskID {
		return nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: remove repo/branch index: %w", err)
	}
	return nil
}

// requireTask fails with model.ErrNotFound unless the task exists. Every write
// that hangs off a task goes through it, so the store cannot accumulate events,
// sessions or state for a task nobody ever created and that therefore can never
// be resolved or explained.
func (f *FS) requireTask(taskID string) (string, error) {
	dir, err := f.taskDir(taskID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, taskFileName)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("store: task %q: %w", taskID, model.ErrNotFound)
		}
		return "", fmt.Errorf("store: stat task %q: %w", taskID, err)
	}
	return dir, nil
}
