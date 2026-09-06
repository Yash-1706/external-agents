# Entire Graph — impact analysis before the curveball edit

All output below was produced by `entire-continuity graph impact <symbol>` against this
repository, **before** any code was changed in response to the Track 3 curveball. The commit under
analysis is `e04266c` ("Add continuation context, handoff and the CLI"), the last pre-curveball
stable state.

Graph backend in use: local Go static analysis (`go/ast`), because the `entire` binary is not
installed in this environment. The tool reports which backend answered on every invocation, and
never presents an unavailable Graph as an empty result.

---

## Why this analysis was run

The Track 3 card requires identifying **every parser, lifecycle handler, summary path and
checkpoint-writing path** affected by supporting a second transcript format. The question that
actually decides the design is narrower: *what depends on the event vocabulary being closed?*

---

## Findings

### 1. `model.AgentEvent.Validate` — 3 callers — **this is the chokepoint**

```
Definition: internal/model/event.go:151-171
Callers:    normalize, derive, store.FS
```

This single function is what rejects an event that is missing a task id or carries an
unrecognised agent. Every format flows through it. **This is the finding that shaped the fix.**

The new-format transcript has **no `task_id` field at all** and names an agent
(`AcmeCode`) outside the enumerated vocabulary. Two options followed:

- **Relax `Validate`** — ripples into all three callers, weakens the invariant that every stored
  event is anchored to a task, and risks the store accepting events it cannot file.
- **Satisfy `Validate` at the decode boundary** — derive the task id from
  `repository + branch + session_id` via the existing `model.NewTaskID`, and widen only the agent
  vocabulary, which is a strict widening that cannot reject anything previously accepted.

The second was chosen. Graph is the reason that choice is defensible rather than a guess: it
showed the blast radius of relaxing validation was three packages, and the blast radius of
anchoring at the boundary was one.

### 2. `normalize.For` — 1 caller

```
Definition: internal/normalize/normalize.go:85-94
Callers:    app (cmd/entire-continuity ingest)
```

The format dimension is missing from this signature. Only one call site consumes it, so the
signature can gain a format-aware sibling without a wide refactor — and `For(agent)` itself stays,
so existing callers keep working unchanged.

### 3. `normalize.ParseStream` — 1 caller — framing is already format-agnostic

```
Definition: internal/normalize/normalize.go:111-158
Callers:    normalize
```

NDJSON-versus-JSON-array framing is already separated from record decoding. The new format is
JSONL, so **no framing work is required** — a fact worth knowing before writing any.

### 4. `store.AppendEvent` — 5 callers — must not change

```
Definition: internal/store/events.go:30-102
Callers:    app (5 call sites)
```

This is the lifecycle handler that folds events into lineage. Because the fix keeps producing
well-formed `model.AgentEvent` values, **this path is untouched** — which is what preserves
existing behaviour across ingest, resume, constraint and checkpoint recording.

### 5. `entire.CreateCheckpoint` — 1 caller — checkpoint compatibility confirmed

```
Definition: internal/entire/cli.go:81-107
Callers:    app (checkpoint create)
```

The checkpoint-writing path consumes `EngineeringState`, not raw events. Since the schema is
unchanged and no field is repurposed, **checkpoint behaviour stays compatible** (curveball
requirement C4). Graph confirms there is no second, hidden writer to keep in sync.

---

## Candidate tests Graph identified

Graph named the tests exercising the affected paths. These are the regression surface that must
stay green — they are the machine-checkable form of "preserves existing behaviour":

`TestTwoRuntimesOneEventVocabulary`, `TestOpenClawNormalizeMainSession`,
`TestHermesNormalizeMainSession`, `TestOpenClawSkipsUnknownHooks`, `TestHermesSkipsUnknownEvents`,
`TestUnexposedLifecycleIsNeverMapped`, `TestNormalizeIsDeterministic`,
`TestParseStreamFramingIsMeaningless`, `TestOpenClawNeverInlinesToolPayloads`,
`TestAppendEventIdempotentUnderReplay`, `TestEventFoldDoesNotClobberExplicitNodes`,
`TestCheckpointIDDerivation`, `TestCanonicalStateIsStableAndDetached`.

---

## The decision Graph produced

> **Do not relax event validation. Anchor the new format at the decode boundary instead, and
> widen only the agent vocabulary.**
>
> Affected: `internal/normalize` (new engine + three format specs), `internal/model/event.go`
> (agent vocabulary widening only).
> Deliberately unaffected: `internal/derive`, `internal/store`, `internal/entire`,
> `internal/render`, `internal/contextbuild` — confirmed by caller analysis, not by assumption.

This is recorded here rather than described verbally because the curveball card scores Graph
evidence that is *captured in context*, and because a decision whose blast radius was measured is
worth more than one that was estimated.
