// Command entire-continuity is the task-continuity CLI for Entire.
//
// It is designed to be surfaced as "entire task ..." once merged into Entire
// itself (plan §19, §20). It ships as a separate binary here because the Entire
// CLI is not installed in this environment, and the plan is explicit that the
// continuity layer must work — degraded but honest — without it (plan §48).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/entire"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/gitrepo"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/graph"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/semantic"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/store"
)

// Version is the CLI version, overridable at build time with -ldflags.
var Version = "0.1.0"

const usage = `entire-continuity — evidence-backed continuation of software tasks across agents

USAGE
  entire-continuity <command> [flags]

TASK
  task init          Create a task for this repository and branch
  task list          List known tasks
  task status        Show the current engineering state of a task
  task handoff       Render an evidence-backed handoff package
  task resume        Rebuild the continuation context for a fresh worker
  task lineage       Show the cross-agent lineage of a task
  task constraint    Record a new mid-task constraint without losing intent

CHECKPOINT
  checkpoint create  Capture the current engineering state as a checkpoint
  checkpoint list    List the checkpoints belonging to a task

INTEGRATION
  ingest             Normalize agent events from an adapter into a task
  graph impact       Run Graph impact analysis on a symbol

  version            Print the version

GLOBAL FLAGS
  --root DIR         Continuity state directory (default .entire-continuity)
  --repo DIR         Repository to inspect (default .)
  --entire-bin BIN   Entire binary to use when present (default entire)

Run "entire-continuity <command> --help" for command flags.
`

// app holds the wired ports. Every port degrades rather than failing, so a
// missing Entire, Graph or git only narrows what the CLI can claim.
type app struct {
	store      *store.FS
	repo       *gitrepo.Repo
	entire     model.EntireClient
	entireNote string
	graph      model.GraphClient
	extractor  model.Extractor
	clock      model.Clock
	repoDir    string
	root       string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "entire-continuity: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}

	// Global flags may appear before the command.
	var root, repoDir, entireBin string
	global := flag.NewFlagSet("entire-continuity", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	global.StringVar(&root, "root", "", "continuity state directory")
	global.StringVar(&repoDir, "repo", ".", "repository directory to inspect")
	global.StringVar(&entireBin, "entire-bin", "entire", "Entire binary to use when available")
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	rest, err := splitGlobal(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		fmt.Print(usage)
		return nil
	}

	if rest[0] == "version" {
		fmt.Printf("entire-continuity %s\n", Version)
		return nil
	}

	if root == "" {
		root = filepath.Join(repoDir, ".entire-continuity")
	}
	a, err := wire(root, repoDir, entireBin)
	if err != nil {
		return err
	}

	ctx := context.Background()
	switch rest[0] {
	case "task":
		return a.task(ctx, rest[1:])
	case "checkpoint":
		return a.checkpoint(ctx, rest[1:])
	case "ingest":
		return a.ingest(ctx, rest[1:])
	case "graph":
		return a.graphCmd(ctx, rest[1:])
	default:
		return fmt.Errorf("unknown command %q (run \"entire-continuity help\")", rest[0])
	}
}

// splitGlobal parses leading global flags and returns the remaining arguments.
// Flags are also tolerated after the subcommand, which is what users actually
// type, so anything unrecognised here is handed back rather than rejected.
func splitGlobal(fs *flag.FlagSet, args []string) ([]string, error) {
	var pre []string
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		pre = append(pre, args[i])
		// Consume a following value when the flag was not given as --k=v.
		if !strings.Contains(args[i], "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			pre = append(pre, args[i])
		}
		i++
	}
	if err := fs.Parse(pre); err != nil {
		return nil, err
	}
	return args[i:], nil
}

// wire builds the ports. Detection order matters: a real Entire and a real
// Graph win when present, and the fallbacks announce themselves plainly so the
// user always knows which backend produced an answer (plan §5).
func wire(root, repoDir, entireBin string) (*app, error) {
	clock := model.SystemClock{}
	st, err := store.NewFS(root, clock)
	if err != nil {
		return nil, fmt.Errorf("opening continuity store at %s: %w", root, err)
	}
	ent, note := entire.Detect(entireBin, filepath.Join(root, "entire"), repoDir, clock)
	return &app{
		store:      st,
		repo:       gitrepo.New(repoDir),
		entire:     ent,
		entireNote: note,
		graph:      graph.Detect(entireBin, repoDir),
		extractor:  semantic.NewHeuristic(clock),
		clock:      clock,
		repoDir:    repoDir,
		root:       root,
	}, nil
}

// resolveRef turns an optional positional reference into a task, falling back
// to the current repository and branch when none was given.
func (a *app) resolveRef(ctx context.Context, ref string) (model.Task, error) {
	var repo, branch string
	if ref == "" {
		head, err := a.repo.Head(ctx)
		if err != nil {
			return model.Task{}, fmt.Errorf("no task named and the repository could not be inspected (%w); pass a task id", err)
		}
		repo, branch = head.Repo, head.Branch
	}
	t, err := a.store.ResolveTask(ctx, ref, repo, branch)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			if ref == "" {
				return model.Task{}, fmt.Errorf("no task exists for %s@%s yet — run \"entire-continuity task init\" first", repo, branch)
			}
			return model.Task{}, fmt.Errorf("no task, session or checkpoint matches %q", ref)
		}
		return model.Task{}, err
	}
	return t, nil
}

// firstArg pops a leading positional argument that is not a flag.
func firstArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// firstNonEmpty returns the first argument that is not blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
