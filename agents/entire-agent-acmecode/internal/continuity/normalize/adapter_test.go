package normalize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// The adapter test layer from plan §63: what the JavaScript hook shims actually
// write must decode through the Go normalizer. Fixtures alone cannot prove this
// — they are what we *believe* the shims emit. This runs the shims.
//
// Node is not a build dependency, so the test skips when it is absent rather
// than failing a Go-only environment.

const adapterDriver = `
const path = require('path');
const os = require('os');
const fs = require('fs');

const root = process.argv[2];
const out = process.argv[3];
const { OpenClawAdapter } = require(path.join(root, 'adapters', 'openclaw', 'hook.js'));
const { HermesAdapter } = require(path.join(root, 'adapters', 'hermes', 'hook.js'));

const ocLog = path.join(out, 'openclaw.ndjson');
const hLog = path.join(out, 'hermes.ndjson');

const oc = new OpenClawAdapter({ taskId: 'task_adapter_demo', logFile: ocLog });
const main = { id: 'oc_main', role: 'main' };
oc.sessionStarted(main, 'Implement subscription pause');
oc.turnStarted(main, 'planning the change');
oc.toolUsed(main, { name: 'shell', ref: 'artifact://run/1' }, 'go test ./billing/...', {
  test_name: 'TestDuplicateWebhook', test_status: 'failed', nested: { dropped: true },
});
oc.subagentStarted({ id: 'oc_child', parent: 'oc_main', role: 'coding' }, 'fix webhook idempotency');
oc.subagentEnded({ id: 'oc_child', parent: 'oc_main', role: 'coding' }, 'done');
oc.checkpointCreated(main, 'cp_from_shim', 'CP2 stable');
oc.sessionEnded(main);
// Simulate a NEWER shim: the host has added a lifecycle point that this build
// does not map yet. The shim's own allowlist is this build's list, so it will
// not write one — the line is appended directly, which is exactly what an
// upgraded shim in the field would produce against an older continuity build.
fs.appendFileSync(ocLog, JSON.stringify({
  hook: 'context_compacted',
  ts: new Date().toISOString(),
  session: { id: 'oc_main', role: 'main' },
  task: { id: 'task_adapter_demo' },
  summary: 'compacted 40k tokens of context',
}) + '\n', 'utf8');

const h = new HermesAdapter({ taskId: 'task_adapter_demo', logFile: hLog });
h.sessionStart('hermes_main', 'continue subscription pause', 'main');
h.preLLM('hermes_main', 'deciding next step');
h.toolInvoked('hermes_main', 'shell', 'artifact://run/2', 'go test ./billing/...');
h.postLLM('hermes_main', 'chose explicit paused state instead of mutating renewal', {});
h.childAgentFinished('hermes_child', 'hermes_main', 'review complete');
h.checkpointWritten('hermes_main', 'cp_hermes_shim', 'CP3 post-curveball');
h.sessionEnd('hermes_main');

fs.writeFileSync(path.join(out, 'ok'), 'ok');
`

func nodeOrSkip(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"node", "node.exe"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p
		}
	}
	t.Skip("node is not installed; the adapter round-trip test needs it to run the shims")
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return abs
}

// TestAdaptersFeedTheNormalizer runs both hook shims and decodes what they wrote.
// It is the end of the §29 claim: two host runtimes, two wire shapes, one event
// vocabulary, with no per-host task-state engine anywhere.
func TestAdaptersFeedTheNormalizer(t *testing.T) {
	node := nodeOrSkip(t)
	root := repoRoot(t)
	out := t.TempDir()

	driver := filepath.Join(out, "driver.js")
	if err := os.WriteFile(driver, []byte(adapterDriver), 0o644); err != nil {
		t.Fatalf("writing driver: %v", err)
	}

	cmd := exec.Command(node, driver, root, out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the shims failed: %v\n%s", err, combined)
	}

	tests := []struct {
		file       string
		wantAgent  model.AgentKind
		wantFormat Format
		wantTypes  []model.EventType
	}{
		{
			file:       "openclaw.ndjson",
			wantAgent:  model.AgentOpenClaw,
			wantFormat: FormatOpenClawV1,
			wantTypes: []model.EventType{
				model.SessionStarted, model.TurnStarted, model.ToolUsed,
				model.SubagentStarted, model.SubagentEnded,
				model.CheckpointCreated, model.SessionEnded,
			},
		},
		{
			file:       "hermes.ndjson",
			wantAgent:  model.AgentHermes,
			wantFormat: FormatHermesV1,
			wantTypes: []model.EventType{
				model.SessionStarted, model.TurnStarted, model.ToolUsed,
				model.TurnEnded, model.SubagentEnded,
				model.CheckpointCreated, model.SessionEnded,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(out, tt.file))
			if err != nil {
				t.Fatalf("the shim wrote no log: %v", err)
			}
			res, err := Stream(raw, Options{})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}

			// Format detection must recognise real shim output, not just fixtures.
			if want := []string{string(tt.wantFormat)}; len(res.Report.Formats) != 1 || res.Report.Formats[0] != want[0] {
				t.Errorf("detected %v, want %v", res.Report.Formats, want)
			}

			var got []model.EventType
			for _, e := range res.Events {
				if e.Type == model.EventUnknown {
					continue
				}
				got = append(got, e.Type)
				if e.Agent != tt.wantAgent {
					t.Errorf("event %s has agent %q, want %q", e.Type, e.Agent, tt.wantAgent)
				}
				if e.TaskID != "task_adapter_demo" {
					t.Errorf("event %s lost the task anchor: %q", e.Type, e.TaskID)
				}
				if err := e.Validate(); err != nil {
					t.Errorf("shim produced an event the domain rejects: %v", err)
				}
			}
			if !sameTypes(got, tt.wantTypes) {
				t.Errorf("lifecycle = %v, want %v", got, tt.wantTypes)
			}
		})
	}

	// The OpenClaw log carries one hook from a newer shim that this build does
	// not map — the curveball scenario in miniature. It must be reported, it
	// must be retained, and it must not have cost the session.
	raw, err := os.ReadFile(filepath.Join(out, "openclaw.ndjson"))
	if err != nil {
		t.Fatalf("reading openclaw log: %v", err)
	}
	res, err := Stream(raw, Options{})
	if err != nil {
		t.Fatalf("an unmapped hook took the shim output down: %v", err)
	}
	if res.Report.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", res.Report.Unknown)
	}
	if res.Report.Complete() {
		t.Error("a log containing an unmapped hook reported itself as complete")
	}

	// Rule 2: a nested meta value is a payload, and a payload travels by
	// reference. The shim must not have copied it into the envelope.
	if strings.Contains(string(raw), "\"nested\"") {
		t.Error("the shim inlined a nested payload into the envelope")
	}
	for _, e := range res.Events {
		if e.Type == model.ToolUsed && e.Attr("test_status") != "failed" {
			t.Errorf("scalar tool metadata was lost: %v", e.Attrs)
		}
	}
}

// TestAdaptersRequireATask covers the one thing a shim refuses to do: emit an
// event that names no task, which nothing downstream could ever file.
func TestAdaptersRequireATask(t *testing.T) {
	node := nodeOrSkip(t)
	root := repoRoot(t)
	out := t.TempDir()

	script := `
const path = require('path');
const { OpenClawAdapter } = require(path.join(process.argv[2], 'adapters', 'openclaw', 'hook.js'));
try { new OpenClawAdapter({}); console.log('NO_ERROR'); }
catch (e) { console.log('REFUSED'); }
`
	driver := filepath.Join(out, "d.js")
	if err := os.WriteFile(driver, []byte(script), 0o644); err != nil {
		t.Fatalf("writing driver: %v", err)
	}
	got, err := exec.Command(node, driver, root).CombinedOutput()
	if err != nil {
		t.Fatalf("running driver: %v\n%s", err, got)
	}
	if !strings.Contains(string(got), "REFUSED") {
		t.Errorf("an adapter accepted construction with no task id: %s", got)
	}
}

func sameTypes(got, want []model.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
