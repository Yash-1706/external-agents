# AcmeCode - External Agent Research

## Verdict: PARTIALLY COMPATIBLE — transcript analysis only

AcmeCode emits a structured JSONL lifecycle transcript that maps cleanly onto the Entire
event vocabulary, which is enough for a useful `transcript_analyzer` integration. It does **not**
have a hook-install surface this build has been able to verify, so hooks are not declared.

That split is deliberate and is the honest reading of the evidence available. Declaring `hooks`
would cause Entire to install hooks that never fire, which presents to a user as "capture is
working" while nothing is captured — worse than declaring nothing.

## Static Checks

| Check | Result | Notes |
|---|---|---|
| Binary present | UNVERIFIED | AcmeCode was not installed in the development environment; no live probe was run |
| Transcript format | PASS | JSONL, one record per line, verified against an official fixture (17 records, `AcmeCode 1.4.2`) |
| Session keywords | PASS | `session_started`, `session_ended`, `session_id`, `status`, `duration_seconds` |
| Prompt / response | PASS | `user_prompt` and `agent_response`, correlated by `message_id` / `parent_message_id` |
| Tool surface | PASS | `tool_call` / `tool_result` correlated by `call_id`; `file_read`, `file_changed` |
| Checkpoint surface | PASS | `checkpoint_created` carries `checkpoint_id`, `git_commit`, `intent`, `open_questions` |
| Hook mechanism | FAIL | No documented hook config path or install surface found |
| Config directory | ASSUMED | `.acmecode/` — used for `Detect` and session storage; not confirmed against a live install |

## Transcript

- Location (assumed): `.acmecode/sessions/<session-id>.jsonl`
- Framing: JSONL, one JSON object per line
- Timestamps: RFC3339 **with offset** (`2026-09-06T09:00:00.000+05:30`), normalized to UTC

### Two live formats

This is the defining property of the integration. AcmeCode shipped a new lifecycle format while
existing installs kept emitting the original, so both are in the field simultaneously. Format is
detected **per record**, so a log containing both decodes correctly.

| Format | Discriminator |
|---|---|
| `lifecycle/v2` (new) | `event` + RFC3339 `timestamp` + `session_id` |
| `openclaw/v1` (original) | `hook` + `ts` |
| `hermes/v1` (original) | `event` + epoch-millis `time` + `agent_session` |

### Event mapping

| AcmeCode | Entire event | Notes |
|---|---|---|
| `session_started` | SessionStarted | Carries `agent.name`, `repository`, `branch` |
| `session_ended` | SessionEnded | `status` other than `completed` maps to SessionInterrupted |
| `user_prompt` | TurnStarted | Source of original intent |
| `agent_response` | TurnEnded | |
| `tool_call` | ToolUsed | Remembered by `call_id` |
| `tool_result` | ToolUsed | Correlated to its call; test commands lift to structured test results |
| `file_read` / `file_changed` | ToolUsed | Only `file_changed` counts as a modification |
| `checkpoint_created` | CheckpointCreated | `intent` and `open_questions` preserved |
| `usage` | *ignored* | Recognised, deliberately not modelled: token accounting carries no engineering state |
| anything else | Unknown | **Retained**, with the host's own name in `raw_kind` |

## Three properties that required design work

1. **No `task_id` anywhere.** The transcript names a `repository`, a `branch` and a `session_id`,
   never a task. The anchor is derived through `model.NewTaskID(repo, branch, session)`, which also
   makes ingestion idempotent — the same transcript always lands on the same task. A transcript
   that begins mid-session falls back to the repository the caller is ingesting into.

2. **The runtime names itself.** `AcmeCode` is outside the enumerated agent vocabulary, so the
   vocabulary was widened to accept any safe identifier — a strict widening that cannot reject
   anything previously accepted.

3. **Tool calls span two records.** A test outcome is only knowable by remembering the call, so the
   decoder is stateful for the length of a stream. This is what lets a failing run and a later
   passing run of the same test resolve against each other.

## Known gaps

- No live AcmeCode install was available; `Detect`, the session directory and the resume command
  are written to the documented shape but not confirmed against a running agent.
- `compact_transcript` and `token_calculator` are not implemented.
