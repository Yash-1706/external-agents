package entire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

const (
	// defaultBin is the binary NewCLI shells out to when the caller names none.
	defaultBin = "entire"

	// probeTimeout bounds Available. "Is Entire there?" is a liveness question,
	// and the answer must arrive quickly even when the binary hangs, because
	// Entire is never allowed to become a fatal dependency of the host agent's
	// own work (plan §48).
	probeTimeout = 2 * time.Second

	// stderrLimit caps how much of the binary's stderr is quoted back inside an
	// error. Raw tool output can carry credentials and must not be copied
	// wholesale into state or logs (plan §34).
	stderrLimit = 512
)

// cliClient talks to the real Entire by shelling out to its binary and parsing
// the JSON it prints.
//
// The subcommands below are the surface this build assumes. When the installed
// binary disagrees, the mismatch surfaces as ErrUnavailable carrying the
// binary's own stderr, so the operator sees what actually failed rather than a
// silently empty history.
type cliClient struct {
	bin string
	dir string
}

// NewCLI returns a client backed by the entire binary. bin defaults to "entire"
// when empty; dir is the working directory the binary runs in, which is how
// Entire knows which repository is being talked about.
func NewCLI(bin, dir string) model.EntireClient {
	if trimmed(bin) == "" {
		bin = defaultBin
	}
	return &cliClient{bin: bin, dir: dir}
}

// Describe names the binary and the directory it runs in, so output that cites
// a checkpoint also says where that checkpoint came from.
func (c *cliClient) Describe() string {
	dir := c.dir
	if trimmed(dir) == "" {
		dir = "the current directory"
	}
	return fmt.Sprintf("entire CLI %q in %s", c.bin, dir)
}

// Available runs a cheap probe under a short deadline. It answers only the
// liveness question: a true here means the binary ran, not that any particular
// checkpoint exists.
func (c *cliClient) Available(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, err := c.run(ctx, nil, "--version")
	return err == nil
}

// CreateCheckpoint records a milestone through the binary and parses the
// checkpoint it reports back. The state travels on stdin rather than in an
// argument: it is large, and a command line is the wrong place for a document.
func (c *cliClient) CreateCheckpoint(ctx context.Context, req model.CheckpointRequest) (model.Checkpoint, error) {
	if trimmed(req.TaskID) == "" {
		return model.Checkpoint{}, errors.New("entire: create checkpoint: task id must not be empty")
	}
	if err := checkStateSchema(req.State); err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: %w", err)
	}

	var stdin []byte
	if req.State != nil {
		b, err := json.Marshal(canonicalState(req.State))
		if err != nil {
			return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: encoding state: %w", err)
		}
		stdin = b
	}

	out, err := c.run(ctx, stdin, checkpointCreateArgs(req)...)
	if err != nil {
		return model.Checkpoint{}, err
	}
	cp, err := parseCheckpoint(out)
	if err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: %w", err)
	}
	return cp, nil
}

// Checkpoint loads one checkpoint by id.
//
// A non-zero exit is reported as ErrUnavailable even when the real cause was
// "no such checkpoint": the binary's exit status does not distinguish the two,
// and inventing model.ErrNotFound from a guess would tell the caller the
// history layer is healthy when we do not know that. The conservative reading
// is the honest one here, since neither answer permits claiming safe resume
// (plan §48). A payload of "null" is unambiguous absence and does map to
// ErrNotFound; see parseCheckpoint.
func (c *cliClient) Checkpoint(ctx context.Context, id string) (model.Checkpoint, error) {
	if trimmed(id) == "" {
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: %w", id, model.ErrNotFound)
	}
	out, err := c.run(ctx, nil, checkpointShowArgs(id)...)
	if err != nil {
		return model.Checkpoint{}, err
	}
	cp, err := parseCheckpoint(out)
	if err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: %w", id, err)
	}
	return cp, nil
}

// Checkpoints lists a task's checkpoints, oldest first.
func (c *cliClient) Checkpoints(ctx context.Context, taskID string) ([]model.Checkpoint, error) {
	if trimmed(taskID) == "" {
		return nil, errors.New("entire: checkpoints: task id must not be empty")
	}
	out, err := c.run(ctx, nil, checkpointListArgs(taskID)...)
	if err != nil {
		return nil, err
	}
	cps, err := parseCheckpointList(out)
	if err != nil {
		return nil, fmt.Errorf("entire: checkpoints for %q: %w", taskID, err)
	}
	return cps, nil
}

// checkpointCreateArgs builds the argv for a checkpoint creation.
//
// It is a pure function so that argument construction is unit-testable without
// exec, and so the metadata ordering below is pinned by a test: map iteration
// order must not reach a command line, or two identical requests would produce
// different commands.
func checkpointCreateArgs(req model.CheckpointRequest) []string {
	args := []string{"checkpoint", "create", "--json", "--task", req.TaskID}
	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	}
	if req.Label != "" {
		args = append(args, "--label", req.Label)
	}
	if req.Agent != "" {
		args = append(args, "--agent", string(normalizeAgent(req.Agent)))
	}
	if req.Message != "" {
		args = append(args, "--message", req.Message)
	}
	for _, k := range sortedKeys(req.Meta) {
		args = append(args, "--meta", k+"="+req.Meta[k])
	}
	if req.State != nil {
		// "-" tells the binary to read the state document from stdin.
		args = append(args, "--state", "-")
	}
	return args
}

// checkpointShowArgs builds the argv that loads a single checkpoint.
func checkpointShowArgs(id string) []string {
	return []string{"checkpoint", "show", id, "--json"}
}

// checkpointListArgs builds the argv that lists a task's checkpoints.
func checkpointListArgs(taskID string) []string {
	return []string{"checkpoint", "list", "--task", taskID, "--json"}
}

// run executes the binary and returns its stdout.
//
// Every exec failure - binary missing, non-zero exit, deadline exceeded - is
// wrapped in model.ErrUnavailable together with the command that failed, so a
// caller can both branch on the sentinel and tell the user what was attempted.
func (c *cliClient) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = c.dir
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("entire: %s %s: %w: %v%s",
			c.bin, strings.Join(args, " "), model.ErrUnavailable, err, stderrDetail(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// stderrDetail renders a bounded, single-line excerpt of stderr for an error
// message. It is deliberately lossy: enough to diagnose, not enough to spill a
// credential-bearing dump into a log (plan §34).
func stderrDetail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if line, _, found := strings.Cut(s, "\n"); found {
		s = strings.TrimSpace(line) + " [...]"
	}
	if len(s) > stderrLimit {
		s = s[:stderrLimit] + " [...]"
	}
	return " (stderr: " + s + ")"
}
