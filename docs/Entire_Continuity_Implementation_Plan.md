# Entire Continuity — End-to-End Implementation Plan

## Project Working Name

**Entire Continuity**

## Track

**T3 — Bring Entire to a New Agent or Workflow**

Primary integration target: **OpenClaw**

Secondary/stretch integration: **Hermes**

---

# 1. Product Definition

## One-sentence definition

> Entire Continuity is a task-centric continuity layer for multi-agent software development that lets a software task move between OpenClaw/Hermes sessions, sub-agents, and humans without losing the engineering state required to make the next correct change.

## Core product thesis

AI agents increasingly work asynchronously, in parallel, and across multiple sessions. The problem is not simply that agents lack memory. The deeper problem is that **software work is fragmented across execution sessions while Git primarily represents the resulting code state**.

A new worker can inspect the repository, branch, diff and commits, but the complete engineering state also contains:

- original intent
- explicit requirements
- completed versus incomplete work
- important decisions
- rejected approaches
- failures encountered
- assumptions
- test state
- relevant files
- known risks
- recommended next action
- historical evidence supporting those conclusions

Entire already captures development context in checkpoints and associates that context with Git/development history. The project should extend this into a task-level continuity workflow rather than creating a generic agent memory product.

---

# 2. The Problem We Are Solving

## Precise problem statement

> When a software task moves between autonomous agents, sub-agents, scheduled sessions, or humans, the next worker has to reconstruct the engineering state of the task from fragmented context. This causes repeated investigation, repeated failed approaches, wrong assumptions, lost decisions, and slower/less reliable continuation.

## What we are NOT solving

We are not building:

- a generic chatbot memory system
- a transcript viewer
- a generic agent dashboard
- another coding agent
- an OpenClaw UI replacement
- a generic observability product
- a generic task manager
- a summarizer that produces prose without evidence
- merely a wrapper around an Entire CLI command

The product must solve a **specific software-development continuity problem**.

---

# 3. Why OpenClaw and Hermes Matter

OpenClaw and Hermes are not being added simply because they are popular agents.

They represent a workflow in which a single engineering task can span:

```text
Main agent
    ↓
planning / research
    ↓
coding sub-agent
    ↓
test sub-agent
    ↓
review sub-agent
    ↓
background / scheduled work
    ↓
human
    ↓
another agent
```

This creates the exact continuity problem we want to solve.

## OpenClaw

The integration should use OpenClaw's supported lifecycle/plugin mechanisms to observe:

- session lifecycle
- agent lifecycle
- turns
- tool activity where safely available
- sub-agent creation/completion
- context/session relationships

OpenClaw's architecture supports isolated sub-agent sessions and background task flows.

## Hermes

Hermes supports persistent sessions, delegated child agents, scheduled sessions and plugin/hook surfaces.

The important workflow property is that a child agent has its own execution context and does not automatically possess the entire parent's reasoning/history.

## Strategic positioning

Do not pitch:

> "We integrated Entire with OpenClaw and Hermes."

Pitch:

> "We make the software task persistent across OpenClaw, Hermes, their sub-agents, and humans. The worker can change; the engineering task does not."

---

# 4. The Product Architecture

```text
                         SOFTWARE TASK
                              │
                              ▼
                     OpenClaw / Hermes
                              │
                         agent work
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
            tools            code            tests
              │               │               │
              └───────────────┼───────────────┘
                              ▼
                           ENTIRE
                         checkpoint
                              │
                              ▼
                    ENGINEERING STATE
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
            HUMAN           AGENT A          AGENT B
              │               │               │
              └───────────────┼───────────────┘
                              ▼
                           CONTINUE
```

Expanded system:

```text
┌─────────────────────────────────────────────────────┐
│                  USER / AGENT                       │
│                                                     │
│      OpenClaw                     Hermes            │
└────────────────┬────────────────────┬───────────────┘
                 │                    │
                 ▼                    ▼
        ┌─────────────────────────────────────┐
        │       Agent Integration Layer       │
        │                                     │
        │ OpenClaw adapter / Hermes adapter   │
        └──────────────────┬──────────────────┘
                           │
                           ▼
        ┌─────────────────────────────────────┐
        │      Entire Agent Protocol          │
        │                                     │
        │ lifecycle / sessions / transcripts  │
        │ sub-agents / checkpoint integration │
        └──────────────────┬──────────────────┘
                           │
                           ▼
        ┌─────────────────────────────────────┐
        │          Entire Checkpoint          │
        │                                     │
        │ Git + session + development state  │
        └──────────────────┬──────────────────┘
                           │
                           ▼
        ┌─────────────────────────────────────┐
        │       Engineering State Engine      │
        │                                     │
        │ intent                              │
        │ requirements                        │
        │ progress                            │
        │ failures                            │
        │ decisions                           │
        │ evidence                            │
        │ tests                               │
        │ next actions                        │
        └──────────────────┬──────────────────┘
                           │
                           ▼
        ┌─────────────────────────────────────┐
        │           Continuation               │
        │                                     │
        │ human handoff / agent resume        │
        │ cross-agent task lineage            │
        └─────────────────────────────────────┘
```

---

# 5. Key Architectural Principle

## Entire remains the authoritative development-history layer.

Do not create a competing source of truth.

The system should conceptually be:

```text
Agent execution
      ↓
Entire session/checkpoint
      ↓
Task-state interpretation
      ↓
human/agent continuity
```

not:

```text
Agent execution
      ↓
new independent database
      ↓
another history system
      ↓
Entire becomes optional
```

The new task-level model should extend the existing Entire semantics.

---

# 6. Core Product Abstraction: Engineering Task

Do not treat a session as the task.

A single engineering task can contain multiple sessions and checkpoints.

Example:

```text
TASK: Subscription Pause
│
├── OpenClaw main session
│      └── Checkpoint A
│
├── OpenClaw coding sub-agent
│      └── Checkpoint B
│
├── Testing/review session
│      └── Checkpoint C
│
└── Hermes continuation session
       └── Checkpoint D
```

The logical identity is:

```text
task_id
```

Sessions and checkpoints are lineage nodes belonging to that task.

---

# 7. Task Identity

A task should have a stable identity, independent of a particular agent.

Suggested conceptual structure:

```json
{
  "task_id": "task_...",
  "repo": "owner/repo",
  "branch": "feature/subscription-pause",
  "root_session_id": "...",
  "title": "Implement subscription pause",
  "created_at": "..."
}
```

For the first implementation:

- repository
- branch
- root session
- explicit task creation/resume

are sufficient.

Do not build sophisticated automatic task clustering in the first version.

---

# 8. Task Identity Propagation

## OpenClaw

When the main task starts:

```text
OpenClaw main session
        ↓
resolve/create task
        ↓
TASK_ID
```

When OpenClaw launches a sub-agent:

```text
parent TASK_ID
+
child session ID
+
agent role
        ↓
child inherits TASK_ID
```

Result:

```text
TASK-123
│
├── OpenClaw session A
├── coding sub-agent B
└── test sub-agent C
```

## Hermes

Use Hermes plugin/hook surfaces.

When the session starts:

```text
Hermes session
      ↓
resolve Entire task
      ↓
TASK_ID
```

When a sub-agent is delegated:

```text
parent TASK_ID
+
child session
+
role
      ↓
child inherits TASK_ID
```

Do not create separate task-state systems for OpenClaw and Hermes.

Both should normalize into one internal model.

---

# 9. Normalized Event Model

Create an internal agent-event representation.

Conceptually:

```text
AgentEvent
{
  type,
  timestamp,
  task_id,
  session_id,
  parent_session_id,
  agent,
  role,
  payload_ref,
}
```

Candidate event types:

```text
SessionStarted
SessionEnded
SessionInterrupted
TurnStarted
TurnEnded
ToolUsed
SubagentStarted
SubagentEnded
CheckpointCreated
TaskResumed
HandoffCreated
```

The exact mapping must follow what the external agent actually exposes.

Never invent lifecycle semantics that do not exist.

---

# 10. Engineering State Schema

The state must be structured, not a free-form summary.

Suggested schema:

```json
{
  "schema_version": 1,

  "task": {
    "id": "task_...",
    "title": "...",
    "original_intent": "..."
  },

  "status": "partial",

  "requirements": [
    {
      "id": "R1",
      "description": "...",
      "status": "complete",
      "evidence": []
    }
  ],

  "completed_work": [],

  "in_progress": [],

  "failed_attempts": [],

  "decisions": [],

  "assumptions": [],

  "tests": [],

  "changed_files": [],

  "risks": [],

  "next_actions": [],

  "sessions": [],

  "checkpoints": [],

  "evidence": []
}
```

Possible requirement status:

```text
complete
partial
unresolved
blocked
unknown
```

Never use "complete" without sufficient evidence.

---

# 11. Evidence Must Be First-Class

Every important semantic claim should point to evidence.

Example:

```json
{
  "claim": "Webhook handling is incomplete",
  "status": "partial",
  "evidence": [
    {
      "type": "test",
      "name": "TestDuplicateWebhook",
      "result": "failed"
    },
    {
      "type": "file",
      "path": "billing/webhooks.go",
      "line_start": 84,
      "line_end": 110
    },
    {
      "type": "checkpoint",
      "id": "cp_..."
    }
  ]
}
```

Evidence can point to:

- checkpoint
- session
- commit
- file
- line range
- test
- Graph result
- runtime result

The user must be able to inspect the basis of important claims.

---

# 12. Fact vs Inference vs Unknown

State should distinguish:

```text
OBSERVED
Directly supported by repository/checkpoint/test/event evidence.

INFERRED
A conclusion derived from observed evidence.

UNKNOWN
Not established.

RECOMMENDED
A proposed next action.
```

Example:

```text
OBSERVED:
TestDuplicateWebhook failed.

INFERRED:
Webhook handling is incomplete.

UNKNOWN:
Whether this occurs in production.

RECOMMENDED:
Implement idempotency and rerun the affected test.
```

This is a core reliability principle.

---

# 13. Requirements Model

Extract requirements from:

1. original user prompt
2. explicit constraints
3. acceptance criteria
4. important constraints discovered during implementation

Example:

```text
R1 — Add pause API
R2 — Preserve current billing cycle
R3 — Prevent duplicate webhook processing
R4 — Add regression tests
```

Task state:

```text
R1 ✓
R2 ✓
R3 ⚠
R4 ✗
```

This is much stronger than:

> "The task is mostly complete."

---

# 14. Decision Model

A decision should contain:

```text
decision
reason
evidence
session
checkpoint
```

Example:

```json
{
  "decision": "Use explicit paused state instead of mutating renewal state",
  "reason": "Direct renewal mutation broke existing renewal behavior",
  "evidence": [
    "TestRenewalAfterPause",
    "checkpoint: cp_..."
  ]
}
```

---

# 15. Rejected Approach Model

Rejected approaches deserve their own record.

```json
{
  "approach": "Modify renewal status directly",
  "status": "rejected",
  "reason": "Breaks legacy renewal path",
  "evidence": [...]
}
```

The purpose is to prevent future agents from rediscovering the same failure.

---

# 16. Test State

Track at minimum:

```text
test name
command
result
timestamp
relevant files
checkpoint
```

Example:

```json
{
  "name": "TestDuplicateWebhook",
  "status": "failed",
  "command": "go test ./billing/...",
  "evidence": "..."
}
```

Do not make claims such as "all tests pass" unless actual results prove it.

---

# 17. Task State Machine

Conceptual model:

```text
NEW
 │
 ▼
ACTIVE
 │
 ├───────────────┐
 ▼               ▼
PARTIAL        BLOCKED
 │               │
 └──────┬────────┘
        ▼
     RESUMED
        │
        ▼
     VERIFIED
        │
        ▼
     COMPLETE
```

Session lifecycle:

```text
TASK
│
├── SESSION_STARTED
├── WORKING
├── CHECKPOINT
├── INTERRUPTED
├── HANDOFF
├── RESUME
├── CHECKPOINT
└── COMPLETE
```

---

# 18. Task Snapshot

The main user-facing object should answer:

- What is this task?
- Why are we doing it?
- What has happened?
- Where are we now?
- What remains?
- What should happen next?

Example:

```text
TASK
────────────────────────────
Implement subscription pause

INTENT
Pause future billing while preserving
the current billing cycle.

REQUIREMENTS
✓ Pause API
✓ Authorization
⚠ Webhook idempotency
✗ Regression tests

DECISIONS
• Explicit pause state chosen
• Direct renewal mutation rejected

FAILED APPROACHES
• Direct renewal status mutation

CURRENT STATE
2 files changed
1 integration test failing

NEXT ACTION
Fix webhook idempotency.

EVIDENCE
checkpoint cp_18291
commit abc123
TestDuplicateWebhook
```

---

# 19. Core User Workflow: Handoff

Primary CLI concept:

```bash
entire task handoff
```

Purpose:

> Convert the current engineering task state into a compact, evidence-backed package that another developer or agent can act on immediately.

Human output:

```text
SUBSCRIPTION PAUSE

ORIGINAL INTENT
Pause future billing without changing
the current billing cycle.

STATUS
3 / 5 requirements complete

COMPLETED
✓ database state
✓ pause API
✓ authorization

INCOMPLETE
⚠ webhook idempotency

FAILED
✗ duplicate webhook integration test

IMPORTANT DECISION
Do not use approach A.
It breaks legacy renewal behavior.

FILES
billing/subscription.go
billing/webhooks.go

NEXT ACTION
Fix webhook idempotency.

CHECKPOINT
cp_81F4...
```

Machine output:

```bash
entire task handoff --json
```

The Markdown and JSON outputs must come from the same Task State object.

---

# 20. Core User Workflow: Resume

Primary concept:

```bash
entire task resume <task-or-checkpoint>
```

Resume must:

1. resolve the task
2. find the latest relevant checkpoint
3. load its engineering state
4. compare historical state to current repository state
5. detect drift
6. prepare continuation context
7. inject state into the fresh agent
8. create lineage linking the new session to the task

The agent should not receive a giant transcript dump.

It should receive a compact, structured continuation context.

---

# 21. Continuation Context

Suggested structure:

```text
ENTIRE TASK CONTEXT

Task:
Subscription Pause

Original intent:
...

Verified complete:
...

Partial/unresolved:
...

Known failed approaches:
...

Important decisions:
...

Current repository state:
...

Relevant files:
...

Test state:
...

Graph verification:
...

Recommended next action:
...

Evidence:
...

Instructions:
This is historical engineering context.
Verify current repository state before editing.
Do not assume historical claims are still true.
```

---

# 22. Resume Safety Rule

Historical state is context, not authority.

Always instruct the receiving agent:

> Verify historical claims against the current repository before making changes.

This protects against stale checkpoints and repository drift.

---

# 23. State Drift Detection

Compare:

```text
checkpoint state
vs
current repository
```

Possible outcomes:

```text
STATE MATCHES
No meaningful change since checkpoint.

STATE DRIFTED
Code changed after checkpoint.

STALE DECISION
A historical assumption may no longer hold.

CONFLICT
Current code contradicts historical task state.
```

Example:

```text
Historical state:
webhooks.go incomplete

Current state:
webhooks.go has already changed

Result:
Repository drift detected.
Historical next action requires revalidation.
```

---

# 24. Human and Agent Handoff Use the Same State

Architecture:

```text
             TASK STATE
                 │
        ┌────────┴────────┐
        ▼                 ▼
     Markdown             JSON
        │                 │
      human             agent
```

Never implement separate logic for human and agent handoffs.

This guarantees consistency.

---

# 25. Handoff Receipt

Create a small receipt:

```json
{
  "task_id": "...",
  "from_session": "...",
  "to_session": "...",
  "source_checkpoint": "...",
  "state_hash": "...",
  "created_at": "...",
  "next_action": "..."
}
```

The user can see:

```text
HANDOFF CREATED

Task:
TASK-123

From:
OpenClaw / Codex

To:
Human / Hermes

Checkpoint:
CP-B

State:
Partial

Outstanding:
2 requirements

Critical failure:
1 integration test

Next action:
...
```

---

# 26. Cross-Agent Continuity

Desired lineage:

```text
                TASK-123
                    │
          ┌─────────┼──────────┐
          ▼         ▼          ▼
      OpenClaw    Hermes    Human
          │         │          │
        CP-001    CP-002     CP-003
```

The agent can change.

The task identity remains the same.

This is one of the primary differentiators of the project.

---

# 27. OpenClaw Integration

Implement an Entire external-agent adapter following the existing repository's adapter conventions.

The adapter should translate OpenClaw's supported lifecycle/events into Entire's external-agent protocol.

Capture, where exposed:

- session start/end
- agent start/end
- turn events
- sub-agent spawn/end
- tool events
- relevant session metadata
- interruption/reset information

Avoid modifying OpenClaw core if plugin/hook integration is sufficient.

The adapter must continue working in the intended OpenClaw operating mode.

---

# 28. Hermes Integration

Implement Hermes integration using its existing extension/plugin/hook mechanisms.

Candidate lifecycle points:

- session start
- session end
- pre-LLM context injection
- post-LLM events
- tool events
- subagent completion

Again, do not modify Hermes core unless absolutely required.

Normalize Hermes events into the same internal model as OpenClaw.

---

# 29. Common Integration Architecture

Bad:

```text
OpenClaw
  → OpenClaw-specific task-state engine

Hermes
  → Hermes-specific task-state engine
```

Good:

```text
OpenClaw ──┐
           ├── Normalized Agent Event Model
Hermes ────┘
                    │
                    ▼
              Task State Engine
                    │
                    ▼
                 Entire
```

This proves that the continuity abstraction is agent-independent.

---

# 30. LLM vs Deterministic Responsibilities

Do not let an LLM own the entire state.

## Deterministic

Derive directly from system data:

- task ID
- repository
- branch
- commit SHA
- changed files
- checkpoint ID
- session IDs
- tool lifecycle
- test command/result when structured
- timestamps
- agent identity
- session lineage

## LLM-assisted

Use a model for:

- intent normalization
- requirement extraction
- decision classification
- separating final approach from abandoned approaches
- identifying likely unfinished work
- recommending next action

Then attach evidence to each conclusion.

---

# 31. Evidence Priority

Use this hierarchy:

```text
Level 1 — Repository facts
Git / source / test results

Level 2 — Entire checkpoint facts
session/checkpoint metadata

Level 3 — Explicit agent statements
prompts/responses

Level 4 — Model inference
derived interpretation
```

If Level 1 contradicts Level 4:

> Level 1 wins.

---

# 32. State Merge Rules

When a new checkpoint arrives:

```text
previous Task State
+
new checkpoint evidence
        ↓
new Task State
```

Rules:

### Requirements
Do not mark a requirement complete without sufficient evidence.

### Failed tests
Remain failed until a later result proves resolution.

### Decisions
Retain historical decisions unless explicitly superseded.

### Rejected approaches
Retain them as historical knowledge.

### Unknown
Use unknown when evidence is insufficient.

The system must prefer uncertainty over fabrication.

---

# 33. Partial Evidence Handling

External agent integrations may not always provide every desired event or transcript.

The product must degrade gracefully.

Example:

```text
STATE PARTIALLY RECOVERED

Verified:
✓ files changed
✓ Git state
✓ test result

Unknown:
? original decision rationale

Action:
Inspect current repository and request
human confirmation when needed.
```

Do not make full transcript availability a hard dependency.

---

# 34. Security and Privacy

Do not create a new secret leak path.

Never persist:

- API keys
- tokens
- passwords
- private keys
- credentials

Do not copy entire tool payloads unnecessarily.

Prefer:

```text
Test X failed.
```

over dumping an entire failed environment or credential-bearing response.

Historical checkpoint data should pass through the existing Entire safety/redaction mechanisms where available.

---

# 35. Graph Integration

Graph is not the product.

Graph is the **verification layer around continuation**.

When task state says:

```text
NEXT ACTION:
modify WebhookProcessor
```

use Entire Graph to verify:

```text
definition
callers
relationships
affected components
candidate tests
```

Then produce an actionable result:

```text
NEXT ACTION VALIDATION

Target:
WebhookProcessor

Impact:
• BillingService
• RetryScheduler
• 7 callers

Relevant tests:
• TestRetry
• TestDuplicateWebhook
• TestBillingResume

Recommendation:
Proceed, but run the 3 affected tests after the change.
```

Do not display raw graph output as the product.

---

# 36. Three Required Graph Moments

The Buildathon requires evidence of:

1. graph search/definition lookup
2. relationship/impact analysis before a high-risk change
3. final semantic-diff analysis

These should be incorporated into the actual workflow, not performed just for compliance.

---

# 37. Graph + Task State Workflow

Use:

```text
checkpoint
   ↓
task state
   ↓
next action
   ↓
Graph target resolution
   ↓
impact analysis
   ↓
candidate tests
   ↓
agent verifies source
   ↓
agent edits
```

At the end:

```text
intent
+
final implementation
+
Graph semantic diff
+
tests
       ↓
final verification
```

---

# 38. Buildathon Workflow Integration

The uploaded Participant Guide is explicit:

- choose one Entire track
- fork only after the official start
- work inside the Entire mirror clone
- enable checkpoints
- create meaningful milestone checkpoints
- activate Entire Graph
- use a fresh agent session after Graph activation
- show Graph search, impact analysis and final semantic diff
- close the existing session for the Noon Curveball
- start a fresh session
- reconstruct the project from checkpoint context
- run Graph impact analysis before changing affected code
- implement and test the constraint
- create a new checkpoint
- finish `BUILDATHON.md`
- submit before the stated deadline

These requirements must shape the implementation and demo rather than be treated as separate administrative tasks.

---

# 39. Required Checkpoints

The participant guide requires checkpoint history containing at least:

```text
CP1 — Initial understanding and intended architecture

CP2 — Last stable state before the Noon Curveball

CP3 — Response to the Noon Curveball

CP4 — Final implementation and verification
```

Checkpoint quality matters more than quantity.

Each should preserve:

- intent
- decisions
- rejected options
- failures
- assumptions
- open risks
- evidence that changed the approach

---

# 40. Pre-Noon Stable Checkpoint

Before the Curveball, ensure the checkpoint contains:

```text
Task intent
Architecture
Requirements
Completed work
Unresolved work
Known risks
Important decisions
Failed approaches
Relevant files
Test state
Current Git state
Useful Graph evidence
```

This checkpoint is a central part of the product demonstration.

---

# 41. Noon Curveball Handling

The actual constraint is intentionally unknown.

Therefore, never hard-code one response.

Represent the new requirement as a separate event:

```json
{
  "type": "constraint_added",
  "text": "...",
  "source": "buildathon_curveball",
  "timestamp": "..."
}
```

The new state should preserve:

```text
ORIGINAL INTENT
+
NEW CONSTRAINT
+
AFFECTED REQUIREMENTS
+
CHANGED ASSUMPTIONS
+
GRAPH IMPACT
+
NEW IMPLEMENTATION PLAN
+
VERIFICATION
```

Do not overwrite original intent.

---

# 42. Curveball Workflow

Exactly follow:

```text
pre-noon stable checkpoint
        ↓
STOP IMPLEMENTATION
        ↓
CLOSE OLD AGENT SESSION
        ↓
START FRESH AGENT SESSION
        ↓
LOAD ENTIRE CHECKPOINT
        ↓
RECONSTRUCT TASK
        ↓
RUN GRAPH IMPACT ANALYSIS
        ↓
IMPLEMENT SMALLEST COMPLETE RESPONSE
        ↓
TEST
        ↓
CREATE POST-CURVEBALL CHECKPOINT
```

This is the strongest proof that the system is actually useful.

---

# 43. Fresh Session Acceptance Test

Given an interrupted task:

```text
old session
+
partial implementation
+
failed approach
+
Entire checkpoint
```

Start a completely new session.

Before editing, the agent must be able to recover:

- original intent
- task status
- completed requirements
- unresolved requirements
- known failed approaches
- important decisions
- current files
- test state
- next action

The agent must not repeat a previously rejected approach merely because the new session has no chat history.

---

# 44. Cross-Agent Acceptance Test

Test:

```text
OpenClaw
   ↓
Entire checkpoint
   ↓
Hermes
```

Expected:

```text
same task_id
same original intent
same requirement state
same decisions
same failed approaches
same Git state
new session identity
```

Also test the reverse if feasible:

```text
Hermes
   ↓
Entire checkpoint
   ↓
OpenClaw
```

---

# 45. Human Handoff Acceptance Test

A human should run:

```bash
entire task handoff
```

and understand the task without reading the full transcript.

The handoff should explain:

- what the task is
- what is done
- what is unfinished
- what failed
- why
- what was decided
- what should happen next
- where the evidence is

---

# 46. Repository Drift Acceptance Test

After creating a checkpoint:

```text
human changes a relevant file
```

Then:

```bash
entire task resume
```

Expected:

```text
Repository drift detected.

Historical state:
...

Current state:
...

Historical next action:
...

Revalidation required.
```

The new agent must not blindly follow stale state.

---

# 47. Missing Context Acceptance Test

Simulate incomplete external-agent capture.

Expected:

```text
STATE PARTIALLY RECOVERED

Verified:
...

Unavailable:
...

Unknown:
...

Safe next action:
inspect current repository / ask for confirmation
```

No invented details.

---

# 48. Failure Handling

## Entire unavailable

The coding agent should continue normally.

Do not make the external integration a fatal dependency.

## Checkpoint unavailable

Do not claim safe resume.

Tell the agent to inspect current state.

## State extraction fails

Fall back to available deterministic checkpoint/session/Git information.

## Graph unavailable

Explicitly mark Graph verification unavailable.

Do not pretend it occurred.

## Agent event missing

Treat the timeline as incomplete and continue with available evidence.

---

# 49. Agent Context Size

Do not inject the entire transcript into a resumed agent.

Order information by importance:

```text
1. original intent
2. unresolved requirements
3. failed approaches
4. important decisions
5. next action
6. relevant files/tests
7. supporting evidence
8. raw transcript excerpts only when necessary
```

The purpose of the system is to reduce reconstruction cost.

---

# 50. Optional “Task Explain” Capability

Possible future command:

```bash
entire task explain <task-id>
```

Example:

```text
Why is this task blocked?

Blocked because:
TestDuplicateWebhook failed.

Likely reason:
Webhook event can be processed twice.

Evidence:
...

Recommended next action:
...
```

This is lower priority than handoff/resume.

---

# 51. Optional “Task Lineage” Capability

Possible command:

```bash
entire task lineage <task-id>
```

Example:

```text
TASK-123
│
├── OpenClaw
│   └── CP-A
│
├── OpenClaw / Codex
│   └── CP-B
│
├── Hermes
│   └── CP-C
│
└── Human
    └── CP-D
```

This would be highly useful in the final demo.

---

# 52. Demo Application / Scenario

Use a small realistic software domain that naturally generates:

- multiple requirements
- one edge case
- at least one failed approach
- a meaningful test suite
- a non-trivial Graph dependency

Good example:

**Subscription / billing service**

Task:

> “Implement subscription pause without changing the current billing cycle.”

Example requirements:

```text
R1 — Pause API
R2 — Authorization
R3 — Preserve current billing cycle
R4 — Prevent duplicate webhook processing
R5 — Add regression tests
```

The task should be intentionally stopped before completion.

---

# 53. Demo Setup

Preconfigure:

```text
OpenClaw
Entire
repository
Graph
a real or prepared agent integration
```

Have the product already runnable.

Do not waste the demo explaining installation.

---

# 54. Demo Scene 1 — Agent Starts

Prompt OpenClaw:

> Implement subscription pause without changing current billing-cycle behavior.

Show:

```text
OpenClaw
 ↓
coding worker
 ↓
code changes
 ↓
tests
 ↓
failed webhook test
 ↓
alternative approach
```

---

# 55. Demo Scene 2 — Interrupt the Agent

Intentionally stop the task.

Say:

> “The agent has completed part of the task, but the session is now gone.”

This creates the problem.

---

# 56. Demo Scene 3 — Fresh Agent Without Continuity

Start a clean agent session.

Tell it only:

> Continue implementing subscription pause.

Show:

- re-investigation
- uncertainty
- possible repeated failed approach
- inability to explain prior decision

This should be real if possible.

---

# 57. Demo Scene 4 — Entire Handoff

Run:

```bash
entire task handoff
```

Show:

```text
Original intent
Requirements
Completed
Incomplete
Failures
Rejected approaches
Important decisions
Tests
Next action
Evidence
```

---

# 58. Demo Scene 5 — Fresh Agent Resume

Start another fresh agent.

Load the same task through Entire.

The agent should say, in its own words, something equivalent to:

> The previous worker already investigated the webhook implementation, rejected the direct status mutation because it breaks renewal behavior, and the remaining issue is duplicate event handling. I will continue from that point.

Then it edits.

---

# 59. Demo Scene 6 — Graph Verification

Before editing:

```text
Graph impact
```

Show:

```text
WebhookProcessor
 ↓
BillingService
 ↓
RetryScheduler
 ↓
multiple callers
```

Then run the relevant tests.

---

# 60. Demo Scene 7 — New Checkpoint

Create a new checkpoint.

Show lineage:

```text
TASK
│
├── OpenClaw / Codex
│   └── CP-A
│
└── Hermes / new worker
    └── CP-B
```

The task survived even though the worker changed.

---

# 61. Curveball Demo

The actual Curveball will be unknown.

The prepared workflow should be:

```text
CP before curveball
        ↓
new constraint
        ↓
fresh agent
        ↓
recover state
        ↓
Graph impact
        ↓
adapt
        ↓
test
        ↓
new checkpoint
```

The system should preserve:

```text
Original intent
New constraint
What changed
Why
Evidence
Tests
```

---

# 62. Final Semantic Verification

At completion:

```text
Original intent
+
requirements
+
final code
+
Graph semantic diff
+
tests
+
final checkpoint
```

Result:

```text
FINAL TASK VERIFICATION

Requirements:
✓ R1
✓ R2
✓ R3
✓ R4
✓ R5

Graph:
Relevant impact verified.

Tests:
Relevant tests pass.

Semantic diff:
Consistent with intended change.

Checkpoint:
CP-final
```

---

# 63. Test Architecture

Use four test layers.

## Unit tests

Test:

- state merging
- requirement classification
- evidence linking
- decision preservation
- failed-approach preservation
- drift detection
- state serialization
- validation rules

## Adapter tests

Test:

```text
OpenClaw event → normalized event
Hermes event → normalized event
```

## Checkpoint integration tests

Test:

```text
session
→ checkpoint
→ task state
```

## End-to-end tests

Test:

```text
real/fixture agent
→ Entire
→ checkpoint
→ task state
→ fresh agent
→ continuation
→ new checkpoint
```

---

# 64. Fixtures

Build deterministic fixtures for:

```text
OpenClaw main session
OpenClaw sub-agent
Hermes main session
Hermes child agent
interrupted session
failed test
multiple checkpoints
repository drift
missing transcript
```

Fixtures should cover the state engine without depending on live agent APIs.

---

# 65. Live Smoke Test

Have at least one actual live workflow:

```text
OpenClaw
→ Entire
→ real checkpoint
→ fresh session
→ resume
→ code change
→ test
→ second checkpoint
```

Fixtures prove correctness.

The live run proves integration.

---

# 66. Implementation Sequence

The sequence below is dependency-aware. Do not skip ahead to UI polish before the core end-to-end flow works.

## Step 1 — Repository Reconnaissance

Have the coding agent inspect:

```text
Existing Entire repository
Existing external-agent adapters
Agent protocol
Lifecycle event definitions
Session persistence
Checkpoint creation
Checkpoint storage
Transcript representation
CLI architecture
Graph integration
Current tests
```

Output a technical map before making changes.

---

## Step 2 — Confirm the Correct Buildathon Fork/Repository

The participant guide requires implementation in the designated Entire fork and requires work through the Entire mirror workflow.

Before implementation:

- fork after the official start
- run the required mirror creation
- clone through Entire
- work only inside the designated clone

Do not assume the repository name/path; inspect the supplied environment and official instructions.

---

## Step 3 — Understand Existing Adapter Conventions

Find an existing external-agent adapter that is structurally closest.

Document:

```text
adapter entrypoint
installation
hook registration
event mapping
session handling
checkpoint interaction
tests
```

Copy the architecture, not just code snippets.

---

## Step 4 — Implement OpenClaw Adapter

First target:

```text
OpenClaw
→ normalized events
→ Entire
```

Make the basic lifecycle integration reliable before building task semantics.

---

## Step 5 — Validate Existing Entire Checkpoint Creation

Prove:

```text
OpenClaw session
→ Entire session
→ Git state
→ checkpoint
```

Do not move on until this works end to end.

---

## Step 6 — Create Normalized Event Model

Implement:

```text
AgentEvent
Task identity
Session identity
Parent/child relationship
```

Make OpenClaw produce the normalized representation.

---

## Step 7 — Task Identity

Create:

```text
task_id
```

and associate:

```text
root session
subagent sessions
checkpoints
```

with the task.

---

## Step 8 — Task Lineage

Implement:

```text
Task
 ├── session
 ├── subagent
 ├── checkpoint
 └── handoff/resume
```

Do not add complex UI yet.

---

## Step 9 — Deterministic Task State

Implement state fields that can be derived without a model:

```text
repo
branch
commit
files
sessions
checkpoints
tests where structured
```

---

## Step 10 — Semantic Task State

Add model-assisted extraction for:

```text
intent
requirements
decisions
failed approaches
unresolved work
next action
```

All semantic fields must attach evidence.

---

## Step 11 — State Validation

Build validation rules:

```text
No requirement complete without evidence
No test pass without result
No decision without source/context
No unsupported claim treated as fact
Unknown allowed
```

---

## Step 12 — State Merge Engine

Merge checkpoint states incrementally.

Ensure historical decisions and failures survive later updates unless explicitly superseded.

---

## Step 13 — `entire task status`

Build the first human-readable view.

The output should be compact and actionable.

---

## Step 14 — `entire task handoff`

Implement:

```bash
entire task handoff
entire task handoff --json
```

Human and agent outputs must share the same internal state.

---

## Step 15 — Context Builder

Implement:

```text
BuildContinuationContext(taskID)
```

Pipeline:

```text
resolve task
→ latest checkpoint
→ task state
→ current repo inspection
→ drift detection
→ Graph verification when required
→ continuation context
```

---

## Step 16 — OpenClaw Resume

Use OpenClaw's supported context injection path.

The agent receives the compact continuation state before it begins its continuation work.

---

## Step 17 — Hermes Integration

Implement Hermes integration against its supported hook/plugin architecture.

Map Hermes events into the same normalized event model.

---

## Step 18 — Cross-Agent Resume

Prove:

```text
OpenClaw
→ Entire
→ Hermes
```

using a single task ID.

Then, where practical:

```text
Hermes
→ Entire
→ OpenClaw
```

---

## Step 19 — State Drift Detection

Implement current-repository verification at resume.

Do not blindly restore stale state.

---

## Step 20 — Graph Verification

Integrate Graph into:

```text
resume next action
→ target lookup
→ impact analysis
→ relevant tests
```

Record the decision/evidence rather than raw Graph output.

---

## Step 21 — Final Semantic Verification

Combine:

```text
task intent
requirements
final code
Graph semantic diff
tests
```

into final verification.

---

## Step 22 — Curveball Workflow

Test:

```text
checkpoint
→ session closed
→ fresh session
→ state reconstruction
→ Graph impact
→ changed requirement
→ implementation
→ tests
→ new checkpoint
```

This is mandatory for the Buildathon.

---

## Step 23 — Demo Hardening

Make the primary demo deterministic.

Eliminate unnecessary live-service dependencies.

Prepare fallback evidence for fragile live steps.

---

## Step 24 — BUILDATHON.md

Maintain:

```text
# Project name
## One-sentence summary
## Problem, intended user and why it matters
## Selected Entire track and why Entire is essential
## Architecture and main workflow
## Entire Graph findings and verification
## Noon Curveball: what changed and how we adapted
## Checkpoint links and what each checkpoint proves
## Setup, run and test instructions
## Known limitations and next steps
```

---

# 67. Code Quality Rules for the Coding Agent

Instruct Claude Code/Codex:

## Rule 1

Reuse existing Entire abstractions wherever possible.

## Rule 2

Do not redesign checkpoint architecture unless necessary.

## Rule 3

Do not duplicate session persistence.

## Rule 4

Do not modify OpenClaw/Hermes core if hooks/plugins can do the job.

## Rule 5

Do not create an unrelated dashboard.

## Rule 6

Do not make an LLM-generated summary the canonical source of truth.

## Rule 7

Do not fabricate missing evidence.

## Rule 8

Do not block normal agent execution if continuity infrastructure fails.

## Rule 9

Do not expose raw Graph output as the product.

## Rule 10

Keep human and machine handoff derived from one state object.

## Rule 11

Every semantic claim should be traceable to evidence whenever possible.

## Rule 12

Write tests before or alongside each major subsystem.

---

# 68. Definition of Done

The project is complete when all of the following work.

## T3 integration

OpenClaw works through Entire's intended external-agent integration mechanism.

Hermes works through its supported integration mechanism, preferably as a second adapter.

## Task continuity

A software task has a stable identity independent of the worker.

## Checkpoint continuity

A checkpoint can be converted into usable engineering state.

## Cross-agent continuation

A fresh agent can continue from another agent's task state.

## Human handoff

A developer can understand the task without reading the entire transcript.

## Evidence

Important claims are evidence-backed.

## Drift

Stale state is detected.

## Graph

Graph evidence is used to verify the next change and final semantics.

## Curveball

A fresh session can reconstruct the project from checkpoint context and adapt to a new constraint.

## Tests

Unit, adapter, integration and end-to-end tests exist.

## Submission

Required checkpoints, Graph evidence, BUILDATHON.md, setup/test instructions and final commit are ready.

---

# 69. Hackathon Judging Strategy

The official Buildathon rubric places 20 points on problem/innovation, 25 on technical implementation, 15 on Curveball response, 15 on checkpoint use, 15 on Graph use, and 10 on demonstration/future potential.

Design the product so the demo naturally demonstrates those criteria.

## Problem / innovation

Show the exact continuity failure.

## Technical implementation

Show OpenClaw → Entire → checkpoint → fresh agent → continuation.

## Checkpoints

Show meaningful task state rather than a decorative checkpoint.

## Graph

Show a real impact decision changing or verifying the continuation.

## Curveball

Show fresh reconstruction and adaptation.

## Demonstration

Use one clear software task and one clean before/after comparison.

---

# 70. The Strongest Demo Narrative

Start with the problem, not architecture.

Say:

> “This agent has been working on this software task for 20 minutes. We're going to stop it.”

Stop the session.

Then:

> “Now another agent has to continue the task.”

Show the new session struggling/reinvestigating.

Then:

> “Now let's recover the engineering state from Entire.”

Run:

```bash
entire task handoff
```

Show:

```text
intent
requirements
decisions
failed approaches
tests
next action
evidence
```

Then start a fresh OpenClaw/Hermes session.

Resume.

Before the change, run Graph impact analysis.

Continue.

Run tests.

Create the new checkpoint.

Finish with:

> **The agent changed. The task didn't.**

---

# 71. Strong Demo Scenario

## Task

Implement subscription pause while preserving the current billing cycle.

## Initial state

```text
R1 ✓
R2 ✓
R3 ⚠
R4 ✗
```

## Historical decision

Directly mutating renewal status breaks existing behavior.

## Failure

Duplicate webhook test fails.

## Checkpoint

Task state persisted.

## Session ends

No transcript handoff manually copied.

## New session

Fresh Hermes/OpenClaw.

## Entire continuity

Restores:

```text
intent
requirements
decision
failure
next action
```

## Graph

Identifies affected billing paths and tests.

## Agent

Fixes the correct subsystem rather than repeating investigation.

## Test

Relevant tests pass.

## New checkpoint

Task state is updated.

---

# 72. Why This Is Better Than a Generic Agent-Memory Project

A generic agent-memory product answers:

> “What did the model remember?”

Entire Continuity answers:

> **“What is the current engineering state of this software task, what evidence supports it, and what should the next worker do?”**

That difference must remain central throughout implementation.

---

# 73. Long-Term Architecture

If the MVP works, the architecture naturally grows into:

```text
                         SOFTWARE TASK
                               │
                 ┌─────────────┴─────────────┐
                 ▼                           ▼
             Human                      Agent system
                                             │
                              ┌──────────────┼──────────────┐
                              ▼              ▼              ▼
                          OpenClaw         Hermes       Other agents
                              │              │              │
                              └──────────────┼──────────────┘
                                             ▼
                                          ENTIRE
                                             │
                            ┌────────────────┼────────────────┐
                            ▼                ▼                ▼
                         Checkpoints       Graph         Git/code
                            │                │                │
                            └────────────────┼────────────────┘
                                             ▼
                                    ENGINEERING STATE
                                             │
                                             ▼
                                  next worker / action
```

Potential future extensions:

- multi-repository tasks
- richer approval workflows
- risk-aware agent autonomy
- task-level analytics
- CI integration
- release readiness
- organizational engineering memory
- accessibility-first agent workflow interfaces

These are future directions, not MVP scope.

---

# 74. Product Boundaries to Protect

Do not allow scope creep toward:

```text
general-purpose agent orchestration
general agent memory
generic code review
generic observability
generic project management
large web dashboard
multi-repo enterprise platform
```

The first product must remain:

> **Evidence-backed continuation of software tasks across agent/session boundaries.**

---

# 75. Final Product Model

```text
                        SOFTWARE TASK
                              │
                              ▼
                   OpenClaw / Hermes
                              │
                      agent execution
                              │
            ┌─────────────────┼─────────────────┐
            ▼                 ▼                 ▼
          tools              code              tests
            │                 │                 │
            └─────────────────┼─────────────────┘
                              ▼
                         Entire
                       checkpoint
                              │
                              ▼
                     ENGINEERING STATE
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
        intent             progress            evidence
        decisions          failures             Git
        requirements       tests                 Graph
                              │
                              ▼
                       TASK LINEAGE
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
        HUMAN             AGENT A              AGENT B
          │                   │                   │
          └───────────────────┼───────────────────┘
                              ▼
                           CONTINUE
```

---

# 76. Final One-Sentence Pitch

> **Entire Continuity makes autonomous software work resumable: it turns fragmented OpenClaw/Hermes agent sessions into a Git-linked engineering task history that another human or agent can verify and continue.**

## Core phrase

> **The agent changes. The task doesn't.**
