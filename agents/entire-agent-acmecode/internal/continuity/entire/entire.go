// Package entire implements the port onto Entire, the authoritative
// development-history layer (plan §5). The continuity layer deliberately does
// not become a competing source of truth: milestones are written to, and read
// back from, Entire checkpoints through model.EntireClient.
//
// Two backends are provided:
//
//   - NewCLI shells out to the entire binary and parses its JSON. This is the
//     real history layer.
//   - NewLocal writes one JSON file per checkpoint under a directory. It exists
//     because the entire binary is not installed everywhere the continuity
//     layer runs, and plan §48 forbids making Entire a fatal dependency of the
//     host agent's own work.
//
// Detect chooses between them and returns a note naming the backend actually
// chosen. Every backend's Describe states plainly what is behind it: a local
// fallback must never be mistaken for Entire history, because a user who
// believes they hold authoritative history when they do not has been misled
// about the one guarantee this product sells (plan §5, §48).
package entire

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Detect picks a checkpoint backend and returns a human-readable note naming
// the one it chose.
//
// The CLI wins whenever the binary answers a probe; otherwise the local
// fallback store takes over; if neither can be opened the returned client
// reports ErrUnavailable from every call rather than being nil, because a
// missing Entire must degrade the output, never crash the host agent
// (plan §48, Rule 8).
//
// The note is returned rather than logged because the caller renders it into
// its output: the user has to be told which Entire they are talking to before
// they trust anything it says (plan §5).
func Detect(bin, root, dir string, clock model.Clock) (model.EntireClient, string) {
	cli := NewCLI(bin, dir)

	// A detached background context bounded by probeTimeout: detection happens
	// during start-up, where a hung binary must not stall the host agent.
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	if cli.Available(ctx) {
		return cli, "checkpoint backend: " + cli.Describe()
	}

	local, err := NewLocal(root, clock)
	if err != nil {
		reason := fmt.Sprintf("the entire binary did not answer and the local fallback store could not be opened (%v)", err)
		return unavailableClient{reason: reason}, "checkpoint backend: none - " + reason + "; checkpoint history is unavailable, so safe resume cannot be claimed (plan §48)"
	}
	return local, "checkpoint backend: " + local.Describe() + "; the entire binary did not answer, so no Entire history was read or written"
}

// unavailableClient stands in when no backend could be opened at all. It exists
// so Detect never hands back a nil client: callers record the gap in Capture
// and degrade, they do not fail (plan §48).
type unavailableClient struct{ reason string }

func (c unavailableClient) Available(context.Context) bool { return false }

func (c unavailableClient) CreateCheckpoint(context.Context, model.CheckpointRequest) (model.Checkpoint, error) {
	return model.Checkpoint{}, c.err()
}

func (c unavailableClient) Checkpoint(context.Context, string) (model.Checkpoint, error) {
	return model.Checkpoint{}, c.err()
}

func (c unavailableClient) Checkpoints(context.Context, string) ([]model.Checkpoint, error) {
	return nil, c.err()
}

func (c unavailableClient) Describe() string {
	return "no checkpoint backend (" + c.reason + ")"
}

func (c unavailableClient) err() error {
	return fmt.Errorf("entire: %s: %w", c.reason, model.ErrUnavailable)
}

// sortCheckpoints orders checkpoints oldest-first, as model.EntireClient
// promises. Equal timestamps are broken on id so that two runs over the same
// history produce byte-identical output; a fixed clock in a fixture makes
// ties routine rather than exotic.
func sortCheckpoints(in []model.Checkpoint) {
	sort.SliceStable(in, func(i, j int) bool {
		if !in[i].CreatedAt.Equal(in[j].CreatedAt) {
			return in[i].CreatedAt.Before(in[j].CreatedAt)
		}
		return in[i].ID < in[j].ID
	})
}

// checkStateSchema refuses an EngineeringState this build cannot interpret.
// A newer schema may carry fields whose meaning we would silently drop, and a
// dropped field is exactly the "incomplete context presented as complete" the
// product exists to prevent (plan §33). A version of 0 means the producer never
// stamped one; it is accepted and left at 0 rather than being back-filled,
// because inventing a version is a claim we cannot support.
func checkStateSchema(s *model.EngineeringState) error {
	if s == nil || s.SchemaVersion <= model.SchemaVersion {
		return nil
	}
	return fmt.Errorf("engineering state schema version %d is newer than this build understands (%d)", s.SchemaVersion, model.SchemaVersion)
}

// normalizeAgent keeps any usable agent identity and falls back to
// AgentUnknown only for a value that cannot be one.
//
// The set of runtimes is open (see model.AgentKind.Valid), so a checkpoint
// produced by a runtime this build has never integrated with keeps the name its
// producer gave — that name is a fact about the checkpoint, and discarding it
// would lose the only thing the producer said about itself. A value that is not
// a safe identifier is still reported as unknown rather than rendered as though
// it named a real runtime (plan §12).
func normalizeAgent(a model.AgentKind) model.AgentKind {
	if a.Valid() {
		return a
	}
	return model.AgentUnknown
}

// canonicalState returns a deep copy of s with every collection sorted.
// Copying keeps the caller's state unmutated; sorting makes the persisted bytes
// identical for two runs that reached the same engineering conclusion, which is
// what makes a state hash comparable across sessions.
func canonicalState(s *model.EngineeringState) *model.EngineeringState {
	if s == nil {
		return nil
	}
	out := s.Clone()
	out.Sort()
	return out
}

// sortedKeys returns a map's keys in a stable order. Map iteration order must
// never reach a command line or a file, or two identical runs stop agreeing.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// trimmed is a small readability helper for the many "is this field actually
// set" checks in this package.
func trimmed(s string) string { return strings.TrimSpace(s) }
