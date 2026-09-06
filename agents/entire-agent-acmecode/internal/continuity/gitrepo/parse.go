package gitrepo

import (
	"strconv"
	"strings"
)

// statusEntry is one path reported by git, together with the raw status code
// git used for it. Orig carries the previous path of a rename or copy; it is
// empty otherwise.
type statusEntry struct {
	// Code is git's raw code: the two-column "XY" form for status output, or
	// the "M" / "R100" form for diff --name-status.
	Code string
	Path string
	Orig string
}

// numstat is the line delta git reported for one path.
type numstat struct {
	Insert int
	Delete int
}

// splitZ splits NUL-terminated git output into records.
//
// Every machine-readable git call in this package uses -z. That is not a style
// choice: without it git quotes and escapes paths containing spaces, quotes or
// non-ASCII bytes, and any parser we wrote would silently mangle exactly the
// paths a user is most likely to have trouble with.
func splitZ(out []byte) []string {
	parts := strings.Split(string(out), "\x00")
	records := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			records = append(records, p)
		}
	}
	return records
}

// parseStatusZ parses "git status --porcelain -z".
//
// Each record is "XY<space><path>", and a rename or copy is followed by a second
// record holding the original path. EITHER column can carry R or C: git detects
// renames in the index ("R ", after git mv) and in the working tree (" R", which
// an intent-to-add of the new path is enough to produce). Testing only the index
// column desynchronises the whole stream from that point on, and the original
// path then gets read as if it were a status record — inventing a changed file
// with a truncated path that no git command ever reported.
func parseStatusZ(out []byte) []statusEntry {
	records := splitZ(out)
	entries := make([]statusEntry, 0, len(records))
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			// Not "XY<space><path>": a truncated record is dropped rather than
			// guessed at.
			continue
		}
		e := statusEntry{Code: rec[:2], Path: rec[3:]}
		if isRenameCode(e.Code[0]) || isRenameCode(e.Code[1]) {
			if i+1 < len(records) {
				i++
				e.Orig = records[i]
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// isRenameCode reports whether a status column means "this entry carries an
// original path in the following record": R for a rename, C for a copy.
func isRenameCode(c byte) bool { return c == 'R' || c == 'C' }

// parseNameStatusZ parses "git diff --name-status -z".
//
// With -z the status and the path are separate records, and a rename or copy
// emits three: the score-bearing status, the old path, then the new path.
func parseNameStatusZ(out []byte) []statusEntry {
	records := splitZ(out)
	entries := make([]statusEntry, 0, len(records)/2+1)
	for i := 0; i < len(records); i++ {
		code := records[i]
		i++
		if i >= len(records) {
			// Output ended mid-record: report what we parsed rather than
			// inventing a path for the dangling status.
			break
		}
		e := statusEntry{Code: code, Path: records[i]}
		if isRenameCode(code[0]) {
			i++
			if i >= len(records) {
				break
			}
			e.Orig = e.Path
			e.Path = records[i]
		}
		entries = append(entries, e)
	}
	return entries
}

// parseNumstatZ parses "git diff --numstat -z" into per-path line deltas.
//
// A normal record is "<add>\t<del>\t<path>". For a rename or copy the record
// ends after the second tab and the old and new paths follow as their own
// records. Binary files report "-" for both counts; they become zero because we
// genuinely do not know a line count for them, and the file still appears in the
// changed-file list on its own merit.
func parseNumstatZ(out []byte) map[string]numstat {
	records := splitZ(out)
	counts := make(map[string]numstat, len(records))
	for i := 0; i < len(records); i++ {
		fields := strings.SplitN(records[i], "\t", 3)
		if len(fields) < 3 {
			continue
		}
		path := fields[2]
		if path == "" {
			// Rename or copy: old path, then new path.
			if i+2 >= len(records) {
				break
			}
			i += 2
			path = records[i]
		}
		counts[path] = numstat{Insert: parseCount(fields[0]), Delete: parseCount(fields[1])}
	}
	return counts
}

// parseCount reads one numstat column. "-" marks a binary file and anything
// unparseable is treated the same way: zero, meaning "not reported".
func parseCount(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// worktreeStatus maps a two-column porcelain code onto model.ChangedFile.Status.
//
// "??" is untracked. It becomes "A" rather than "?" because we do know what
// happened — the file is new in the tree — and "?" is reserved by the contract
// for "unknown". Otherwise the index column wins when it says something, since
// that is the change closest to being committed.
func worktreeStatus(code string) string {
	if len(code) < 2 {
		return "?"
	}
	if code == "??" {
		return "A"
	}
	for _, c := range []byte{code[0], code[1]} {
		if c == ' ' || c == '?' {
			continue
		}
		return diffStatus(string(c))
	}
	return "?"
}

// diffStatus maps a git diff status letter onto model.ChangedFile.Status, whose
// contract allows M, A, D, R or "?" for unknown.
//
// The letter may carry a similarity score ("R100"), which we drop. A copy
// becomes "A": the destination path really is new, and there is no copy code in
// the contract. A typechange becomes "M": from a reader's point of view the file
// at that path changed. Anything else — notably "U" for an unmerged conflict —
// becomes "?" rather than being forced into a code that would overstate what we
// know.
func diffStatus(raw string) string {
	if raw == "" {
		return "?"
	}
	switch raw[0] {
	case 'M', 'A', 'D', 'R':
		return raw[:1]
	case 'C':
		return "A"
	case 'T':
		return "M"
	default:
		return "?"
	}
}

// normalizeRemote reduces a git remote URL to "owner/repo".
//
// It accepts every shape git itself accepts: https URLs, ssh URLs, scp-style
// "git@host:owner/repo", and plain filesystem paths (a local clone is still a
// repository with an identity). Userinfo is dropped before anything else, which
// also means a token embedded in a remote URL never reaches persisted state
// (plan §34: do not create a new secret leak path).
//
// The last two path segments are used, so a nested GitLab group collapses to its
// final owner/repo pair. That is a deliberate trade: task identity has to stay
// short and stable, and the tail is the part humans recognise.
func normalizeRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		s = stripUserinfo(s)
		switch {
		case strings.HasPrefix(s, "/"):
			// file:// URL: the authority is empty, so there is no host segment
			// to drop and the whole remainder is already the path.
		case strings.Contains(s, "/"):
			s = s[strings.Index(s, "/")+1:] // drop the host
		default:
			s = ""
		}
	} else if i := strings.Index(s, ":"); i > 1 {
		// scp-style "host:path" or "user@host:path". The i > 1 guard keeps a
		// Windows drive letter ("C:\repos\thing") from being read as a host.
		s = s[i+1:]
	}

	s = strings.ReplaceAll(s, "\\", "/")
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimRight(s, "/")

	segments := make([]string, 0, 4)
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." {
			continue
		}
		segments = append(segments, p)
	}
	switch len(segments) {
	case 0:
		return ""
	case 1:
		return segments[0]
	default:
		return segments[len(segments)-2] + "/" + segments[len(segments)-1]
	}
}

// stripUserinfo removes "user:token@" from the authority section only. The "@"
// is looked for before the first "/" so an "@" inside the repository path is
// left alone.
func stripUserinfo(s string) string {
	authority := s
	if slash := strings.Index(s, "/"); slash >= 0 {
		authority = s[:slash]
	}
	if at := strings.Index(authority, "@"); at >= 0 {
		return s[at+1:]
	}
	return s
}

// baseName returns the final segment of a path, tolerating either separator.
// git reports its top level with forward slashes even on Windows, so we cannot
// rely on filepath alone.
func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}
