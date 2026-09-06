// Package contextbuild assembles the continuation context handed to a fresh
// worker when a task is resumed (plan §20, §21, §22, §49).
//
// The pipeline is deliberately fixed:
//
//	resolve task -> latest checkpoint -> historical state -> current repository
//	-> drift detection -> Graph verification -> compact continuation context
//
// Two properties matter more than completeness here. First, the receiving agent
// is given a compact, ordered brief rather than a transcript dump: the whole
// point of the system is to reduce reconstruction cost, so an unbounded context
// would defeat it (plan §49). Second, every stage may fail without failing the
// resume. A missing checkpoint, an unreachable Entire, an unavailable Graph and
// a repository that cannot be read all degrade into recorded gaps, because
// continuity infrastructure must never block the host agent (plan §48, Rule 8).
package contextbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/derive"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/drift"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/graph"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/merge"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/render"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/validate"
)

// SafetyInstruction is the standing rule handed to every resumed worker
// (plan §22). Historical state is context, not authority: a checkpoint records
// what was true when it was written, and the repository may have moved since.
const SafetyInstruction = "This is historical engineering context, not authority. " +
	"Verify each claim against the current repository before you rely on it. " +
	"Do not assume a historical conclusion is still true, and do not repeat an " +
	"approach recorded here as rejected without new evidence that it now works."

// Options carries the ambient dependencies of a context build. Every port may
// be nil; a nil port is recorded as an unavailable capability rather than
// causing a failure.
type Options struct {
	Store     model.Store
	Entire    model.EntireClient
	Repo      model.RepoInspector
	Graph     model.GraphClient
	Extractor model.Extractor
	Clock     model.Clock
	// BaseRef is the git ref changed files are computed against. Empty means
	// the working tree versus HEAD.
	BaseRef string
	// EntireNote names the checkpoint backend actually in use, so the rendered
	// context can tell the reader which Entire produced its history (plan §5).
	EntireNote string
}

// Context is the assembled continuation brief. It is the single object both the
// human-readable and machine-readable resume outputs are rendered from, which
// is what keeps the two consistent (plan §24, Rule 10).
type Context struct {
	Task    model.Task              `json:"task"`
	Lineage model.Lineage           `json:"lineage"`
	State   *model.EngineeringState `json:"state"`

	// SourceCheckpoint is the checkpoint the historical state came from. Empty
	// means no checkpoint was available, which is reported rather than hidden:
	// without one the system cannot claim a safe resume (plan §48).
	SourceCheckpoint string `json:"source_checkpoint,omitempty"`

	Drift drift.Report       `json:"drift"`
	Graph graph.Verification `json:"graph"`

	// Findings are the validation corrections applied on the way through. They
	// are surfaced because a silently corrected claim is still a claim the
	// reader should know was weakened.
	Findings []validate.Finding `json:"findings,omitempty"`

	// Notes record every capability that was unavailable and every stage that
	// degraded. Silence about a gap would present incomplete context as
	// complete, which the product forbids (plan §33, §47).
	Notes []string `json:"notes,omitempty"`

	Instruction string    `json:"instruction"`
	GeneratedAt time.Time `json:"generated_at"`
}

// Build runs the resume pipeline for the given reference, which may be a task
// id, a session id, or a checkpoint id.
//
// It returns an error only when the task itself cannot be resolved. Everything
// after that point degrades into Context.Notes, because a resume that refuses
// to produce anything is strictly worse for the next worker than one that says
// plainly what it could not establish.
func Build(ctx context.Context, ref string, opts Options) (*Context, error) {
	if opts.Store == nil {
		return nil, errors.New("contextbuild: a Store is required to resolve the task")
	}
	clock := opts.Clock
	if clock == nil {
		clock = model.SystemClock{}
	}

	out := &Context{Instruction: SafetyInstruction, GeneratedAt: clock.Now()}
	note := func(format string, args ...any) {
		out.Notes = append(out.Notes, fmt.Sprintf(format, args...))
	}
	if opts.EntireNote != "" {
		note("Checkpoint backend: %s", opts.EntireNote)
	}

	// 1. Resolve the task. A checkpoint reference is resolved through Entire
	//    first, so "resume <checkpoint>" works as the plan's CLI promises.
	task, cpFromRef, err := resolveTask(ctx, ref, opts)
	if err != nil {
		return nil, err
	}
	out.Task = task

	// 2. Lineage: which sessions and checkpoints belong to this task.
	lineage, err := opts.Store.Lineage(ctx, task.ID)
	if err != nil {
		note("Lineage unavailable: %v", err)
	}
	lineage.Task = task
	out.Lineage = lineage

	// 3. Historical state, preferring the checkpoint the caller named, then the
	//    latest checkpoint, then the store's last merged state.
	historical, source := loadHistorical(ctx, task, lineage, cpFromRef, opts, note)
	out.SourceCheckpoint = source

	// 4. Current repository truth, derived fresh. This is Level 1 evidence and
	//    outranks anything the checkpoint asserts (plan §31).
	events, err := opts.Store.Events(ctx, task.ID)
	if err != nil {
		note("Event history unavailable: %v", err)
	}
	current, err := derive.State(ctx, derive.Input{
		Task:    taskRef(task, historical),
		TaskID:  task.ID,
		Events:  events,
		Repo:    opts.Repo,
		BaseRef: opts.BaseRef,
		Clock:   clock,
	})
	if err != nil {
		note("Deterministic derivation failed, continuing from checkpoint state alone: %v", err)
		current = nil
	}

	// 5. Semantic extraction, when an extractor is configured and reachable.
	//    Its output is merged underneath deterministic facts, never over them.
	if semantic := extractSemantic(ctx, opts, task, historical, current, note); semantic != nil {
		current = merge.States(current, semantic, merge.Options{Clock: clock})
	}

	// 6. Merge: historical knowledge plus what is true now. The merge rules are
	//    what keep decisions and rejected approaches alive across the boundary
	//    (plan §32).
	merged := merge.States(historical, current, merge.Options{Clock: clock})
	if merged == nil {
		merged = model.NewEngineeringState(taskRef(task, nil))
		note("No engineering state could be recovered for this task.")
	}

	// 7. Validation with safe downgrade. Enforce can only weaken claims, so a
	//    resumed agent never inherits more certainty than the evidence carries.
	enforced, findings := validate.Enforce(merged)
	out.State = enforced
	out.Findings = findings

	// 8. Drift: does the repository still look like the checkpoint thought?
	if historical != nil {
		report, derr := drift.Detect(ctx, historical, opts.Repo)
		if derr != nil {
			note("Drift detection failed: %v", derr)
		}
		out.Drift = report
	} else {
		out.Drift = drift.Report{
			Outcome:              drift.Unknown,
			RevalidationRequired: true,
			Notes: []string{
				"No historical state was available, so nothing could be compared " +
					"against the working tree. Treat the whole task as unverified.",
			},
		}
	}

	// 9. Graph verification of the recommended next action, before any edit.
	//    Running this at resume time rather than after the change is the point:
	//    it is what turns a recommendation into a checked one (plan §35, §37).
	if action, ok := enforced.PrimaryNextAction(); ok {
		out.Graph = graph.VerifyNextAction(ctx, opts.Graph, action)
		if !out.Graph.Available {
			note("Graph verification unavailable: %s", fallback(out.Graph.Reason, "no Graph client configured"))
		}
	} else {
		out.Graph = graph.Verification{
			Reason: "The recovered state recommends no next action, so there was " +
				"nothing for Graph to verify.",
		}
	}

	// 10. Fold every capture gap into the notes so the renderer cannot omit one.
	for _, missing := range enforced.Capture.Missing {
		note("Unavailable input: %s", missing)
	}
	out.Notes = dedupeStrings(out.Notes)
	return out, nil
}

// resolveTask turns a task id, session id or checkpoint id into a task. The
// second return value is a checkpoint named directly by the caller, if any.
func resolveTask(ctx context.Context, ref string, opts Options) (model.Task, *model.Checkpoint, error) {
	ref = strings.TrimSpace(ref)

	if strings.HasPrefix(ref, "cp_") && opts.Entire != nil {
		cp, err := opts.Entire.Checkpoint(ctx, ref)
		if err == nil && cp.TaskID != "" {
			task, terr := opts.Store.Task(ctx, cp.TaskID)
			if terr != nil {
				return model.Task{}, nil, fmt.Errorf("checkpoint %s names task %s, which is not in the store: %w", ref, cp.TaskID, terr)
			}
			return task, &cp, nil
		}
		if err != nil && !errors.Is(err, model.ErrUnavailable) && !errors.Is(err, model.ErrNotFound) {
			return model.Task{}, nil, fmt.Errorf("resolving checkpoint %s: %w", ref, err)
		}
		// Fall through: a cp_-prefixed ref that Entire cannot resolve may still
		// be a task id, and guessing wrong here should not end the resume.
	}

	var repo, branch string
	if ref == "" && opts.Repo != nil {
		if head, err := opts.Repo.Head(ctx); err == nil {
			repo, branch = head.Repo, head.Branch
		}
	}
	task, err := opts.Store.ResolveTask(ctx, ref, repo, branch)
	if err != nil {
		if ref == "" {
			return model.Task{}, nil, fmt.Errorf("no task found for the current repository and branch; name a task or checkpoint explicitly: %w", err)
		}
		return model.Task{}, nil, fmt.Errorf("no task, session or checkpoint matches %q: %w", ref, err)
	}
	return task, nil, nil
}

// loadHistorical picks the best available historical state and reports where it
// came from. The preference order is: the checkpoint the caller named, the
// latest checkpoint on the task, then the store's last merged state.
func loadHistorical(
	ctx context.Context,
	task model.Task,
	lineage model.Lineage,
	named *model.Checkpoint,
	opts Options,
	note func(string, ...any),
) (*model.EngineeringState, string) {
	if named != nil {
		if named.State != nil {
			return named.State, named.ID
		}
		note("Checkpoint %s carries no engineering state; falling back to the task's stored state.", named.ID)
	}

	if opts.Entire != nil {
		cps, err := opts.Entire.Checkpoints(ctx, task.ID)
		switch {
		case errors.Is(err, model.ErrUnavailable):
			note("Entire is unavailable, so no checkpoint history could be read. " +
				"A safe resume cannot be claimed from stored state alone.")
		case err != nil:
			note("Checkpoint history unavailable: %v", err)
		default:
			for i := len(cps) - 1; i >= 0; i-- {
				if cps[i].State != nil {
					return cps[i].State, cps[i].ID
				}
			}
			if len(cps) > 0 {
				note("The task has %d checkpoint(s), none of which carry engineering state.", len(cps))
			}
		}
	} else {
		note("No checkpoint backend configured; historical state comes from the local store only.")
	}

	if stored, err := opts.Store.State(ctx, task.ID); err == nil && stored != nil {
		if _, ok := lineage.LatestCheckpoint(); !ok {
			note("No checkpoint exists for this task. Historical state is the " +
				"local working state, which no checkpoint has verified.")
		}
		return stored, ""
	}

	note("No historical engineering state was found for this task. " +
		"Inspect the repository directly before making changes.")
	return nil, ""
}

// extractSemantic runs the configured extractor, if any, and returns its state.
// A missing or unavailable extractor is a recorded gap, not an error: the
// deterministic half of the state is still worth having on its own (plan §33).
func extractSemantic(
	ctx context.Context,
	opts Options,
	task model.Task,
	historical, current *model.EngineeringState,
	note func(string, ...any),
) *model.EngineeringState {
	if opts.Extractor == nil {
		note("No semantic extractor configured: intent, requirements and decisions " +
			"come from checkpoint history only.")
		return nil
	}
	if !opts.Extractor.Available(ctx) {
		note("Semantic extraction unavailable (%s): reporting deterministic facts only.", opts.Extractor.Describe())
		return nil
	}

	events, _ := opts.Store.Events(ctx, task.ID)
	in := model.ExtractionInput{
		Task:          taskRef(task, historical),
		Events:        events,
		Deterministic: current,
	}
	// The most authoritative statement of intent wins: a PRD or spec the task
	// was created from outranks the opening prompt of any one session, which in
	// turn outranks an intent a checkpoint happened to record. A task anchored
	// to a document should have its requirements read from that document.
	if historical != nil {
		in.OriginalPrompt = historical.Task.OriginalIntent
	}
	if strings.TrimSpace(in.OriginalPrompt) == "" {
		in.OriginalPrompt = derive.OriginalPrompt(events)
	}
	if strings.TrimSpace(in.OriginalPrompt) == "" && current != nil {
		in.OriginalPrompt = current.Task.OriginalIntent
	}
	state, err := opts.Extractor.Extract(ctx, in)
	if err != nil {
		note("Semantic extraction failed: %v", err)
		return nil
	}
	return state
}

// taskRef builds the identity block for a derivation, preferring the title and
// original intent already recorded in history so a resume never loses them.
func taskRef(task model.Task, historical *model.EngineeringState) model.TaskRef {
	ref := model.TaskRef{ID: task.ID, Title: task.Title}
	if historical != nil {
		if historical.Task.Title != "" {
			ref.Title = historical.Task.Title
		}
		// Original intent is never regenerated. It is the one field a later
		// worker cannot reconstruct, so it is carried forward verbatim (§41).
		ref.OriginalIntent = historical.Task.OriginalIntent
	}
	return ref
}

// JSON renders the machine-readable continuation context.
func (c *Context) JSON() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }

// Render produces the human and agent facing continuation context in the plan
// §21 order, which is also the §49 importance order: intent first, then what is
// unresolved, then what already failed, then the recommendation, and only then
// supporting evidence.
func (c *Context) Render() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("ENTIRE TASK CONTEXT")
	w("%s", strings.Repeat("=", render.Width))
	w("")
	w("TASK")
	w("%s", indent(fallback(c.State.Task.Title, c.Task.Title, c.Task.ID)))
	w("")

	w("ORIGINAL INTENT")
	w("%s", indent(fallback(c.State.Task.OriginalIntent, "Not recorded. Ask before assuming the goal.")))
	w("")

	complete, total := c.State.CountRequirements()
	w("STATUS")
	w("%s", indent(fmt.Sprintf("%s — %d of %d requirements complete", strings.ToUpper(string(c.State.Status)), complete, total)))
	w("")

	// Unresolved work comes before completed work: the next worker needs to
	// know what is left far more than what is done (plan §49).
	if out := c.State.Outstanding(); len(out) > 0 {
		w("UNRESOLVED REQUIREMENTS")
		for _, r := range out {
			w("  %s %-4s %s [%s]", r.Status.Glyph(), r.ID, r.Description, r.Confidence.Label())
		}
		w("")
	}
	if done := completedRequirements(c.State); len(done) > 0 {
		w("VERIFIED COMPLETE")
		for _, r := range done {
			w("  %s %-4s %s", r.Status.Glyph(), r.ID, r.Description)
		}
		w("")
	}

	if len(c.State.Rejected) > 0 {
		w("KNOWN FAILED APPROACHES — do not repeat these without new evidence")
		for _, r := range c.State.Rejected {
			w("  ✗ %s", r.Approach)
			if r.Reason != "" {
				w("      because %s", r.Reason)
			}
		}
		w("")
	}

	if d := c.State.ActiveDecisions(); len(d) > 0 {
		w("IMPORTANT DECISIONS")
		for _, dec := range d {
			w("  • %s", dec.Decision)
			if dec.Reason != "" {
				w("      because %s", dec.Reason)
			}
		}
		w("")
	}

	w("CURRENT REPOSITORY STATE")
	w("%s", indent(repoLine(c.State.Repo)))
	w("%s", indent(c.Drift.Summary()))
	w("")

	if len(c.State.ChangedFiles) > 0 {
		w("RELEVANT FILES")
		for _, f := range c.State.ChangedFiles {
			w("  %-2s %s", fallback(f.Status, "?"), f.Path)
		}
		w("")
	}

	if len(c.State.Tests) > 0 {
		w("TEST STATE")
		for _, t := range c.State.Tests {
			w("  %-8s %s", strings.ToUpper(string(t.Status)), t.Name)
			if t.Command != "" {
				w("           %s", t.Command)
			}
		}
		w("")
	}

	w("GRAPH VERIFICATION")
	w("%s", indentBlock(c.Graph.Render()))
	w("")

	if action, ok := c.State.PrimaryNextAction(); ok {
		w("RECOMMENDED NEXT ACTION")
		w("%s", indent(action.Description))
		if action.Rationale != "" {
			w("%s", indent("because "+action.Rationale))
		}
		w("%s", indent("["+action.Confidence.Label()+"]"))
		w("")
	}

	// The partial-recovery block is mandatory whenever anything was missing.
	// Presenting a degraded reconstruction as a complete one is the single
	// failure mode this product exists to prevent (plan §33, §47).
	if !c.State.Capture.Complete() || len(c.Notes) > 0 {
		w("STATE PARTIALLY RECOVERED")
		for _, n := range c.Notes {
			w("  ? %s", n)
		}
		w("")
	}

	if ev := c.State.AllEvidence(); len(ev) > 0 {
		w("EVIDENCE")
		w("%s", indentBlock(render.EvidenceList(ev, 12)))
		w("")
	}

	w("INSTRUCTIONS")
	w("%s", indentBlock(wrap(c.Instruction, render.Width-2)))
	return b.String()
}

func completedRequirements(s *model.EngineeringState) []model.Requirement {
	var out []model.Requirement
	for _, r := range s.Requirements {
		if r.Status == model.ReqComplete {
			out = append(out, r)
		}
	}
	return out
}

func repoLine(r model.RepoState) string {
	if r.CommitSHA == "" && r.Branch == "" {
		return "Repository state unavailable."
	}
	dirty := "clean"
	if r.Dirty {
		dirty = "uncommitted changes present"
	}
	return fmt.Sprintf("%s @ %s (%s), %s", fallback(r.Repo, "unknown repo"), fallback(r.Branch, "unknown branch"), shortSHA(r.CommitSHA), dirty)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "unknown commit"
	}
	return sha
}

func fallback(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func indent(s string) string { return indentBlock(s) }

func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// wrap breaks text at width columns on word boundaries.
func wrap(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	return strings.Join(append(lines, line), "\n")
}

// dedupeStrings removes repeats while preserving first-seen order. Order is
// preserved rather than sorted because the notes are written in pipeline order,
// which is the order a reader needs them in; the input order is already
// deterministic, so the result is too.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
