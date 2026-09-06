package normalize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// openClawEnvelope is the JSON record our OpenClaw hook shim writes, one per
// lifecycle point (plan §27).
//
// The shim reports the session the hook fired *for*: on subagent_start and
// subagent_end that is the child session, with session.parent naming the
// delegating session. That is what makes the sub-agent events usable for lineage
// (plan §8) — a SubagentStarted whose SessionID was the parent's would tell a
// later reader nothing about which child ran.
//
// Unknown fields are ignored by encoding/json, which is deliberate: a newer shim
// adding a field must not break an older continuity build. A record that is not
// an OpenClaw envelope at all still parses, but carries no hook name and is
// therefore skipped — and a whole stream of those is reported as an error by
// normalizeStream, which is how "you pointed the wrong normalizer at this file"
// surfaces.
type openClawEnvelope struct {
	Hook    string `json:"hook"`
	TS      string `json:"ts"`
	Session struct {
		ID     string `json:"id"`
		Parent string `json:"parent"`
		Role   string `json:"role"`
	} `json:"session"`
	Task struct {
		ID string `json:"id"`
	} `json:"task"`
	Tool struct {
		Name string `json:"name"`
		Ref  string `json:"ref"`
	} `json:"tool"`
	Summary string                     `json:"summary"`
	Meta    map[string]json.RawMessage `json:"meta"`
}

// openClawHooks maps the OpenClaw hook names to normalized event types.
//
// It is intentionally not exhaustive over model.AllEventTypes(). TaskResumed,
// HandoffCreated and ConstraintAdded have no OpenClaw hook: they are recorded by
// the continuity CLI itself when a human resumes or hands off a task, or when a
// mid-task constraint arrives (plan §41). Mapping some other hook onto them
// would be exactly the invented lifecycle semantic plan §9 rules out.
var openClawHooks = map[string]model.EventType{
	"session_start":     model.SessionStarted,
	"session_end":       model.SessionEnded,
	"session_interrupt": model.SessionInterrupted,
	"turn_start":        model.TurnStarted,
	"turn_end":          model.TurnEnded,
	"tool_use":          model.ToolUsed,
	"subagent_start":    model.SubagentStarted,
	"subagent_end":      model.SubagentEnded,
	"checkpoint":        model.CheckpointCreated,
}

var openClawHookIndex = lowerIndex(openClawHooks)

type openClawNormalizer struct{}

func (openClawNormalizer) Agent() model.AgentKind { return model.AgentOpenClaw }

// Normalize implements Normalizer for OpenClaw hook output.
func (n openClawNormalizer) Normalize(raw []byte) ([]model.AgentEvent, error) {
	return normalizeStream(model.AgentOpenClaw, raw, n.mapRecord)
}

func (openClawNormalizer) mapRecord(index int, rec json.RawMessage) (mapped, error) {
	var env openClawEnvelope
	if err := json.Unmarshal(rec, &env); err != nil {
		// A record that is not even shaped like an envelope is corrupt input, not
		// an unmapped lifecycle point, so it is reported rather than skipped.
		return mapped{}, fmt.Errorf("normalize: %s: record %d: %w", model.AgentOpenClaw, index, err)
	}

	name := strings.TrimSpace(env.Hook)
	typ, known := openClawHookIndex[strings.ToLower(name)]
	if !known {
		return mapped{name: name}, nil
	}

	ts, err := parseRFC3339(env.TS, model.AgentOpenClaw, index, name)
	if err != nil {
		return mapped{}, err
	}

	attrs := scalarAttrs(env.Meta)
	role, roleKnown := hostRole(env.Session.Role)
	if !roleKnown {
		attrs = setAttr(attrs, attrRawRole, env.Session.Role)
	}

	switch typ {
	case model.ToolUsed:
		// The name of the tool is the whole of what we keep about a tool call.
		// The call's arguments and output stay in the artifact PayloadRef points
		// at; copying them here is how a credential ends up in a checkpoint
		// (plan §34).
		attrs = setAttr(attrs, attrToolName, env.Tool.Name)
	case model.CheckpointCreated:
		// The shim may name the checkpoint explicitly in meta; when it does not,
		// the ref carried on a checkpoint hook *is* the checkpoint. An explicit
		// name wins, because it is the host's own claim rather than our reading
		// of a generic field.
		id := attrs[attrCheckpointID]
		if strings.TrimSpace(id) == "" {
			id = env.Tool.Ref
		}
		attrs = setAttr(attrs, attrCheckpointID, id)
	}

	ev := model.AgentEvent{
		Type:            typ,
		Timestamp:       ts,
		TaskID:          strings.TrimSpace(env.Task.ID),
		SessionID:       strings.TrimSpace(env.Session.ID),
		ParentSessionID: strings.TrimSpace(env.Session.Parent),
		Role:            role,
		PayloadRef:      env.Tool.Ref,
		Summary:         env.Summary,
		Attrs:           attrs,
	}

	out, err := finalize(ev, model.AgentOpenClaw, index, name)
	if err != nil {
		return mapped{}, err
	}
	return mapped{event: out, name: name, ok: true}, nil
}
