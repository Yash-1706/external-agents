# entire-agent-acmecode

External agent for **AcmeCode**, plus the task-continuity layer it is built on.

```bash
mise run build     # or: go build -o entire-agent-acmecode ./cmd/entire-agent-acmecode
mise run test      # or: go test ./...
```

## What makes this integration different

AcmeCode shipped a **new lifecycle transcript format while its existing installs kept emitting the
old one**. A reader that understands only one of them is wrong for half the fleet from the day it
lands, so the transcript layer here is format-*aware* rather than format-*assuming*:

- **Format is detected per record**, not per file, so a single log containing both shapes — what a
  partially-migrated fleet actually produces — decodes correctly.
- **One engine, three mapping tables.** Adding a format adds a table, not a pipeline. Framing,
  redaction, validation and lineage folding are shared by construction.
- **Unknown lifecycle names are retained**, not skipped, as `Unknown` events carrying the host's own
  name in `raw_kind`. A timeline silently missing records it could not read still renders as a
  complete timeline, and the reader has no way to tell.
- **A truncated transcript costs the truncated line, not the session.** An agent killed mid-write is
  exactly the case this exists for.

Every decode returns a `Report` — total, accepted, ignored, unrecognised, unreadable — and
`ExtractSummary` appends the gaps to its answer rather than presenting a partial reconstruction as
a complete one.

## Declared capabilities

| Capability | Declared | Why |
|---|---|---|
| `transcript_analyzer` | ✅ | Prompts, modified files and summary, across both formats |
| `hooks` | ❌ | AcmeCode exposes no hook-install surface this build could verify. Declaring it would make Entire install hooks that never fire — which reads to a user as "capture is working" while nothing is captured |

Claiming only what it can honour is the same rule the rest of this code follows: never present
incomplete context as complete.

## Layout

```
cmd/entire-agent-acmecode/   protocol entrypoint (JSON over stdin/stdout)
internal/acmecode/           the agent: Info, Detect, transcript analysis
internal/protocol/           the external-agent protocol
internal/continuity/         the engine — normalize, derive, merge, drift, graph, checkpoints
cmd/entire-continuity/       CLI over the engine: task status / handoff / resume / lineage
adapters/                    Node hook shims for two other runtimes, same event vocabulary
examples/billing/            demo domain with one deliberately failing test
```

`internal/continuity` is the task-continuity layer: it turns agent sessions into an
evidence-backed engineering state a *different* agent or a human can verify and continue. The
transcript reader above is its normalization front end. See
[`../../BUILDATHON.md`](../../BUILDATHON.md).

## Try it

```bash
go build -o entire-continuity ./cmd/entire-continuity
./entire-continuity ingest --file internal/continuity/normalize/testdata/lifecycle_v2_acmecode.jsonl
./entire-continuity task status <task-id>
./entire-continuity task handoff <task-id>
```

That recovers the intent, both test runs (the later pass resolving the earlier failure), the
checkpoint and the session lineage — from a transcript in a format the build had never seen,
emitted by a runtime it had never heard of.
