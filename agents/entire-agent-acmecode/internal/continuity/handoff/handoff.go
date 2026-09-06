// Package handoff converts the current engineering state of a task into a
// compact, evidence-backed package another developer or agent can act on
// immediately (plan §19, §24, §25).
//
// It deliberately does not build its own view of the task. It calls
// contextbuild and renders what that returns, so the human handoff, the machine
// handoff and the resume context are all the same state object seen three ways.
// Implementing them separately is the fastest route to the two drifting apart,
// which is why the plan forbids it (plan §24, Rule 10).
package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/contextbuild"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/render"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/validate"
)

// Party identifies one side of a handoff. Both sides are optional: a handoff
// created for "whoever picks this up next" has no destination yet, and that is
// a normal case rather than an error.
type Party struct {
	SessionID string
	Agent     model.AgentKind
}

// Options carries the ambient dependencies. They mirror contextbuild.Options
// because the state is built by that package.
type Options struct {
	Store      model.Store
	Entire     model.EntireClient
	Repo       model.RepoInspector
	Graph      model.GraphClient
	Extractor  model.Extractor
	Clock      model.Clock
	BaseRef    string
	EntireNote string
	// Persist controls whether the receipt is written to the store. A dry-run
	// handoff (rendering the state to read it) should not leave a lineage node
	// claiming a transfer happened.
	Persist bool
}

// Result is one handoff: the receipt, the state it was derived from, and the
// lineage it belongs to.
type Result struct {
	Receipt  model.HandoffNode       `json:"receipt"`
	Task     model.Task              `json:"task"`
	State    *model.EngineeringState `json:"state"`
	Lineage  model.Lineage           `json:"lineage"`
	Findings []validate.Finding      `json:"findings,omitempty"`
	Notes    []string                `json:"notes,omitempty"`
}

// Create assembles the handoff for the task named by ref.
func Create(ctx context.Context, ref string, from, to Party, opts Options) (*Result, error) {
	built, err := contextbuild.Build(ctx, ref, contextbuild.Options{
		Store:      opts.Store,
		Entire:     opts.Entire,
		Repo:       opts.Repo,
		Graph:      opts.Graph,
		Extractor:  opts.Extractor,
		Clock:      opts.Clock,
		BaseRef:    opts.BaseRef,
		EntireNote: opts.EntireNote,
	})
	if err != nil {
		return nil, err
	}

	clock := opts.Clock
	if clock == nil {
		clock = model.SystemClock{}
	}
	createdAt := clock.Now()

	// The receipt records what the next worker most needs to know before they
	// even open the state: how much is outstanding, what is broken, and what to
	// do first (plan §25).
	outstanding := len(built.State.Outstanding())
	failing := len(built.State.FailingTests())
	nextAction := ""
	if action, ok := built.State.PrimaryNextAction(); ok {
		nextAction = action.Description
	}

	// StateHash fingerprints the engineering conclusion, not the rendering, so
	// two handoffs of an unchanged task carry the same hash and a reader can
	// tell at a glance whether anything actually moved.
	stateHash := built.State.Hash()

	receipt := model.HandoffNode{
		ID:               receiptID(built.Task.ID, from.SessionID, to.SessionID, stateHash, createdAt.String()),
		TaskID:           built.Task.ID,
		FromSessionID:    from.SessionID,
		FromAgent:        from.Agent,
		ToSessionID:      to.SessionID,
		ToAgent:          to.Agent,
		SourceCheckpoint: built.SourceCheckpoint,
		StateHash:        stateHash,
		CreatedAt:        createdAt,
		NextAction:       nextAction,
		Outstanding:      outstanding,
		CriticalFailures: failing,
	}

	res := &Result{
		Receipt:  receipt,
		Task:     built.Task,
		State:    built.State,
		Lineage:  built.Lineage,
		Findings: built.Findings,
		Notes:    built.Notes,
	}

	if opts.Persist && opts.Store != nil {
		if err := opts.Store.PutHandoff(ctx, receipt); err != nil {
			// A receipt that cannot be persisted is worth reporting but not
			// worth discarding: the rendered handoff is still correct and
			// useful, and continuity infrastructure must not block the work
			// (plan §48, Rule 8).
			res.Notes = append(res.Notes, fmt.Sprintf("Handoff receipt could not be recorded in the store: %v", err))
		} else {
			res.Lineage.Handoffs = append(res.Lineage.Handoffs, receipt)
		}
	}
	return res, nil
}

// receiptID derives a deterministic identifier for a handoff.
func receiptID(taskID, from, to, stateHash, createdAt string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{taskID, from, to, stateHash, createdAt}, "\x00")))
	return "ho_" + hex.EncodeToString(sum[:])[:12]
}

// Text renders the human handoff of plan §19, followed by the receipt.
func (r *Result) Text() string {
	var b strings.Builder
	b.WriteString(render.Handoff(r.State, r.Lineage))
	b.WriteString("\n")
	b.WriteString(render.Receipt(r.Receipt))
	if len(r.Notes) > 0 {
		b.WriteString("\nCAPTURE NOTES\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  ? %s\n", n)
		}
	}
	return b.String()
}

// Markdown renders the same handoff as Markdown, from the same state object.
func (r *Result) Markdown() string {
	var b strings.Builder
	b.WriteString(render.HandoffMarkdown(r.State, r.Lineage))
	b.WriteString("\n## Handoff receipt\n\n")
	fmt.Fprintf(&b, "- **Receipt:** `%s`\n", r.Receipt.ID)
	fmt.Fprintf(&b, "- **Task:** `%s`\n", r.Receipt.TaskID)
	fmt.Fprintf(&b, "- **From:** %s\n", party(r.Receipt.FromAgent, r.Receipt.FromSessionID))
	fmt.Fprintf(&b, "- **To:** %s\n", party(r.Receipt.ToAgent, r.Receipt.ToSessionID))
	if r.Receipt.SourceCheckpoint != "" {
		fmt.Fprintf(&b, "- **Source checkpoint:** `%s`\n", r.Receipt.SourceCheckpoint)
	} else {
		b.WriteString("- **Source checkpoint:** none — this handoff is not backed by a checkpoint\n")
	}
	fmt.Fprintf(&b, "- **State hash:** `%s`\n", r.Receipt.StateHash)
	fmt.Fprintf(&b, "- **Outstanding requirements:** %d\n", r.Receipt.Outstanding)
	fmt.Fprintf(&b, "- **Failing tests:** %d\n", r.Receipt.CriticalFailures)
	if r.Receipt.NextAction != "" {
		fmt.Fprintf(&b, "- **Next action:** %s\n", r.Receipt.NextAction)
	}
	if len(r.Notes) > 0 {
		b.WriteString("\n### Capture notes\n\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

func party(agent model.AgentKind, session string) string {
	name := agent.Display()
	if agent == "" {
		name = "unspecified"
	}
	if session == "" {
		return name
	}
	return fmt.Sprintf("%s (`%s`)", name, session)
}

// JSON renders the machine-readable handoff. It is produced from the same
// Result as Text and Markdown, which is the guarantee plan §24 asks for.
func (r *Result) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }
