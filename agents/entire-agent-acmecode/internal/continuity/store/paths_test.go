package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

func TestValidID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{"derived task id", "task_0123456789abcdef", true},
		{"uuid style session", "01JD4Z8Q-7K2M-4C1A", true},
		{"dotted version", "cp_001.v2", true},

		{"empty", "", false},
		{"current directory", ".", false},
		{"parent directory", "..", false},
		{"relative escape", "../../etc/passwd", false},
		{"posix separator", "a/b", false},
		{"windows separator", `a\b`, false},
		{"drive letter", "c:evil", false},
		{"absolute posix", "/etc/passwd", false},
		{"absolute windows", `C:\Windows\System32`, false},
		{"nul byte", "task\x00evil", false},
		{"newline", "task\nevil", false},
		{"space", "task evil", false},
		{"leading dot hides the file", ".hidden", false},
		{"leading dot collides with temp files", ".tmp-1234", false},
		{"trailing dot aliases on windows", "task.", false},
		{"reserved device", "con", false},
		{"reserved device with extension", "AUX.json", false},
		{"non ascii normalises differently per platform", "task_\u00fc", false},
		{"too long", strings.Repeat("a", maxIDLen+1), false},
		{"at the length limit", strings.Repeat("a", maxIDLen), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validID(tc.id)
			if tc.ok && err != nil {
				t.Fatalf("validID(%q) = %v, want accepted", tc.id, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validID(%q) = nil, want rejected", tc.id)
			}
		})
	}
}

func TestContained(t *testing.T) {
	base := filepath.FromSlash("/srv/store")
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"base itself", base, true},
		{"direct child", filepath.Join(base, "tasks"), true},
		{"deep child", filepath.Join(base, "tasks", "t1", "task.json"), true},
		{"parent", filepath.Dir(base), false},
		{"sibling", filepath.Join(filepath.Dir(base), "other"), false},
		{"climbing out", filepath.Join(base, "..", "..", "etc"), false},
		{"prefix lookalike", base + "-backup", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := contained(base, tc.path); got != tc.want {
				t.Errorf("contained(%q, %q) = %v, want %v", base, tc.path, got, tc.want)
			}
		})
	}
}

func TestRepoBranchKeyIsUnambiguous(t *testing.T) {
	// The NUL separator is what stops "ab" + "c" and "a" + "bc" from colliding,
	// which would silently point two different branches at one task.
	if repoBranchKey("ab", "c") == repoBranchKey("a", "bc") {
		t.Error("repo/branch keys collide across the separator boundary")
	}
	if repoBranchKey(testRepo, testBranch) != repoBranchKey(testRepo, testBranch) {
		t.Error("repo/branch key is not stable across calls")
	}
	key := repoBranchKey(testRepo, "feature/with/slashes")
	if err := validID(key); err != nil {
		// The key is used directly as a filename, so it must survive the same
		// scrutiny as any other path segment even though it is hashed.
		t.Errorf("repo/branch key %q is not a usable path segment: %v", key, err)
	}
}

// TestHostileIDsNeverTouchTheFilesystem drives every entry point with ids
// crafted to escape the store root and then proves the tree is untouched.
// Task and session ids come from external agents, so this is the boundary that
// keeps a crafted id from writing anywhere it likes.
func TestHostileIDsNeverTouchTheFilesystem(t *testing.T) {
	ctx := context.Background()

	hostile := []string{
		"../../escape",
		"..",
		".",
		"",
		"a/b",
		`..\..\escape`,
		"task\x00evil",
		"con",
		strings.Repeat("a", maxIDLen+1),
	}

	calls := []struct {
		name string
		// emptyIsUnknown marks the entry points where an absent id is a
		// legitimate "the agent did not tell us" rather than an escape attempt.
		// Those are degradation paths and are covered by their own tests.
		emptyIsUnknown bool
		call           func(f *FS, id string) error
	}{
		{name: "CreateTask", call: func(f *FS, id string) error {
			return f.CreateTask(ctx, model.Task{ID: id, Repo: testRepo, Branch: "hostile"})
		}},
		{name: "CreateTask root session", emptyIsUnknown: true, call: func(f *FS, id string) error {
			return f.CreateTask(ctx, model.Task{ID: "task_hostile_root", Repo: testRepo, Branch: "hostile", RootSessionID: id})
		}},
		{name: "UpdateTask", call: func(f *FS, id string) error {
			return f.UpdateTask(ctx, model.Task{ID: id})
		}},
		{name: "Task", call: func(f *FS, id string) error {
			_, err := f.Task(ctx, id)
			return err
		}},
		{name: "ResolveTask", call: func(f *FS, id string) error {
			_, err := f.ResolveTask(ctx, id, "", "")
			return err
		}},
		{name: "AppendEvent task id", call: func(f *FS, id string) error {
			ev := demoEvents()[0]
			ev.TaskID = id
			return f.AppendEvent(ctx, ev)
		}},
		{name: "AppendEvent session id", call: func(f *FS, id string) error {
			ev := demoEvents()[0]
			ev.SessionID = id
			return f.AppendEvent(ctx, ev)
		}},
		{name: "AppendEvent checkpoint id", emptyIsUnknown: true, call: func(f *FS, id string) error {
			ev := demoEvents()[0]
			ev.Type = model.CheckpointCreated
			ev.Attrs = map[string]string{"checkpoint_id": id}
			return f.AppendEvent(ctx, ev)
		}},
		{name: "Events", call: func(f *FS, id string) error {
			_, err := f.Events(ctx, id)
			return err
		}},
		{name: "PutSession", call: func(f *FS, id string) error {
			return f.PutSession(ctx, testTaskID, model.SessionNode{SessionID: id})
		}},
		{name: "PutCheckpoint", call: func(f *FS, id string) error {
			return f.PutCheckpoint(ctx, testTaskID, model.CheckpointNode{CheckpointID: id})
		}},
		{name: "PutHandoff", call: func(f *FS, id string) error {
			return f.PutHandoff(ctx, model.HandoffNode{ID: id, TaskID: testTaskID})
		}},
		{name: "PutState", call: func(f *FS, id string) error {
			return f.PutState(ctx, id, sampleState())
		}},
		{name: "State", call: func(f *FS, id string) error {
			_, err := f.State(ctx, id)
			return err
		}},
		{name: "Lineage", call: func(f *FS, id string) error {
			_, err := f.Lineage(ctx, id)
			return err
		}},
	}

	for _, c := range calls {
		for _, id := range hostile {
			if id == "" && c.emptyIsUnknown {
				continue
			}
			t.Run(c.name+"/"+strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
				// The store lives inside an outer directory so an escape by one
				// level would be visible.
				outer := t.TempDir()
				f, err := NewFS(filepath.Join(outer, "store"), testClock())
				if err != nil {
					t.Fatalf("NewFS: %v", err)
				}
				seedTask(t, f)
				before := tree(t, outer)

				if err := c.call(f, id); err == nil {
					t.Fatalf("%s(%q) = nil error, want rejection", c.name, id)
				}

				after := tree(t, outer)
				if len(before) != len(after) {
					t.Fatalf("%s(%q) changed the tree:\nbefore %v\nafter  %v", c.name, id, before, after)
				}
				for i := range before {
					if before[i] != after[i] {
						t.Fatalf("%s(%q) changed %q into %q", c.name, id, before[i], after[i])
					}
				}
			})
		}
	}
}
