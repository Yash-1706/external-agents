package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// testClock returns a deterministic clock. Step is non-zero so successive
// timestamps are distinguishable without ever reading the wall clock.
func testClock() *model.FixedClock {
	return &model.FixedClock{Current: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), Step: time.Minute}
}

func newStore(t *testing.T) *FS {
	t.Helper()
	f, err := NewFS(t.TempDir(), testClock())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return f
}

const (
	testTaskID  = "task_0123456789abcdef"
	testRootSID = "sess_root"
	testRepo    = "entire/continuity"
	testBranch  = "feature/subscription-pause"
)

func sampleTask() model.Task {
	return model.Task{
		ID:            testTaskID,
		Repo:          testRepo,
		Branch:        testBranch,
		RootSessionID: testRootSID,
		Title:         "Implement subscription pause",
		Status:        model.StatusActive,
	}
}

func seedTask(t *testing.T, f *FS) model.Task {
	t.Helper()
	if err := f.CreateTask(context.Background(), sampleTask()); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, err := f.Task(context.Background(), testTaskID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	return got
}

// tree fingerprints a directory so two independently built stores can be
// compared byte for byte. Determinism is a product requirement, not a nicety:
// two agents that saw the same events must produce the same store.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out = append(out, rel+"/")
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out = append(out, rel+" "+hex.EncodeToString(sum[:8]))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func TestNewFS(t *testing.T) {
	t.Run("rejects missing inputs", func(t *testing.T) {
		tests := []struct {
			name  string
			root  string
			clock model.Clock
		}{
			{"empty root", "", testClock()},
			{"blank root", "   ", testClock()},
			// A nil clock would silently push the store onto the wall clock and
			// make every stored timestamp irreproducible.
			{"nil clock", t.TempDir(), nil},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := NewFS(tc.root, tc.clock); err == nil {
					t.Fatalf("NewFS(%q, %v) = nil error, want rejection", tc.root, tc.clock)
				}
			})
		}
	})

	t.Run("creates the layout", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "nested", "store")
		f, err := NewFS(root, testClock())
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		if !filepath.IsAbs(f.Root()) {
			t.Errorf("Root() = %q, want an absolute path", f.Root())
		}
		for _, rel := range []string{"tasks", "handoffs", "index/session", "index/repobranch"} {
			p := filepath.Join(f.Root(), filepath.FromSlash(rel))
			info, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %q: %v", rel, err)
			}
			if !info.IsDir() {
				t.Errorf("%q is not a directory", rel)
			}
		}
	})

	t.Run("reopening is not destructive", func(t *testing.T) {
		root := t.TempDir()
		f, err := NewFS(root, testClock())
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		seedTask(t, f)
		reopened, err := NewFS(root, testClock())
		if err != nil {
			t.Fatalf("NewFS reopen: %v", err)
		}
		if _, err := reopened.Task(context.Background(), testTaskID); err != nil {
			t.Fatalf("Task after reopen: %v", err)
		}
	})
}

func TestCreateTask(t *testing.T) {
	ctx := context.Background()

	t.Run("stamps timestamps from the clock", func(t *testing.T) {
		f := newStore(t)
		got := seedTask(t, f)
		want := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
		if !got.CreatedAt.Equal(want) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want)
		}
		if !got.UpdatedAt.Equal(want) {
			t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want)
		}
	})

	t.Run("preserves caller supplied timestamps", func(t *testing.T) {
		f := newStore(t)
		task := sampleTask()
		task.CreatedAt = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := f.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		got, err := f.Task(ctx, testTaskID)
		if err != nil {
			t.Fatalf("Task: %v", err)
		}
		if !got.CreatedAt.Equal(task.CreatedAt) {
			t.Errorf("CreatedAt = %v, want the supplied %v", got.CreatedAt, task.CreatedAt)
		}
	})

	t.Run("rejects a duplicate", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		err := f.CreateTask(ctx, sampleTask())
		if err == nil {
			t.Fatal("CreateTask on an existing task = nil error, want rejection")
		}
		if errors.Is(err, model.ErrNotFound) {
			t.Errorf("duplicate reported as ErrNotFound: %v", err)
		}
		// The original must survive untouched: a re-create is a caller bug, and
		// overwriting would discard recorded history.
		got, err := f.Task(ctx, testTaskID)
		if err != nil {
			t.Fatalf("Task: %v", err)
		}
		if got.Title != "Implement subscription pause" {
			t.Errorf("Title = %q, want the original", got.Title)
		}
	})

	t.Run("rejects a root session owned by another task", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		second := sampleTask()
		second.ID = "task_ffffffffffffffff"
		if err := f.CreateTask(ctx, second); err == nil {
			t.Fatal("CreateTask reusing another task's root session = nil error, want rejection")
		}
		if _, err := f.Task(ctx, second.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("rejected task was partially created: %v", err)
		}
	})
}

func TestTaskNotFound(t *testing.T) {
	f := newStore(t)
	_, err := f.Task(context.Background(), "task_absent")
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Task(absent) error = %v, want model.ErrNotFound", err)
	}
}

func TestUpdateTask(t *testing.T) {
	ctx := context.Background()

	t.Run("absent task", func(t *testing.T) {
		f := newStore(t)
		task := sampleTask()
		if err := f.UpdateTask(ctx, task); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("UpdateTask(absent) error = %v, want model.ErrNotFound", err)
		}
	})

	t.Run("stamps UpdatedAt and pins CreatedAt", func(t *testing.T) {
		f := newStore(t)
		created := seedTask(t, f)

		next := sampleTask()
		next.Status = model.StatusPartial
		// A caller round-tripping a partly filled Task must not be able to
		// rewrite when the task was first seen.
		next.CreatedAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := f.UpdateTask(ctx, next); err != nil {
			t.Fatalf("UpdateTask: %v", err)
		}
		got, err := f.Task(ctx, testTaskID)
		if err != nil {
			t.Fatalf("Task: %v", err)
		}
		if !got.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("CreatedAt = %v, want the original %v", got.CreatedAt, created.CreatedAt)
		}
		if !got.UpdatedAt.After(created.UpdatedAt) {
			t.Errorf("UpdatedAt = %v, want a later stamp than %v", got.UpdatedAt, created.UpdatedAt)
		}
		if got.Status != model.StatusPartial {
			t.Errorf("Status = %q, want partial", got.Status)
		}
	})

	t.Run("follows a branch rename", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)

		moved := sampleTask()
		moved.Branch = "feature/subscription-pause-v2"
		if err := f.UpdateTask(ctx, moved); err != nil {
			t.Fatalf("UpdateTask: %v", err)
		}
		if _, err := f.ResolveTask(ctx, "", testRepo, moved.Branch); err != nil {
			t.Errorf("resolve by new branch: %v", err)
		}
		if _, err := f.ResolveTask(ctx, "", testRepo, testBranch); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("resolve by old branch error = %v, want model.ErrNotFound", err)
		}
	})

	t.Run("rejects a root session owned by another task", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		other := model.Task{ID: "task_other", Repo: testRepo, Branch: "other", RootSessionID: "sess_other"}
		if err := f.CreateTask(ctx, other); err != nil {
			t.Fatalf("CreateTask other: %v", err)
		}
		steal := sampleTask()
		steal.RootSessionID = "sess_other"
		if err := f.UpdateTask(ctx, steal); err == nil {
			t.Fatal("UpdateTask stealing another task's session = nil error, want rejection")
		}
	})
}

func TestResolveTask(t *testing.T) {
	ctx := context.Background()

	t.Run("all three lookup paths", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		// A session that only became known through an event still resolves.
		if err := f.AppendEvent(ctx, model.AgentEvent{
			Type:      model.SubagentStarted,
			Timestamp: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			TaskID:    testTaskID,
			SessionID: "sess_child",
			Agent:     model.AgentHermes,
			Role:      model.RoleTest,
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}

		tests := []struct {
			name   string
			ref    string
			repo   string
			branch string
		}{
			{"by task id", testTaskID, "", ""},
			{"by root session id", testRootSID, "", ""},
			{"by subagent session id", "sess_child", "", ""},
			{"by repo and branch", "", testRepo, testBranch},
			// A ref wins over repo+branch, so a caller naming a task explicitly
			// is never redirected to whatever else is on the branch.
			{"ref beats repo and branch", testTaskID, "other/repo", "other"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := f.ResolveTask(ctx, tc.ref, tc.repo, tc.branch)
				if err != nil {
					t.Fatalf("ResolveTask: %v", err)
				}
				if got.ID != testTaskID {
					t.Errorf("ID = %q, want %q", got.ID, testTaskID)
				}
			})
		}
	})

	t.Run("not found paths", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		tests := []struct {
			name   string
			ref    string
			repo   string
			branch string
		}{
			{"unknown ref", "sess_nobody", "", ""},
			{"unknown repo and branch", "", "other/repo", "main"},
			{"known repo, unknown branch", "", testRepo, "main"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := f.ResolveTask(ctx, tc.ref, tc.repo, tc.branch)
				if !errors.Is(err, model.ErrNotFound) {
					t.Fatalf("error = %v, want model.ErrNotFound", err)
				}
			})
		}
	})

	t.Run("nothing to resolve by", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		// Distinct from ErrNotFound on purpose: the caller gave the store no
		// question to answer, and answering anyway would be a guess.
		tests := []struct{ name, repo, branch string }{
			{"no inputs at all", "", ""},
			{"repo without branch", testRepo, ""},
			{"branch without repo", "", testBranch},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := f.ResolveTask(ctx, "", tc.repo, tc.branch)
				if err == nil {
					t.Fatal("error = nil, want rejection")
				}
				if errors.Is(err, model.ErrNotFound) {
					t.Errorf("an unanswerable question was reported as ErrNotFound: %v", err)
				}
			})
		}
	})

	t.Run("unusable reference is rejected, not reported as absent", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		_, err := f.ResolveTask(ctx, "../../etc/passwd", "", "")
		if err == nil {
			t.Fatal("error = nil, want rejection")
		}
		if errors.Is(err, model.ErrNotFound) {
			t.Errorf("refusing to look was reported as looking and finding nothing: %v", err)
		}
	})

	t.Run("dangling session index names the damage", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		if err := os.RemoveAll(filepath.Join(f.Root(), tasksDirName, testTaskID)); err != nil {
			t.Fatalf("remove task dir: %v", err)
		}
		_, err := f.ResolveTask(ctx, testRootSID, "", "")
		if err == nil {
			t.Fatal("error = nil, want a report of the dangling index")
		}
		if !strings.Contains(err.Error(), "unreadable") {
			t.Errorf("error = %v, want it to name the dangling index", err)
		}
	})
}

func TestTasks(t *testing.T) {
	ctx := context.Background()

	t.Run("empty store", func(t *testing.T) {
		f := newStore(t)
		got, err := f.Tasks(ctx)
		if err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("Tasks = %d entries, want 0", len(got))
		}
	})

	t.Run("oldest first", func(t *testing.T) {
		f := newStore(t)
		// Created newest-title-first so filename order alone cannot pass.
		for _, id := range []string{"task_zzz", "task_aaa", "task_mmm"} {
			task := model.Task{ID: id, Repo: testRepo, Branch: "b-" + id, RootSessionID: "sess-" + id}
			if err := f.CreateTask(ctx, task); err != nil {
				t.Fatalf("CreateTask %q: %v", id, err)
			}
		}
		got, err := f.Tasks(ctx)
		if err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		want := []string{"task_zzz", "task_aaa", "task_mmm"}
		if len(got) != len(want) {
			t.Fatalf("Tasks = %d entries, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Errorf("Tasks[%d].ID = %q, want %q", i, got[i].ID, want[i])
			}
		}
	})

	t.Run("a task directory without task.json is an error, not a silent omission", func(t *testing.T) {
		f := newStore(t)
		seedTask(t, f)
		if err := os.MkdirAll(filepath.Join(f.Root(), tasksDirName, "task_halfwritten"), dirPerm); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := f.Tasks(ctx); err == nil {
			t.Fatal("Tasks = nil error, want the incomplete listing reported")
		}
	})
}

// TestDeterminism is the byte-for-byte guarantee: the same call sequence against
// two fresh stores produces two identical trees.
func TestDeterminism(t *testing.T) {
	ctx := context.Background()

	build := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		f, err := NewFS(root, testClock())
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		if err := f.CreateTask(ctx, sampleTask()); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		for _, ev := range demoEvents() {
			if err := f.AppendEvent(ctx, ev); err != nil {
				t.Fatalf("AppendEvent %s: %v", ev.Type, err)
			}
		}
		st := model.NewEngineeringState(model.TaskRef{ID: testTaskID, Title: "Implement subscription pause"})
		st.Requirements = []model.Requirement{
			{ID: "R2", Description: "second", Status: model.ReqUnresolved, Confidence: model.Unknown},
			{ID: "R1", Description: "first", Status: model.ReqComplete, Confidence: model.Observed},
		}
		if err := f.PutState(ctx, testTaskID, st); err != nil {
			t.Fatalf("PutState: %v", err)
		}
		if err := f.PutHandoff(ctx, model.HandoffNode{
			ID:        "handoff_001",
			TaskID:    testTaskID,
			StateHash: st.Hash(),
			CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("PutHandoff: %v", err)
		}
		return f.Root()
	}

	a, b := tree(t, build(t)), tree(t, build(t))
	if len(a) != len(b) {
		t.Fatalf("tree sizes differ: %d vs %d\n%v\n%v", len(a), len(b), a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("entry %d differs:\n got %s\nwant %s", i, a[i], b[i])
		}
	}
}

// TestUpdateTaskPreservesIdentityFields guards the round-trip a caller makes
// when it only means to record one field. model.NewTaskID derives the task id
// from repo, branch and root session, so blanking those blanks what the id
// means -- and it used to take the repo+branch index with it, which is the entry
// point a bare "resume" uses (plan §8).
func TestUpdateTaskPreservesIdentityFields(t *testing.T) {
	ctx := context.Background()
	f := newStore(t)
	seedTask(t, f)

	// A caller that loaded nothing and only wants to record a status change.
	if err := f.UpdateTask(ctx, model.Task{ID: testTaskID, Status: model.StatusBlocked}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	got, err := f.Task(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if got.Status != model.StatusBlocked {
		t.Errorf("Status = %q, want blocked: the field the caller did state was dropped", got.Status)
	}
	if got.Repo != testRepo || got.Branch != testBranch {
		t.Errorf("repo/branch = %q/%q, want %q/%q preserved", got.Repo, got.Branch, testRepo, testBranch)
	}
	if got.RootSessionID != testRootSID {
		t.Errorf("RootSessionID = %q, want %q preserved: the task id is derived from it", got.RootSessionID, testRootSID)
	}

	// The load-bearing consequence: the task is still resumable from the branch.
	resolved, err := f.ResolveTask(ctx, "", testRepo, testBranch)
	if err != nil {
		t.Fatalf("ResolveTask by repo/branch after a partial update: %v", err)
	}
	if resolved.ID != testTaskID {
		t.Errorf("resolved %q, want %q", resolved.ID, testTaskID)
	}
	if _, err := os.Stat(filepath.Join(f.Root(), indexDirName, repoBranchIdxName, repoBranchKey(testRepo, testBranch))); err != nil {
		t.Errorf("repo/branch index was destroyed by a partial update: %v", err)
	}

	// A stated value still wins, so a real rename is unaffected.
	renamed := sampleTask()
	renamed.Branch = "feature/subscription-pause-v2"
	if err := f.UpdateTask(ctx, renamed); err != nil {
		t.Fatalf("UpdateTask rename: %v", err)
	}
	got, err = f.Task(ctx, testTaskID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if got.Branch != "feature/subscription-pause-v2" {
		t.Errorf("Branch = %q, want the renamed branch", got.Branch)
	}
}
