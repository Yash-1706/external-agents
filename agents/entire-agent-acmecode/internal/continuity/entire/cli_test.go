package entire

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// samplePayload is a checkpoint as the entire binary is expected to print it.
// Its created_at deliberately carries a non-UTC offset, because a wire format
// that varies by zone must not leak that variation into our output.
const samplePayload = `{
  "id": "cp_a705026eb3c1",
  "task_id": "task_demo",
  "session_id": "sess_root",
  "label": "CP2 pre-curveball",
  "commit_sha": "abc1234",
  "branch": "master",
  "created_at": "2026-03-01T14:30:00+05:30",
  "agent": "openclaw",
  "meta": {"message": "stable point"},
  "state": {"schema_version": 1, "status": "partial"}
}`

func TestParseCheckpoint(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantErr     string
		wantSentine error
		check       func(t *testing.T, cp model.Checkpoint)
	}{
		{
			name:    "full payload",
			payload: samplePayload,
			check: func(t *testing.T, cp model.Checkpoint) {
				if cp.ID != "cp_a705026eb3c1" || cp.TaskID != "task_demo" || cp.SessionID != "sess_root" {
					t.Errorf("identity fields = %+v", cp)
				}
				if cp.CommitSHA != "abc1234" || cp.Branch != "master" {
					t.Errorf("repo fields = %q %q", cp.CommitSHA, cp.Branch)
				}
				want := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
				if !cp.CreatedAt.Equal(want) {
					t.Errorf("CreatedAt = %s, want %s", cp.CreatedAt, want)
				}
				if cp.CreatedAt.Location() != time.UTC {
					t.Errorf("CreatedAt location = %s, want UTC; a zone-dependent timestamp breaks byte-identical output", cp.CreatedAt.Location())
				}
				if cp.Agent != model.AgentOpenClaw {
					t.Errorf("Agent = %q, want %q", cp.Agent, model.AgentOpenClaw)
				}
				if cp.Meta["message"] != "stable point" {
					t.Errorf("Meta = %v", cp.Meta)
				}
				if cp.State == nil || cp.State.Status != model.StatusPartial {
					t.Errorf("State = %+v, want the embedded engineering state", cp.State)
				}
			},
		},
		{
			// A runtime this build does not integrate with is still a runtime
			// the checkpoint named. Coercing it to "unknown" discarded the one
			// fact the producer stated about itself, which is why the agent
			// vocabulary was widened; see model.AgentKind.Valid.
			name:    "third-party agent keeps the name the checkpoint reported",
			payload: `{"id":"cp_1","created_at":"2026-03-01T09:00:00Z","agent":"borg"}`,
			check: func(t *testing.T, cp model.Checkpoint) {
				if cp.Agent != model.AgentKind("borg") {
					t.Errorf("Agent = %q, want %q", cp.Agent, "borg")
				}
			},
		},
		{
			// Widening the vocabulary is not the same as accepting anything:
			// a value that cannot be a safe identifier is still reported as
			// unknown rather than rendered as though it named a real runtime.
			name:    "malformed agent still becomes unknown",
			payload: `{"id":"cp_1","created_at":"2026-03-01T09:00:00Z","agent":"Borg Corp!! <script>"}`,
			check: func(t *testing.T, cp model.Checkpoint) {
				if cp.Agent != model.AgentUnknown {
					t.Errorf("Agent = %q, want %q", cp.Agent, model.AgentUnknown)
				}
			},
		},
		{
			name:    "absent agent becomes unknown",
			payload: `{"id":"cp_1","created_at":"2026-03-01T09:00:00Z"}`,
			check: func(t *testing.T, cp model.Checkpoint) {
				if cp.Agent != model.AgentUnknown {
					t.Errorf("Agent = %q, want %q", cp.Agent, model.AgentUnknown)
				}
			},
		},
		{
			name:    "no state is fine",
			payload: `{"id":"cp_1","created_at":"2026-03-01T09:00:00Z"}`,
			check: func(t *testing.T, cp model.Checkpoint) {
				if cp.State != nil {
					t.Errorf("State = %+v, want nil", cp.State)
				}
			},
		},
		{
			name:        "null is absence, not breakage",
			payload:     `null`,
			wantSentine: model.ErrNotFound,
		},
		{
			name:    "empty payload",
			payload: "   \n",
			wantErr: "empty checkpoint payload",
		},
		{
			name:    "malformed json",
			payload: `{"id": `,
			wantErr: "malformed checkpoint payload",
		},
		{
			name:    "no id",
			payload: `{"task_id":"task_demo","created_at":"2026-03-01T09:00:00Z"}`,
			wantErr: "no id",
		},
		{
			name:    "no created_at",
			payload: `{"id":"cp_1","task_id":"task_demo"}`,
			wantErr: "no created_at",
		},
		{
			name:    "state schema from the future",
			payload: `{"id":"cp_1","created_at":"2026-03-01T09:00:00Z","state":{"schema_version":99}}`,
			wantErr: "newer than this build understands",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp, err := parseCheckpoint([]byte(tt.payload))

			switch {
			case tt.wantSentine != nil:
				if !errors.Is(err, tt.wantSentine) {
					t.Fatalf("error = %v, want %v", err, tt.wantSentine)
				}
			case tt.wantErr != "":
				if err == nil {
					t.Fatalf("error = nil, want one mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				tt.check(t, cp)
			}
		})
	}
}

func TestParseCheckpointList(t *testing.T) {
	// Deliberately out of order, and with a tie on created_at, so both the
	// primary ordering and the tie-break are exercised.
	unordered := `[
	  {"id":"cp_c","created_at":"2026-03-01T09:02:00Z"},
	  {"id":"cp_b","created_at":"2026-03-01T09:00:00Z"},
	  {"id":"cp_a","created_at":"2026-03-01T09:00:00Z"}
	]`

	tests := []struct {
		name    string
		payload string
		wantIDs []string
		wantErr string
		// absentSentinel, when set, must NOT be reachable through the returned
		// error: some failures must never masquerade as a known outcome.
		absentSentinel error
	}{
		{
			name:    "sorted oldest first with a deterministic tie-break",
			payload: unordered,
			wantIDs: []string{"cp_a", "cp_b", "cp_c"},
		},
		{
			name:    "empty list is a legitimate answer",
			payload: `[]`,
			wantIDs: []string{},
		},
		{
			name:    "null list is a legitimate answer",
			payload: `null`,
			wantIDs: []string{},
		},
		{
			name:    "empty payload",
			payload: "",
			wantErr: "empty checkpoint list payload",
		},
		{
			// The wire contract is one shape. An object where an array was
			// promised is a contract violation, not something to guess around.
			name:    "object where an array was promised",
			payload: `{"checkpoints":[]}`,
			wantErr: "malformed checkpoint list payload",
		},
		{
			name:    "unreadable element is named, not dropped",
			payload: `[{"id":"cp_a","created_at":"2026-03-01T09:00:00Z"},{"task_id":"task_demo","created_at":"2026-03-01T09:01:00Z"}]`,
			wantErr: "entry 1",
		},
		{
			// A null element is a malformed list, not the answer "this task has
			// no checkpoints". The sentinel assertion below is the point of
			// this case: leaking ErrNotFound here would let a caller read a
			// broken payload as an empty but truthful history.
			name:           "a null element is malformed, not an absence",
			payload:        `[{"id":"cp_a","created_at":"2026-03-01T09:00:00Z"},null]`,
			wantErr:        "entry 1",
			absentSentinel: model.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCheckpointList([]byte(tt.payload))
			if tt.absentSentinel != nil && errors.Is(err, tt.absentSentinel) {
				t.Errorf("error = %v carries %v; a list-level failure must not be reported as that sentinel", err, tt.absentSentinel)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("error = nil, want one mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("a failed list parse returned %d checkpoint(s); a partly-understood list must not look complete", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, cp := range got {
				ids = append(ids, cp.ID)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

func TestCheckpointCreateArgs(t *testing.T) {
	tests := []struct {
		name string
		req  model.CheckpointRequest
		want []string
	}{
		{
			name: "minimal request",
			req:  model.CheckpointRequest{TaskID: "task_demo"},
			want: []string{"checkpoint", "create", "--json", "--task", "task_demo"},
		},
		{
			// Map iteration order is randomised by Go; if it reached the argv,
			// two identical requests would issue different commands.
			name: "metadata is emitted in a stable order",
			req: model.CheckpointRequest{
				TaskID:    "task_demo",
				SessionID: "sess_root",
				Label:     "CP2",
				Agent:     model.AgentOpenClaw,
				Message:   "stable point",
				Meta:      map[string]string{"zulu": "z", "alpha": "a", "mike": "m"},
			},
			want: []string{
				"checkpoint", "create", "--json",
				"--task", "task_demo",
				"--session", "sess_root",
				"--label", "CP2",
				"--agent", "openclaw",
				"--message", "stable point",
				"--meta", "alpha=a",
				"--meta", "mike=m",
				"--meta", "zulu=z",
			},
		},
		{
			name: "third-party agent is passed through under its own name",
			req:  model.CheckpointRequest{TaskID: "task_demo", Agent: model.AgentKind("borg")},
			want: []string{"checkpoint", "create", "--json", "--task", "task_demo", "--agent", "borg"},
		},
		{
			name: "state travels on stdin",
			req:  model.CheckpointRequest{TaskID: "task_demo", State: model.NewEngineeringState(model.TaskRef{ID: "task_demo"})},
			want: []string{"checkpoint", "create", "--json", "--task", "task_demo", "--state", "-"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkpointCreateArgs(tt.req)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("checkpointCreateArgs =\n%v\nwant\n%v", got, tt.want)
			}
			// Repeat: the ordering must hold across runs, not just once.
			for i := 0; i < 8; i++ {
				if again := checkpointCreateArgs(tt.req); !reflect.DeepEqual(again, got) {
					t.Fatalf("checkpointCreateArgs is not deterministic:\n%v\nthen\n%v", got, again)
				}
			}
		})
	}
}

func TestCheckpointReadArgs(t *testing.T) {
	if got, want := checkpointShowArgs("cp_a"), []string{"checkpoint", "show", "cp_a", "--json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("checkpointShowArgs = %v, want %v", got, want)
	}
	if got, want := checkpointListArgs("task_demo"), []string{"checkpoint", "list", "--task", "task_demo", "--json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("checkpointListArgs = %v, want %v", got, want)
	}
}

// TestCLIUnavailableBinary is the degradation path that actually happens in
// practice: the entire binary is not installed. Every call must report
// ErrUnavailable so the caller records the gap and keeps working (plan §48).
func TestCLIUnavailableBinary(t *testing.T) {
	ctx := context.Background()
	c := NewCLI("entire-binary-that-does-not-exist-6f2a", t.TempDir())

	if c.Available(ctx) {
		t.Fatal("Available() = true for a binary that does not exist")
	}

	calls := []struct {
		name string
		call func() error
	}{
		{"CreateCheckpoint", func() error {
			_, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{TaskID: "task_demo", Label: "CP1"})
			return err
		}},
		{"Checkpoint", func() error {
			_, err := c.Checkpoint(ctx, "cp_a705026eb3c1")
			return err
		}},
		{"Checkpoints", func() error {
			_, err := c.Checkpoints(ctx, "task_demo")
			return err
		}},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, model.ErrUnavailable) {
				t.Fatalf("error = %v, want ErrUnavailable", err)
			}
			// The message has to say what was attempted, or an operator cannot
			// tell a missing binary from a rejected subcommand.
			if !strings.Contains(err.Error(), "entire-binary-that-does-not-exist-6f2a") {
				t.Errorf("error = %v, want it to name the command that failed", err)
			}
		})
	}
}

// TestCLIValidatesBeforeExec keeps input errors distinguishable from a genuinely
// unreachable Entire: neither of these should ever be reported as unavailable.
func TestCLIValidatesBeforeExec(t *testing.T) {
	ctx := context.Background()
	c := NewCLI("entire-binary-that-does-not-exist-6f2a", t.TempDir())

	if _, err := c.CreateCheckpoint(ctx, model.CheckpointRequest{Label: "CP1"}); err == nil {
		t.Error("CreateCheckpoint without a task id = nil error")
	} else if errors.Is(err, model.ErrUnavailable) {
		t.Errorf("CreateCheckpoint without a task id reported ErrUnavailable: %v", err)
	}

	if _, err := c.Checkpoints(ctx, ""); err == nil {
		t.Error("Checkpoints with an empty task id = nil error")
	} else if errors.Is(err, model.ErrUnavailable) {
		t.Errorf("Checkpoints with an empty task id reported ErrUnavailable: %v", err)
	}

	if _, err := c.Checkpoint(ctx, ""); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Checkpoint with an empty id error = %v, want ErrNotFound", err)
	}
}

func TestCLIDescribeNamesTheBinary(t *testing.T) {
	tests := []struct {
		name string
		bin  string
		dir  string
		want []string
	}{
		{name: "explicit binary and directory", bin: "entire.exe", dir: `D:\repo`, want: []string{"entire CLI", "entire.exe", `D:\repo`}},
		{name: "defaults", bin: "", dir: "", want: []string{"entire CLI", `"entire"`, "current directory"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewCLI(tt.bin, tt.dir).Describe()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Describe() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// TestStderrDetail covers the privacy bound on error text: enough of the
// binary's complaint to diagnose it, never a wholesale dump that could carry a
// credential into a log (plan §34).
func TestStderrDetail(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		want     string   // exact result, when the whole output is short and safe
		contains []string // otherwise, what must appear
		absent   []string // and what must not
	}{
		{name: "empty output adds nothing", stderr: "   \n", want: ""},
		{name: "single line is quoted whole", stderr: "unknown subcommand", want: " (stderr: unknown subcommand)"},
		{
			name:     "only the first line survives",
			stderr:   "failed to authenticate\nAUTH_TOKEN=hunter2\n",
			contains: []string{"failed to authenticate", "[...]"},
			absent:   []string{"hunter2", "AUTH_TOKEN"},
		},
		{
			name:     "a long first line is truncated",
			stderr:   strings.Repeat("x", 4096),
			contains: []string{"[...]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stderrDetail([]byte(tt.stderr))
			// With no substring expectations the result is pinned exactly,
			// which is also how the empty case is held to "" rather than
			// passing vacuously.
			if len(tt.contains) == 0 && got != tt.want {
				t.Fatalf("stderrDetail = %q, want %q", got, tt.want)
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("stderrDetail = %q, want it to contain %q", got, want)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(got, bad) {
					t.Errorf("stderrDetail = %q leaked %q", got, bad)
				}
			}
			if len(got) > stderrLimit+64 {
				t.Errorf("stderrDetail returned %d bytes, want it bounded near %d", len(got), stderrLimit)
			}
		})
	}
}
