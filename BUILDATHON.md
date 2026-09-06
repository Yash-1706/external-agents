# Entire Continuity

## One-sentence summary

Entire Continuity makes autonomous software work resumable: it turns fragmented agent sessions
into a Git-linked, evidence-backed engineering task state that another agent or a human can
verify and continue.

**The agent changes. The task doesn't.**

---

## Problem, intended user and why it matters

When a software task moves between agents, sub-agents, scheduled sessions or humans, the next
worker has to reconstruct the engineering state from fragments. Git shows what the code *is*. It
does not show what was intended, what was already tried and abandoned, which requirement is still
open, which test is failing, or why the obvious approach was rejected.

So the next worker re-investigates, re-derives, and — the expensive failure — **repeats an
approach the previous worker already proved doesn't work**, because a fresh session has no memory
of the failure and the repository does not record it.

The intended user is a developer running agents that work asynchronously and in parallel: an
OpenClaw main session delegating to coding and test sub-agents, a Hermes continuation, a human
picking it up on Monday. The unit that has to survive all of them is not the session. It is the
**task**.

What this deliberately is *not*: a generic agent-memory product. A memory product answers "what
did the model remember?" This answers **"what is the current engineering state of this task, what
evidence supports it, and what should the next worker do?"**

---

## Selected Entire track and why Entire is essential

**Track T3 — Bring Entire to a New Agent or Workflow.**

Entire is the authoritative development-history layer, and this project deliberately does not
compete with it. The architecture is:

```
agent execution → Entire session/checkpoint → task-state interpretation → human/agent continuity
```

and explicitly *not*:

```
agent execution → a new independent database → Entire becomes optional
```

Checkpoints are the substrate. A checkpoint already ties development context to Git history; this
project reads that and turns it into a structured engineering state with evidence attached, then
hands it to the next worker. Without a history layer there is nothing to continue *from* — the
system would be reduced to summarising a transcript, which is exactly the thing the plan rules
out.

---

## Architecture and main workflow

```
   OpenClaw ──┐
   Hermes ────┼──▶  Normalized Agent Event Model  ──▶  Task State Engine  ──▶  Entire
   AcmeCode ──┘         (one vocabulary)              (evidence + merge)      (checkpoints)
                                                              │
                                                              ▼
                                          ┌───────────────────┴──────────────────┐
                                          ▼                                      ▼
                                    Markdown / text                            JSON
                                        human                                  agent
```

Both outputs render from **one** `EngineeringState` object. Human and machine handoffs are never
implemented separately — that is what keeps them consistent.

Two rules govern every layer, and they are correctness requirements rather than style:

1. **Evidence.** Every semantic claim points at a concrete artifact — a commit, a file and line
   range, a test result, a checkpoint. `model.ConfidenceFor` is the single chokepoint: a claim
   with no evidence can never be better than `UNKNOWN`, and one backed only by inference can never
   be `OBSERVED`.
2. **Honesty about gaps.** Every state carries a `Capture` block recording which inputs were
   actually available. When anything is missing the output prints a `STATE PARTIALLY RECOVERED`
   block naming exactly what is unverified. `validate.Enforce` is monotonically downgrading by
   construction — it can weaken a claim, never strengthen one.

### Packages

| Package | Responsibility |
|---|---|
| `internal/model` | Frozen domain contract: events, task identity, engineering state, evidence, ports |
| `internal/normalize` | Three transcript formats → one event vocabulary, with per-record format detection |
| `internal/redact` | Credentials never reach a checkpoint |
| `internal/gitrepo` | Level 1 repository facts |
| `internal/derive` | Deterministic state — what the system can prove without a model |
| `internal/semantic` | Model-assisted extraction (rule-based here), always evidence-attached |
| `internal/merge` | The §32 merge rules: failures survive, decisions survive, completion needs evidence |
| `internal/validate` | Validation with safe downgrade |
| `internal/drift` | Historical state vs the live tree |
| `internal/graph` | Graph verification — real Go static analysis when Entire Graph is absent |
| `internal/entire` | Checkpoint port, real CLI or local fallback, always says which |
| `internal/contextbuild` | The resume pipeline |
| `internal/handoff` | Human, Markdown and JSON handoff from one state |
| `internal/render` | Every human-facing view |
| `internal/e2e` | Acceptance tests against a real git repository |
| `examples/billing` | Demo domain (separate module) with one deliberately failing test |

### Test layers (plan §63)

| Layer | Where |
|---|---|
| Unit | every `internal/*` package — merge rules, evidence linking, drift, validation, redaction |
| Adapter | `internal/normalize` — fixtures per format, plus the real Node shims round-tripped |
| Integration | `internal/entire`, `internal/store` — session → checkpoint → task state |
| End-to-end | `internal/e2e` — the four acceptance tests below, against a throwaway git repo |

The acceptance tests are the ones that assert the product's actual claims:

- **`TestFreshSessionRecoversTheTask`** (plan §43) — a session with no chat history recovers the
  intent, the failing test, the requirements and, critically, **the approach already rejected**,
  so it does not re-derive the broken one.
- **`TestRepositoryDriftIsDetected`** (plan §46) — a human commits a change to a file the
  checkpoint depends on; the next resume demands revalidation instead of replaying stale claims.
- **`TestCrossAgentContinuity`** (plan §44) — one task id, two runtimes, identical intent,
  rejected approaches and test state on both sides of the boundary.
- **`TestNewFormatFlowsThroughTheWholePipeline`** — the curveball fixture end to end.

### Main workflow

```bash
# a task exists independently of whoever is working on it
entire-continuity task init --title "Implement subscription pause" --intent "..."

# agent activity flows in from any supported adapter format
entire-continuity ingest --file transcript.jsonl

# milestone
entire-continuity checkpoint create --label "CP2 pre-curveball stable"

# ... session ends, agent is gone ...

# a fresh worker recovers the task
entire-continuity task handoff          # human
entire-continuity task resume --json    # agent
```

`task resume` runs: resolve task → latest checkpoint → historical state → derive current
repository truth → merge → validate → **drift detection** → **Graph verification of the next
action** → compact continuation context. The receiving agent gets an ordered brief, never a
transcript dump, and it is always told: *this is historical context, not authority — verify before
you rely on it.*

---

## Entire Graph findings and verification

Full analysis with raw output: **[docs/GRAPH.md](docs/GRAPH.md)**.

Graph is used in three places in the actual workflow, not as a compliance step:

1. **Definition lookup** — `graph search` / `graph impact` resolve a next-action target to a real
   definition with a file and line range.
2. **Impact analysis before a high-risk change** — run at *resume* time, before the agent edits,
   and again by us before the curveball edit. It reports callers, dependent components and the
   tests that exercise them, and produces a *decision*, not a dump.
3. **Semantic diff** — exported declaration sets compared across two revisions.

The decisive finding: **`model.AgentEvent.Validate` has three callers** (`normalize`, `derive`,
`store.FS`). That single fact chose the design. The new transcript format has no task id and names
an unknown runtime, so the options were to relax validation — rippling across three packages and
weakening the invariant that every stored event is anchored — or to satisfy validation at the
decode boundary, touching one. We took the second. Graph is why that is a measured decision rather
than a guess.

Graph also confirmed `store.AppendEvent` (5 callers) and `entire.CreateCheckpoint` (1 caller) sit
*outside* the blast radius, which is what let us assert that checkpoint behaviour stays compatible
instead of hoping so.

---

## Noon Curveball: what changed and how we adapted

Full write-up: **[docs/CURVEBALL.md](docs/CURVEBALL.md)**.

**Track 3 card: the agent changed its format.** Existing users still produce the original format.

### The assumption that was invalidated

> *"Each host agent emits exactly one, stable, fully-enumerable lifecycle format. A normalizer is
> therefore a total function from raw envelope to known event, and anything it does not recognise
> is noise that can be discarded."*

It was wrong three ways at once, and the real fixture broke it harder than the card implied:

| Assumption | How it broke |
|---|---|
| One format per agent | `normalize.For(agent)` had no format dimension at all — two formats are live simultaneously |
| Unknown records are noise | Silently skipping them makes a short timeline render as a complete one |
| A transcript is complete or invalid | One truncated line discarded an entire recoverable session |
| *(unpredicted)* The world is enumerable | The fixture carries **no task id** and names a runtime, **AcmeCode**, outside the agent vocabulary — so every one of its events failed validation |

### How the design changed

- **Format became a dimension**, detected *per record* — so a stream mixing old and new records,
  which is what a partially-migrated fleet actually produces, decodes correctly.
- **One engine, three mapping tables.** The v1 mappers are reused verbatim; framing, redaction,
  validation and lineage folding are shared. The second format added a table, not a pipeline.
- **Unknown events are retained**, not dropped — as `model.EventUnknown` carrying the host's own
  name in `raw_kind` — and counted in a `Report` that flows into `Capture`.
- **Anchoring moved to the decode boundary.** No task id? Derive one from repository + branch +
  session via the existing `model.NewTaskID`, which also makes ingestion idempotent.
- **The agent vocabulary was widened**, a strict widening that cannot reject anything it accepted
  before. Two packages had already built private workarounds for the closed enum; both became
  redundant, which is the tell that the defect was in the model.

### Why the new result is safe

1. **The original format is pinned by comparison, not by hope.** `TestOriginalFormatsStillDecode`
   runs the v1 fixtures through *both* the old path and the new one and asserts the events are
   byte-identical.
2. **The checkpoint path is untouched** — confirmed by Graph caller analysis, not assumed.
   `schema_version` is unchanged and no field was repurposed.
3. **New information can only make output more cautious.** Unknown and malformed counts only widen
   `Capture.Missing`, and `validate.Enforce` cannot strengthen a claim.
4. **The failure mode moved in the safe direction**: from *confidently wrong* (a session silently
   discarded, rendering clean) to *explicitly incomplete*.

---

## Checkpoint links and what each checkpoint proves

| Checkpoint | Commit | What it proves |
|---|---|---|
| **CP1** — Initial understanding and intended architecture | `ebc1da4` | The domain contract frozen before any implementation: evidence model, normalized events, task identity, port interfaces |
| **CP2** — Last stable state before the Noon Curveball | `e04266c` | A complete, working v1: engine, CLI, resume and handoff. The state the curveball revision had to preserve |
| **CP2.5** — Graph impact analysis, before editing | `2a6250b` | Graph run *before* the change, with the finding that chose the design |
| **CP3** — Response to the Noon Curveball | `3f2ffa6` | Both formats supported, unknown events retained, partial transcripts recovered, existing behaviour pinned |
| **CP4** — Final implementation and verification | see `git log` | Adapters, demo, and final verification |

Run `entire-continuity checkpoint list` to see the checkpoints the tool itself recorded, and
`entire-continuity task lineage` for the cross-agent tree.

---

## Setup, run and test instructions

Requires Go 1.26+. No third-party dependencies — standard library only.

```bash
go build ./...
go test ./...                     # 347 test functions across four layers
go build -o entire-continuity ./cmd/entire-continuity
```

Try it against the curveball fixture — a format this build had never seen, from a runtime it had
never heard of:

```bash
./entire-continuity ingest --file internal/normalize/testdata/lifecycle_v2_acmecode.jsonl
./entire-continuity task list
./entire-continuity task status <task-id>
./entire-continuity task handoff <task-id>
./entire-continuity task resume <task-id>
./entire-continuity task explain <task-id>
./entire-continuity graph impact Validate
```

Or against the demo domain, where a task is deliberately unfinished:

```bash
cd examples/billing && go test ./...     # TestDuplicateWebhook fails on purpose
cd ../.. && ./entire-continuity --repo examples/billing graph impact WebhookProcessor
```

That last command is the §35 next-action validation: it names the callers, the components
downstream and the six tests to run after the change — *before* the edit, not after it.

---

## Known limitations and next steps

Stated plainly, because a product whose thesis is "never present incomplete context as complete"
would be a poor advertisement for itself otherwise.

- **Entire was not installed in the development environment.** Checkpoints therefore go to a local
  fallback store. The `EntireClient` port has a real CLI implementation that shells to
  `entire checkpoint …`, selected automatically when the binary answers a probe. Every command
  prints which backend answered, and the fallback explicitly refuses to describe itself as Entire.
  **Re-running in an environment with the `entire` binary switches the backend with no code
  change** — that is what the port is for.
- **Entire Graph was likewise unavailable**, so `graph.Detect` falls back to a real local Go static
  analyser built on `go/ast`: genuine definition lookup, caller analysis and semantic diff over Go
  source. The findings in `docs/GRAPH.md` are real analysis of this repository, but they were
  produced by that analyser rather than by Entire Graph. `graph.NewCLI` targets the real thing.
- **OpenClaw and Hermes were not installed.** The hook shims in `adapters/` implement their
  documented surfaces, and `TestAdaptersFeedTheNormalizer` runs the real shims under Node and
  decodes what they actually wrote — but no shim has been driven by a live host process. Wiring
  one is a matter of registering it against that host's plugin API; the normalization layer it
  feeds would not change.
- **Semantic extraction is rule-based, not model-backed.** It sits behind `model.Extractor` so a
  model-backed implementation drops in unchanged. It deliberately **never** marks a requirement
  complete — a keyword rule has no basis to certify completion.
- `internal/model` has no direct unit tests; it is exercised through every package that uses it.
  `contextbuild` and `handoff` are covered by the end-to-end layer rather than by unit tests.

**Next steps:** live hook shims driven by real OpenClaw and Hermes processes; a model-backed
extractor behind the existing `model.Extractor` port; multi-repository tasks.
