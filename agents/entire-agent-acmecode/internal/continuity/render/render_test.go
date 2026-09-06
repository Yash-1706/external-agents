package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// checkGolden compares rendered output against a committed golden file.
//
// Golden files are the only practical way to hold a rendering contract still:
// they fail on any drift, including drift a hand-written assertion would miss.
// CR bytes are stripped on read so a checkout that normalised line endings
// cannot turn a formatting question into a false failure.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: go test ./internal/render -update)", path, err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got == want {
		return
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		g, w := "", ""
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("%s differs at line %d:\n got: %q\nwant: %q", path, i+1, g, w)
		}
	}
	t.Fatalf("%s differs", path)
}

// TestGolden pins every view for a full state, a partial-capture state, an
// empty state and a state carrying a mid-task constraint.
func TestGolden(t *testing.T) {
	cases := []struct {
		name   string
		golden string
		render func() string
	}{
		{"status full", "status_full.txt", func() string { return Status(fullState(), fullLineage()) }},
		{"snapshot full", "snapshot_full.txt", func() string { return Snapshot(fullState()) }},
		{"handoff full", "handoff_full.txt", func() string { return Handoff(fullState(), fullLineage()) }},
		{"handoff markdown full", "handoff_full.md", func() string { return HandoffMarkdown(fullState(), fullLineage()) }},
		{"lineage full", "lineage_full.txt", func() string { return Lineage(fullLineage()) }},
		{"receipt full", "receipt_full.txt", func() string { return Receipt(fullReceipt()) }},
		{"receipt bare", "receipt_bare.txt", func() string { return Receipt(bareReceipt()) }},
		{"evidence list", "evidence_list.txt", func() string { return EvidenceList(fullState().AllEvidence(), 5) }},
		{"evidence empty", "evidence_empty.txt", func() string { return EvidenceList(nil, 0) }},

		{"status partial", "status_partial.txt", func() string { return Status(partialState(), partialLineage()) }},
		{"handoff partial", "handoff_partial.txt", func() string { return Handoff(partialState(), partialLineage()) }},
		{"handoff markdown partial", "handoff_partial.md", func() string { return HandoffMarkdown(partialState(), partialLineage()) }},
		{"lineage partial", "lineage_partial.txt", func() string { return Lineage(partialLineage()) }},

		{"status empty", "status_empty.txt", func() string { return Status(emptyState(), model.Lineage{}) }},
		{"handoff empty", "handoff_empty.txt", func() string { return Handoff(emptyState(), model.Lineage{}) }},
		{"lineage empty", "lineage_empty.txt", func() string { return Lineage(model.Lineage{}) }},
		{"status nil", "status_nil.txt", func() string { return Status(nil, model.Lineage{}) }},

		{"status constraint", "status_constraint.txt", func() string { return Status(constraintState(), model.Lineage{}) }},
		{"handoff constraint", "handoff_constraint.txt", func() string { return Handoff(constraintState(), model.Lineage{}) }},

		// Capture that model.Capture.Complete calls complete but is not: the
		// graph, test-parsing and semantic layers were unreachable.
		{"status degraded", "status_degraded.txt", func() string { return Status(degradedFullState(), fullLineage()) }},
		// A state whose extractor rated every claim above its evidence.
		{"snapshot overstated", "snapshot_overstated.txt", func() string { return Snapshot(overstatedState()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkGolden(t, tc.golden, tc.render())
		})
	}
}

// TestDeterministic is the byte-identical requirement. It renders twice and
// also renders a shuffled copy of the same inputs: if any map iteration or any
// unsorted slice reached the output, one of these two would drift.
func TestDeterministic(t *testing.T) {
	shuffle := func(s *model.EngineeringState, l model.Lineage) (*model.EngineeringState, model.Lineage) {
		c := s.Clone()
		reverseReq(c.Requirements)
		reverseTest(c.Tests)
		reverseFile(c.ChangedFiles)
		reverseDecision(c.Decisions)
		reverseAction(c.NextActions)
		reverseString(c.Capture.Missing)
		reverseString(c.Capture.Notes)
		out := model.Lineage{Task: l.Task}
		out.Sessions = append([]model.SessionNode(nil), l.Sessions...)
		out.Checkpoints = append([]model.CheckpointNode(nil), l.Checkpoints...)
		out.Handoffs = append([]model.HandoffNode(nil), l.Handoffs...)
		reverseSession(out.Sessions)
		reverseCheckpoint(out.Checkpoints)
		reverseHandoff(out.Handoffs)
		return c, out
	}

	cases := []struct {
		name  string
		state func() *model.EngineeringState
		line  func() model.Lineage
	}{
		{"full", fullState, fullLineage},
		{"partial", partialState, partialLineage},
		{"constraint", constraintState, func() model.Lineage { return model.Lineage{} }},
		{"empty", emptyState, func() model.Lineage { return model.Lineage{} }},
		{"degraded", degradedFullState, fullLineage},
		{"overstated", overstatedState, fullLineage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			views := map[string]func(*model.EngineeringState, model.Lineage) string{
				"Status":          Status,
				"Handoff":         Handoff,
				"HandoffMarkdown": HandoffMarkdown,
				"Snapshot":        func(s *model.EngineeringState, _ model.Lineage) string { return Snapshot(s) },
				"Lineage":         func(_ *model.EngineeringState, l model.Lineage) string { return Lineage(l) },
			}
			// Iteration order over this map does not reach any output; it only
			// picks which view to assert on.
			for name, fn := range views {
				a := fn(tc.state(), tc.line())
				b := fn(tc.state(), tc.line())
				if a != b {
					t.Errorf("%s: repeated render differs", name)
				}
				s2, l2 := shuffle(tc.state(), tc.line())
				if c := fn(s2, l2); c != a {
					t.Errorf("%s: render depends on input ordering", name)
				}
			}
		})
	}
}

// TestRenderDoesNotMutateInput protects the caller: a view is read-only, and
// the state it was handed must be byte-identical afterwards. model.Sort and
// model.SortEvidence both sort in place, so this is a real hazard.
func TestRenderDoesNotMutateInput(t *testing.T) {
	s := fullState()
	l := fullLineage()
	before := s.Hash()
	firstReq := s.Requirements[0].ID
	firstSession := l.Sessions[0].SessionID
	firstCheckpoint := l.Checkpoints[0].CheckpointID

	Status(s, l)
	Handoff(s, l)
	HandoffMarkdown(s, l)
	Snapshot(s)
	Lineage(l)
	EvidenceList(s.Evidence, 2)

	if got := s.Hash(); got != before {
		t.Errorf("state hash changed after rendering: %s -> %s", before, got)
	}
	if s.Requirements[0].ID != firstReq {
		t.Errorf("rendering reordered requirements: %s -> %s", firstReq, s.Requirements[0].ID)
	}
	if l.Sessions[0].SessionID != firstSession {
		t.Errorf("rendering reordered sessions: %s -> %s", firstSession, l.Sessions[0].SessionID)
	}
	if l.Checkpoints[0].CheckpointID != firstCheckpoint {
		t.Errorf("rendering reordered checkpoints: %s -> %s", firstCheckpoint, l.Checkpoints[0].CheckpointID)
	}
}

// TestPartialCaptureNamesMissingInputs is the honesty test for plan §33 and
// §47. It is not enough to know the state is degraded: the view must name each
// input it did not get, or the reader cannot tell which piece of context is
// absent.
func TestPartialCaptureNamesMissingInputs(t *testing.T) {
	s := partialState()
	views := []struct {
		name string
		out  string
	}{
		{"Status", Status(s, partialLineage())},
		{"Handoff", Handoff(s, partialLineage())},
		{"HandoffMarkdown", HandoffMarkdown(s, partialLineage())},
		{"Snapshot", Snapshot(s)},
	}
	for _, v := range views {
		t.Run(v.name, func(t *testing.T) {
			if !has(v.out, "STATE PARTIALLY RECOVERED") {
				t.Fatalf("degraded capture rendered without a STATE PARTIALLY RECOVERED block:\n%s", v.out)
			}
			for _, want := range s.Capture.Missing {
				if !has(v.out, want) {
					t.Errorf("missing input %q is never named in the output", want)
				}
			}
			for _, want := range s.Capture.Notes {
				if !has(v.out, want) {
					t.Errorf("capture note %q is never shown", want)
				}
			}
			// Every unavailable input must be named under Unavailable, not
			// merely omitted from Verified.
			for _, in := range captureInputs(s.Capture) {
				if in.ok {
					continue
				}
				if !has(v.out, in.name) {
					t.Errorf("unavailable input %q is never named", in.name)
				}
			}
			for _, want := range []string{"Verified:", "Unavailable:", "Unknown:", "Safe next action:"} {
				if !has(v.out, want) {
					t.Errorf("partial block is missing the %q heading", want)
				}
			}
		})
	}
}

// TestCompleteCaptureHasNoPartialBlock is the other half: the warning must not
// cry wolf on a state that was fully captured, or readers will learn to skip it.
func TestCompleteCaptureHasNoPartialBlock(t *testing.T) {
	s := fullState()
	// captureDegraded, not model.Capture.Complete, is the trigger: Complete
	// ignores the graph, test-parsing and semantic layers and would call this
	// fixture complete even if all three were unavailable.
	if captureDegraded(s.Capture) {
		t.Fatalf("fixture is supposed to have complete capture")
	}
	for name, out := range map[string]string{
		"Status":   Status(s, fullLineage()),
		"Handoff":  Handoff(s, fullLineage()),
		"Snapshot": Snapshot(s),
	} {
		if has(out, "STATE PARTIALLY RECOVERED") {
			t.Errorf("%s: complete capture rendered a partial-recovery warning", name)
		}
	}
}

// TestDegradedCaptureFiresBeyondCompletePredicate is the regression test for
// the §33/§47 block being gated on model.Capture.Complete, which inspects only
// git, checkpoints, events, transcript and Missing. A state whose graph,
// test-parsing or semantic-extraction layer was unreachable satisfies Complete
// and used to render as a full recovery, which is exactly the silence §33/§47
// forbid and which §48 forbids by name for the graph.
func TestDegradedCaptureFiresBeyondCompletePredicate(t *testing.T) {
	complete := model.Capture{
		GitAvailable: true, CheckpointAvailable: true,
		TranscriptAvailable: true, EventsAvailable: true,
		GraphAvailable: true, TestResultsParsed: true, SemanticExtraction: true,
	}
	cases := []struct {
		name    string
		mutate  func(*model.Capture)
		names   string
		degrade bool
	}{
		{"graph unavailable", func(c *model.Capture) { c.GraphAvailable = false }, "entire graph", true},
		{"test results unparsed", func(c *model.Capture) { c.TestResultsParsed = false }, "test results", true},
		{"no semantic extraction", func(c *model.Capture) { c.SemanticExtraction = false }, "semantic extraction", true},
		{"transcript unavailable", func(c *model.Capture) { c.TranscriptAvailable = false }, "agent transcript", true},
		{"note only", func(c *model.Capture) { c.Notes = []string{"Entire timed out during capture."} }, "Entire timed out during capture.", true},
		{"missing only", func(c *model.Capture) { c.Missing = []string{"original decision rationale"} }, "original decision rationale", true},
		{"nothing missing", func(c *model.Capture) {}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fullState()
			s.Capture = complete
			tc.mutate(&s.Capture)
			out := Status(s, fullLineage())
			if got := has(out, "STATE PARTIALLY RECOVERED"); got != tc.degrade {
				t.Fatalf("partial block present = %v, want %v:\n%s", got, tc.degrade, out)
			}
			if !tc.degrade {
				return
			}
			if !has(out, tc.names) {
				t.Errorf("the degraded input %q is never named:\n%s", tc.names, out)
			}
			// Plan §47 pins the shape of the block; a heading that vanishes
			// when its list happens to be empty leaves the reader without an
			// answer to the question it asks.
			for _, head := range []string{"Verified:", "Unavailable:", "Unknown:", "Safe next action:"} {
				if !has(out, head) {
					t.Errorf("partial block is missing the %q heading:\n%s", head, out)
				}
			}
		})
	}
}

// TestCaptureNotesAlwaysRendered: Capture.Notes are the only human-readable
// record of why capture degraded, and this block is the only place they are
// printed. A note that reaches no view is a gap the product stayed silent
// about.
func TestCaptureNotesAlwaysRendered(t *testing.T) {
	s := fullState()
	s.Capture.Notes = []string{"The OpenClaw adapter replayed events out of order."}
	for _, out := range []string{
		Status(s, fullLineage()),
		Handoff(s, fullLineage()),
		HandoffMarkdown(s, fullLineage()),
		Snapshot(s),
	} {
		if !has(out, s.Capture.Notes[0]) {
			t.Errorf("capture note never reaches the reader:\n%s", out)
		}
	}
}

// TestCheckpointWithoutHashCannotClaimSafeResume: a checkpoint record proves a
// checkpoint existed, not that a resume can be verified against it. Plan §22
// and §48 forbid claiming safe resume from one that carries neither a commit
// nor a state hash, and rendering those fields as simply absent let an
// unverifiable checkpoint read as a safe resume point.
func TestCheckpointWithoutHashCannotClaimSafeResume(t *testing.T) {
	bare := model.Lineage{
		Task:        model.Task{ID: "TASK-4417"},
		Sessions:    []model.SessionNode{{SessionID: "s1", Agent: model.AgentOpenClaw, StartedAt: at(1)}},
		Checkpoints: []model.CheckpointNode{{CheckpointID: "cp-x", SessionID: "s1", Agent: model.AgentOpenClaw, CreatedAt: at(2)}},
	}
	out := Handoff(fullState(), bare)
	for _, want := range []string{
		"commit: not recorded",
		"state hash: not recorded — a resume cannot be verified against this checkpoint",
	} {
		if !has(out, want) {
			t.Errorf("an unverifiable checkpoint does not say %q:\n%s", want, out)
		}
	}

	// A checkpoint whose session is not in the lineage has broken provenance,
	// and the handoff must say so before a reader resumes from it.
	orphan := model.Lineage{
		Task:        model.Task{ID: "TASK-4417"},
		Sessions:    []model.SessionNode{{SessionID: "s1", Agent: model.AgentOpenClaw, StartedAt: at(1)}},
		Checkpoints: []model.CheckpointNode{{CheckpointID: "cp-y", SessionID: "ses-gone", Agent: model.AgentOpenClaw, CommitSHA: "abc123", StateHash: "sha256:1", CreatedAt: at(2)}},
	}
	if out := Handoff(fullState(), orphan); !has(out, "session: ses-gone is not in the recorded lineage") {
		t.Errorf("a checkpoint with broken provenance is presented as clean:\n%s", out)
	}

	// The complete case must stay quiet, or the caveat stops meaning anything.
	good := model.Lineage{
		Task:        model.Task{ID: "TASK-4417"},
		Sessions:    []model.SessionNode{{SessionID: "s1", Agent: model.AgentOpenClaw, StartedAt: at(1)}},
		Checkpoints: []model.CheckpointNode{{CheckpointID: "cp-z", SessionID: "s1", Agent: model.AgentOpenClaw, CommitSHA: "abc123", StateHash: "sha256:1", CreatedAt: at(2)}},
	}
	if out := Handoff(fullState(), good); has(out, "not recorded — a resume cannot be verified") {
		t.Errorf("a fully recorded checkpoint is flagged as unverifiable:\n%s", out)
	}
}

// TestWorkItemsReachTheSnapshot: plan §18 asks "What has happened?".
// CompletedWork and InProgress are the state's answer, and dropping them left
// their citations stranded in EVIDENCE with no claim to support.
func TestWorkItemsReachTheSnapshot(t *testing.T) {
	s := fullState()
	for _, view := range []struct {
		name string
		out  string
	}{
		{"Snapshot", Snapshot(s)},
		{"Status", Status(s, fullLineage())},
		{"Handoff", Handoff(s, fullLineage())},
	} {
		for _, w := range s.CompletedWork {
			if !has(view.out, w.Description) {
				t.Errorf("%s drops completed work %q", view.name, w.Description)
			}
		}
		for _, w := range s.InProgress {
			if !has(view.out, w.Description) {
				t.Errorf("%s drops in-progress work %q", view.name, w.Description)
			}
		}
	}
}

// TestChangedFileProvenanceAndEvidence: a file list read out of git is a
// Level 1 fact; the same list without git is not, and the two must not render
// identically. Evidence attached to a changed file used to be dropped from
// both the row and the EVIDENCE index, because model.AllEvidence does not walk
// ChangedFiles.
func TestChangedFileProvenanceAndEvidence(t *testing.T) {
	s := emptyState()
	s.ChangedFiles = []model.ChangedFile{
		{Path: "billing/pause.go", Status: "M", Insert: 3, Evidence: []model.Evidence{commitEv("deadbeefcafe")}},
	}

	s.Capture.GitAvailable = true
	out := Snapshot(s)
	if !has(out, "["+model.Observed.Label()+"] 1 file changed") {
		t.Errorf("a git-derived file list is not labelled OBSERVED:\n%s", out)
	}
	if !has(out, "evidence: commit deadbeefcafe") {
		t.Errorf("a changed file's citation is dropped from its row:\n%s", out)
	}
	ev := out[strings.Index(out, "\nEVIDENCE\n"):]
	if !has(ev, "commit deadbeefcafe") {
		t.Errorf("a changed file's citation never reaches the EVIDENCE index:\n%s", ev)
	}

	s.Capture.GitAvailable = false
	blind := Snapshot(s)
	if !has(blind, "git state was unavailable — this list was not verified") {
		t.Errorf("an unverified file list is presented as a verified diff:\n%s", blind)
	}
	if has(blind, "["+model.Observed.Label()+"] 1 file changed") {
		t.Errorf("an unverified file list claims to be OBSERVED:\n%s", blind)
	}
}

// TestConstraintEvidenceReachesTheIndex: model.AllEvidence does not walk
// Constraints either, so a citation carried only by a mid-task constraint was
// rendered beside the constraint and then missing from the list that is meant
// to index it.
func TestConstraintEvidenceReachesTheIndex(t *testing.T) {
	s := constraintState()
	s.Constraints[0].Evidence = []model.Evidence{{Kind: model.EvidenceEvent, Ref: "ev-only-here"}}
	out := Status(s, model.Lineage{})
	ev := out[strings.Index(out, "\nEVIDENCE\n"):]
	if !has(ev, "event ev-only-here") {
		t.Errorf("a constraint-only citation never reaches the EVIDENCE index:\n%s", ev)
	}
}

// TestDistinctAgentsAreNotCollapsed: two third-party runtimes must draw two
// distinguishable branches. They once both rendered as "Unknown", so the
// summary read "Unknown → Unknown" and one runtime appeared to have run twice.
// The runtime identity is the one thing the §51 tree exists to show (plan §44).
func TestDistinctAgentsAreNotCollapsed(t *testing.T) {
	l := model.Lineage{
		Task: model.Task{ID: "TASK-1"},
		Sessions: []model.SessionNode{
			{SessionID: "s1", Agent: model.AgentKind("cursor"), Role: model.RoleCoding, StartedAt: at(1)},
			{SessionID: "s2", Agent: model.AgentKind("aider"), Role: model.RoleCoding, StartedAt: at(2)},
		},
	}
	out := Lineage(l)
	for _, want := range []string{"Cursor", "Aider"} {
		if !has(out, want) {
			t.Errorf("lineage never names the runtime %q:\n%s", want, out)
		}
	}
	if has(out, "agents: Unknown → Unknown") {
		t.Errorf("two distinct runtimes render as the same agent:\n%s", out)
	}
	// A recognised kind keeps its clean display name.
	if !has(Lineage(fullLineage()), "agents: OpenClaw → Hermes → Human") {
		t.Errorf("a recognised agent stopped rendering by name")
	}
	// An unrecorded agent is reported as absent, not as the AgentUnknown
	// runtime: "we did not record who" and "the runtime identified itself as
	// unknown" are different statements.
	if got := agentName(""); got != "(agent not recorded)" {
		t.Errorf("an unrecorded agent renders as %q", got)
	}
	if got := agentName(model.AgentUnknown); got != "Unknown" {
		t.Errorf("AgentUnknown renders as %q", got)
	}
}

// TestRenderedStateHashMatchesModelHash: the hash printed on the handoff is
// what a receiver compares against the JSON handoff. If normalising the state
// for rendering changed it, the two projections would disagree about which
// state was transferred (plan §24, §25).
func TestRenderedStateHashMatchesModelHash(t *testing.T) {
	for _, build := range []func() *model.EngineeringState{fullState, partialState, constraintState, emptyState} {
		want := build().Hash()
		out := Handoff(build(), fullLineage())
		if !has(out, want) {
			t.Errorf("handoff does not print model.Hash() %s:\n%s", want, out)
		}
	}
}

// TestEmptyStateAdmitsWhatItDoesNotKnow guards the worst failure mode: an
// empty state rendering as a calm, complete-looking report.
func TestEmptyStateAdmitsWhatItDoesNotKnow(t *testing.T) {
	out := Status(emptyState(), model.Lineage{})
	for _, want := range []string{
		"STATE PARTIALLY RECOVERED",
		"the original intent was not captured",
		"no requirements recorded",
		"no next action was recorded",
		"no evidence recorded",
		model.Unknown.Label(),
	} {
		if !has(out, want) {
			t.Errorf("empty state render never says %q:\n%s", want, out)
		}
	}
	// The label form is what matters: the word also appears inside the safe
	// next action ("anything above that is not marked OBSERVED"), which is
	// guidance rather than a claim.
	if has(out, "["+model.Observed.Label()+"]") {
		t.Errorf("empty state render claims something is OBSERVED:\n%s", out)
	}
}

// TestConfidenceLabels asserts the §12 distinction actually reaches the page.
func TestConfidenceLabels(t *testing.T) {
	out := Status(fullState(), fullLineage())
	for _, want := range []string{
		model.Observed.Label(),
		model.Inferred.Label(),
		model.Unknown.Label(),
		model.Recommended.Label(),
	} {
		if !has(out, "["+want+"]") {
			t.Errorf("status output never renders the %s label", want)
		}
	}
	// R4 has no evidence at all. It must not be dressed up as observed.
	if !has(out, "[UNKNOWN] R4") {
		t.Errorf("a requirement with no evidence is not labelled UNKNOWN:\n%s", out)
	}
	if !has(out, "evidence: none recorded — this claim is unverified") {
		t.Errorf("a claim with no evidence does not say so:\n%s", out)
	}
}

// TestNextActionIsAlwaysRecommended pins the one direction the label must not
// overstate in: a proposal must never render as an observation, even when the
// state was built with the wrong confidence on it (plan §12).
func TestNextActionIsAlwaysRecommended(t *testing.T) {
	s := fullState()
	s.NextActions[0].Confidence = model.Observed
	out := Snapshot(s)
	idx := strings.Index(out, "NEXT ACTION")
	if idx < 0 {
		t.Fatalf("no NEXT ACTION section:\n%s", out)
	}
	next := out[idx:]
	if has(next, "→ ["+model.Observed.Label()+"]") {
		t.Errorf("a next action rendered as OBSERVED:\n%s", next)
	}
	if !has(next, "→ ["+model.Recommended.Label()+"]") {
		t.Errorf("a next action did not render as RECOMMENDED:\n%s", next)
	}
}

// TestDerivedStatusDisagreementIsSurfaced: when the stored status and the
// evidence disagree, hiding the disagreement would let a stale "complete" ride
// on top of a failing test (plan §17).
func TestDerivedStatusDisagreementIsSurfaced(t *testing.T) {
	s := fullState()
	s.Status = model.StatusComplete
	out := Snapshot(s)
	if !has(out, "derived from evidence: "+string(s.DeriveStatus())) {
		t.Errorf("a stored status contradicted by the evidence is not flagged:\n%s", out)
	}
}

// TestSupersededDecisionsRetained: plan §32 keeps replaced decisions rather
// than deleting them, so the view has to show them somewhere.
func TestSupersededDecisionsRetained(t *testing.T) {
	out := Snapshot(fullState())
	if !has(out, "SUPERSEDED DECISIONS") {
		t.Fatalf("superseded decisions are dropped from the snapshot:\n%s", out)
	}
	if !has(out, "superseded by: D1") {
		t.Errorf("a superseded decision does not name what replaced it:\n%s", out)
	}
	di := strings.Index(out, "\nDECISIONS\n")
	si := strings.Index(out, "\nSUPERSEDED DECISIONS\n")
	if di < 0 || si < 0 || si < di {
		t.Errorf("superseded decisions are not kept separate from active ones")
	}
	if has(out[di:si], "D0 Derive pause from a null renewal date.") {
		t.Errorf("a superseded decision is presented as active:\n%s", out[di:si])
	}
}

// TestConstraintIsAdditive: a mid-task constraint annotates the original
// intent, it does not replace it (plan §41). Both must survive the render.
func TestConstraintIsAdditive(t *testing.T) {
	s := constraintState()
	out := Status(s, model.Lineage{})
	if !has(out, s.Task.OriginalIntent) {
		t.Errorf("the original intent was dropped once a constraint arrived:\n%s", out)
	}
	if !has(out, "CONSTRAINTS ADDED MID-TASK") {
		t.Fatalf("no constraint section:\n%s", out)
	}
	for _, want := range []string{
		"C1 Paused subscriptions must auto-resume",
		"affects requirements: R2, R3",
		"invalidates assumptions: A pause lasts until the customer resumes it.",
		"changed by constraint: C1",
		"invalidated by later evidence",
	} {
		if !has(out, want) {
			t.Errorf("constraint render never says %q:\n%s", want, out)
		}
	}
}

// TestHandoffProjectionsAgree is Rule 10 in test form: the plain-text and
// Markdown handoffs are two renderings of one state and must never disagree
// about a fact.
func TestHandoffProjectionsAgree(t *testing.T) {
	cases := []struct {
		name  string
		state func() *model.EngineeringState
		line  func() model.Lineage
		facts []string
	}{
		{"full", fullState, fullLineage, []string{
			"Make webhook handling idempotent under replay.",
			"Mutate the renewal status directly on pause.",
			"billing/webhooks.go",
			"TestDuplicateWebhook",
		}},
		{"partial", partialState, partialLineage, []string{
			"STATE PARTIALLY RECOVERED",
			"original decision rationale",
			"entire checkpoint history",
		}},
		{"constraint", constraintState, func() model.Lineage { return model.Lineage{} }, []string{
			"Paused subscriptions must auto-resume after 30 days unless cancelled.",
			"affects requirements: R2, R3",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := Handoff(tc.state(), tc.line())
			md := HandoffMarkdown(tc.state(), tc.line())
			for _, f := range tc.facts {
				if !has(text, f) {
					t.Errorf("plain-text handoff is missing %q", f)
				}
				if !has(md, f) {
					t.Errorf("markdown handoff is missing %q", f)
				}
			}
			// Section headers must match one-for-one, or the two audiences are
			// reading structurally different documents.
			if got, want := markdownHeaders(md), textHeaders(text); !equalStrings(got, want) {
				t.Errorf("section sets differ:\nmarkdown: %v\ntext:     %v", got, want)
			}
		})
	}
}

// TestNoCheckpointIsReported: "no checkpoint" must be stated, because it means
// a resume cannot be verified against a known-good state (plan §48).
func TestNoCheckpointIsReported(t *testing.T) {
	out := Handoff(partialState(), partialLineage())
	if !has(out, "no checkpoint recorded for this task") {
		t.Errorf("a task with no checkpoint does not say so:\n%s", out)
	}
	rec := Receipt(bareReceipt())
	if !has(rec, "not anchored to a checkpoint") {
		t.Errorf("a receipt with no source checkpoint does not say so:\n%s", rec)
	}
	if !has(rec, "cannot prove which state was handed over") {
		t.Errorf("a receipt with no state hash does not say so:\n%s", rec)
	}
}

// TestLineageTreeGroupsByAgent covers the §51 tree, including the two shapes
// that are easy to drop on the floor: an interrupted session and a checkpoint
// whose session is unknown.
func TestLineageTreeGroupsByAgent(t *testing.T) {
	out := Lineage(fullLineage())
	for _, want := range []string{
		"├── OpenClaw",
		"├── Hermes",
		"└── (checkpoints not attributable to a recorded session)",
		"interrupted",
		"(no checkpoint)",
		"cp-d",
		// cp-c records no session at all; cp-d records "ses-gone", which is
		// simply not in the lineage. Collapsing both to one phrase would state
		// something false about cp-d and throw away the id a reader needs.
		"cp-c \"adapter lost the session id\" 2026-01-15T10:42:00Z (no session recorded)",
		"cp-d 2026-01-15T10:44:00Z (session ses-gone is not in the recorded lineage)",
	} {
		if !has(out, want) {
			t.Errorf("lineage tree is missing %q:\n%s", want, out)
		}
	}
	// Grouping is by agent: each runtime appears exactly once as a branch.
	if n := strings.Count(out, "── OpenClaw\n"); n != 1 {
		t.Errorf("OpenClaw appears as %d branches, want 1:\n%s", n, out)
	}
	if !has(out, "agents: OpenClaw → Hermes → Human") {
		t.Errorf("lineage summary does not show the cross-agent chain:\n%s", out)
	}
}

// TestLineageEmptyDoesNotImplyNoWork: an empty lineage is a capture gap. It
// must not read as a finding that nothing happened (plan §48).
func TestLineageEmptyDoesNotImplyNoWork(t *testing.T) {
	out := Lineage(model.Lineage{})
	if !has(out, "no sessions or checkpoints are recorded") {
		t.Errorf("empty lineage is silent about being empty:\n%s", out)
	}
	if !has(out, "not evidence that no work happened") {
		t.Errorf("empty lineage does not distinguish a gap from a finding:\n%s", out)
	}
}

// TestEvidenceListTruncationIsAnnounced: shortening a citation list silently
// would understate how much support a claim has.
func TestEvidenceListTruncationIsAnnounced(t *testing.T) {
	all := fullState().AllEvidence()
	if len(all) <= 3 {
		t.Fatalf("fixture needs more than 3 evidence records, has %d", len(all))
	}
	out := EvidenceList(all, 3)
	if n := strings.Count(out, "[L"); n != 3 {
		t.Errorf("limit 3 rendered %d citations", n)
	}
	if !has(out, "not shown (limit 3)") {
		t.Errorf("truncation is not announced:\n%s", out)
	}
	full := EvidenceList(all, 0)
	if has(full, "not shown") {
		t.Errorf("an unlimited list claims truncation:\n%s", full)
	}
	if strings.Count(full, "[L") < len(model.DedupeEvidence(all)) {
		t.Errorf("an unlimited list dropped citations:\n%s", full)
	}
	if !has(EvidenceList(nil, 0), "no evidence recorded") {
		t.Errorf("an empty evidence list renders as nothing at all")
	}
}

// TestPlainTextIsPipeable: no ANSI escapes, no carriage returns, exactly one
// trailing newline.
func TestPlainTextIsPipeable(t *testing.T) {
	outs := map[string]string{
		"status":   Status(fullState(), fullLineage()),
		"handoff":  Handoff(fullState(), fullLineage()),
		"snapshot": Snapshot(fullState()),
		"lineage":  Lineage(fullLineage()),
		"receipt":  Receipt(fullReceipt()),
		"evidence": EvidenceList(fullState().AllEvidence(), 0),
	}
	for name, out := range outs {
		if strings.Contains(out, "\x1b") {
			t.Errorf("%s contains an ANSI escape", name)
		}
		if strings.Contains(out, "\r") {
			t.Errorf("%s contains a carriage return", name)
		}
		if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
			t.Errorf("%s does not end in exactly one newline", name)
		}
	}
}

// TestWrapWidth holds the 72-column contract for prose views. The §51 tree is
// excluded on purpose: its geometry is fixed by the box-drawing prefixes and
// wrapping it would break the drawing.
func TestWrapWidth(t *testing.T) {
	outs := map[string]string{
		"status full":        Status(fullState(), fullLineage()),
		"status partial":     Status(partialState(), partialLineage()),
		"status constraint":  Status(constraintState(), model.Lineage{}),
		"status empty":       Status(emptyState(), model.Lineage{}),
		"handoff full":       Handoff(fullState(), fullLineage()),
		"handoff partial":    Handoff(partialState(), partialLineage()),
		"snapshot full":      Snapshot(fullState()),
		"receipt":            Receipt(fullReceipt()),
		"receipt bare":       Receipt(bareReceipt()),
		"evidence":           EvidenceList(fullState().AllEvidence(), 0),
		"status overstated":  Status(overstatedState(), fullLineage()),
		"handoff overstated": Handoff(overstatedState(), fullLineage()),
		"status degraded":    Status(degradedFullState(), fullLineage()),
		"handoff degraded":   Handoff(degradedFullState(), fullLineage()),
		// The only checkpoint here carries neither a commit nor a state hash,
		// which is what produces the longest caveat notes in the package.
		"handoff bare checkpoint": Handoff(fullState(), model.Lineage{
			Task:        model.Task{ID: "TASK-4417"},
			Sessions:    []model.SessionNode{{SessionID: "s1", Agent: model.AgentOpenClaw, StartedAt: at(1)}},
			Checkpoints: []model.CheckpointNode{{CheckpointID: "cp-x", Agent: model.AgentOpenClaw, CreatedAt: at(2)}},
		}),
	}
	for name, out := range outs {
		for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if n := runeLen(line); n > Width {
				t.Errorf("%s line %d is %d columns (limit %d): %q", name, i+1, n, Width, line)
			}
		}
	}
}

// TestWrapLines covers the wrapper directly, including the case the golden
// files cannot reach: a token longer than the column limit. Commit shas, file
// paths and evidence refs must survive intact even when they overflow, because
// a broken citation is not a citation.
func TestWrapLines(t *testing.T) {
	long := strings.Repeat("x", 100)
	cases := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"empty", "", 20, nil},
		{"whitespace only", "   \t \n ", 20, nil},
		{"fits", "one two three", 20, []string{"one two three"}},
		{"wraps greedily", "aaaa bbbb cccc dddd", 10, []string{"aaaa bbbb", "cccc dddd"}},
		{"exact fit", "abcde fghij", 11, []string{"abcde fghij"}},
		{"one over", "abcde fghij", 10, []string{"abcde", "fghij"}},
		{"long token unbroken", long, 20, []string{long}},
		{"long token isolated", "short " + long + " tail", 20, []string{"short", long, "tail"}},
		{"collapses runs of space", "a    b", 20, []string{"a b"}},
		{"degenerate width", "aa bb", 0, []string{"aa bb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapLines(tc.in, tc.width)
			if !equalStrings(got, tc.want) {
				t.Errorf("wrapLines(%q, %d) = %v, want %v", tc.in, tc.width, got, tc.want)
			}
		})
	}
}

// TestLabelNeverBlank: every claim path must yield one of the four §12 labels.
// A blank label would read as an unqualified fact.
func TestLabelNeverBlank(t *testing.T) {
	valid := map[string]bool{
		model.Observed.Label():    true,
		model.Inferred.Label():    true,
		model.Unknown.Label():     true,
		model.Recommended.Label(): true,
	}
	cases := []struct {
		name string
		conf model.Confidence
		ev   []model.Evidence
		want string
	}{
		{"explicit is honoured when the evidence carries it", model.Observed, []model.Evidence{commitEv("abc")}, model.Observed.Label()},
		{"a weaker explicit rating is never raised", model.Inferred, []model.Evidence{commitEv("abc")}, model.Inferred.Label()},
		{"a stronger explicit rating is capped", model.Observed, nil, model.Unknown.Label()},
		{"observed is capped to inferred by statement evidence", model.Observed, []model.Evidence{promptEv("said so")}, model.Inferred.Label()},
		{"recommended is not weighed against evidence", model.Recommended, nil, model.Recommended.Label()},
		{"no evidence is unknown", "", nil, model.Unknown.Label()},
		{"garbage confidence falls back", model.Confidence("wishful"), nil, model.Unknown.Label()},
		{"repository evidence is observed", "", []model.Evidence{commitEv("abc")}, model.Observed.Label()},
		{"checkpoint evidence is observed", "", []model.Evidence{cpEv("cp-a")}, model.Observed.Label()},
		{"statement evidence is inferred", "", []model.Evidence{promptEv("said so")}, model.Inferred.Label()},
		{"model inference is inferred", "", []model.Evidence{inferenceEv("guess")}, model.Inferred.Label()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := label(tc.conf, tc.ev)
			if got != tc.want {
				t.Errorf("label = %q, want %q", got, tc.want)
			}
			if !valid[got] {
				t.Errorf("label %q is not one of the four §12 labels", got)
			}
		})
	}
}

// TestOverstatedConfidenceIsCapped: §12 defines OBSERVED as "directly
// supported by repository/checkpoint/test/event evidence", so a claim recorded
// as OBSERVED with no evidence is not an observation whatever the state says.
// The strong marker used to be printed verbatim, directly above a citation
// line reading "evidence: none recorded — this claim is unverified" — a false
// signal in exactly the place the spec requires a reader to tell a fact from a
// guess at a glance.
func TestOverstatedConfidenceIsCapped(t *testing.T) {
	s := emptyState()
	s.Capture.GitAvailable = true
	s.Requirements = []model.Requirement{
		{ID: "R1", Description: "Everything works.", Status: model.ReqComplete, Confidence: model.Observed},
	}
	s.Assumptions = []model.Assumption{{Statement: "The cache is warm.", Confidence: model.Observed}}
	s.Risks = []model.Risk{{Description: "Nothing to worry about.", Confidence: model.Observed}}
	s.Rejected = []model.RejectedApproach{{ID: "RA1", Approach: "A dead end.", Confidence: model.Observed}}
	s.CompletedWork = []model.WorkItem{{Description: "Shipped the whole thing.", Confidence: model.Observed}}
	s.InProgress = []model.WorkItem{{Description: "Polishing.", Confidence: model.Observed}}

	// Every claim type that carries a Confidence field, in every view that
	// renders it. ASSUMPTIONS is snapshot-only, so it is asserted separately.
	shared := []string{
		"[UNKNOWN] R1 Everything works.",
		"[UNKNOWN] Nothing to worry about.",
		"[UNKNOWN] RA1 A dead end.",
		"[UNKNOWN] Shipped the whole thing.",
		"[UNKNOWN] Polishing.",
	}
	views := []struct {
		name string
		out  string
		want []string
	}{
		{"Snapshot", Snapshot(s), append(append([]string(nil), shared...), "[UNKNOWN] The cache is warm.")},
		{"Status", Status(s, model.Lineage{}), append(append([]string(nil), shared...), "[UNKNOWN] The cache is warm.")},
		{"Handoff", Handoff(s, model.Lineage{}), shared},
		{"HandoffMarkdown", HandoffMarkdown(s, model.Lineage{}), nil},
	}
	for _, v := range views {
		if has(v.out, "["+model.Observed.Label()+"] R1 Everything works.") {
			t.Errorf("%s: an unevidenced claim still renders as OBSERVED:\n%s", v.name, v.out)
		}
		for _, want := range v.want {
			if !has(v.out, want) {
				t.Errorf("%s: expected %q in:\n%s", v.name, want, v.out)
			}
		}
		// The overstatement is reported, not quietly corrected: the machine
		// handoff is built from the same object and still says "observed", and
		// the two projections have to stay reconcilable (Rule 10, plan §24).
		if !has(v.out, "recorded as OBSERVED, but the evidence supports no more than UNKNOWN") {
			t.Errorf("%s: the downgrade is applied silently:\n%s", v.name, v.out)
		}
	}

	// Statement-level evidence supports INFERRED and no more.
	s2 := emptyState()
	s2.Assumptions = []model.Assumption{{
		Statement:  "The provider sends stable ids.",
		Confidence: model.Observed,
		Evidence:   []model.Evidence{promptEv("the agent said so")},
	}}
	out := Snapshot(s2)
	if !has(out, "["+model.Inferred.Label()+"] The provider sends stable ids.") {
		t.Errorf("statement evidence did not cap OBSERVED to INFERRED:\n%s", out)
	}

	// A rating weaker than the evidence permits is left alone: an extractor may
	// be less sure than its evidence allows, never more.
	s3 := emptyState()
	s3.Requirements = []model.Requirement{{
		ID: "R9", Description: "Cautiously reported.", Status: model.ReqPartial,
		Confidence: model.Inferred, Evidence: []model.Evidence{commitEv("abc123")},
	}}
	if out := Snapshot(s3); !has(out, "["+model.Inferred.Label()+"] R9 Cautiously reported.") {
		t.Errorf("a deliberately cautious rating was raised to match its evidence:\n%s", out)
	}
}

// TestGraphVerificationIsNotClaimedWhenGraphWasUnavailable: plan §48 says to
// mark graph verification unavailable and not pretend it occurred. A stale
// GraphVerified flag used to print "impact analysis backs this action" in the
// same document whose capture block said the graph was unreachable.
func TestGraphVerificationIsNotClaimedWhenGraphWasUnavailable(t *testing.T) {
	cases := []struct {
		name      string
		verified  bool
		available bool
		want      string
		notWant   string
	}{
		{"verified with a reachable graph", true, true, "impact analysis backs this action", "could not be confirmed"},
		{"verified flag but no graph", true, false, "the graph was unavailable during this capture, so the impact analysis could not be confirmed", "impact analysis backs this action"},
		{"not verified", false, true, "blast radius is unconfirmed", "impact analysis backs this action"},
		{"not verified and no graph", false, false, "blast radius is unconfirmed", "impact analysis backs this action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fullState()
			s.Capture.GraphAvailable = tc.available
			s.NextActions = []model.NextAction{{
				Description: "Make the webhook handler idempotent.",
				Priority:    1, Confidence: model.Recommended, GraphVerified: tc.verified,
			}}
			out := Snapshot(s)
			if !has(out, tc.want) {
				t.Errorf("expected %q:\n%s", tc.want, out)
			}
			if has(out, tc.notWant) {
				t.Errorf("must not say %q:\n%s", tc.notWant, out)
			}
		})
	}
}

// TestUnknownTimestampsAreNotFabricated: a zero time must print "unknown"
// rather than a year-1 date that looks like a real observation.
func TestUnknownTimestampsAreNotFabricated(t *testing.T) {
	if got := ts(time.Time{}); got != "unknown" {
		t.Errorf("zero time rendered as %q", got)
	}
	if got := ts(at(0)); got != "2026-01-15T09:00:00Z" {
		t.Errorf("timestamp rendered as %q", got)
	}
	out := Receipt(bareReceipt())
	if has(out, "0001-01-01") {
		t.Errorf("receipt fabricated a timestamp:\n%s", out)
	}
	if !has(out, "unknown") {
		t.Errorf("receipt does not admit an unknown creation time:\n%s", out)
	}
}

// TestFilesGapDistinguishedFromEmptyDiff: "nothing changed" and "we could not
// look" are different claims (plan §33).
func TestFilesGapDistinguishedFromEmptyDiff(t *testing.T) {
	seen := emptyState()
	seen.Capture.GitAvailable = true
	if out := Snapshot(seen); !has(out, "["+model.Observed.Label()+"] no files changed") {
		t.Errorf("an observed empty diff is not reported as observed:\n%s", out)
	}
	blind := emptyState()
	if out := Snapshot(blind); !has(out, "changed files were not captured") {
		t.Errorf("an uninspected repository is reported as having no changes:\n%s", out)
	}
}

// TestAbsenceOfFailureIsNotSuccess: with no parsed test results, a handoff
// must not let an empty FAILED section read as a green build.
func TestAbsenceOfFailureIsNotSuccess(t *testing.T) {
	out := Handoff(partialState(), partialLineage())
	if !has(out, "absence of failure is not evidence of success") {
		t.Errorf("an unparsed test suite renders as no failures:\n%s", out)
	}
	if !has(out, "test results were not captured; passing cannot be claimed") {
		t.Errorf("missing test results are not called out:\n%s", out)
	}
}

// --- helpers -------------------------------------------------------------

// has reports whether out contains the phrase, ignoring where the 72-column
// wrapper happened to break the line. Without this every honesty assertion
// would double as a brittle assertion about line breaks.
func has(out, phrase string) bool { return strings.Contains(flat(out), flat(phrase)) }

func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// textHeaders lists the CAPS section headers of a plain-text view. The
// document title is skipped: it is underlined with a rule rather than being a
// section, and Markdown renders it as an H1.
func textHeaders(s string) []string {
	lines := strings.Split(s, "\n")
	start := 0
	if len(lines) > 1 && strings.HasPrefix(lines[1], "─") {
		start = 2
	}
	var out []string
	for _, line := range lines[start:] {
		if line == "" || line != strings.ToUpper(line) {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.ContainsAny(line, "─•✓✗⚠→?[") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func markdownHeaders(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "## ") {
			out = append(out, strings.TrimPrefix(line, "## "))
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reverseReq(in []model.Requirement) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseTest(in []model.TestResult) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseFile(in []model.ChangedFile) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseDecision(in []model.Decision) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseAction(in []model.NextAction) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseString(in []string) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseSession(in []model.SessionNode) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseCheckpoint(in []model.CheckpointNode) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

func reverseHandoff(in []model.HandoffNode) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}
