// Package e2e exercises the whole product against a real git repository and
// real on-disk state: the fourth test layer of plan §63.
//
// The unit tests prove each rule in isolation. These prove the thing the
// product actually claims — that a task interrupted in one session can be
// picked up in a fresh one, with the intent, the failure, the rejected approach
// and the next action intact, and with stale claims flagged rather than
// repeated.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/contextbuild"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/derive"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/drift"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/entire"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/gitrepo"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/graph"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/handoff"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/merge"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/normalize"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/semantic"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/store"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/validate"
)

const (
	taskID      = "task_e2e_billing"
	mainSession = "oc_main_e2e"
	// The prompt is the demo scenario from plan §52: several requirements, an
	// edge case, and work that stops before completion.
	prompt = "Implement subscription pause without changing the current billing cycle. " +
		"Must add a pause API. Must preserve the current billing cycle. " +
		"Must prevent duplicate webhook processing. Needs regression tests."
)

// harness is the fully wired product, pointed at a throwaway repository.
type harness struct {
	dir   string
	root  string
	store *store.FS
	repo  *gitrepo.Repo
	ent   model.EntireClient
	graph model.GraphClient
	extr  model.Extractor
	clock model.Clock
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Identity is forced so the test works on a machine with no global git config.
	full := append([]string{
		"-c", "user.email=e2e@example.test",
		"-c", "user.name=E2E",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; the end-to-end layer needs a real repository")
	}

	dir := t.TempDir()
	git(t, dir, "init", "-q")
	writeFile(t, dir, "billing/subscription.go", `package billing

// Pause suspends future billing while leaving the current cycle intact.
func Pause(id string) error { return nil }
`)
	writeFile(t, dir, "billing/webhooks.go", `package billing

// WebhookProcessor handles inbound billing webhooks.
type WebhookProcessor struct{}

// Process handles one webhook event. Duplicate delivery is not yet guarded.
func (w *WebhookProcessor) Process(id string) error { return nil }
`)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "billing skeleton")

	root := filepath.Join(dir, ".entire-continuity")
	clock := &model.FixedClock{Current: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), Step: time.Minute}
	st, err := store.NewFS(root, clock)
	if err != nil {
		t.Fatalf("store.NewFS: %v", err)
	}
	ent, err := entire.NewLocal(filepath.Join(root, "entire"), clock)
	if err != nil {
		t.Fatalf("entire.NewLocal: %v", err)
	}

	return &harness{
		dir: dir, root: root, store: st,
		repo:  gitrepo.New(dir),
		ent:   ent,
		graph: graph.NewLocalGo(dir),
		extr:  semantic.NewHeuristic(clock),
		clock: clock,
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (h *harness) ctxOptions() contextbuild.Options {
	return contextbuild.Options{
		Store: h.store, Entire: h.ent, Repo: h.repo, Graph: h.graph,
		Extractor: h.extr, Clock: h.clock, EntireNote: "local fallback (test)",
	}
}

// seed plays a realistic interrupted session: the agent implements part of the
// task, hits a failing webhook test, rejects an approach, and stops.
func (h *harness) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := h.clock.Now()

	head, err := h.repo.Head(ctx)
	if err != nil {
		t.Fatalf("repo.Head: %v", err)
	}
	if err := h.store.CreateTask(ctx, model.Task{
		ID: taskID, Repo: head.Repo, Branch: head.Branch, RootSessionID: mainSession,
		Title: "Implement subscription pause", Status: model.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	events := []model.AgentEvent{
		{Type: model.SessionStarted, TaskID: taskID, SessionID: mainSession,
			Agent: model.AgentOpenClaw, Role: model.RoleMain, Summary: "starting"},
		{Type: model.TurnStarted, TaskID: taskID, SessionID: mainSession,
			Agent: model.AgentOpenClaw, Summary: prompt},
		{Type: model.TurnEnded, TaskID: taskID, SessionID: mainSession, Agent: model.AgentOpenClaw,
			Summary: "Reverted the direct renewal status mutation because it broke the legacy renewal path"},
		{Type: model.TurnEnded, TaskID: taskID, SessionID: mainSession, Agent: model.AgentOpenClaw,
			Summary: "Decided to use an explicit paused state instead of mutating renewal state"},
		{Type: model.ToolUsed, TaskID: taskID, SessionID: mainSession, Agent: model.AgentOpenClaw,
			Summary: "go test ./billing/...", PayloadRef: "artifact://run/1",
			Attrs: map[string]string{
				"tool_name": "shell", "test_name": "TestDuplicateWebhook",
				"test_status": "failed", "test_command": "go test ./billing/...",
			}},
		{Type: model.SessionInterrupted, TaskID: taskID, SessionID: mainSession,
			Agent: model.AgentOpenClaw, Summary: "session terminated"},
	}
	for i := range events {
		events[i].Timestamp = h.clock.Now()
		if err := h.store.AppendEvent(ctx, events[i]); err != nil {
			t.Fatalf("AppendEvent(%s): %v", events[i].Type, err)
		}
	}

	// The agent got partway: a paused state exists, webhook idempotency does not.
	writeFile(t, h.dir, "billing/subscription.go", `package billing

// Status records whether a subscription is active or explicitly paused.
type Status string

const (
	Active Status = "active"
	Paused Status = "paused"
)

// Pause suspends future billing while leaving the current cycle intact.
func Pause(id string) error { return nil }
`)
}

// state rebuilds the current engineering state the way the CLI does.
func (h *harness) state(t *testing.T) *model.EngineeringState {
	t.Helper()
	ctx := context.Background()
	events, err := h.store.Events(ctx, taskID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	ref := model.TaskRef{ID: taskID, Title: "Implement subscription pause"}
	current, err := derive.State(ctx, derive.Input{
		Task: ref, TaskID: taskID, Events: events, Repo: h.repo, Clock: h.clock,
	})
	if err != nil {
		t.Fatalf("derive.State: %v", err)
	}
	sem, err := h.extr.Extract(ctx, model.ExtractionInput{
		Task: ref, OriginalPrompt: derive.OriginalPrompt(events),
		Events: events, Deterministic: current,
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stored, _ := h.store.State(ctx, taskID)
	merged := merge.States(stored, merge.States(current, sem, merge.Options{Clock: h.clock}), merge.Options{Clock: h.clock})
	enforced, _ := validate.Enforce(merged)
	if err := h.store.PutState(ctx, taskID, enforced); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	return enforced
}

// TestFreshSessionRecoversTheTask is the plan §43 acceptance test. A completely
// new session, with no chat history, must be able to recover what the previous
// worker established — above all, the approach that was already rejected.
func TestFreshSessionRecoversTheTask(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	ctx := context.Background()

	state := h.state(t)
	if _, err := h.ent.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: taskID, SessionID: mainSession, Label: "CP2 pre-interruption",
		Agent: model.AgentOpenClaw, State: state,
	}); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// A fresh worker arrives knowing only the task id.
	built, err := contextbuild.Build(ctx, taskID, h.ctxOptions())
	if err != nil {
		t.Fatalf("contextbuild.Build: %v", err)
	}
	rendered := built.Render()

	t.Run("original intent survives", func(t *testing.T) {
		if !strings.Contains(built.State.Task.OriginalIntent, "without changing the current billing cycle") {
			t.Errorf("intent lost: %q", built.State.Task.OriginalIntent)
		}
	})

	t.Run("the rejected approach is carried forward", func(t *testing.T) {
		// This is the single most valuable thing the system preserves: without
		// it the fresh agent re-derives the same broken approach, which is the
		// exact failure the product exists to prevent.
		if len(built.State.Rejected) == 0 {
			t.Fatalf("no rejected approach recovered:\n%s", rendered)
		}
		if !strings.Contains(strings.ToLower(rendered), "renewal") {
			t.Errorf("the continuation context never warns about the renewal mutation:\n%s", rendered)
		}
		if !strings.Contains(rendered, "KNOWN FAILED APPROACHES") {
			t.Errorf("failed approaches are not surfaced to the fresh worker:\n%s", rendered)
		}
	})

	t.Run("the failing test survives", func(t *testing.T) {
		failing := built.State.FailingTests()
		if len(failing) != 1 || failing[0].Name != "TestDuplicateWebhook" {
			t.Fatalf("failing tests = %+v, want TestDuplicateWebhook", failing)
		}
	})

	t.Run("requirements are recovered and none is falsely complete", func(t *testing.T) {
		if len(built.State.Requirements) == 0 {
			t.Fatal("no requirements were recovered from the prompt")
		}
		for _, r := range built.State.Requirements {
			if r.Status != model.ReqComplete {
				continue
			}
			// Completion is only ever claimed on repository- or checkpoint-level
			// evidence; anything else must have been downgraded.
			if lvl, ok := model.StrongestLevel(r.Evidence); !ok || lvl > model.LevelCheckpoint {
				t.Errorf("requirement %s claims complete on weak evidence: %+v", r.ID, r.Evidence)
			}
		}
	})

	t.Run("the fresh worker is told not to trust history", func(t *testing.T) {
		if !strings.Contains(rendered, "historical engineering context") {
			t.Errorf("the resume safety rule is missing:\n%s", rendered)
		}
		if !strings.Contains(rendered, "INSTRUCTIONS") {
			t.Errorf("no instructions block:\n%s", rendered)
		}
	})

	t.Run("graph verified the next action before any edit", func(t *testing.T) {
		if !built.Graph.Available {
			t.Fatalf("Graph was unavailable in a repository full of Go source: %s", built.Graph.Reason)
		}
		if !strings.Contains(rendered, "GRAPH VERIFICATION") {
			t.Errorf("graph verification is not surfaced:\n%s", rendered)
		}
	})

	t.Run("machine and human output share one state", func(t *testing.T) {
		res, err := handoff.Create(ctx, taskID,
			handoff.Party{SessionID: mainSession, Agent: model.AgentOpenClaw},
			handoff.Party{SessionID: "hermes_new", Agent: model.AgentHermes},
			handoff.Options{
				Store: h.store, Entire: h.ent, Repo: h.repo, Graph: h.graph,
				Extractor: h.extr, Clock: h.clock, Persist: true,
			})
		if err != nil {
			t.Fatalf("handoff.Create: %v", err)
		}
		if res.Receipt.StateHash != res.State.Hash() {
			t.Errorf("the receipt does not fingerprint the state it was built from")
		}
		if res.Receipt.CriticalFailures != 1 {
			t.Errorf("CriticalFailures = %d, want 1", res.Receipt.CriticalFailures)
		}
		jsonOut, err := res.JSON()
		if err != nil {
			t.Fatalf("JSON: %v", err)
		}
		// Both renderings must describe the same task, or the human and the
		// agent are being told different things (plan §24).
		if !strings.Contains(string(jsonOut), "TestDuplicateWebhook") {
			t.Error("the JSON handoff omits the failing test")
		}
		if !strings.Contains(res.Text(), "TestDuplicateWebhook") {
			t.Error("the human handoff omits the failing test")
		}
	})
}

// TestRepositoryDriftIsDetected is the plan §46 acceptance test. After a
// checkpoint, a human edits a file the state depends on. The next resume must
// notice and demand revalidation rather than replaying stale conclusions.
func TestRepositoryDriftIsDetected(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	ctx := context.Background()

	state := h.state(t)
	if _, err := h.ent.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: taskID, SessionID: mainSession, Label: "CP2", Agent: model.AgentOpenClaw, State: state,
	}); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	before, err := contextbuild.Build(ctx, taskID, h.ctxOptions())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if before.Drift.RevalidationRequired && before.Drift.Outcome != drift.Matches {
		t.Logf("baseline drift outcome: %s", before.Drift.Outcome)
	}

	// A human changes the very file the historical state is about, and commits.
	writeFile(t, h.dir, "billing/webhooks.go", `package billing

// WebhookProcessor handles inbound billing webhooks.
type WebhookProcessor struct{ seen map[string]bool }

// Process handles one webhook event, ignoring duplicate delivery.
func (w *WebhookProcessor) Process(id string) error {
	if w.seen[id] {
		return nil
	}
	return nil
}
`)
	git(t, h.dir, "add", "-A")
	git(t, h.dir, "commit", "-q", "-m", "human edits webhooks")

	after, err := contextbuild.Build(ctx, taskID, h.ctxOptions())
	if err != nil {
		t.Fatalf("Build after drift: %v", err)
	}

	if after.Drift.Outcome == drift.Matches {
		t.Errorf("drift went undetected after a commit changed a tracked file: %+v", after.Drift)
	}
	if !after.Drift.RevalidationRequired {
		t.Error("revalidation was not required after drift")
	}
	rendered := after.Render()
	if !strings.Contains(strings.ToLower(rendered), "revalidat") {
		t.Errorf("the continuation context does not ask for revalidation:\n%s", rendered)
	}
}

// TestCrossAgentContinuity is the plan §44 acceptance test: one task id, two
// runtimes, the same engineering conclusions on both sides of the boundary.
func TestCrossAgentContinuity(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	ctx := context.Background()

	state := h.state(t)
	if _, err := h.ent.CreateCheckpoint(ctx, model.CheckpointRequest{
		TaskID: taskID, SessionID: mainSession, Label: "CP2", Agent: model.AgentOpenClaw, State: state,
	}); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	fromOpenClaw, err := contextbuild.Build(ctx, taskID, h.ctxOptions())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A Hermes session joins the same task.
	now := h.clock.Now()
	for _, ev := range []model.AgentEvent{
		{Type: model.SessionStarted, Timestamp: now, TaskID: taskID, SessionID: "hermes_cont",
			Agent: model.AgentHermes, Role: model.RoleMain, Summary: "continuing"},
		{Type: model.TaskResumed, Timestamp: now, TaskID: taskID, SessionID: "hermes_cont",
			Agent: model.AgentHermes, Summary: "resumed from checkpoint"},
	} {
		if err := h.store.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	fromHermes, err := contextbuild.Build(ctx, taskID, h.ctxOptions())
	if err != nil {
		t.Fatalf("Build after handover: %v", err)
	}

	if fromHermes.Task.ID != fromOpenClaw.Task.ID {
		t.Errorf("the task id changed with the worker: %q then %q", fromOpenClaw.Task.ID, fromHermes.Task.ID)
	}
	if fromHermes.State.Task.OriginalIntent != fromOpenClaw.State.Task.OriginalIntent {
		t.Error("original intent changed across the agent boundary")
	}
	if len(fromHermes.State.Rejected) != len(fromOpenClaw.State.Rejected) {
		t.Errorf("rejected approaches changed across the boundary: %d then %d",
			len(fromOpenClaw.State.Rejected), len(fromHermes.State.Rejected))
	}
	if len(fromHermes.State.FailingTests()) != len(fromOpenClaw.State.FailingTests()) {
		t.Error("failing tests changed across the agent boundary")
	}

	lineage, err := h.store.Lineage(ctx, taskID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if got := len(lineage.Agents()); got < 2 {
		t.Errorf("lineage records %d runtime(s), want at least 2", got)
	}
}

// TestNewFormatFlowsThroughTheWholePipeline closes the loop on the curveball: a
// transcript in the format this build had never seen, from a runtime it had
// never heard of, must produce usable engineering state through the same path
// as everything else.
func TestNewFormatFlowsThroughTheWholePipeline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, err := os.ReadFile(filepath.Join("..", "normalize", "testdata", "lifecycle_v2_acmecode.jsonl"))
	if err != nil {
		t.Fatalf("reading the curveball fixture: %v", err)
	}
	res, err := normalize.Stream(raw, normalize.Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(res.Events) == 0 {
		t.Fatal("the curveball fixture produced no events")
	}

	ingestedTask := res.Events[0].TaskID
	head, _ := h.repo.Head(ctx)
	now := h.clock.Now()
	if err := h.store.CreateTask(ctx, model.Task{
		ID: ingestedTask, Repo: head.Repo, Branch: head.Branch,
		RootSessionID: res.Events[0].SessionID, Title: "Ingested",
		Status: model.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, ev := range res.Events {
		if err := h.store.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent(%s): %v", ev.Type, err)
		}
	}

	built, err := contextbuild.Build(ctx, ingestedTask, h.ctxOptions())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if !strings.Contains(built.State.Task.OriginalIntent, "coupons") &&
		!strings.Contains(built.State.Task.OriginalIntent, "Reject expired") {
		t.Errorf("intent not recovered from the new format: %q", built.State.Task.OriginalIntent)
	}
	// The fixture runs one test twice: failing, then passing. The §32 merge rule
	// resolves the failure only because both runs carry the same test name.
	if got := len(built.State.FailingTests()); got != 0 {
		t.Errorf("%d failing test(s); the later passing run should have resolved the failure", got)
	}
	if len(built.State.Tests) != 1 {
		t.Errorf("tests = %d, want 1 (two runs of one test)", len(built.State.Tests))
	}
	if len(built.State.Checkpoints) == 0 {
		t.Error("the checkpoint recorded in the transcript was not recovered")
	}
	if got := built.Lineage.Agents(); len(got) != 1 || got[0] != model.AgentKind("acmecode") {
		t.Errorf("lineage agents = %v, want [acmecode]", got)
	}
}
