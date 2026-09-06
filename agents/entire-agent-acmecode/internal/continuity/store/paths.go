package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

const (
	// dirPerm and filePerm keep the store owner-only. Task state carries
	// summaries of real engineering work and, through PayloadRef, pointers into
	// an agent's transcripts; plan §34 says that material must not be casually
	// readable by every user on the machine.
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600

	// maxIDLen bounds an externally supplied identifier. An id long enough to
	// blow the platform path limit would fail partway through a multi-file
	// write, which is exactly the half-applied state atomic writes exist to
	// prevent, so it is rejected up front instead.
	maxIDLen = 128
)

// windowsReserved lists the DOS device names. Windows resolves these to devices
// in every directory, so a session id of "aux" would open a device rather than
// create a file. This store is developed and demoed on Windows, so the check is
// not theoretical.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// validID reports why id cannot be used as a path segment, or nil when it can.
//
// Task, session, checkpoint and handoff ids all arrive from external agents and
// all become directory or file names, so this is a strict allowlist rather than
// a blocklist of known-bad sequences: ASCII letters, digits, '-', '_' and an
// interior '.'. That rules out '/', '\', ':', NUL, "." and ".." in a single
// pass, with no ordering dependency between unescaping and normalisation.
//
// Two deliberate choices:
//
//   - Ids are rejected, never rewritten. Rewriting "a/b" into "a_b" would mint a
//     second identity for one session, and a product whose whole purpose is to
//     keep one task's identity stable across agents must not quietly do that.
//   - Non-ASCII is rejected. macOS and Windows normalise Unicode filenames, so
//     two ids that differ as strings can land on one file and silently merge two
//     sessions' lineage.
func validID(id string) error {
	if id == "" {
		return errors.New("identifier is empty")
	}
	if len(id) > maxIDLen {
		return fmt.Errorf("identifier is %d bytes, limit is %d", len(id), maxIDLen)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return fmt.Errorf("identifier %q contains a disallowed byte at offset %d", id, i)
		}
	}
	if id[0] == '.' {
		// Covers "." and ".." outright, and keeps ids out of the dotfile space
		// this package uses for its own in-flight temp files.
		return fmt.Errorf("identifier %q starts with a dot", id)
	}
	if id[len(id)-1] == '.' {
		// Windows silently strips trailing dots, so "abc." and "abc" would alias.
		return fmt.Errorf("identifier %q ends with a dot", id)
	}
	base := id
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return fmt.Errorf("identifier %q is a reserved device name", id)
	}
	return nil
}

// join builds a path under base from segments that have already passed validID
// and then re-checks containment.
//
// The check is redundant if validID is correct. It is here because "the id
// validator had a bug" must never be allowed to mean "an external agent wrote
// outside the store root".
func join(base string, segs ...string) (string, error) {
	p := filepath.Join(append([]string{base}, segs...)...)
	if !contained(base, p) {
		return "", fmt.Errorf("store: path %q escapes root %q", p, base)
	}
	return p, nil
}

// contained reports whether p is base itself or lies beneath it.
func contained(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// repoBranchKey is the index filename for a repository and branch pair. It is a
// hash rather than the literal pair because a branch name legitimately contains
// '/' and would otherwise become a directory tree, and because hashing settles
// any question of a crafted branch name escaping the index directory.
func repoBranchKey(repo, branch string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + branch))
	return hex.EncodeToString(sum[:])
}

// writeAtomic replaces path with data, or leaves the previous contents intact.
//
// The temp file is created in the destination directory so the rename is a
// same-filesystem metadata operation. Without this, a crash mid-write could
// leave a truncated task.json or a torn final line in events.ndjson, and a torn
// event line is unreadable evidence, which is worse than no evidence at all.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: create temp in %q: %w", dir, err)
	}
	name := tmp.Name()
	// Clears the temp file on every failure path. After a successful rename the
	// name no longer exists and the removal is a harmless no-op.
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: write %q: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: sync %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close %q: %w", name, err)
	}
	if err := os.Chmod(name, filePerm); err != nil {
		return fmt.Errorf("store: chmod %q: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("store: rename onto %q: %w", path, err)
	}
	return nil
}

// writeJSON stores v as an indented JSON document with a trailing newline.
// Indented rather than compact because a human reads these files directly when
// investigating a handoff; encoding/json sorts map keys, so the bytes stay
// identical across runs.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode %q: %w", filepath.Base(path), err)
	}
	return writeAtomic(path, append(b, '\n'))
}

// readJSON decodes a stored document. A missing file is reported as
// model.ErrNotFound so callers can tell "absent" from "unreadable": the product
// degrades honestly on absence, but must never treat corruption as absence
// (plan §48).
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return model.ErrNotFound
		}
		return fmt.Errorf("store: read %q: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("store: decode %q: %w", filepath.Base(path), err)
	}
	return nil
}

// readNodes decodes every *.json document in dir, in filename order. A missing
// directory yields no nodes and no error, because "this task has no checkpoints
// yet" is a fact about the task rather than a failure of the store.
//
// Non-JSON entries are skipped, which is also what discards a ".tmp-*" file left
// behind by a process that died mid-write: a partial write can never be mistaken
// for a lineage node.
func readNodes[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read dir %q: %w", dir, err)
	}
	out := make([]T, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var v T
		if err := readJSON(filepath.Join(dir, e.Name()), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
