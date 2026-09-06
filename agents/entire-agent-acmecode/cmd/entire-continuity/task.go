package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/contextbuild"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/derive"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/handoff"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/merge"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/render"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/validate"
)

const taskUsage = `entire-continuity task <subcommand>

  init        Create a task for this repository and branch
  list        List known tasks
  status      Show the current engineering state of a task
  handoff     Render an evidence-backed handoff package
  resume      Rebuild the continuation context for a fresh worker
  lineage     Show the cross-agent lineage of a task
  explain     Explain why the task is where it is, with evidence
  constraint  Record a new mid-task constraint without losing original intent
`

func (a *app) task(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(taskUsage)
		return nil
	}
	switch args[0] {
	case "init":
		return a.taskInit(ctx, args[1:])
	case "list":
		return a.taskList(ctx)
	case "status":
		return a.taskStatus(ctx, args[1:])
	case "handoff":
		return a.taskHandoff(ctx, args[1:])
	case "resume":
		return a.taskResume(ctx, args[1:])
	case "lineage":
		return a.taskLineage(ctx, args[1:])
	case "explain":
		return a.taskExplain(ctx, args[1:])
	case "constraint":
		return a.taskConstraint(ctx, args[1:])
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

func (a *app) taskInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task init", flag.ContinueOnError)
	title := fs.String("title", "", "short human title for the task (required)")
	intent := fs.String("intent", "", "the original intent, in the requester's own words")
	session := fs.String("session", "", "root session id (defaults to a derived local id)")
	agentName := fs.String("agent", string(model.AgentHuman), "agent runtime starting the task")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" {
		return fmt.Errorf("--title is required")
	}

	head, err := a.repo.Head(ctx)
	if err != nil {
		return fmt.Errorf("this command needs a git repository to anchor the task: %w", err)
	}
	root := *session
	if root == "" {
		root = "local_" + strings.ToLower(strings.ReplaceAll(head.Branch, "/", "_"))
	}

	now := a.clock.Now()
	t := model.Task{
		ID:            model.NewTaskID(head.Repo, head.Branch, root),
		Repo:          head.Repo,
		Branch:        head.Branch,
		RootSessionID: root,
		Title:         *title,
		Status:        model.StatusNew,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := a.store.CreateTask(ctx, t); err != nil {
		return err
	}

	// Record the root session and the opening intent as the first event, so the
	// task has a timeline from the moment it exists.
	agent := model.AgentKind(*agentName)
	if !agent.Valid() {
		agent = model.AgentUnknown
	}
	ev := model.AgentEvent{
		Type:      model.SessionStarted,
		Timestamp: now,
		TaskID:    t.ID,
		SessionID: root,
		Agent:     agent,
		Role:      model.RoleMain,
		Summary:   *intent,
	}
	if err := a.store.AppendEvent(ctx, ev); err != nil {
		return err
	}

	// The original intent is the one field a later worker cannot reconstruct,
	// so it is persisted immediately rather than waiting for a checkpoint.
	state := model.NewEngineeringState(model.TaskRef{ID: t.ID, Title: t.Title, OriginalIntent: *intent})
	state.GeneratedAt = now
	if err := a.store.PutState(ctx, t.ID, state); err != nil {
		return err
	}

	fmt.Printf("Created task %s\n  %s\n  %s @ %s\n  root session: %s\n\n%s\n",
		t.ID, t.Title, t.Repo, t.Branch, root, a.entireNote)
	return nil
}

func (a *app) taskList(ctx context.Context) error {
	tasks, err := a.store.Tasks(ctx)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("No tasks yet. Run \"entire-continuity task init --title ...\" to create one.")
		return nil
	}
	for _, t := range tasks {
		fmt.Printf("%-24s %-10s %s @ %s\n    %s\n", t.ID, t.Status, t.Repo, t.Branch, t.Title)
	}
	return nil
}

// currentState rebuilds the task's engineering state from events, repository
// facts and semantic extraction, merged over whatever was already stored.
// It is the shared path behind status, checkpoint and constraint.
func (a *app) currentState(ctx context.Context, t model.Task) (*model.EngineeringState, model.Lineage, []validate.Finding, error) {
	lineage, err := a.store.Lineage(ctx, t.ID)
	if err != nil {
		return nil, lineage, nil, err
	}
	lineage.Task = t

	events, err := a.store.Events(ctx, t.ID)
	if err != nil {
		return nil, lineage, nil, err
	}
	stored, _ := a.store.State(ctx, t.ID)

	ref := model.TaskRef{ID: t.ID, Title: t.Title}
	if stored != nil {
		ref.OriginalIntent = stored.Task.OriginalIntent
	}

	current, err := derive.State(ctx, derive.Input{
		Task:   ref,
		TaskID: t.ID,
		Events: events,
		Repo:   a.repo,
		Clock:  a.clock,
	})
	if err != nil {
		return nil, lineage, nil, err
	}

	// A task created by ingestion has no typed-in intent, but the transcript may
	// state one; derivation recovers it. Feed that to the extractor, or a task
	// whose goal we actually know would still report "no prompt captured" and
	// extract no requirements from it.
	prompt := derive.OriginalPrompt(events)
	if strings.TrimSpace(prompt) == "" {
		prompt = ref.OriginalIntent
	}
	if strings.TrimSpace(prompt) == "" && current != nil {
		prompt = current.Task.OriginalIntent
	}

	if a.extractor != nil && a.extractor.Available(ctx) {
		sem, serr := a.extractor.Extract(ctx, model.ExtractionInput{
			Task:           ref,
			OriginalPrompt: prompt,
			Events:         events,
			Deterministic:  current,
		})
		if serr == nil && sem != nil {
			current = merge.States(current, sem, merge.Options{Clock: a.clock})
		}
	}

	merged := merge.States(stored, current, merge.Options{Clock: a.clock})
	enforced, findings := validate.Enforce(merged)
	return enforced, lineage, findings, nil
}

func (a *app) taskStatus(ctx context.Context, args []string) error {
	ref, args := firstArg(args)
	fs := flag.NewFlagSet("task status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the engineering state as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	state, lineage, findings, err := a.currentState(ctx, t)
	if err != nil {
		return err
	}
	if err := a.store.PutState(ctx, t.ID, state); err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(state)
	}
	fmt.Print(render.Status(state, lineage))
	if len(findings) > 0 {
		fmt.Printf("\nVALIDATION (%d claim(s) weakened to match their evidence)\n", len(findings))
		for _, f := range findings {
			fmt.Printf("  %-8s %-28s %s\n", f.Severity, f.Code, f.Path)
		}
	}
	fmt.Printf("\n%s\n", a.entireNote)
	return nil
}

func (a *app) taskHandoff(ctx context.Context, args []string) error {
	ref, args := firstArg(args)
	fs := flag.NewFlagSet("task handoff", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the handoff as JSON for an agent")
	asMD := fs.Bool("markdown", false, "emit the handoff as Markdown")
	fromSession := fs.String("from-session", "", "session handing the task over")
	fromAgent := fs.String("from-agent", "", "agent handing the task over")
	toSession := fs.String("to-session", "", "session receiving the task")
	toAgent := fs.String("to-agent", "", "agent receiving the task")
	record := fs.Bool("record", false, "persist the handoff receipt into the task lineage")
	if err := fs.Parse(args); err != nil {
		return err
	}

	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}

	// Refresh and persist state first, so the handoff reflects the repository as
	// it is now rather than as the last checkpoint left it.
	state, _, _, err := a.currentState(ctx, t)
	if err == nil {
		_ = a.store.PutState(ctx, t.ID, state)
	}

	res, err := handoff.Create(ctx, t.ID,
		handoff.Party{SessionID: *fromSession, Agent: model.AgentKind(*fromAgent)},
		handoff.Party{SessionID: *toSession, Agent: model.AgentKind(*toAgent)},
		handoff.Options{
			Store: a.store, Entire: a.entire, Repo: a.repo, Graph: a.graph,
			Extractor: a.extractor, Clock: a.clock, EntireNote: a.entireNote,
			Persist: *record,
		})
	if err != nil {
		return err
	}

	switch {
	case *asJSON:
		b, err := res.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	case *asMD:
		fmt.Print(res.Markdown())
	default:
		fmt.Print(res.Text())
	}
	return nil
}

func (a *app) taskResume(ctx context.Context, args []string) error {
	ref, args := firstArg(args)
	fs := flag.NewFlagSet("task resume", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the continuation context as JSON")
	session := fs.String("session", "", "the new session id continuing the task")
	agentName := fs.String("agent", "", "the agent runtime continuing the task")
	if err := fs.Parse(args); err != nil {
		return err
	}

	built, err := contextbuild.Build(ctx, ref, contextbuild.Options{
		Store: a.store, Entire: a.entire, Repo: a.repo, Graph: a.graph,
		Extractor: a.extractor, Clock: a.clock, EntireNote: a.entireNote,
	})
	if err != nil {
		return err
	}

	// Recording the resume is what makes lineage cross-agent: the new session
	// joins the existing task rather than starting a new one (plan §8, §26).
	if *session != "" {
		agent := model.AgentKind(*agentName)
		if !agent.Valid() {
			agent = model.AgentUnknown
		}
		now := a.clock.Now()
		for _, ev := range []model.AgentEvent{
			{Type: model.SessionStarted, Timestamp: now, TaskID: built.Task.ID, SessionID: *session, Agent: agent, Role: model.RoleMain,
				Summary: "Resumed task from continuity context"},
			{Type: model.TaskResumed, Timestamp: now, TaskID: built.Task.ID, SessionID: *session, Agent: agent,
				Summary: "Resumed from checkpoint " + orNone(built.SourceCheckpoint)},
		} {
			if err := a.store.AppendEvent(ctx, ev); err != nil {
				return err
			}
		}
	}

	if *asJSON {
		b, err := built.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(built.Render())
	return nil
}

func (a *app) taskLineage(ctx context.Context, args []string) error {
	ref, _ := firstArg(args)
	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	lineage, err := a.store.Lineage(ctx, t.ID)
	if err != nil {
		return err
	}
	lineage.Task = t
	fmt.Print(render.Lineage(lineage))
	return nil
}

// taskConstraint records a requirement that arrived mid-task. It is additive by
// construction: the original intent is never rewritten, the constraint is
// recorded as its own event and state entry, and affected requirements are
// marked rather than deleted (plan §41).
func (a *app) taskConstraint(ctx context.Context, args []string) error {
	ref, args := firstArg(args)
	fs := flag.NewFlagSet("task constraint", flag.ContinueOnError)
	text := fs.String("text", "", "the new constraint, verbatim (required)")
	source := fs.String("source", "manual", "where the constraint came from")
	affects := fs.String("affects", "", "comma-separated requirement ids this constraint changes")
	invalidates := fs.String("invalidates", "", "comma-separated assumptions this constraint invalidates")
	session := fs.String("session", "", "session recording the constraint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*text) == "" {
		return fmt.Errorf("--text is required")
	}

	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	sess := *session
	if sess == "" {
		sess = t.RootSessionID
	}

	now := a.clock.Now()
	ev := model.AgentEvent{
		Type:      model.ConstraintAdded,
		Timestamp: now,
		TaskID:    t.ID,
		SessionID: sess,
		Agent:     model.AgentHuman,
		Summary:   *text,
		Attrs:     map[string]string{"constraint_source": *source},
	}
	if err := a.store.AppendEvent(ctx, ev); err != nil {
		return err
	}

	state, _, _, err := a.currentState(ctx, t)
	if err != nil {
		return err
	}
	// The event was already appended, so the derivation in currentState has
	// produced a Constraint for it with an id of its own. Reuse that record
	// rather than minting a second one: inventing an id here produced C2 for the
	// first constraint a task ever received, and left two entries describing one
	// event. This command's job is only to say what the constraint *affects*,
	// which the event cannot express.
	c := model.Constraint{
		ID:       fmt.Sprintf("C%d", len(state.Constraints)+1),
		Text:     *text,
		Source:   *source,
		AddedAt:  now,
		Evidence: []model.Evidence{ev.AsEvidence()},
	}
	for _, existing := range state.Constraints {
		if strings.TrimSpace(existing.Text) == strings.TrimSpace(*text) {
			c = existing
			break
		}
	}
	c.AffectedRequirements = splitList(*affects)
	c.ChangedAssumptions = splitList(*invalidates)
	updated := merge.ApplyConstraint(state, c, merge.Options{Clock: a.clock})
	if err := a.store.PutState(ctx, t.ID, updated); err != nil {
		return err
	}

	fmt.Printf("Recorded constraint %s from %s\n\n  %s\n\n", c.ID, c.Source, c.Text)
	if len(c.AffectedRequirements) > 0 {
		fmt.Printf("Affected requirements: %s\n", strings.Join(c.AffectedRequirements, ", "))
	}
	fmt.Printf("Original intent is unchanged:\n  %s\n", orNone(updated.Task.OriginalIntent))
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, model.NormalizeID(p))
		}
	}
	return out
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(not recorded)"
	}
	return s
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// taskExplain answers "why is this task where it is?" in the plan §50 shape:
// the blocking fact, the likely reason, the evidence, and what to do next.
//
// It is deliberately assembled from the state rather than narrated: every line
// below is either a recorded fact or is labelled as a recommendation, so the
// answer cannot drift away from what the evidence supports.
func (a *app) taskExplain(ctx context.Context, args []string) error {
	ref, _ := firstArg(args)
	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	state, _, _, err := a.currentState(ctx, t)
	if err != nil {
		return err
	}

	fmt.Printf("WHY IS THIS TASK %s?\n\n", strings.ToUpper(string(state.Status)))

	var evidence []model.Evidence
	reasons := 0

	if failing := state.FailingTests(); len(failing) > 0 {
		reasons++
		fmt.Println("BLOCKED BECAUSE")
		for _, test := range failing {
			fmt.Printf("  ✗ %s failed\n", test.Name)
			if test.Command != "" {
				fmt.Printf("      %s\n", test.Command)
			}
			evidence = append(evidence, test.Evidence...)
		}
		fmt.Println()
	}

	// Outstanding is every requirement that is not complete, which is the same
	// rule DeriveStatus uses. Listing only blocked and unresolved ones left a
	// merely-unverified requirement invisible, so the answer read "nothing is
	// blocking it" directly under a status of PARTIAL.
	if outstanding := state.Outstanding(); len(outstanding) > 0 {
		reasons++
		fmt.Println("REQUIREMENTS NOT YET COMPLETE")
		for _, r := range outstanding {
			fmt.Printf("  %s %-4s %s [%s]\n", r.Status.Glyph(), r.ID, r.Description, r.Confidence.Label())
			evidence = append(evidence, r.Evidence...)
		}
		fmt.Println()
	}

	// A rejected approach explains why the obvious fix is not the fix. Without
	// it the reader re-derives it, which is the failure the product removes.
	if len(state.Rejected) > 0 {
		fmt.Println("WHY THE OBVIOUS APPROACH WAS NOT TAKEN")
		for _, r := range state.Rejected {
			fmt.Printf("  ✗ %s\n", r.Approach)
			if r.Reason != "" {
				fmt.Printf("      because %s\n", r.Reason)
			}
			evidence = append(evidence, r.Evidence...)
		}
		fmt.Println()
	}

	if reasons == 0 {
		fmt.Println("NOTHING IS BLOCKING IT")
		fmt.Println("  No failing test and no unresolved requirement is recorded.")
		if !state.Capture.Complete() {
			fmt.Println("  Note that capture was incomplete, so absence of a blocker")
			fmt.Println("  here is not proof that none exists.")
		}
		fmt.Println()
	}

	if action, ok := state.PrimaryNextAction(); ok {
		fmt.Println("RECOMMENDED NEXT ACTION")
		fmt.Printf("  → [%s] %s\n", action.Confidence.Label(), action.Description)
		if action.Rationale != "" {
			fmt.Printf("      because %s\n", action.Rationale)
		}
		if len(action.SuggestedTests) > 0 {
			fmt.Printf("      then run: %s\n", strings.Join(action.SuggestedTests, ", "))
		}
		fmt.Println()
	}

	if ev := model.DedupeEvidence(evidence); len(ev) > 0 {
		// EvidenceList renders its own heading.
		fmt.Println(render.EvidenceList(ev, 12))
	} else {
		fmt.Println("EVIDENCE")
		fmt.Println("  None recorded. Treat everything above as unverified.")
	}
	return nil
}
