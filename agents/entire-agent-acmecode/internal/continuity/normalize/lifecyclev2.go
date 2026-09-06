package normalize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// lifecycleV2Envelope is the flat lifecycle JSONL a newer host emits.
//
// Three things about it broke assumptions the v1 adapters were built on, and
// each is handled explicitly below rather than papered over:
//
//   - It carries no task id. The anchor is derived from repository, branch and
//     session id instead, through the same model.NewTaskID the rest of the
//     system uses, so a transcript ingested twice lands on the same task.
//   - It names its own runtime, in session_started. The format is therefore not
//     tied to one agent the way the OpenClaw and Hermes envelopes are.
//   - It splits a tool call across two records correlated by call_id, so the
//     outcome of a command is only knowable by remembering the call.
type lifecycleV2Envelope struct {
	Timestamp string `json:"timestamp"`
	Event     string `json:"event"`
	SessionID string `json:"session_id"`
	ParentID  string `json:"parent_session_id"`
	Agent     struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agent"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Role       string `json:"role"`

	MessageID string `json:"message_id"`
	ParentMsg string `json:"parent_message_id"`
	Text      string `json:"text"`

	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
	Input  struct {
		Command string `json:"command"`
		Query   string `json:"query"`
	} `json:"input"`
	Output struct {
		ExitCode *int   `json:"exit_code"`
		Summary  string `json:"summary"`
		Stderr   string `json:"stderr"`
	} `json:"output"`

	Path         string          `json:"path"`
	Lines        json.RawMessage `json:"lines"`
	Change       string          `json:"change"`
	LinesAdded   *int            `json:"lines_added"`
	LinesRemoved *int            `json:"lines_removed"`
	Summary      string          `json:"summary"`

	CheckpointID  string   `json:"checkpoint_id"`
	GitCommit     string   `json:"git_commit"`
	Intent        string   `json:"intent"`
	OpenQuestions []string `json:"open_questions"`

	Status string `json:"status"`
}

// lifecycleV2Events maps the format's lifecycle names onto the domain vocabulary.
//
// file_read and file_changed become ToolUsed rather than gaining event types of
// their own: they are things the agent did with a tool, and the domain models
// what changed through repository facts, which are Level 1 evidence and do not
// depend on an agent's self-report (plan §31).
var lifecycleV2Events = map[string]model.EventType{
	"session_started":    model.SessionStarted,
	"session_ended":      model.SessionEnded,
	"user_prompt":        model.TurnStarted,
	"agent_response":     model.TurnEnded,
	"tool_call":          model.ToolUsed,
	"tool_result":        model.ToolUsed,
	"file_read":          model.ToolUsed,
	"file_changed":       model.ToolUsed,
	"checkpoint_created": model.CheckpointCreated,
}

var lifecycleV2Index = lowerIndex(lifecycleV2Events)

// lifecycleV2Ignored are names this build recognises and deliberately does not
// model. Listing them is what keeps them out of the "unrecognised" count: a
// usage record is not a hole in the timeline, it simply has no bearing on the
// engineering state of a task.
var lifecycleV2Ignored = map[string]string{
	"usage": "token accounting carries no engineering state",
}

// lifecycleV2 decodes the format. It is stateful for the length of one stream.
type lifecycleV2 struct {
	// taskBySession remembers the anchor derived from each session header, so
	// records after it inherit the same task id.
	taskBySession map[string]string
	// agentBySession remembers the runtime each session reported for itself.
	agentBySession map[string]model.AgentKind
	// commandByCall remembers a tool call so its result can be attributed to the
	// command that produced it.
	commandByCall map[string]string
	// toolByCall does the same for the tool name.
	toolByCall map[string]string
}

func newLifecycleV2() *lifecycleV2 {
	return &lifecycleV2{
		taskBySession:  map[string]string{},
		agentBySession: map[string]model.AgentKind{},
		commandByCall:  map[string]string{},
		toolByCall:     map[string]string{},
	}
}

func (d *lifecycleV2) format() Format { return FormatLifecycleV2 }

func (d *lifecycleV2) mapRecord(index int, rec json.RawMessage, opts Options) (mapped, error) {
	var env lifecycleV2Envelope
	if err := json.Unmarshal(rec, &env); err != nil {
		return mapped{}, fmt.Errorf("normalize: %s: record %d: %w", FormatLifecycleV2, index, err)
	}

	name := strings.TrimSpace(env.Event)
	session := strings.TrimSpace(env.SessionID)

	if reason, ok := lifecycleV2Ignored[strings.ToLower(name)]; ok {
		return mapped{name: name, ignored: true, event: model.AgentEvent{Summary: reason}}, nil
	}

	ts, err := parseRFC3339(env.Timestamp, model.AgentUnknown, index, name)
	if err != nil {
		// Keep enough of the record for the unknown path to anchor it, if the
		// caller is in tolerant mode; a record without a usable timestamp still
		// cannot be ordered, so it will be reported rather than stored.
		return mapped{name: name}, nil
	}

	typ, known := lifecycleV2Index[strings.ToLower(name)]
	agent := d.resolveAgent(session, env)
	taskID := d.resolveTask(session, env, opts)

	if !known {
		// Hand the unknown path a partially-built event so it can be anchored.
		return mapped{name: name, event: model.AgentEvent{
			Timestamp: ts, TaskID: taskID, SessionID: session,
			ParentSessionID: strings.TrimSpace(env.ParentID), Agent: agent,
		}}, nil
	}

	role, roleKnown := hostRole(env.Role)
	var attrs map[string]string
	if !roleKnown {
		attrs = setAttr(attrs, attrRawRole, env.Role)
	}

	ev := model.AgentEvent{
		Type:            typ,
		Timestamp:       ts,
		TaskID:          taskID,
		SessionID:       session,
		ParentSessionID: strings.TrimSpace(env.ParentID),
		Agent:           agent,
		Role:            role,
	}

	switch strings.ToLower(name) {
	case "session_started":
		ev.Summary = env.Summary
		attrs = setAttr(attrs, model.AttrAgentName, env.Agent.Name)
		attrs = setAttr(attrs, "agent_version", env.Agent.Version)
		attrs = setAttr(attrs, "repository", env.Repository)
		attrs = setAttr(attrs, "branch", env.Branch)

	case "session_ended":
		ev.Summary = env.Summary
		attrs = setAttr(attrs, "status", env.Status)
		// Only a clean completion is a clean end. Anything else leaves the
		// session's capture incomplete, and the domain has an event that says
		// exactly that rather than a status string nobody downstream reads.
		if s := strings.ToLower(strings.TrimSpace(env.Status)); s != "" && s != "completed" && s != "complete" {
			ev.Type = model.SessionInterrupted
		}

	case "user_prompt":
		// The opening prompt is where original intent comes from, and original
		// intent is the one field a later worker cannot reconstruct (plan §41).
		ev.Summary = env.Text
		attrs = setAttr(attrs, "message_id", env.MessageID)

	case "agent_response":
		ev.Summary = env.Text
		attrs = setAttr(attrs, "message_id", env.MessageID)
		attrs = setAttr(attrs, "parent_message_id", env.ParentMsg)

	case "tool_call":
		ev.Summary = firstNonEmpty(env.Input.Command, env.Input.Query, env.Summary)
		ev.PayloadRef = env.CallID
		attrs = setAttr(attrs, attrToolName, env.Tool)
		attrs = setAttr(attrs, "call_id", env.CallID)
		// Remember the call so its result can name the command that produced it.
		if id := strings.TrimSpace(env.CallID); id != "" {
			d.commandByCall[id] = strings.TrimSpace(env.Input.Command)
			d.toolByCall[id] = strings.TrimSpace(env.Tool)
		}

	case "tool_result":
		ev.PayloadRef = env.CallID
		ev.Summary = env.Output.Summary
		call := strings.TrimSpace(env.CallID)
		command := d.commandByCall[call]
		attrs = setAttr(attrs, attrToolName, firstNonEmpty(env.Tool, d.toolByCall[call]))
		attrs = setAttr(attrs, "call_id", call)
		if env.Output.ExitCode != nil {
			attrs = setAttr(attrs, "exit_code", fmt.Sprint(*env.Output.ExitCode))
		}
		// A test run is the single most load-bearing fact in a handoff, so it is
		// lifted into the structured form the deterministic derivation reads
		// (plan §16). The stderr is deliberately not carried over: it is host
		// output that may contain anything, and a summary is enough to act on.
		if isTestCommand(command) && env.Output.ExitCode != nil {
			attrs = setAttr(attrs, "test_name", testNameFromCommand(command))
			attrs = setAttr(attrs, "test_command", command)
			status := model.TestFailed
			if *env.Output.ExitCode == 0 {
				status = model.TestPassed
			}
			attrs = setAttr(attrs, "test_status", string(status))
		}

	case "file_read":
		ev.Summary = "read " + env.Path
		attrs = setAttr(attrs, attrToolName, "file_read")
		attrs = setAttr(attrs, "path", env.Path)
		if s, ok := scalarString(env.Lines); ok {
			attrs = setAttr(attrs, "lines", s)
		}

	case "file_changed":
		ev.Summary = firstNonEmpty(env.Summary, env.Change+" "+env.Path)
		attrs = setAttr(attrs, attrToolName, "file_changed")
		attrs = setAttr(attrs, "path", env.Path)
		attrs = setAttr(attrs, "change", env.Change)
		if env.LinesAdded != nil {
			attrs = setAttr(attrs, "lines_added", fmt.Sprint(*env.LinesAdded))
		}
		if env.LinesRemoved != nil {
			attrs = setAttr(attrs, "lines_removed", fmt.Sprint(*env.LinesRemoved))
		}

	case "checkpoint_created":
		ev.Summary = env.Summary
		attrs = setAttr(attrs, attrCheckpointID, env.CheckpointID)
		attrs = setAttr(attrs, "commit_sha", env.GitCommit)
		attrs = setAttr(attrs, "intent", env.Intent)
		if len(env.OpenQuestions) > 0 {
			// Open questions are recorded rather than resolved. They are exactly
			// the kind of thing a fresh worker would otherwise have to rediscover.
			attrs = setAttr(attrs, "open_questions", strings.Join(env.OpenQuestions, " | "))
		}
	}

	ev.Attrs = attrs
	out, err := finalize(ev, agent, index, name)
	if err != nil {
		return mapped{}, err
	}
	return mapped{event: out, name: name, ok: true}, nil
}

// resolveAgent determines which runtime a record belongs to, remembering what
// the session header declared so later records inherit it.
func (d *lifecycleV2) resolveAgent(session string, env lifecycleV2Envelope) model.AgentKind {
	if name := strings.TrimSpace(env.Agent.Name); name != "" {
		agent := model.NormalizeAgent(name)
		if session != "" {
			d.agentBySession[session] = agent
		}
		return agent
	}
	if agent, ok := d.agentBySession[session]; ok {
		return agent
	}
	// The runtime is genuinely not stated on this record. Unknown is the honest
	// answer; guessing a runtime from the format would be inventing a fact.
	return model.AgentUnknown
}

// resolveTask derives the task anchor for a record.
//
// The precedence is deliberate. An explicit caller-supplied task id wins, since
// the caller knows which task it is ingesting into. Otherwise the anchor comes
// from the session header's repository and branch, which makes ingestion
// idempotent: the same transcript always derives the same task id. Only when
// neither exists do the caller's repository fallbacks apply, which is what lets
// a transcript that begins mid-session — the incomplete case — still be filed
// against the right task instead of being discarded.
func (d *lifecycleV2) resolveTask(session string, env lifecycleV2Envelope, opts Options) string {
	if id := strings.TrimSpace(opts.TaskID); id != "" {
		return id
	}
	if known, ok := d.taskBySession[session]; ok && known != "" {
		return known
	}

	repo := firstNonEmpty(strings.TrimSpace(env.Repository), strings.TrimSpace(opts.Repo))
	branch := firstNonEmpty(strings.TrimSpace(env.Branch), strings.TrimSpace(opts.Branch))
	if repo == "" || session == "" {
		return ""
	}
	id := model.NewTaskID(repo, branch, session)
	d.taskBySession[session] = id
	return id
}

// isTestCommand reports whether a shell command looks like a test run. It is a
// heuristic, and it is used only to enrich an event with structured test
// attributes — never to claim a test passed. A missed detection costs a little
// structure; a false positive would put a fabricated test result into task
// state, so the patterns stay narrow.
func isTestCommand(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	if c == "" {
		return false
	}
	for _, marker := range []string{
		"npm test", "yarn test", "pnpm test", "go test", "pytest", "jest",
		"vitest", "cargo test", "mvn test", "gradle test", "rspec", "phpunit",
		"dotnet test", "bun test", "make test",
	} {
		if strings.Contains(c, marker) {
			return true
		}
	}
	return false
}

// testNameFromCommand picks the most specific label a test command carries: the
// test file or target it names, falling back to the command itself. The name is
// what a later result is matched against when deciding whether a failure has
// been resolved (plan §32), so it must be stable across runs of the same test.
func testNameFromCommand(command string) string {
	fields := strings.Fields(command)
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		if strings.HasPrefix(f, "-") {
			continue
		}
		lower := strings.ToLower(f)
		if strings.Contains(lower, "test") || strings.Contains(lower, "spec") {
			if lower == "test" || lower == "npm" || lower == "yarn" || lower == "pnpm" || lower == "go" {
				continue
			}
			return f
		}
	}
	return strings.TrimSpace(command)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// compile-time assertions that the decoders satisfy the interface.
var (
	_ decoder = (*lifecycleV2)(nil)
	_ decoder = legacyDecoder{}
)
