// Package drift compares the engineering state captured in the past against the
// repository as it stands right now.
//
// Plan §22 states the rule this package exists to enforce: historical state is
// context, not authority. A resumed agent must verify historical claims against
// the current repository before it changes anything, and this package produces
// the evidence for that verification (plan §23, §46, Step 19).
//
// # Dependency on the derive package
//
// File-content comparison depends on historical file evidence carrying its
// content fingerprint in Evidence.Detail as "sha256:<hex>", which the derive
// package writes when it records repository facts. model.ChangedFile has no
// hash field, so evidence Detail is the only channel available. Detail that does
// not carry the "sha256:" prefix is treated as prose, not as a fingerprint.
//
// When a fingerprint the comparison actually needed is missing, ambiguous or
// unreadable, the file is reported as unverifiable and the report degrades to
// Unknown rather than to Matches: an unchecked file must never be allowed to
// produce a "state matches" verdict (plan §33, §48).
//
// "Actually needed" is load-bearing in both directions. A path the history
// records as changed, or one a failing test cites, has a §23 rule asking whether
// its content moved, so a missing fingerprint there is a genuine blind spot. A
// path that historical evidence only cites has exactly one rule against it -
// does it still exist - and that check needs no hash, so its lack of one is not
// a gap. Degrading those to Unknown would manufacture doubt out of nothing and
// make the Matches verdict effectively unreachable, since the semantic extractor
// cites files without fingerprinting them.
package drift

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// Outcome is the verdict of a drift comparison, using the four outcomes named in
// plan §23 plus Unknown for the case where the comparison could not be made.
type Outcome string

const (
	// Matches means no meaningful change since the historical state was captured.
	Matches Outcome = "state_matches"
	// Drifted means code changed after the historical state was captured.
	Drifted Outcome = "state_drifted"
	// StaleDecision means a historical conclusion may no longer hold because the
	// evidence it rests on moved underneath it.
	StaleDecision Outcome = "stale_decision"
	// Conflict means the current repository contradicts the historical state.
	Conflict Outcome = "conflict"
	// Unknown means the comparison could not be performed. It is never a synonym
	// for Matches: an uninspectable repository proves nothing (plan §48).
	Unknown Outcome = "unknown"
)

// Finding kinds. These values are part of the Finding wire shape; renderers may
// switch on them.
const (
	kindCommit   = "commit"   // repository HEAD moved
	kindContent  = "content"  // tracked file content changed
	kindMissing  = "missing"  // cited file absent from the working tree
	kindDecision = "decision" // an active decision cites a file that changed
	kindTest     = "test"     // a failing test cites a file that changed
)

// Finding is one concrete disagreement between historical state and the current
// repository. Every finding names the artifact it was derived from, so the
// report is evidence-backed rather than a narrative (plan §11).
//
// Kind is one of "commit", "content", "missing", "decision" or "test". Path is
// the working-tree path, empty for a repository-wide finding such as "commit".
type Finding struct {
	Path       string
	Kind       string
	Historical string
	Current    string
	Detail     string
}

// Report is the result of a drift comparison.
//
// CheckedFiles counts the distinct working-tree paths that were actually looked
// up in the repository, so a reader can tell a clean comparison of forty files
// apart from a clean comparison of none.
//
// Notes carry the honest degradation record: every check that could not be
// performed is named here rather than silently dropped (plan §33).
type Report struct {
	Outcome              Outcome
	Findings             []Finding
	CheckedFiles         int
	RevalidationRequired bool
	Notes                []string
}

// hashPrefix is the fingerprint encoding the derive package writes into
// Evidence.Detail. See the package comment.
const hashPrefix = "sha256:"

// Detect compares historical against the live repository and reports how far the
// world has moved (plan §23).
//
// Outcome precedence when several rules fire is Conflict > StaleDecision >
// Drifted > Matches, because a resuming agent must be told the worst thing that
// is true, not the most common one.
//
// A nil repo, or a repo returning model.ErrUnavailable, is not an error: drift
// detection runs at resume time and must never break the host agent's own work
// (plan §48, Rule 8). Those cases produce Outcome Unknown with an explanatory
// note. Any other error is returned, and the accompanying Report is still
// Unknown so that a caller which ignores the error cannot mistake the failure
// for a clean repository.
func Detect(ctx context.Context, historical *model.EngineeringState, repo model.RepoInspector) (Report, error) {
	r := Report{Outcome: Unknown, RevalidationRequired: true}

	// A comparison that was cancelled before it began proves nothing. Without
	// this guard a caller whose context is already done gets Matches back from a
	// state with no files to walk, which is exactly the "aborted comparison read
	// as a clean repository" failure this package exists to prevent.
	if err := ctx.Err(); err != nil {
		return unknownReport(), fmt.Errorf("drift: %w", err)
	}

	if historical == nil {
		r.Notes = append(r.Notes, "No historical state was supplied, so there was nothing to verify against the repository.")
		return r, nil
	}
	if repo == nil {
		r.Notes = append(r.Notes, "Repository inspection is unavailable, so historical state could not be verified against the working tree. Treat every historical claim as unverified.")
		return r, nil
	}

	head, err := repo.Head(ctx)
	if err != nil {
		if errors.Is(err, model.ErrUnavailable) {
			r.Notes = append(r.Notes, "Repository inspection is unavailable, so historical state could not be verified against the working tree. Treat every historical claim as unverified.")
			return r, nil
		}
		return unknownReport(), fmt.Errorf("drift: read repository head: %w", err)
	}

	// worst tracks the strongest verdict any single rule produced. blind counts
	// checks that could not be performed at all; see finalOutcome.
	worst := Matches
	blind := 0

	worst, blind = compareCommit(&r, historical.Repo, head, worst, blind)

	// A tree that is dirty now but was clean when the state was captured is
	// reported as context only. Plan §23 lists no dirty-tree outcome, so this
	// never changes the verdict; it exists so the reader is not surprised by
	// uncommitted work the findings cannot cite.
	if head.Dirty && !historical.Repo.Dirty {
		r.Notes = append(r.Notes, "The working tree has uncommitted changes that the historical state does not record.")
	}

	checks, order := collectChecks(historical, &r)

	var unverified, unavailable []string
	for _, p := range order {
		if err := ctx.Err(); err != nil {
			return unknownReport(), fmt.Errorf("drift: comparing %s: %w", p, err)
		}
		fc := checks[p]
		r.CheckedFiles++

		// Asked once: two calls could disagree if the tree changes underneath
		// us, which would report the file as both present and absent.
		exists := repo.Exists(ctx, p)

		if fc.deletedInHistory && exists {
			// The reverse of the case below: history says the task deleted this
			// file and the tree still has it. Plan §23 defines no outcome for
			// this, so it is reported as context rather than as a verdict, but
			// staying silent about it would hide a real disagreement.
			r.Notes = append(r.Notes, "The historical state records "+p+" as deleted, but it is present in the working tree.")
		}

		if !exists {
			if fc.deletedInHistory {
				// The historical state itself records this file as deleted, so
				// its absence confirms the history rather than contradicting it.
				// Reporting a conflict here would be a fabricated alarm.
				continue
			}
			r.Findings = append(r.Findings, fc.missingFinding())
			worst = escalate(worst, Conflict)
			continue
		}

		if fc.histHash == "" {
			// No usable fingerprint. Whether that is a blind spot depends on
			// whether any §23 rule wanted a content comparison for this path at
			// all; see fingerprintExpected. A path the history only ever cites
			// has been fully checked by the existence test above, so calling it
			// unverified would manufacture doubt and suppress a true Matches.
			if fc.fingerprintExpected() {
				blind++
				if !fc.ambiguous {
					// An ambiguous path is already named in its own note; saying
					// "no recorded content hash" about a file with two recorded
					// hashes would be a false statement.
					unverified = append(unverified, p)
				}
			}
			continue
		}

		current, err := repo.FileHash(ctx, p)
		switch {
		case err == nil:
		case errors.Is(err, model.ErrUnavailable):
			blind++
			unavailable = append(unavailable, p)
			continue
		case errors.Is(err, model.ErrNotFound):
			// Exists said the path was present and the hash lookup says it is
			// not. Either way the file the history cites is not there to read.
			r.Findings = append(r.Findings, fc.missingFinding())
			worst = escalate(worst, Conflict)
			continue
		default:
			return unknownReport(), fmt.Errorf("drift: hash %s: %w", p, err)
		}

		if canonicalHash(current) == canonicalHash(fc.histHash) {
			continue
		}

		r.Findings = append(r.Findings, Finding{
			Path:       p,
			Kind:       kindContent,
			Historical: fc.histHash,
			Current:    current,
			Detail:     "file content changed since the historical state was captured",
		})
		worst = escalate(worst, Drifted)

		// A changed file that historical conclusions rest on is worse than a
		// changed file nobody cited: the conclusion itself is now suspect.
		for _, c := range fc.decisions {
			r.Findings = append(r.Findings, Finding{
				Path:       p,
				Kind:       kindDecision,
				Historical: c.text,
				Current:    "the file this decision cites changed; the decision is unverified",
				Detail:     "decision " + c.id + " may no longer hold",
			})
			worst = escalate(worst, StaleDecision)
		}
		for _, c := range fc.tests {
			r.Findings = append(r.Findings, Finding{
				Path:       p,
				Kind:       kindTest,
				Historical: c.text,
				Current:    "the file this test cites changed; the recorded failure is unverified",
				Detail:     "failing test " + c.id + " must be re-run before it is trusted",
			})
			worst = escalate(worst, StaleDecision)
		}
	}

	if len(unverified) > 0 {
		sort.Strings(unverified)
		r.Notes = append(r.Notes, fmt.Sprintf("No recorded content hash for %d %s, so %s not compared: %s.",
			len(unverified), plural(len(unverified), "file", "files"), was(len(unverified)), strings.Join(unverified, ", ")))
	}
	if len(unavailable) > 0 {
		sort.Strings(unavailable)
		r.Notes = append(r.Notes, fmt.Sprintf("The repository could not hash %d %s, so %s not compared: %s.",
			len(unavailable), plural(len(unavailable), "file", "files"), was(len(unavailable)), strings.Join(unavailable, ", ")))
	}

	sortFindings(r.Findings)
	r.Outcome = finalOutcome(worst, blind, &r)
	r.RevalidationRequired = r.Outcome != Matches
	return r, nil
}

// unknownReport is the report returned alongside an error. It is deliberately
// Unknown and revalidation-required so an error a caller drops on the floor
// still cannot read as a clean repository.
func unknownReport() Report {
	return Report{Outcome: Unknown, RevalidationRequired: true}
}

// finalOutcome applies the §23 precedence and the honesty rule that an
// incomplete comparison may never be reported as a match.
func finalOutcome(worst Outcome, blind int, r *Report) Outcome {
	if worst != Matches {
		// Real drift was observed. That observation stands even though some
		// other checks were skipped; the skipped ones are recorded in Notes.
		return worst
	}
	if blind > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d %s could not be verified, so this comparison cannot claim that the state matches.",
			blind, plural(blind, "check", "checks")))
		return Unknown
	}
	return Matches
}

// compareCommit applies the §23 commit rule: a HEAD that moved is drift.
func compareCommit(r *Report, hist, head model.RepoState, worst Outcome, blind int) (Outcome, int) {
	h := strings.TrimSpace(hist.CommitSHA)
	c := strings.TrimSpace(head.CommitSHA)
	switch {
	case h == "" && c == "":
		blind++
		r.Notes = append(r.Notes, "Neither the historical state nor the current repository records a commit sha, so commit drift could not be checked.")
	case h == "":
		blind++
		r.Notes = append(r.Notes, "The historical state records no commit sha, so commit drift could not be checked. The repository is now at "+c+".")
	case c == "":
		blind++
		r.Notes = append(r.Notes, "The current repository reports no commit sha, so commit drift could not be checked. The historical state was captured at "+h+".")
	case !sameCommit(h, c):
		r.Findings = append(r.Findings, Finding{
			Kind:       kindCommit,
			Historical: h,
			Current:    c,
			Detail:     "repository HEAD moved since the historical state was captured",
		})
		worst = escalate(worst, Drifted)
	}
	return worst, blind
}

// citation records which historical conclusion depends on a file, so a finding
// can name it instead of asserting an anonymous "assumption may be stale".
type citation struct {
	id   string
	text string
}

// fileCheck is everything known about one path before the repository is asked
// about it.
type fileCheck struct {
	path string
	// histHash is the recorded fingerprint, empty when unknown or ambiguous.
	histHash string
	// hashes holds every distinct canonical fingerprint recorded for the path.
	// More than one means the history disagrees with itself and no comparison
	// is defensible.
	hashes []string
	// ambiguous marks a path whose recorded fingerprints disagree. Such a path
	// is already named in its own note, so it must not also be described as
	// having no recorded hash.
	ambiguous bool
	// tracked marks a path the historical state records as a ChangedFile, which
	// is the only kind of path plan §23 states a content rule for and the only
	// kind the derive package fingerprints.
	tracked bool
	// deletedInHistory marks a file the historical state records as deleted, so
	// its absence now is expected rather than a conflict.
	deletedInHistory bool
	decisions        []citation
	tests            []citation
}

// fingerprintExpected reports whether any plan §23 rule wanted to know if this
// path's content moved, and therefore whether a missing fingerprint is a real
// blind spot rather than an irrelevance.
//
// Two rules ask that question: the ChangedFile content rule, and the rule that a
// recorded test failure goes stale when a file it cites has changed. A path that
// is merely cited by a decision or by some other evidence record has only one
// rule against it - "does it still exist" - and that check needs no hash.
func (fc *fileCheck) fingerprintExpected() bool {
	return fc.tracked || len(fc.tests) > 0
}

// missingFinding describes a cited file that is not in the working tree.
func (fc *fileCheck) missingFinding() Finding {
	historical := "present in the historical state"
	if fc.histHash != "" {
		historical = fc.histHash
	}
	detail := "a file cited by historical evidence is absent from the working tree"
	switch {
	case len(fc.decisions) > 0 && len(fc.tests) > 0:
		detail += "; it is cited by decision " + fc.decisions[0].id + " and failing test " + fc.tests[0].id
	case len(fc.decisions) > 0:
		detail += "; it is cited by decision " + fc.decisions[0].id
	case len(fc.tests) > 0:
		detail += "; it is cited by failing test " + fc.tests[0].id
	}
	return Finding{
		Path:       fc.path,
		Kind:       kindMissing,
		Historical: historical,
		Current:    "absent from the working tree",
		Detail:     detail,
	}
}

// collectChecks builds the set of paths worth asking the repository about: every
// file the task touched, and every file any historical evidence cites. The
// returned order is sorted, because map iteration order must never reach output.
func collectChecks(s *model.EngineeringState, r *Report) (map[string]*fileCheck, []string) {
	checks := make(map[string]*fileCheck)
	get := func(p string) *fileCheck {
		fc, ok := checks[p]
		if !ok {
			fc = &fileCheck{path: p}
			checks[p] = fc
		}
		return fc
	}
	record := func(fc *fileCheck, e model.Evidence) {
		h := evidenceHash(e)
		if h == "" {
			return
		}
		canon := canonicalHash(h)
		for _, existing := range fc.hashes {
			if existing == canon {
				return
			}
		}
		fc.hashes = append(fc.hashes, canon)
		if len(fc.hashes) == 1 {
			fc.histHash = strings.TrimSpace(h)
		}
	}

	// ChangedFile evidence is not part of AllEvidence, so it is walked directly.
	for _, cf := range s.ChangedFiles {
		p := normalizePath(cf.Path)
		if p == "" {
			continue
		}
		fc := get(p)
		fc.tracked = true
		if strings.EqualFold(strings.TrimSpace(cf.Status), "D") {
			fc.deletedInHistory = true
		}
		for _, e := range cf.Evidence {
			if ep := evidencePath(e); ep != "" && ep != p {
				continue
			}
			record(fc, e)
		}
	}

	for _, e := range s.AllEvidence() {
		p := evidencePath(e)
		if p == "" {
			continue
		}
		record(get(p), e)
	}

	// Only active decisions can go stale: plan §32 retains superseded decisions
	// for the record, but they no longer make a claim about the current code.
	for _, d := range s.ActiveDecisions() {
		id := strings.TrimSpace(d.ID)
		if id == "" {
			id = "(unidentified)"
		}
		text := strings.TrimSpace(d.Decision)
		if text == "" {
			text = "decision " + id
		}
		for _, p := range citedPaths(d.Evidence, nil) {
			fc := get(p)
			fc.decisions = addCitation(fc.decisions, citation{id: id, text: text})
		}
	}

	// Plan §23: a recorded failure whose cited file has moved is a stale
	// conclusion, not a current fact.
	for _, t := range s.FailingTests() {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			name = "(unnamed test)"
		}
		for _, p := range citedPaths(t.Evidence, t.Files) {
			fc := get(p)
			fc.tests = addCitation(fc.tests, citation{id: name, text: "test " + name + " failed"})
		}
	}

	order := make([]string, 0, len(checks))
	var ambiguous []string
	for p, fc := range checks {
		order = append(order, p)
		if len(fc.hashes) > 1 {
			// The history records two different fingerprints for one path. No
			// comparison against either is defensible, so the file is treated as
			// unverifiable rather than compared against an arbitrary pick.
			fc.histHash = ""
			fc.ambiguous = true
			ambiguous = append(ambiguous, p)
		}
		sortCitations(fc.decisions)
		sortCitations(fc.tests)
	}
	sort.Strings(order)
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		r.Notes = append(r.Notes, fmt.Sprintf("Conflicting recorded content hashes for %d %s, so %s not compared: %s.",
			len(ambiguous), plural(len(ambiguous), "file", "files"), was(len(ambiguous)), strings.Join(ambiguous, ", ")))
	}
	return checks, order
}

// citedPaths returns the sorted, de-duplicated working-tree paths referenced by
// a set of evidence records plus any explicitly listed files.
func citedPaths(ev []model.Evidence, files []string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, e := range ev {
		add(evidencePath(e))
	}
	for _, f := range files {
		add(normalizePath(f))
	}
	sort.Strings(out)
	return out
}

func addCitation(in []citation, c citation) []citation {
	for _, existing := range in {
		if existing.id == c.id {
			return in
		}
	}
	return append(in, c)
}

func sortCitations(in []citation) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].id != in[j].id {
			return in[i].id < in[j].id
		}
		return in[i].text < in[j].text
	})
}

// evidencePath returns the working-tree path an evidence record cites, or "".
// Evidence.Path is a file path for whichever kind carries one; a file-kind
// record may instead carry the path in Ref (see model.Evidence.Ref).
func evidencePath(e model.Evidence) string {
	if p := normalizePath(e.Path); p != "" {
		return p
	}
	if e.Kind == model.EvidenceFile {
		return normalizePath(e.Ref)
	}
	return ""
}

// evidenceHash extracts the content fingerprint the derive package writes into
// file evidence. Detail without the "sha256:" prefix is prose and is ignored:
// guessing at it would manufacture a comparison that was never recorded.
func evidenceHash(e model.Evidence) string {
	if e.Kind != model.EvidenceFile {
		return ""
	}
	d := strings.TrimSpace(e.Detail)
	if !strings.HasPrefix(strings.ToLower(d), hashPrefix) {
		return ""
	}
	return d
}

// canonicalHash reduces a fingerprint to the form used for equality. The
// RepoInspector port does not fix an encoding, so drift accepts a bare digest or
// a "sha256:"-prefixed one and compares case-insensitively. Only equality
// matters here, never ordering.
func canonicalHash(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimPrefix(s, hashPrefix)
}

// sameCommit reports whether two shas name the same commit. An abbreviated sha
// is accepted as equal to a full one it prefixes, so a state captured with a
// short sha does not report drift against an unchanged repository. Seven is
// git's own minimum abbreviation, below which a prefix is not a safe identity.
func sameCommit(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(short) >= 7 && strings.HasPrefix(long, short)
}

// normalizePath puts a recorded path into the repository's own form. Git reports
// forward slashes on every platform, so a state captured on Windows must not
// report every one of its files as missing.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	return strings.TrimPrefix(p, "/")
}

// rank orders the §23 outcomes by how much they should alarm a resuming agent.
// Unknown is not part of the chain: it means "no verdict", not "a mild verdict".
func rank(o Outcome) int {
	switch o {
	case Conflict:
		return 4
	case StaleDecision:
		return 3
	case Drifted:
		return 2
	case Matches:
		return 1
	}
	return 0
}

func escalate(current, next Outcome) Outcome {
	if rank(next) > rank(current) {
		return next
	}
	return current
}

// severity orders findings within one path so the worst news reads first.
func severity(kind string) int {
	switch kind {
	case kindMissing:
		return 3
	case kindDecision, kindTest:
		return 2
	}
	return 1
}

// sortFindings makes the report byte-identical across runs: repository-wide
// findings first, then grouped by path, worst kind first within a path.
func sortFindings(in []Finding) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if (a.Path == "") != (b.Path == "") {
			return a.Path == ""
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if sa, sb := severity(a.Kind), severity(b.Kind); sa != sb {
			return sa > sb
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Detail < b.Detail
	})
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// was renders subject-verb agreement for the note sentences above.
func was(n int) string {
	if n == 1 {
		return "it was"
	}
	return "they were"
}
