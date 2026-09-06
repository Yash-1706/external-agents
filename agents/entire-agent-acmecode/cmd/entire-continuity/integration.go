package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/graph"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/normalize"
)

// ---------------------------------------------------------------- checkpoints

func (a *app) checkpoint(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print("entire-continuity checkpoint <create|list>\n")
		return nil
	}
	switch args[0] {
	case "create":
		return a.checkpointCreate(ctx, args[1:])
	case "list":
		return a.checkpointList(ctx, args[1:])
	default:
		return fmt.Errorf("unknown checkpoint subcommand %q", args[0])
	}
}

func (a *app) checkpointCreate(ctx context.Context, args []string) error {
	ref, args := firstArg(args)
	fs := flag.NewFlagSet("checkpoint create", flag.ContinueOnError)
	label := fs.String("label", "", "short label describing what this checkpoint proves (required)")
	message := fs.String("message", "", "longer note: decisions, rejected options, open risks")
	session := fs.String("session", "", "session creating the checkpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*label) == "" {
		return fmt.Errorf("--label is required: a checkpoint without a stated purpose is not worth writing")
	}

	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	state, _, _, err := a.currentState(ctx, t)
	if err != nil {
		return err
	}
	if err := a.store.PutState(ctx, t.ID, state); err != nil {
		return err
	}

	sess := *session
	if sess == "" {
		sess = t.RootSessionID
	}
	cp, err := a.entire.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID:    t.ID,
		SessionID: sess,
		Label:     *label,
		Agent:     model.AgentHuman,
		Message:   *message,
		State:     state,
	})
	if err != nil {
		if errors.Is(err, model.ErrUnavailable) {
			return fmt.Errorf("no checkpoint backend is available, so nothing was recorded: %w", err)
		}
		return err
	}

	// Record the checkpoint in the task timeline too, so lineage stays complete
	// even if the checkpoint backend is later unreachable.
	ev := model.AgentEvent{
		Type: model.CheckpointCreated, Timestamp: cp.CreatedAt, TaskID: t.ID,
		SessionID: sess, Agent: model.AgentHuman, Summary: *label,
		Attrs: map[string]string{
			"checkpoint_id": cp.ID,
			"commit_sha":    state.Repo.CommitSHA,
			"branch":        state.Repo.Branch,
			"label":         *label,
		},
	}
	if err := a.store.AppendEvent(ctx, ev); err != nil {
		return err
	}
	if err := a.store.PutCheckpoint(ctx, t.ID, model.CheckpointNode{
		CheckpointID: cp.ID, SessionID: sess, Agent: model.AgentHuman, Label: *label,
		CommitSHA: state.Repo.CommitSHA, Branch: state.Repo.Branch,
		CreatedAt: cp.CreatedAt, StateHash: state.Hash(),
	}); err != nil {
		return err
	}

	complete, total := state.CountRequirements()
	fmt.Printf("Created checkpoint %s\n  %s\n  task %s\n  state %s\n  %d/%d requirements complete, %d failing test(s)\n\n%s\n",
		cp.ID, *label, t.ID, state.Hash(), complete, total, len(state.FailingTests()), a.entireNote)
	return nil
}

func (a *app) checkpointList(ctx context.Context, args []string) error {
	ref, _ := firstArg(args)
	t, err := a.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	cps, err := a.entire.Checkpoints(ctx, t.ID)
	if err != nil {
		if errors.Is(err, model.ErrUnavailable) {
			fmt.Printf("Checkpoint history unavailable: %v\n%s\n", err, a.entireNote)
			return nil
		}
		return err
	}
	if len(cps) == 0 {
		fmt.Printf("Task %s has no checkpoints yet.\n", t.ID)
		return nil
	}
	for _, cp := range cps {
		hash := "(no state)"
		if cp.State != nil {
			hash = cp.State.Hash()
		}
		fmt.Printf("%-20s %-28s %s\n    %s\n", cp.ID, cp.CreatedAt.UTC().Format("2006-01-02 15:04:05"), cp.Label, hash)
	}
	return nil
}

// -------------------------------------------------------------------- ingest

func (a *app) ingest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	format := fs.String("format", string(normalize.FormatAuto),
		"transcript format, or auto to detect per record: "+formatList())
	task := fs.String("task", "", "task to ingest into (default: derived from the transcript)")
	file := fs.String("file", "", "file to read (default: stdin)")
	quiet := fs.Bool("quiet", false, "suppress the per-event summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	raw, err := readInput(*file)
	if err != nil {
		return err
	}

	// The repository is the fallback anchor for a transcript that carries no
	// task id and no session header — the incomplete case. Passing it here is
	// what turns "this transcript cannot be filed" into a partial result.
	opts := normalize.Options{Format: normalize.Format(*format), TaskID: *task}
	if head, herr := a.repo.Head(ctx); herr == nil {
		opts.Repo, opts.Branch = head.Repo, head.Branch
	}

	res, err := normalize.Stream(raw, opts)
	if err != nil {
		return err
	}
	events := res.Events

	// The report is printed before anything is stored, and whether or not the
	// read was clean. A caller must never have to infer from silence that a
	// transcript was fully understood (plan §33, §47).
	fmt.Print(res.Report.Summary())

	if len(events) == 0 {
		fmt.Println("\nNothing could be ingested from this input.")
		return nil
	}
	fmt.Println()

	// Events arrive from an external agent that has no idea whether the task
	// exists here yet, so ingest is responsible for anchoring them. Refusing
	// would drop a real session on the floor for a bookkeeping reason.
	created := map[string]bool{}
	for _, ev := range events {
		if created[ev.TaskID] {
			continue
		}
		if _, err := a.store.Task(ctx, ev.TaskID); err == nil {
			created[ev.TaskID] = true
			continue
		} else if !errors.Is(err, model.ErrNotFound) {
			return err
		}
		head, _ := a.repo.Head(ctx)
		now := a.clock.Now()
		repo := firstNonEmpty(ev.Attr("repository"), head.Repo)
		branch := firstNonEmpty(ev.Attr("branch"), head.Branch)
		if err := a.store.CreateTask(ctx, model.Task{
			ID: ev.TaskID, Repo: repo, Branch: branch,
			RootSessionID: ev.SessionID, Title: "Ingested from " + ev.Agent.Display(),
			Status: model.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		created[ev.TaskID] = true
	}

	for _, ev := range events {
		if err := a.store.AppendEvent(ctx, ev); err != nil {
			return fmt.Errorf("appending %s from session %s: %w", ev.Type, ev.SessionID, err)
		}
	}

	fmt.Printf("Ingested %d event(s) into %d task(s).\n", len(events), len(created))
	for id := range created {
		fmt.Printf("  task %s\n", id)
	}
	if !*quiet {
		fmt.Println()
		for _, ev := range events {
			kind := string(ev.Type)
			if ev.Type == model.EventUnknown {
				// Say plainly that this record was retained without being
				// understood, rather than letting it read as a normal event.
				kind = "Unknown(" + ev.Attr(model.AttrRawKind) + ")"
			}
			fmt.Printf("  %-28s %-22s %s\n", kind, ev.SessionID, truncate(ev.Summary, 60))
		}
	}
	return nil
}

func formatList() string {
	out := make([]string, 0, len(normalize.Formats()))
	for _, f := range normalize.Formats() {
		out = append(out, string(f))
	}
	return strings.Join(out, ", ")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func readInput(file string) ([]byte, error) {
	if file == "" || file == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(file)
}

// --------------------------------------------------------------------- graph

func (a *app) graphCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print("entire-continuity graph <impact|search> SYMBOL\n")
		return nil
	}
	sub := args[0]
	target, _ := firstArg(args[1:])
	if target == "" {
		return fmt.Errorf("graph %s needs a symbol name", sub)
	}

	if !a.graph.Available(ctx) {
		// Saying so plainly is the requirement: an unavailable Graph must never
		// be rendered as an empty result, which reads as "nothing found"
		// (plan §48).
		fmt.Printf("Graph verification unavailable (%s).\nNo impact analysis was performed.\n", a.graph.Describe())
		return nil
	}

	switch sub {
	case "search":
		syms, err := a.graph.Search(ctx, target)
		if err != nil {
			return err
		}
		if len(syms) == 0 {
			fmt.Printf("Graph analysed the tree and found no definition named %q.\n", target)
			return nil
		}
		for _, s := range syms {
			fmt.Printf("%-10s %-28s %s:%d-%d\n", s.Kind, s.Name, s.Path, s.LineStart, s.LineEnd)
		}
		return nil

	case "impact":
		v := graph.VerifyNextAction(ctx, a.graph, model.NextAction{
			Description: "Modify " + target,
			Target:      target,
			Confidence:  model.Recommended,
		})
		// Render already carries its own evidence block; printing a second one
		// here duplicated the heading.
		fmt.Print(v.Render())
		return nil

	default:
		return fmt.Errorf("unknown graph subcommand %q", sub)
	}
}
