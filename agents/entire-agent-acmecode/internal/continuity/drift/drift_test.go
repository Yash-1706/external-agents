package drift

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// fakeRepo is a hand-written in-memory model.RepoInspector. Drift detection must
// be testable without a git working tree, and every degradation path (missing
// file, unavailable capability, hard error) has to be reachable on demand.
type fakeRepo struct {
	head    model.RepoState
	headErr error
	// files maps a working-tree path to the fingerprint FileHash returns. A path
	// absent from this map does not exist as far as the repository is concerned.
	files map[string]string
	// hashErr forces FileHash to fail for a specific path.
	hashErr map[string]error

	changedFilesCalled bool
}

func (f *fakeRepo) Head(context.Context) (model.RepoState, error) {
	if f.headErr != nil {
		return model.RepoState{}, f.headErr
	}
	return f.head, nil
}

func (f *fakeRepo) ChangedFiles(context.Context, string) ([]model.ChangedFile, error) {
	// Detect must derive its file set from the historical state, not from the
	// live diff, so this port is expected to stay untouched.
	f.changedFilesCalled = true
	return nil, model.ErrUnavailable
}

func (f *fakeRepo) FileHash(_ context.Context, path string) (string, error) {
	if err, ok := f.hashErr[path]; ok {
		return "", err
	}
	h, ok := f.files[path]
	if !ok {
		return "", fmt.Errorf("hash %s: %w", path, model.ErrNotFound)
	}
	return h, nil
}

func (f *fakeRepo) Exists(_ context.Context, path string) bool {
	_, ok := f.files[path]
	return ok
}

const (
	histSHA = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
	nextSHA = "aabbccddeeff00112233445566778899aabbccdd"
)

func newState(commit string) *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{ID: "T1", Title: "webhook retries"})
	s.Repo = model.RepoState{Repo: "acme/api", Branch: "main", CommitSHA: commit}
	return s
}

// fileEv builds the file evidence shape the derive package emits: the content
// fingerprint travels in Detail as "sha256:...". Pass an empty hash for a
// citation that records no fingerprint.
func fileEv(path, hash string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceFile, Ref: path, Path: path, Detail: hash}
}

func changed(path, status, hash string) model.ChangedFile {
	cf := model.ChangedFile{Path: path, Status: status}
	if hash != "" {
		cf.Evidence = []model.Evidence{fileEv(path, hash)}
	} else {
		cf.Evidence = []model.Evidence{{Kind: model.EvidenceFile, Ref: path, Path: path, Detail: "touched during the task"}}
	}
	return cf
}

// keys renders findings as "path|kind" so a table can assert on them compactly.
func keys(in []Finding) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, f.Path+"|"+f.Kind)
	}
	return out
}

func hasNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

func TestDetect(t *testing.T) {
	webhooks := "internal/webhooks.go"
	retry := "internal/retry.go"

	tests := []struct {
		name         string
		state        func() *model.EngineeringState
		repo         *fakeRepo
		want         Outcome
		wantFindings []string
		wantChecked  int
		wantNotes    []string
		wantNoNotes  bool
	}{
		{
			name: "clean repository matches the captured state",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:aaaa"}},
			want:        Matches,
			wantChecked: 1,
			wantNoNotes: true,
		},
		{
			name:        "nothing recorded to compare but the commit still matches",
			state:       func() *model.EngineeringState { return newState(histSHA) },
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}},
			want:        Matches,
			wantChecked: 0,
			wantNoNotes: true,
		},
		{
			name: "abbreviated historical sha is the same commit",
			state: func() *model.EngineeringState {
				return newState(histSHA[:8])
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}},
			want:        Matches,
			wantNoNotes: true,
		},
		{
			name:         "head moved since capture",
			state:        func() *model.EngineeringState { return newState(histSHA) },
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: nextSHA}},
			want:         Drifted,
			wantFindings: []string{"|" + kindCommit},
		},
		{
			name: "tracked file content changed with nothing depending on it",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb"}},
			want:         Drifted,
			wantChecked:  1,
			wantFindings: []string{webhooks + "|" + kindContent},
		},
		{
			name: "changed file is cited by an active decision",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				s.Decisions = []model.Decision{{
					ID:       "D1",
					Decision: "retry queue is per tenant",
					Evidence: []model.Evidence{fileEv(webhooks, "")},
				}}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb"}},
			want:         StaleDecision,
			wantChecked:  1,
			wantFindings: []string{webhooks + "|" + kindDecision, webhooks + "|" + kindContent},
		},
		{
			name: "superseded decision cannot go stale",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				s.Decisions = []model.Decision{{
					ID:           "D1",
					Decision:     "retry queue is global",
					SupersededBy: "D2",
					Evidence:     []model.Evidence{fileEv(webhooks, "")},
				}}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb"}},
			want:         Drifted,
			wantChecked:  1,
			wantFindings: []string{webhooks + "|" + kindContent},
		},
		{
			name: "failing test cites a file that has since changed",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				s.Tests = []model.TestResult{{
					Name:   "TestWebhookRetry",
					Status: model.TestFailed,
					Files:  []string{webhooks},
				}}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb"}},
			want:         StaleDecision,
			wantChecked:  1,
			wantFindings: []string{webhooks + "|" + kindTest, webhooks + "|" + kindContent},
		},
		{
			name: "passing test citing a changed file is only drift",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				s.Tests = []model.TestResult{{
					Name:   "TestWebhookRetry",
					Status: model.TestPassed,
					Files:  []string{webhooks},
				}}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb"}},
			want:         Drifted,
			wantChecked:  1,
			wantFindings: []string{webhooks + "|" + kindContent},
		},
		{
			name: "cited file no longer exists",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.Evidence = []model.Evidence{fileEv(webhooks, "sha256:aaaa")}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}},
			want:         Conflict,
			wantChecked:  1,
			wantFindings: []string{webhooks + "|" + kindMissing},
		},
		{
			name: "file the history deleted is expected to be absent",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{
					changed(webhooks, "M", "sha256:aaaa"),
					changed("internal/legacy.go", "D", "sha256:dddd"),
				}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:aaaa"}},
			want:        Matches,
			wantChecked: 2,
			wantNoNotes: true,
		},
		{
			name: "file the history deleted is back in the working tree",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				// derive never fingerprints a deleted file, so the only honest
				// verdict here is uncertainty plus a note naming the surprise.
				s.ChangedFiles = []model.ChangedFile{changed("internal/legacy.go", "D", "")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{"internal/legacy.go": "sha256:zzzz"}},
			want:        Unknown,
			wantChecked: 1,
			wantNotes:   []string{"records internal/legacy.go as deleted, but it is present"},
		},
		{
			name: "conflict outranks stale decision and drift",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{
					changed(webhooks, "M", "sha256:aaaa"),
					changed(retry, "M", "sha256:cccc"),
				}
				s.Decisions = []model.Decision{{
					ID:       "D1",
					Decision: "retry queue is per tenant",
					Evidence: []model.Evidence{fileEv(webhooks, "")},
				}}
				s.Evidence = []model.Evidence{fileEv("internal/gone.go", "sha256:eeee")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: nextSHA}, files: map[string]string{webhooks: "sha256:bbbb", retry: "sha256:cccc"}},
			want:        Conflict,
			wantChecked: 3,
			wantFindings: []string{
				"|" + kindCommit,
				"internal/gone.go|" + kindMissing,
				webhooks + "|" + kindDecision,
				webhooks + "|" + kindContent,
			},
		},
		{
			name: "stale decision outranks plain drift",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{
					changed(webhooks, "M", "sha256:aaaa"),
					changed(retry, "M", "sha256:cccc"),
				}
				s.Decisions = []model.Decision{{
					ID:       "D1",
					Decision: "retry queue is per tenant",
					Evidence: []model.Evidence{fileEv(retry, "")},
				}}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb", retry: "sha256:dddd"}},
			want:        StaleDecision,
			wantChecked: 2,
			wantFindings: []string{
				retry + "|" + kindDecision,
				retry + "|" + kindContent,
				webhooks + "|" + kindContent,
			},
		},
		{
			name: "no recorded hash means unknown, never a match",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:aaaa"}},
			want:        Unknown,
			wantChecked: 1,
			wantNotes:   []string{"No recorded content hash for 1 file", webhooks, "cannot claim that the state matches"},
		},
		{
			name: "repository cannot hash a file",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				return s
			},
			repo: &fakeRepo{
				head:    model.RepoState{CommitSHA: histSHA},
				files:   map[string]string{webhooks: "sha256:aaaa"},
				hashErr: map[string]error{webhooks: model.ErrUnavailable},
			},
			want:        Unknown,
			wantChecked: 1,
			wantNotes:   []string{"could not hash 1 file", webhooks},
		},
		{
			name: "observed drift still reported when other checks are blind",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{
					changed(webhooks, "M", "sha256:aaaa"),
					changed(retry, "M", ""),
				}
				return s
			},
			repo:         &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:bbbb", retry: "sha256:cccc"}},
			want:         Drifted,
			wantChecked:  2,
			wantFindings: []string{webhooks + "|" + kindContent},
			wantNotes:    []string{"No recorded content hash for 1 file", retry},
		},
		{
			name: "history disagrees with itself about a file hash",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				s.Evidence = []model.Evidence{fileEv(webhooks, "sha256:bbbb")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:aaaa"}},
			want:        Unknown,
			wantChecked: 1,
			wantNotes:   []string{"Conflicting recorded content hashes for 1 file", webhooks},
		},
		{
			name:      "historical capture recorded no commit sha",
			state:     func() *model.EngineeringState { return newState("") },
			repo:      &fakeRepo{head: model.RepoState{CommitSHA: nextSHA}},
			want:      Unknown,
			wantNotes: []string{"historical state records no commit sha", nextSHA},
		},
		{
			name:      "repository reports no commit sha",
			state:     func() *model.EngineeringState { return newState(histSHA) },
			repo:      &fakeRepo{head: model.RepoState{}},
			want:      Unknown,
			wantNotes: []string{"current repository reports no commit sha", histSHA},
		},
		{
			name:      "neither side records a commit sha",
			state:     func() *model.EngineeringState { return newState("") },
			repo:      &fakeRepo{head: model.RepoState{}},
			want:      Unknown,
			wantNotes: []string{"Neither the historical state nor the current repository"},
		},
		{
			name: "uncommitted work is reported but does not decide the outcome",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA, Dirty: true}, files: map[string]string{webhooks: "sha256:aaaa"}},
			want:        Matches,
			wantChecked: 1,
			wantNotes:   []string{"uncommitted changes"},
		},
		{
			name: "path recorded with windows separators is the same file",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(`internal\webhooks.go`, "M", "sha256:aaaa")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:aaaa"}},
			want:        Matches,
			wantChecked: 1,
			wantNoNotes: true,
		},
		{
			name: "bare digest and prefixed digest are the same fingerprint",
			state: func() *model.EngineeringState {
				s := newState(histSHA)
				s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:AAAA")}
				return s
			},
			repo:        &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "aaaa"}},
			want:        Matches,
			wantChecked: 1,
			wantNoNotes: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Detect(context.Background(), tc.state(), tc.repo)
			if err != nil {
				t.Fatalf("Detect returned error: %v", err)
			}
			if got.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q (notes: %v)", got.Outcome, tc.want, got.Notes)
			}
			wantRevalidation := tc.want != Matches
			if got.RevalidationRequired != wantRevalidation {
				t.Errorf("RevalidationRequired = %v, want %v", got.RevalidationRequired, wantRevalidation)
			}
			if got.CheckedFiles != tc.wantChecked {
				t.Errorf("CheckedFiles = %d, want %d", got.CheckedFiles, tc.wantChecked)
			}
			if tc.wantFindings == nil {
				tc.wantFindings = []string{}
			}
			if gotKeys := keys(got.Findings); !reflect.DeepEqual(gotKeys, tc.wantFindings) {
				t.Errorf("findings = %v, want %v", gotKeys, tc.wantFindings)
			}
			for _, want := range tc.wantNotes {
				if !hasNote(got.Notes, want) {
					t.Errorf("no note containing %q; notes: %v", want, got.Notes)
				}
			}
			if tc.wantNoNotes && len(got.Notes) != 0 {
				t.Errorf("expected no notes, got %v", got.Notes)
			}
			if tc.repo.changedFilesCalled {
				t.Error("Detect called RepoInspector.ChangedFiles; the historical state defines the file set")
			}
			// Every finding must name the artifact it came from (plan §11).
			for _, f := range got.Findings {
				if f.Kind == "" || f.Detail == "" {
					t.Errorf("finding %+v is missing kind or detail", f)
				}
			}
		})
	}
}

// TestDetectDegradesWithoutRepository covers the two paths that must never fail
// the host agent's own work: no inspector at all, and an inspector whose backing
// system is unreachable (plan §48).
func TestDetectUnavailableRepository(t *testing.T) {
	tests := []struct {
		name string
		repo model.RepoInspector
	}{
		{name: "nil inspector", repo: nil},
		{name: "head unavailable", repo: &fakeRepo{headErr: model.ErrUnavailable}},
		{name: "head unavailable wrapped", repo: &fakeRepo{headErr: fmt.Errorf("git: %w", model.ErrUnavailable)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newState(histSHA)
			s.ChangedFiles = []model.ChangedFile{changed("internal/webhooks.go", "M", "sha256:aaaa")}

			got, err := Detect(context.Background(), s, tc.repo)
			if err != nil {
				t.Fatalf("unavailable repository must not be an error, got %v", err)
			}
			if got.Outcome != Unknown {
				t.Errorf("outcome = %q, want %q", got.Outcome, Unknown)
			}
			if !got.RevalidationRequired {
				t.Error("RevalidationRequired = false; an unverified state always needs revalidation")
			}
			if got.CheckedFiles != 0 {
				t.Errorf("CheckedFiles = %d, want 0", got.CheckedFiles)
			}
			if len(got.Notes) == 0 {
				t.Error("an unknown outcome must explain itself in Notes")
			}
			if len(got.Findings) != 0 {
				t.Errorf("no repository was read, so nothing can be claimed: %v", got.Findings)
			}
		})
	}
}

func TestDetectNilStateIsUnknown(t *testing.T) {
	got, err := Detect(context.Background(), nil, &fakeRepo{head: model.RepoState{CommitSHA: histSHA}})
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if got.Outcome != Unknown || !got.RevalidationRequired {
		t.Fatalf("got %+v, want unknown and revalidation required", got)
	}
	if !hasNote(got.Notes, "No historical state") {
		t.Errorf("notes = %v, want an explanation of the missing state", got.Notes)
	}
}

// TestDetectHardErrors checks that a genuine failure surfaces instead of being
// laundered into a verdict, and that the returned report still cannot be read as
// a clean repository.
func TestDetectHardErrors(t *testing.T) {
	boom := errors.New("git exploded")
	webhooks := "internal/webhooks.go"

	state := func() *model.EngineeringState {
		s := newState(histSHA)
		s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
		return s
	}

	tests := []struct {
		name string
		repo *fakeRepo
	}{
		{name: "head fails", repo: &fakeRepo{headErr: boom}},
		{name: "hash fails", repo: &fakeRepo{
			head:    model.RepoState{CommitSHA: histSHA},
			files:   map[string]string{webhooks: "sha256:aaaa"},
			hashErr: map[string]error{webhooks: boom},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Detect(context.Background(), state(), tc.repo)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap %v", err, boom)
			}
			if got.Outcome != Unknown || !got.RevalidationRequired {
				t.Errorf("report on error = %+v, want unknown and revalidation required", got)
			}
		})
	}
}

// TestDetectExistsButCannotBeHashed covers the race where Exists succeeds and the
// hash lookup then reports the path gone: either way the cited file is not there.
func TestDetectExistsButHashNotFound(t *testing.T) {
	webhooks := "internal/webhooks.go"
	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}

	repo := &fakeRepo{
		head:    model.RepoState{CommitSHA: histSHA},
		files:   map[string]string{webhooks: "sha256:aaaa"},
		hashErr: map[string]error{webhooks: fmt.Errorf("stat: %w", model.ErrNotFound)},
	}
	got, err := Detect(context.Background(), s, repo)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if got.Outcome != Conflict {
		t.Fatalf("outcome = %q, want %q", got.Outcome, Conflict)
	}
	if want := []string{webhooks + "|" + kindMissing}; !reflect.DeepEqual(keys(got.Findings), want) {
		t.Errorf("findings = %v, want %v", keys(got.Findings), want)
	}
}

func TestDetectHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{changed("internal/webhooks.go", "M", "sha256:aaaa")}

	got, err := Detect(ctx, s, &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{"internal/webhooks.go": "sha256:aaaa"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got.Outcome != Unknown {
		t.Errorf("outcome = %q, want %q", got.Outcome, Unknown)
	}
}

// TestDetectIsDeterministic guards the product requirement that two runs over the
// same input are byte-identical. Go randomises map iteration per range, so
// running the same comparison repeatedly in one process exercises it.
func TestDetectIsDeterministic(t *testing.T) {
	build := func() *model.EngineeringState {
		s := newState(histSHA)
		for _, p := range []string{"z.go", "a.go", "m/k.go", "b.go", "m/a.go"} {
			s.ChangedFiles = append(s.ChangedFiles, changed(p, "M", "sha256:"+p))
		}
		s.ChangedFiles = append(s.ChangedFiles, changed("gone.go", "M", "sha256:gone"))
		s.Decisions = []model.Decision{
			{ID: "D2", Decision: "second", Evidence: []model.Evidence{fileEv("z.go", "")}},
			{ID: "D1", Decision: "first", Evidence: []model.Evidence{fileEv("z.go", "")}},
		}
		s.Tests = []model.TestResult{{Name: "TestZ", Status: model.TestFailed, Files: []string{"z.go", "a.go"}}}
		return s
	}
	repo := func() *fakeRepo {
		return &fakeRepo{
			head: model.RepoState{CommitSHA: nextSHA},
			files: map[string]string{
				"z.go":   "sha256:changed",
				"a.go":   "sha256:changed",
				"m/k.go": "sha256:m/k.go",
				"b.go":   "sha256:b.go",
				"m/a.go": "sha256:m/a.go",
			},
		}
	}

	first, err := Detect(context.Background(), build(), repo())
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := Detect(context.Background(), build(), repo())
		if err != nil {
			t.Fatalf("Detect returned error: %v", err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n first: %+v\n got:   %+v", i, first, got)
		}
		if got.Summary() != first.Summary() {
			t.Fatalf("run %d summary differs:\n%s\n---\n%s", i, first.Summary(), got.Summary())
		}
	}

	// The worst true thing wins, and the missing file is reported before the
	// content drift on the same run.
	if first.Outcome != Conflict {
		t.Errorf("outcome = %q, want %q", first.Outcome, Conflict)
	}
	if got := keys(first.Findings); got[0] != "|"+kindCommit {
		t.Errorf("first finding = %q, want the repository-wide commit finding", got[0])
	}
}

// TestDetectDoesNotMutateHistoricalState protects the caller's state: drift
// detection reads history, it never edits it.
func TestDetectDoesNotMutateHistoricalState(t *testing.T) {
	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{
		changed("z.go", "M", "sha256:z"),
		changed("a.go", "M", "sha256:a"),
	}
	before := s.Clone()

	if _, err := Detect(context.Background(), s, &fakeRepo{head: model.RepoState{CommitSHA: histSHA}}); err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if !reflect.DeepEqual(s, before) {
		t.Errorf("Detect mutated the historical state:\n before: %+v\n after:  %+v", before, s)
	}
}

// TestDetectCitedFileWithoutFingerprintStillMatches pins the scope of the
// honesty degradation. The semantic extractor cites files without fingerprinting
// them, so treating every hashless citation as a blind check would demote almost
// every real comparison to Unknown and make the plan §46 "STATE MATCHES" block
// unreachable. The only rule §23 states for a cited-only path is that it still
// exists, and that check was performed.
func TestDetectCitedFileWithoutFingerprintStillMatches(t *testing.T) {
	webhooks := "internal/webhooks.go"
	queue := "internal/queue.go"

	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
	s.Decisions = []model.Decision{{
		ID:       "D1",
		Decision: "retry queue is per tenant",
		Evidence: []model.Evidence{fileEv(queue, "")},
	}}
	repo := &fakeRepo{
		head:  model.RepoState{CommitSHA: histSHA},
		files: map[string]string{webhooks: "sha256:aaaa", queue: "sha256:qqqq"},
	}

	got, err := Detect(context.Background(), s, repo)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if got.Outcome != Matches {
		t.Errorf("outcome = %q, want %q; notes: %v", got.Outcome, Matches, got.Notes)
	}
	if got.RevalidationRequired {
		t.Error("RevalidationRequired = true; every applicable check was performed")
	}
	if len(got.Notes) != 0 {
		t.Errorf("a fully checked comparison must not invent doubt: %v", got.Notes)
	}
	if got.CheckedFiles != 2 {
		t.Errorf("CheckedFiles = %d, want 2", got.CheckedFiles)
	}
}

// TestDetectCitedFileWithFingerprintIsStillCompared is the other half of the
// rule above: a citation that does carry a fingerprint is compared, so the
// narrowed blind-check scope cannot be mistaken for "citations are not checked".
func TestDetectCitedFileWithFingerprintIsStillCompared(t *testing.T) {
	queue := "internal/queue.go"

	s := newState(histSHA)
	s.Decisions = []model.Decision{{
		ID:       "D1",
		Decision: "retry queue is per tenant",
		Evidence: []model.Evidence{fileEv(queue, "sha256:aaaa")},
	}}
	repo := &fakeRepo{
		head:  model.RepoState{CommitSHA: histSHA},
		files: map[string]string{queue: "sha256:bbbb"},
	}

	got, err := Detect(context.Background(), s, repo)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if got.Outcome != StaleDecision {
		t.Fatalf("outcome = %q, want %q; notes: %v", got.Outcome, StaleDecision, got.Notes)
	}
	want := []string{queue + "|" + kindDecision, queue + "|" + kindContent}
	if gotKeys := keys(got.Findings); !reflect.DeepEqual(gotKeys, want) {
		t.Errorf("findings = %v, want %v", gotKeys, want)
	}
}

// TestDetectFailingTestCitationWithoutFingerprintIsBlind guards the boundary of
// the same rule. Plan §23 does ask whether a file a failing test cites has
// changed, so when no fingerprint was recorded that question genuinely could not
// be answered and the comparison must not claim a match.
func TestDetectFailingTestCitationWithoutFingerprintIsBlind(t *testing.T) {
	queue := "internal/queue.go"

	s := newState(histSHA)
	s.Tests = []model.TestResult{{
		Name:   "TestQueueRetry",
		Status: model.TestFailed,
		Files:  []string{queue},
	}}
	repo := &fakeRepo{
		head:  model.RepoState{CommitSHA: histSHA},
		files: map[string]string{queue: "sha256:qqqq"},
	}

	got, err := Detect(context.Background(), s, repo)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if got.Outcome != Unknown {
		t.Errorf("outcome = %q, want %q; notes: %v", got.Outcome, Unknown, got.Notes)
	}
	if !hasNote(got.Notes, "No recorded content hash for 1 file") || !hasNote(got.Notes, queue) {
		t.Errorf("notes must name the unverifiable file: %v", got.Notes)
	}
}

// TestDetectAmbiguousHashIsNotAlsoReportedAsMissing catches a contradiction in
// the report itself: a path with two recorded fingerprints was described both as
// conflicting and as having no recorded hash. A drift report whose whole purpose
// is honest reporting cannot state two incompatible things about one file.
func TestDetectAmbiguousHashIsNotAlsoReportedAsMissing(t *testing.T) {
	webhooks := "internal/webhooks.go"

	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{changed(webhooks, "M", "sha256:aaaa")}
	s.Evidence = []model.Evidence{fileEv(webhooks, "sha256:bbbb")}
	repo := &fakeRepo{head: model.RepoState{CommitSHA: histSHA}, files: map[string]string{webhooks: "sha256:aaaa"}}

	got, err := Detect(context.Background(), s, repo)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if got.Outcome != Unknown {
		t.Errorf("outcome = %q, want %q", got.Outcome, Unknown)
	}
	if !hasNote(got.Notes, "Conflicting recorded content hashes for 1 file") {
		t.Errorf("notes must report the disagreement: %v", got.Notes)
	}
	if hasNote(got.Notes, "No recorded content hash") {
		t.Errorf("a file with two recorded hashes must not be described as having none: %v", got.Notes)
	}
	// One file, one blind check: the double note must not double-count either.
	if !hasNote(got.Notes, "1 check could not be verified") {
		t.Errorf("notes = %v, want exactly one blind check reported", got.Notes)
	}
}

// TestDetectCancelledContextBeforeAnyCheck covers the case the mid-loop
// cancellation check cannot reach: a state with no files to walk. Without an
// up-front guard the comparison never runs and its emptiness reads as a clean
// repository, which is the one verdict an aborted run must never produce.
func TestDetectCancelledContextBeforeAnyCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := Detect(ctx, newState(histSHA), &fakeRepo{head: model.RepoState{CommitSHA: histSHA}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got.Outcome != Unknown {
		t.Errorf("outcome = %q, want %q", got.Outcome, Unknown)
	}
	if !got.RevalidationRequired {
		t.Error("RevalidationRequired = false; a cancelled comparison verified nothing")
	}
}

// TestDetectAsksExistenceOnce protects against a torn read: when Exists is
// consulted twice for one path and the tree changes in between, the same file
// can be reported as both present and absent in a single report.
func TestDetectAsksExistenceOnce(t *testing.T) {
	legacy := "internal/legacy.go"
	s := newState(histSHA)
	s.ChangedFiles = []model.ChangedFile{changed(legacy, "D", "")}

	repo := &countingRepo{fakeRepo: fakeRepo{
		head:  model.RepoState{CommitSHA: histSHA},
		files: map[string]string{legacy: "sha256:zzzz"},
	}}
	if _, err := Detect(context.Background(), s, repo); err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if repo.existsCalls[legacy] != 1 {
		t.Errorf("Exists(%s) called %d times, want 1", legacy, repo.existsCalls[legacy])
	}
}

// countingRepo records how often each path's existence was asked about.
type countingRepo struct {
	fakeRepo
	existsCalls map[string]int
}

func (c *countingRepo) Exists(ctx context.Context, path string) bool {
	if c.existsCalls == nil {
		c.existsCalls = map[string]int{}
	}
	c.existsCalls[path]++
	return c.fakeRepo.Exists(ctx, path)
}
