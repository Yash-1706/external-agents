# Agent adapters

An adapter's only job is to write what its host actually exposes, in that host's own shape, to a
newline-delimited JSON log. It performs no interpretation. Everything else —
normalization, redaction, lineage, task state — happens in `internal/normalize` and below, once,
for every host.

```
OpenClaw ──┐
Hermes ────┼──▶  NDJSON log  ──▶  entire-continuity ingest  ──▶  one event vocabulary
AcmeCode ──┘
```

That split is the point of the project. If a fourth runtime appears tomorrow it adds a mapping
table to `internal/normalize`, and nothing downstream changes. It is also why the noon curveball —
a host shipping a new transcript format — was absorbed by adding a table rather than a pipeline.

## Three rules every adapter follows

1. **Emit only what the host actually exposes.** Never synthesise a lifecycle point the host does
   not have. An unmapped hook is a gap to report, not a semantic to invent.
2. **Never inline a tool payload.** Emit the tool's *name* and a reference to the artifact. A
   payload is how a credential ends up in a checkpoint. The normalizer redacts as a second line of
   defence, but the first line is not writing it down.
3. **Never block the host.** Every write is best-effort and swallows its own errors. If continuity
   capture fails, the agent's own work must continue unaffected.

## Formats

| Format | Emitted by | Discriminator |
|---|---|---|
| `openclaw/v1` | `openclaw/hook.js` | `hook` field, RFC3339 `ts` |
| `hermes/v1` | `hermes/hook.js` | `event` field, epoch-millis `time`, `agent_session` |
| `lifecycle/v2` | the host itself | `event` field, RFC3339 `timestamp`, `session_id` |

`lifecycle/v2` has no shim here because it is emitted by the host directly — that is what the
curveball introduced. It is consumed, not produced:

```bash
entire-continuity ingest --file transcript.jsonl
```

Format is detected **per record**, so a single log containing more than one format — which is what
a partially-migrated fleet produces — is read correctly. Pass `--format` to override detection.

## Ingesting

```bash
entire-continuity ingest --file .entire-continuity/events/openclaw.ndjson
entire-continuity ingest --format lifecycle/v2 --file transcript.jsonl
cat transcript.jsonl | entire-continuity ingest
```

Ingest always prints what it could and could not read before storing anything. A transcript that
was only partly understood is never reported as if it were complete.

## Status

These shims implement the documented hook surfaces and are covered by fixtures in
`internal/normalize/testdata/`. **They have not been run against a live OpenClaw or Hermes
process** — neither was available in the development environment. The normalization layer they
feed is the part that would not change; wiring a shim to a real host is a matter of registering it
against that host's plugin API.
