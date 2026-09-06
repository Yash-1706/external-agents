package gitrepo

import (
	"reflect"
	"strings"
	"testing"
)

// These tests run against raw git output rather than a live repository, which is
// how the awkward shapes (renames, binary files, truncated streams, paths with
// spaces) can be covered deterministically on any machine.

func TestParseStatusZ(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []statusEntry
	}{
		{
			name: "empty output is no entries, not an error",
			in:   "",
			want: []statusEntry{},
		},
		{
			name: "modified staged and untracked",
			in:   " M a.go\x00M  b.go\x00?? c.txt\x00",
			want: []statusEntry{
				{Code: " M", Path: "a.go"},
				{Code: "M ", Path: "b.go"},
				{Code: "??", Path: "c.txt"},
			},
		},
		{
			name: "rename consumes the original path record",
			in:   "R  new.go\x00old.go\x00 M other.go\x00",
			want: []statusEntry{
				{Code: "R ", Path: "new.go", Orig: "old.go"},
				{Code: " M", Path: "other.go"},
			},
		},
		{
			name: "copy consumes the original path record",
			in:   "C  copy.go\x00src.go\x00",
			want: []statusEntry{{Code: "C ", Path: "copy.go", Orig: "src.go"}},
		},
		{
			// git detects renames in the working tree as well as in the index,
			// and reports them in the second column (" R"). Consuming the extra
			// record only for an index rename desynchronises the stream and the
			// original path is then read as a status record of its own, which
			// invents a changed file git never reported.
			name: "working tree rename also consumes the original path record",
			in:   " R new.go\x00original.go\x00 M other.go\x00",
			want: []statusEntry{
				{Code: " R", Path: "new.go", Orig: "original.go"},
				{Code: " M", Path: "other.go"},
			},
		},
		{
			name: "working tree copy also consumes the original path record",
			in:   " C copy.go\x00source.go\x00 M other.go\x00",
			want: []statusEntry{
				{Code: " C", Path: "copy.go", Orig: "source.go"},
				{Code: " M", Path: "other.go"},
			},
		},
		{
			name: "index rename with a further working tree edit consumes exactly one record",
			in:   "RM new.go\x00original.go\x00 M other.go\x00",
			want: []statusEntry{
				{Code: "RM", Path: "new.go", Orig: "original.go"},
				{Code: " M", Path: "other.go"},
			},
		},
		{
			name: "paths containing spaces survive because of -z",
			in:   " M dir/a file.go\x00?? my notes.md\x00",
			want: []statusEntry{
				{Code: " M", Path: "dir/a file.go"},
				{Code: "??", Path: "my notes.md"},
			},
		},
		{
			name: "truncated record is dropped rather than guessed at",
			in:   "M\x00 M ok.go\x00",
			want: []statusEntry{{Code: " M", Path: "ok.go"}},
		},
		{
			name: "rename with no following record does not invent an original",
			in:   "R  new.go\x00",
			want: []statusEntry{{Code: "R ", Path: "new.go"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatusZ([]byte(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseStatusZ(%q)\n got %#v\nwant %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseNameStatusZ(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []statusEntry
	}{
		{
			name: "empty output is no entries",
			in:   "",
			want: []statusEntry{},
		},
		{
			name: "plain statuses",
			in:   "M\x00a.go\x00A\x00b.go\x00D\x00c.go\x00",
			want: []statusEntry{
				{Code: "M", Path: "a.go"},
				{Code: "A", Path: "b.go"},
				{Code: "D", Path: "c.go"},
			},
		},
		{
			name: "rename carries a score and three records",
			in:   "R100\x00old.go\x00new.go\x00M\x00after.go\x00",
			want: []statusEntry{
				{Code: "R100", Path: "new.go", Orig: "old.go"},
				{Code: "M", Path: "after.go"},
			},
		},
		{
			name: "dangling status without a path is dropped",
			in:   "M\x00a.go\x00A\x00",
			want: []statusEntry{{Code: "M", Path: "a.go"}},
		},
		{
			name: "truncated rename is dropped rather than half reported",
			in:   "R100\x00old.go\x00",
			want: []statusEntry{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNameStatusZ([]byte(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNameStatusZ(%q)\n got %#v\nwant %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseNumstatZ(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]numstat
	}{
		{
			name: "empty output",
			in:   "",
			want: map[string]numstat{},
		},
		{
			name: "counts and binary files",
			in:   "1\t2\ta.go\x00-\t-\timg.png\x00",
			want: map[string]numstat{
				"a.go": {Insert: 1, Delete: 2},
				// A binary file has no line count. Zero here means "not
				// reported"; the file still appears in the changed list.
				"img.png": {},
			},
		},
		{
			name: "rename keys the counts on the destination path",
			in:   "3\t0\t\x00old.go\x00new.go\x00",
			want: map[string]numstat{"new.go": {Insert: 3}},
		},
		{
			name: "truncated rename contributes nothing",
			in:   "3\t0\t\x00old.go\x00",
			want: map[string]numstat{},
		},
		{
			name: "malformed record is skipped",
			in:   "garbage\x005\t5\tok.go\x00",
			want: map[string]numstat{"ok.go": {Insert: 5, Delete: 5}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNumstatZ([]byte(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNumstatZ(%q)\n got %#v\nwant %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestWorktreeStatus(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{"unstaged modification", " M", "M"},
		{"staged modification", "M ", "M"},
		{"staged and unstaged modification", "MM", "M"},
		{"untracked counts as added, not unknown", "??", "A"},
		{"staged add", "A ", "A"},
		{"deleted in the working tree", " D", "D"},
		{"rename", "R ", "R"},
		{"working tree rename", " R", "R"},
		{"copy reports the destination as added", "C ", "A"},
		{"working tree copy reports the destination as added", " C", "A"},
		{"typechange reads as a modification", " T", "M"},
		{"unmerged conflict is honestly unknown", "UU", "?"},
		{"index wins when both columns speak", "AD", "A"},
		{"short code is unknown", "", "?"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := worktreeStatus(tc.code); got != tc.want {
				t.Fatalf("worktreeStatus(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestDiffStatus(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{"modified", "M", "M"},
		{"added", "A", "A"},
		{"deleted", "D", "D"},
		{"rename keeps only the letter", "R100", "R"},
		{"copy becomes added", "C75", "A"},
		{"typechange becomes modified", "T", "M"},
		{"unmerged is unknown", "U", "?"},
		{"unrecognised letter is unknown", "X", "?"},
		{"empty is unknown", "", "?"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffStatus(tc.code); got != tc.want {
				t.Fatalf("diffStatus(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestNormalizeRemote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/acme/widget.git", "acme/widget"},
		{"https without suffix", "https://github.com/acme/widget", "acme/widget"},
		{"https with trailing slash", "https://github.com/acme/widget.git/", "acme/widget"},
		{"scp style", "git@github.com:acme/widget.git", "acme/widget"},
		{"scp style without suffix", "git@github.com:acme/widget", "acme/widget"},
		{"ssh url", "ssh://git@github.com/acme/widget.git", "acme/widget"},
		{"nested groups collapse to the tail", "ssh://git@gitlab.com/group/sub/widget.git", "sub/widget"},
		{"file url keeps its leading path segment", "file:///srv/repos/widget.git", "repos/widget"},
		{"local path", "/srv/git/acme/widget.git", "acme/widget"},
		{"windows path is not read as a host", `C:\repos\widget`, "repos/widget"},
		{"single segment", "widget.git", "widget"},
		{"empty", "", ""},
		{"whitespace only", "   \n", ""},
		{"trailing newline from git config", "git@github.com:acme/widget.git\n", "acme/widget"},
		{"at sign inside the path is preserved", "https://github.com/acme/wid@get.git", "acme/wid@get"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRemote(tc.in); got != tc.want {
				t.Fatalf("normalizeRemote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Credentials embedded in a remote URL must never survive into the repository
// name, because that name is persisted in every serialized state (plan §34).
func TestNormalizeRemoteDropsCredentials(t *testing.T) {
	tests := []string{
		"https://x-token:s3cr3t@github.com/acme/widget.git",
		"https://s3cr3t@github.com/acme/widget.git",
		"ssh://s3cr3t@github.com/acme/widget.git",
	}

	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			got := normalizeRemote(in)
			if got != "acme/widget" {
				t.Fatalf("normalizeRemote(%q) = %q, want %q", in, got, "acme/widget")
			}
			if strings.Contains(got, "s3cr3t") {
				t.Fatalf("normalizeRemote(%q) leaked the credential: %q", in, got)
			}
		})
	}
}

func TestBaseName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"posix", "/home/me/projects/continuity", "continuity"},
		{"git style windows top level", "D:/work/continuity", "continuity"},
		{"native windows separators", `D:\work\continuity`, "continuity"},
		{"trailing separator", "/home/me/continuity/", "continuity"},
		{"no separator", "continuity", "continuity"},
		{"empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := baseName(tc.in); got != tc.want {
				t.Fatalf("baseName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitZDropsEmptyRecords(t *testing.T) {
	got := splitZ([]byte("a\x00\x00b\x00"))
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitZ = %#v, want %#v", got, want)
	}
}
