package entire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

const (
	// checkpointsDir is the subdirectory of the store root that holds one JSON
	// file per checkpoint.
	checkpointsDir = "checkpoints"

	// localRecordVersion versions the on-disk record. It is separate from
	// model.SchemaVersion, which versions the EngineeringState the record
	// carries: the envelope and its payload evolve independently.
	localRecordVersion = 1

	// backendLocal is stamped into every record so a file found on disk can
	// never be mistaken for exported Entire history (plan §5).
	backendLocal = "local-fallback"
)

const (
	// metaMessage carries CheckpointRequest.Message onto the returned
	// checkpoint. model.Checkpoint has no Message field, and dropping the
	// message would discard the operator's own account of the milestone, so it
	// is surfaced under this metadata key instead. It is only injected when the
	// caller has not already used the key.
	metaMessage = "message"

	// metaCommitSHA and metaBranch are the request metadata keys the local
	// backend lifts onto the typed Checkpoint fields. CheckpointRequest carries
	// neither of its own, and a checkpoint that cannot cite a commit is much
	// weaker evidence (plan §11, §31), so callers may supply them through Meta.
	metaCommitSHA = "commit_sha"
	metaBranch    = "branch"
)

// checkpointRecord is the on-disk form of a locally stored checkpoint. It is a
// superset of model.Checkpoint so that a round trip through the filesystem
// loses nothing the caller handed us.
type checkpointRecord struct {
	RecordVersion int    `json:"record_version"`
	Backend       string `json:"backend"`

	ID        string                  `json:"id"`
	TaskID    string                  `json:"task_id"`
	SessionID string                  `json:"session_id,omitempty"`
	Label     string                  `json:"label,omitempty"`
	Message   string                  `json:"message,omitempty"`
	Agent     model.AgentKind         `json:"agent,omitempty"`
	CommitSHA string                  `json:"commit_sha,omitempty"`
	Branch    string                  `json:"branch,omitempty"`
	CreatedAt time.Time               `json:"created_at"`
	Meta      map[string]string       `json:"meta,omitempty"`
	State     *model.EngineeringState `json:"state,omitempty"`
}

// checkpoint projects the record onto the port's type. Both CreateCheckpoint
// and the read path go through this one function, so what a caller gets back
// from a write is identical to what it later reads from disk.
func (r checkpointRecord) checkpoint() model.Checkpoint {
	meta := make(map[string]string, len(r.Meta)+1)
	for k, v := range r.Meta {
		meta[k] = v
	}
	if _, taken := meta[metaMessage]; !taken && r.Message != "" {
		meta[metaMessage] = r.Message
	}
	if len(meta) == 0 {
		meta = nil
	}
	return model.Checkpoint{
		ID:        r.ID,
		SessionID: r.SessionID,
		TaskID:    r.TaskID,
		Label:     r.Label,
		CommitSHA: r.CommitSHA,
		Branch:    r.Branch,
		CreatedAt: r.CreatedAt.UTC(),
		Agent:     normalizeAgent(r.Agent),
		Meta:      meta,
		State:     r.State,
	}
}

// localClient stores checkpoints as JSON files under root/checkpoints.
type localClient struct {
	root  string
	dir   string
	clock model.Clock
}

// NewLocal opens a fallback checkpoint store rooted at root, creating
// root/checkpoints if needed. It reports an error when that location cannot be
// used, so Detect can say so plainly instead of failing later on first write.
//
// clock supplies every CreatedAt; a nil clock falls back to model.SystemClock.
// Nothing in this package calls time.Now directly, because the checkpoint id is
// derived from the timestamp and a fixture must be able to reproduce it exactly.
func NewLocal(root string, clock model.Clock) (model.EntireClient, error) {
	if trimmed(root) == "" {
		return nil, errors.New("entire: local store root must not be empty")
	}
	if clock == nil {
		clock = model.SystemClock{}
	}
	dir := filepath.Join(root, checkpointsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("entire: local store at %s is unusable: %w", root, err)
	}
	return &localClient{root: root, dir: dir, clock: clock}, nil
}

// Describe names this backend for exactly what it is. It must never read as
// Entire: the fallback holds local files that no other machine, agent or human
// can independently verify, and presenting them as authoritative history would
// be the single most damaging lie this package could tell (plan §5, §48).
func (c *localClient) Describe() string {
	return fmt.Sprintf("local fallback checkpoint store at %s (not Entire: these are local files, no history layer was contacted)", c.root)
}

// Available reports whether the store directory is still there. The local
// backend has no service to reach, so this is a filesystem question.
func (c *localClient) Available(ctx context.Context) bool {
	if err := ctxErr(ctx); err != nil {
		return false
	}
	return c.storeUnavailable() == nil
}

// storeUnavailable reports the store being gone or no longer a directory,
// wrapped in model.ErrUnavailable; it returns nil while the store is usable.
//
// It exists so the two failures stay separable. "This checkpoint is not here"
// is a fact about a working store; "the store is not here" is a fact about the
// backend, and only the second permits a caller to keep going while recording
// that it never got to look (plan §48). Reporting the second as the first would
// dress an unanswered question up as a definitive no.
func (c *localClient) storeUnavailable() error {
	info, err := os.Stat(c.dir)
	switch {
	case err != nil:
		return fmt.Errorf("local checkpoint store at %s is not readable (%v): %w", c.root, err, model.ErrUnavailable)
	case !info.IsDir():
		return fmt.Errorf("local checkpoint store at %s is not a directory: %w", c.root, model.ErrUnavailable)
	}
	return nil
}

// CreateCheckpoint records a milestone as a single JSON file.
//
// The id is derived from the request rather than allocated, so the same
// milestone recorded from the same inputs always lands on the same id. An
// existing file for that id is never overwritten: checkpoint history is
// append-only, and silently replacing a record would destroy the evidence a
// later reader is meant to be able to cite (plan §32).
func (c *localClient) CreateCheckpoint(ctx context.Context, req model.CheckpointRequest) (model.Checkpoint, error) {
	if err := ctxErr(ctx); err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: %w", err)
	}
	if trimmed(req.TaskID) == "" {
		return model.Checkpoint{}, errors.New("entire: create checkpoint: task id must not be empty")
	}
	if err := checkStateSchema(req.State); err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: %w", err)
	}

	createdAt := c.clock.Now().UTC()
	rec := checkpointRecord{
		RecordVersion: localRecordVersion,
		Backend:       backendLocal,
		ID:            checkpointID(req.TaskID, req.SessionID, req.Label, createdAt),
		TaskID:        req.TaskID,
		SessionID:     req.SessionID,
		Label:         req.Label,
		Message:       req.Message,
		Agent:         normalizeAgent(req.Agent),
		CommitSHA:     req.Meta[metaCommitSHA],
		Branch:        req.Meta[metaBranch],
		CreatedAt:     createdAt,
		Meta:          copyMeta(req.Meta),
		State:         canonicalState(req.State),
	}

	name, ok := checkpointFileName(rec.ID)
	if !ok {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: derived id %q is not a usable filename", rec.ID)
	}
	path := filepath.Join(c.dir, name)
	if _, err := os.Stat(path); err == nil {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint: %s already exists (same task, session, label and timestamp); refusing to overwrite recorded history", rec.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint %s: %w", rec.ID, err)
	}

	if err := writeJSONAtomic(c.dir, name, rec); err != nil {
		if gone := c.storeUnavailable(); gone != nil {
			return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint %s: %v: %w", rec.ID, err, gone)
		}
		return model.Checkpoint{}, fmt.Errorf("entire: create checkpoint %s: %w", rec.ID, err)
	}
	return rec.checkpoint(), nil
}

// Checkpoint loads one checkpoint by id, reporting model.ErrNotFound when it is
// simply absent. Absent and unreachable are kept distinct because a caller may
// legitimately continue past the first and must degrade on the second: saying
// "that checkpoint does not exist" when the store itself has vanished would be
// a certainty we do not hold (plan §48).
func (c *localClient) Checkpoint(ctx context.Context, id string) (model.Checkpoint, error) {
	if err := ctxErr(ctx); err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: %w", id, err)
	}
	name, ok := checkpointFileName(id)
	if !ok {
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q is not a usable identifier: %w", id, model.ErrNotFound)
	}
	b, err := os.ReadFile(filepath.Join(c.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		// "Missing file" only means "no such checkpoint" while the store is
		// still there to be missing it from.
		if gone := c.storeUnavailable(); gone != nil {
			return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: %w", id, gone)
		}
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: %w", id, model.ErrNotFound)
	}
	if err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: reading checkpoint %q: %w", id, err)
	}
	rec, err := decodeRecord(b)
	if err != nil {
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: %w", id, err)
	}
	if rec.ID != id {
		// Ids are sanitised onto a path segment, so two different ids can land
		// on one file. Returning the occupant would hand the caller a
		// checkpoint it never asked for and let it be cited as evidence for the
		// id it did ask for (plan §11). The requested id genuinely is not here.
		return model.Checkpoint{}, fmt.Errorf("entire: checkpoint %q: the record stored at %s identifies itself as %q: %w",
			id, name, rec.ID, model.ErrNotFound)
	}
	return rec.checkpoint(), nil
}

// Checkpoints lists a task's checkpoints oldest-first.
//
// Files that cannot be decoded are reported: the successfully decoded
// checkpoints are returned *together with* a non-nil error naming the ones that
// were skipped. Silently dropping an unreadable record would present a
// truncated history as a complete one, which is the failure mode plan §33 and
// §48 exist to prevent. A caller that wants to degrade may use the partial
// slice; a caller that wants certainty must treat the error as fatal.
func (c *localClient) Checkpoints(ctx context.Context, taskID string) ([]model.Checkpoint, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, fmt.Errorf("entire: checkpoints for %q: %w", taskID, err)
	}
	if trimmed(taskID) == "" {
		// Matching everything on an empty id would attribute unrelated history
		// to the task, which is fabrication by omission.
		return nil, errors.New("entire: checkpoints: task id must not be empty")
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		// The store could not be listed at all, so we cannot say anything about
		// this task's history. That is unavailability, and it must carry the
		// sentinel or the caller cannot record the gap in Capture (plan §48).
		return nil, fmt.Errorf("entire: local store at %s: %v: %w", c.root, err, model.ErrUnavailable)
	}

	var out []model.Checkpoint
	var unreadable []string
	for _, e := range entries {
		// Temp files from an interrupted write do not end in .json, so this
		// also skips them.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			unreadable = append(unreadable, e.Name())
			continue
		}
		rec, err := decodeRecord(b)
		if err != nil {
			unreadable = append(unreadable, e.Name())
			continue
		}
		if rec.TaskID != taskID {
			continue
		}
		out = append(out, rec.checkpoint())
	}
	sortCheckpoints(out)

	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		return out, fmt.Errorf("entire: local store at %s: %d checkpoint file(s) could not be read (%s); the returned history is incomplete",
			c.root, len(unreadable), strings.Join(unreadable, ", "))
	}
	return out, nil
}

// checkpointID derives the deterministic identifier of a local checkpoint from
// the facts that identify the milestone. Deriving rather than allocating is
// what lets a replayed capture recognise a checkpoint it already wrote instead
// of forking history into a near-duplicate.
func checkpointID(taskID, sessionID, label string, createdAt time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		taskID,
		sessionID,
		label,
		createdAt.UTC().Format(time.RFC3339Nano),
	}, "|")))
	return "cp_" + hex.EncodeToString(sum[:])[:12]
}

// checkpointFileName maps an id onto the file that stores it.
func checkpointFileName(id string) (string, bool) {
	seg, ok := sanitiseSegment(id)
	if !ok {
		return "", false
	}
	return seg + ".json", true
}

// windowsReservedNames are the legacy device names Windows refuses to use as a
// filename. This package is developed and run on Windows, so an id colliding
// with one has to be handled rather than discovered as a mystery error.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitiseSegment reduces an identifier to one safe path segment.
//
// Ids reach this package from agent adapters and from checkpoint payloads, i.e.
// from outside the process. An id such as "../../.ssh/config" must not be able
// to steer a read or a write out of the store root, so everything outside a
// conservative allow-list is replaced. The mapping is deterministic and total:
// the same id always resolves to the same file, including when it had to be
// shortened, so a sanitised id is still round-trippable.
func sanitiseSegment(id string) (string, bool) {
	const maxLen = 96

	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()

	// "." and ".." survive the allow-list but are directory references, not
	// names. Anything that is only dots is rejected outright.
	if out == "" || strings.Trim(out, ".") == "" {
		return "", false
	}
	if len(out) > maxLen {
		// Truncation alone would let two distinct long ids collide onto one
		// file, so the discarded tail is replaced by a fingerprint of the whole
		// original id. Still deterministic, no longer lossy in practice.
		sum := sha256.Sum256([]byte(id))
		out = out[:maxLen-13] + "-" + hex.EncodeToString(sum[:])[:12]
	}
	if base, _, _ := strings.Cut(out, "."); windowsReservedNames[strings.ToUpper(base)] {
		out = "_" + out
	}
	return out, true
}

// decodeRecord parses one on-disk record and rejects anything this build cannot
// honestly interpret.
func decodeRecord(b []byte) (checkpointRecord, error) {
	var rec checkpointRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return checkpointRecord{}, fmt.Errorf("malformed checkpoint record: %w", err)
	}
	if rec.RecordVersion > localRecordVersion {
		return checkpointRecord{}, fmt.Errorf("checkpoint record version %d is newer than this build understands (%d)", rec.RecordVersion, localRecordVersion)
	}
	if rec.ID == "" {
		// A checkpoint that cannot name itself cannot be cited as evidence.
		return checkpointRecord{}, errors.New("checkpoint record has no id")
	}
	if rec.CreatedAt.IsZero() {
		// Without a timestamp the record cannot be ordered against its
		// siblings, and "oldest first" would become a guess.
		return checkpointRecord{}, fmt.Errorf("checkpoint record %q has no created_at", rec.ID)
	}
	if err := checkStateSchema(rec.State); err != nil {
		return checkpointRecord{}, fmt.Errorf("checkpoint record %q: %w", rec.ID, err)
	}
	return rec, nil
}

// writeJSONAtomic writes v into dir/name via a temp file and a rename, so a
// crash mid-write leaves the previous state intact rather than a half-written
// checkpoint that later reads as corrupt history.
func writeJSONAtomic(dir, name string, v any) error {
	// MarshalIndent keeps records readable by a human inspecting the store, and
	// encoding/json sorts map keys, so metadata ordering cannot vary run to run.
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding record: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Harmless once the rename has succeeded; the cleanup that matters is the
	// failure path.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// copyMeta detaches the caller's metadata map, so a later mutation on their
// side cannot change what we recorded.
func copyMeta(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ctxErr reports cancellation, tolerating a nil context so a caller that has
// none cannot panic its way through the history layer.
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
