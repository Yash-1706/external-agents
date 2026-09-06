# Noon Curveball — Track 3: The agent changed its format

**Status:** implemented and tested.
**Pre-curveball baseline:** commit `e04266c` — the last stable state before any curveball edit.
**Graph impact analysis:** `2a6250b`, run *before* editing. See [GRAPH.md](GRAPH.md).
**Implementation:** `3f2ffa6`.

---

## 1. What the curveball requires

> The agent or workflow you integrated has released a new transcript and lifecycle event format.
> Existing users still produce the original format.

| # | Requirement | Where it lands |
|---|---|---|
| C1 | Support both the original and the new format | `internal/normalize` |
| C2 | Unknown events must not crash the integration | `internal/normalize`, `internal/model` event validation |
| C3 | An incomplete transcript must produce a **partial** result, not a corrupted or discarded session | `internal/normalize` → `internal/derive` → `Capture` |
| C4 | Existing Checkpoint behaviour must remain compatible | `internal/entire`, `EngineeringState` serialization |
| C5 | Tests for: original format, new format, unknown events, incomplete input | `internal/normalize/*_test.go` + fixtures |
| C6 | Do not duplicate the entire implementation for each format | architecture of `internal/normalize` |

---

## 2. The assumption the curveball invalidated

The pre-curveball design encoded one assumption in three places:

> **"Each host agent emits exactly one, stable, fully-enumerable lifecycle format. A normalizer is
> therefore a total function from raw envelope to known event, and anything it does not recognise
> is noise that can be discarded."**

Concretely, that assumption is visible as:

**A1 — One format per agent.**
`normalize.For(agent model.AgentKind) (Normalizer, error)` keys the parser off *agent identity
alone*. There is no format dimension anywhere in the signature, so there is no way to express
"OpenClaw, new format" versus "OpenClaw, original format".

**A2 — Unrecognised records carry no information.**
The v1 rule was *skip unknown hook/event names silently*. That was defensible when the vocabulary
was closed: an unknown name meant a host feature we deliberately did not model. It is not
defensible once the host ships a new format, because then unknown names are exactly the records
that matter — and silently dropping them makes the timeline under-report while still *looking*
complete. That directly contradicts our own honesty rule (plan §33/§47): the product must never
present incomplete context as complete.

**A3 — A transcript is either complete or invalid.**
`model.AgentEvent.Validate()` rejects a malformed record, and v1 normalize propagates that as an
error for the whole batch. One truncated trailing line in a JSONL transcript would therefore
discard an entire session's worth of recoverable events.

The curveball breaks all three at once: **A1** because both formats are live simultaneously
("existing users still produce the original format"), **A2** because unknown events must be
survivable *and* visible, and **A3** because an incomplete transcript must still yield a usable
partial result.

---

## 3. How the design changes

The fix is a **format dimension plus a declarative mapping table**, not a second parser.

### 3.1 Formats become data, not code

v1 had one hand-written decode path per agent. v2 introduces a `FormatSpec`: a declarative
description of an envelope — where the kind, timestamp, session, parent, role, task and payload
reference live, and a kind → `model.EventType` mapping table. One shared decode engine walks any
`FormatSpec`. Adding a format is adding a table, not a pipeline.

This is what satisfies **C6**: the original and new formats share every line of decoding,
validation, redaction and lineage-folding logic. They differ only in their table.

### 3.2 Format is detected, not assumed

`normalize.For(agent, format)` gains the missing dimension, and `normalize.Detect(record)` sniffs
the envelope so a mixed stream — original and new records interleaved, which is exactly what a
partially-migrated fleet produces — decodes correctly record by record. An explicit override stays
available for when the caller knows better.

### 3.3 Unknown events become first-class

Unknown records are **retained**, not dropped: an event of type `Unknown` carrying its original
kind in `Attrs["raw_kind"]`. Nothing crashes (**C2**), and the fact that the timeline contains
records we could not interpret becomes *visible* rather than invisible.

### 3.4 Partial input degrades into a partial result

Decoding returns a `Report` — total, accepted, unknown, malformed, formats seen — which flows into
`Capture`. A truncated final line increments `malformed` and is skipped; the surrounding session
survives (**C3**). The renderer already has a `STATE PARTIALLY RECOVERED` path, and this feeds it
honest numbers.

---

## 4. Why the new result is safe

This is the part that matters for "preserves existing behaviour":

1. **The original format is pinned by golden tests written before the change.** Original-format
   fixtures must normalize byte-identically after the refactor. If they do not, the refactor is
   wrong, and the test says so.

2. **The checkpoint writing path is untouched (C4).** `EngineeringState` keeps `schema_version: 1`;
   no field is removed or repurposed. A checkpoint written by v1 still loads, and a checkpoint
   written by v2 from original-format input is identical to the one v1 would have written.

3. **The new information can only make output more cautious.** Unknown and malformed counts only
   ever *widen* `Capture.Missing`. Every downstream consumer treats a wider `Missing` as less
   certainty, never more. `validate.Enforce` is monotonically downgrading by construction — it
   cannot strengthen a claim — so no new code path can turn a partial transcript into a confident
   assertion.

4. **The failure mode moved in the safe direction.** v1 could discard a whole session on one bad
   line (silent data loss that still rendered as a clean result). v2 keeps the session and reports
   the gap. The worst case changed from *confidently wrong* to *explicitly incomplete*.

---

## 5. Graph impact analysis

> Run **before** editing. Findings recorded in [GRAPH.md](GRAPH.md).

Target paths to enumerate with Graph impact analysis, per the curveball card — every parser,
lifecycle handler, summary path and checkpoint-writing path that consumes normalized events:

- `normalize.Normalizer` implementations and their callers
- `model.AgentEvent.Validate` callers
- `model.EventType.Valid` callers
- `derive.State` (lifecycle → state)
- `store.AppendEvent` (lifecycle → lineage)
- `entire` checkpoint write path
- `render` summary paths that report capture completeness

_This section is completed with real `graph impact` output before any code is edited._

---

## 6. What the real fixture changed

The official JSONL fixture arrived after the analysis above was written, and it invalidated part
of it. Recording that rather than quietly editing the prediction, because the difference is the
point: the fixture broke *more* than the card's description implied.

`internal/normalize/testdata/lifecycle_v2_acmecode.jsonl`, 17 records from a runtime calling
itself **AcmeCode 1.4.2**. Three properties were not predictable from the card:

1. **It carries no `task_id` at all.** The predicted design assumed the new format would still
   name a task. It does not — it names a `repository`, a `branch` and a `session_id`. This is what
   turned the Graph finding about `Validate`'s three callers from interesting into decisive: an
   event with no task id fails validation at the boundary. The anchor is now *derived* through the
   existing `model.NewTaskID(repo, branch, session)`, which also makes ingestion idempotent — the
   same transcript always lands on the same task.

2. **It names a runtime outside the enumerated vocabulary.** `AgentKind.Valid` rejected anything
   but the four constants, so every AcmeCode event would have failed validation. The closed agent
   vocabulary was part of the same invalidated assumption — *we can enumerate the world* — and was
   widened to accept any safe identifier. Two packages had already built private workarounds for
   this (`render` printed `Unknown (cursor)`, `entire` coerced to `unknown`); both are now
   redundant, which is the tell that the defect was in the model rather than in them.

3. **It splits a tool call across two records** correlated by `call_id`. A test outcome is only
   knowable by remembering the call that produced it, so the decoder is stateful for the length of
   a stream. This is what lets the fixture's failing run (`exit_code 1`, "1 failed, 7 passed") and
   its later passing run (`exit_code 0`, "8 passed") resolve against each other under the §32 merge
   rule — a failing test stays failed until a *newer result for the same name* proves otherwise.

The fixture also exercised two defects that had nothing to do with formats, found only because a
real transcript was run end to end:

- The heuristic extractor mined rejection cues from **user prompts**, so the prompt "Coupons
  should be **rejected** if expired…" was recorded as a *rejected approach*. That tells the next
  worker not to build the very thing they were asked for — worse than extracting nothing. Cue
  mining is now restricted to agent-authored events.
- A task created by ingestion never recovered its **original intent**, the one field a later
  worker cannot reconstruct from the diff. It is now lifted from the checkpoint's stated intent,
  and the opening prompt is fed separately to requirement extraction, because the polished intent
  line and the prompt are different things and only the prompt carries the obligations.

## 7. Verification

```
go test ./...          # 323 test functions, all packages green
```

The four cases the card requires each have a test in `internal/normalize/format_test.go`:

| Case | Test |
|---|---|
| Original format | `TestOriginalFormatsStillDecode` — asserts the v1 fixtures decode **byte-identically** through the new path and the old one |
| New format | `TestLifecycleV2Fixture`, `TestLifecycleV2CorrelatesToolResults` |
| Unknown events | `TestUnknownEventsAreRetainedNotDropped`, `TestUnknownEventsInOriginalFormat` |
| Incomplete input | `TestIncompleteTranscriptProducesPartialResult`, `TestHeadlessTranscriptStillAnchors` |

Plus `TestMixedFormatStream`, which decodes a single stream containing both formats at once.

The first row is the one that matters for "preserves existing behaviour": it does not check that
the old fixtures still *work*, it checks that they produce exactly the same events they did
before, by running both paths and comparing.

End to end, through the real CLI:

```bash
entire-continuity ingest --file internal/normalize/testdata/lifecycle_v2_acmecode.jsonl
entire-continuity task status <task-id>
```

recovers the intent, both test runs (resolved failing → passing by the merge rule), the
checkpoint, the session lineage and the runtime name — from a transcript in a format this build
had never seen, emitted by a runtime it had never heard of.
