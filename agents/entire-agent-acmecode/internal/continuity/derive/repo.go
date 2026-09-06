package derive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// hashPrefix is the algorithm tag every content fingerprint carries in
// Evidence.Detail. The drift package compares those strings directly, so the
// prefix is part of the contract and not cosmetic (plan §23).
const hashPrefix = "sha256:"

// repoFacts is the Level 1 half of the state: everything git can prove.
type repoFacts struct {
	state    model.RepoState
	files    []model.ChangedFile
	evidence []model.Evidence
	// available mirrors Capture.GitAvailable: the inspector existed and Head
	// answered. Changed files may still be missing on top of that.
	available bool
	notes     []string
}

// deriveRepo reads repository facts through the inspector.
//
// It returns an error only when the caller's context is done. A nil inspector
// or one reporting model.ErrUnavailable produces an honest gap instead: the
// agent's own work must keep running when the continuity layer cannot see the
// repository (plan §48, Rule 8).
func deriveRepo(ctx context.Context, repo model.RepoInspector, baseRef string, now time.Time) (repoFacts, error) {
	if repo == nil {
		return repoFacts{notes: []string{
			"no repository inspector was supplied; git state and changed files are unknown",
		}}, nil
	}

	state, err := repo.Head(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Do not describe our own cancellation as a repository outage.
			return repoFacts{}, fmt.Errorf("derive: %w", ctxErr)
		}
		return repoFacts{notes: []string{"repository head " + describeGap(err)}}, nil
	}

	out := repoFacts{state: state, available: true}
	if out.state.ObservedAt.IsZero() {
		// ObservedAt is excluded from EngineeringState.Hash, which is why the
		// derivation can stamp wall-clock time here without making the state
		// fingerprint churn between otherwise identical runs.
		out.state.ObservedAt = now
	}
	if sha := strings.TrimSpace(state.CommitSHA); sha != "" {
		out.evidence = append(out.evidence, model.Evidence{
			Kind:   model.EvidenceCommit,
			Ref:    sha,
			Detail: state.Branch,
		})
	} else {
		out.notes = append(out.notes,
			"repository reported no commit sha; changes cannot be pinned to a revision")
	}

	files, err := repo.ChangedFiles(ctx, baseRef)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return repoFacts{}, fmt.Errorf("derive: %w", ctxErr)
		}
		// Head answered, so git itself is available; only the diff is missing.
		out.notes = append(out.notes, "changed files "+describeGap(err))
		return out, nil
	}

	changed, hashNotes := changedFiles(ctx, repo, files)
	out.files = changed
	out.notes = append(out.notes, hashNotes...)
	return out, nil
}

// changedFiles attaches Level 1 evidence to each reported file, including the
// content fingerprint drift detection needs.
func changedFiles(ctx context.Context, repo model.RepoInspector, in []model.ChangedFile) ([]model.ChangedFile, []string) {
	out := make([]model.ChangedFile, 0, len(in))
	seen := make(map[string]bool, len(in))
	var hashable, hashed, unnamed int

	for _, f := range in {
		path := strings.TrimSpace(f.Path)
		if path == "" {
			unnamed++
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		f.Path = path
		f.Status = strings.TrimSpace(f.Status)
		if f.Status == "" {
			// model.ChangedFile documents "?" as the unknown marker; leaving it
			// blank would read as "no change" (plan §12).
			f.Status = "?"
		}

		ev := model.Evidence{
			Kind:   model.EvidenceFile,
			Ref:    path,
			Path:   path,
			Result: f.Status,
		}
		// A deleted file has no content to fingerprint, so it is not counted as
		// a missing hash.
		if !isDeletion(f.Status) {
			hashable++
			if h, err := repo.FileHash(ctx, path); err == nil {
				if h = normalizeHash(h); h != "" {
					ev.Detail = h
					hashed++
				}
			}
		}
		// Copied rather than appended in place: the inspector still owns the
		// slice it handed us and must not have its spare capacity written into.
		evidence := make([]model.Evidence, 0, len(f.Evidence)+1)
		evidence = append(evidence, f.Evidence...)
		evidence = append(evidence, ev)
		f.Evidence = model.DedupeEvidence(evidence)
		out = append(out, f)
	}

	var notes []string
	if unnamed > 0 {
		notes = append(notes, fmt.Sprintf("%d changed file(s) were reported without a path and were dropped", unnamed))
	}
	if hashed < hashable {
		// Drift detection compares these fingerprints; without them it cannot
		// tell whether a relevant file moved underneath the task (plan §23).
		notes = append(notes, fmt.Sprintf(
			"content hashes unavailable for %d of %d changed file(s); drift detection is degraded",
			hashable-hashed, hashable))
	}
	return out, notes
}

// isDeletion reports whether a git-style status code marks a removed file.
//
// Status codes arrive verbatim from whichever inspector produced them, so the
// porcelain forms ("D ", " D", "DD") and a lower-cased one all have to read as
// a deletion. Getting this wrong costs twice: the derivation asks for the
// content hash of a file that is gone, and then counts the inevitable failure
// as degraded drift coverage, so a clean deletion reports as a capture gap.
func isDeletion(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "D", "DD":
		return true
	}
	return false
}

// normalizeHash renders an inspector's fingerprint in the "sha256:..." form the
// drift package expects, without re-tagging one that already carries it.
func normalizeHash(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if strings.HasPrefix(h, hashPrefix) {
		return h
	}
	return hashPrefix + h
}

// describeGap phrases a port failure for a human reader, keeping the distinction
// between "the system is not reachable" and "the system failed". Both degrade,
// but a reader deciding whether to trust the state needs to know which happened
// (plan §33).
func describeGap(err error) string {
	if errors.Is(err, model.ErrUnavailable) {
		return "unavailable: " + err.Error()
	}
	return "could not be read: " + err.Error()
}
