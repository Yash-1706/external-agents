package normalize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// hermesEnvelope is the JSON record the Hermes plugin emits (plan §28).
//
// It shares nothing but JSON with the OpenClaw envelope: different field names,
// a numeric epoch-millisecond clock instead of RFC3339, "note" instead of
// "summary", "fields" instead of "meta", and no dedicated tool block. Mapping
// the two shapes onto one model.AgentEvent here is the actual work behind the
// agent-independence claim in plan §29; if this were easy the claim would be
// worth less.
//
// As with OpenClaw, delegated_from names the parent of agent_session, so a
// ChildAgentFinished record describes the child.
type hermesEnvelope struct {
	Event         string                     `json:"event"`
	Time          json.Number                `json:"time"`
	AgentSession  string                     `json:"agent_session"`
	DelegatedFrom string                     `json:"delegated_from"`
	AgentRole     string                     `json:"agent_role"`
	EntireTask    string                     `json:"entire_task"`
	PayloadRef    string                     `json:"payload_ref"`
	Note          string                     `json:"note"`
	Fields        map[string]json.RawMessage `json:"fields"`
}

// hermesEvents maps the Hermes lifecycle names to normalized event types.
//
// The interesting entry is the one that is absent. Hermes reports
// ChildAgentFinished but has no child-agent *start* hook, so a Hermes stream can
// never produce a SubagentStarted. We do not synthesise one from the child's
// first event: the host never claimed a spawn instant, and manufacturing one
// would put a fabricated timestamp into the lineage a later reader is supposed
// to trust (plan §9). A reader that needs the spawn moment for a Hermes task
// should see it missing, not see a guess.
var hermesEvents = map[string]model.EventType{
	"SessionStart":       model.SessionStarted,
	"SessionEnd":         model.SessionEnded,
	"SessionAborted":     model.SessionInterrupted,
	"PreLLM":             model.TurnStarted,
	"PostLLM":            model.TurnEnded,
	"ToolInvoked":        model.ToolUsed,
	"ChildAgentFinished": model.SubagentEnded,
	"CheckpointWritten":  model.CheckpointCreated,
}

var hermesEventIndex = lowerIndex(hermesEvents)

// hermesToolNameKeys are the field names Hermes uses for the tool it invoked,
// in precedence order. Hermes has no structured tool block, so the name has to
// come out of the free-form fields bag; when neither key is present we emit the
// ToolUsed event without a tool_name rather than inventing a label for it.
var hermesToolNameKeys = []string{attrToolName, "tool"}

type hermesNormalizer struct{}

func (hermesNormalizer) Agent() model.AgentKind { return model.AgentHermes }

// Normalize implements Normalizer for Hermes event output.
func (n hermesNormalizer) Normalize(raw []byte) ([]model.AgentEvent, error) {
	return normalizeStream(model.AgentHermes, raw, n.mapRecord)
}

func (hermesNormalizer) mapRecord(index int, rec json.RawMessage) (mapped, error) {
	var env hermesEnvelope
	if err := json.Unmarshal(rec, &env); err != nil {
		return mapped{}, fmt.Errorf("normalize: %s: record %d: %w", model.AgentHermes, index, err)
	}

	name := strings.TrimSpace(env.Event)
	typ, known := hermesEventIndex[strings.ToLower(name)]
	if !known {
		return mapped{name: name}, nil
	}

	ts, err := parseEpochMillis(env.Time, model.AgentHermes, index, name)
	if err != nil {
		return mapped{}, err
	}

	attrs := scalarAttrs(env.Fields)
	role, roleKnown := hostRole(env.AgentRole)
	if !roleKnown {
		attrs = setAttr(attrs, attrRawRole, env.AgentRole)
	}

	switch typ {
	case model.ToolUsed:
		for _, key := range hermesToolNameKeys {
			if v := strings.TrimSpace(attrs[key]); v != "" {
				attrs = setAttr(attrs, attrToolName, v)
				break
			}
		}
	case model.CheckpointCreated:
		// Same rule as OpenClaw: an explicit field wins, otherwise the payload
		// ref on a CheckpointWritten record is the checkpoint it wrote. Both
		// runtimes therefore land the id in the same attribute, which is what
		// lets downstream code read checkpoint lineage without knowing which
		// agent produced it.
		id := attrs[attrCheckpointID]
		if strings.TrimSpace(id) == "" {
			id = env.PayloadRef
		}
		attrs = setAttr(attrs, attrCheckpointID, id)
	}

	ev := model.AgentEvent{
		Type:            typ,
		Timestamp:       ts,
		TaskID:          strings.TrimSpace(env.EntireTask),
		SessionID:       strings.TrimSpace(env.AgentSession),
		ParentSessionID: strings.TrimSpace(env.DelegatedFrom),
		Role:            role,
		PayloadRef:      env.PayloadRef,
		Summary:         env.Note,
		Attrs:           attrs,
	}

	out, err := finalize(ev, model.AgentHermes, index, name)
	if err != nil {
		return mapped{}, err
	}
	return mapped{event: out, name: name, ok: true}, nil
}
