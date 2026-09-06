# Entire Graph — findings and verification

All output below is from **Entire Graph v0.4.0** (`entire plugin install graph`), run inside the
Entire mirror clone of `Yash-1706/external-agents`.

An earlier version of this document recorded findings from a local `go/ast` fallback analyser,
used while Entire was not yet installed. Those are superseded by the real runs below. The fallback
still ships (`internal/continuity/graph`) because the product must degrade honestly when Graph is
absent — but it is no longer what this evidence rests on.

---

## 1. Definition lookup (`graph search`)

```
$ entire graph search --query "detect transcript format per record and decode both formats" \
    --repo . --top-k 5 --format text
```

```
CLOSED SET Format (const-group, 4 variants): adding a variant without adding an arm
  compiles and then falls out of the switch
  .../normalize/format.go:150 newDecoder 3/4 arms, default absent, checked at runtime

1. .../normalize/format.go:193-215 score=38.2395 symbol=DetectFormat kind=function
   signals=path,body,symbol-name,graph:callers,complete-symbol
2. .../normalize/format.go:39-43   score=36.8280 symbol=Formats
3. .../cmd/entire-continuity/integration.go:231-237 symbol=formatList
```

**This is the finding that changed the code.** Graph did not just locate the format logic — it
reported that `Format` is a closed set of 4 variants and that `newDecoder` covered 3 arms with no
`default`, so a future variant would compile and then fall out of the switch at runtime.

Verifying it against source turned up a **live defect**, not just a latent one: `FormatAuto` is a
*valid* `Format`, but it reached the fallthrough and was reported as an **"unknown format"**. That
is a wrong answer to a caller who passed something legitimate — `auto` is not undecodable, it is
*unresolved*, and must be turned into a concrete format by `DetectFormat` first.

Fixed in `a58128b`: the switch is now exhaustive with an explicit `FormatAuto` arm naming the real
mistake, plus `default`. `TestNewDecoderIsExhaustiveOverFormat` pins it, so a format added without
an arm fails a test instead of failing at runtime.

Our own fallback analyser did **not** find this. Closed-set analysis over a const group is exactly
the structural reasoning a real code graph does and a name-resolving AST walk does not.

---

## 2. Impact analysis before a high-risk change (`graph impact`)

Run **before** editing the format layer.

```
$ entire graph impact --symbol DetectFormat --repo . --format text
```

```
Impact: DetectFormat (.../normalize/format.go:193) def=193 span=193-215 [function]
Blast radius: 14 callers (2 direct, 12 transitive), 3 callees, 2 type consumers,
              0 data flows, 0 co-change files, 0 siblings.

Callers (2 direct, 12 transitive; who breaks if behavior changes):
- Stream (.../normalize/format.go:250, def :227)
- TestDetectFormat (.../normalize/format_test.go:425)
- TestAdaptersFeedTheNormalizer (.../normalize/adapter_test.go:140) [+1 more call site, via Stream]
- TestLifecycleV2Fixture, TestLifecycleV2CorrelatesToolResults,
  TestOriginalFormatsStillDecode, TestMixedFormatStream,
  TestUnknownEventsAreRetainedNotDropped, TestUnknownEventsInOriginalFormat,
  TestIncompleteTranscriptProducesPartialResult, TestHeadlessTranscriptStillAnchors,
  TestStreamIsDeterministic, TestForcedFormatOverridesDetection,
  TestExplicitTaskIDWins                                            [all via Stream]

Callees (3): bytes.TrimSpace, encoding/json.Unmarshal, strings.TrimSpace
Type consumers: -> Format [RETURNS_TYPE], -> Format [USES_TYPE]
```

**The decision this produced:** every consumer of format detection reaches it through `Stream`, and
`TestOriginalFormatsStillDecode` sits in that blast radius. That test is the one asserting the v1
fixtures decode **byte-identically** through the new path and the old one — so it is the specific
regression guard for "preserves existing behaviour," and it is confirmed to cover the change rather
than assumed to.

Two direct callers rather than a wide fan-out also told us the format dimension could be added
without a broad refactor.

---

## 3. Final semantic diff (`graph diff`)

```
$ entire graph diff --base HEAD~1 --head HEAD --repo .
```

```
Semantic changes HEAD~1..HEAD

.../normalize/format.go (Go)
  ~ function newDecoder body changed (2 dependents)

.../normalize/format_test.go (Go)
  + function TestNewDecoderIsExhaustiveOverFormat added
```

Consistent with the intended change: one function body tightened, one test added, nothing else
semantically touched. The 2 dependents are `Stream` and the new test — no unintended surface moved.

---

## What Graph changed, in one line

> It found that a closed set of formats was matched non-exhaustively, which turned out to be
> misreporting a valid input as unknown — and it named the exact test that guards the behaviour we
> had promised to preserve.

Graph results are evidence, not an oracle. Both findings above were verified against source before
being acted on, and the fix carries a test.

---


## 4. A reproducible Graph limitation: analysis scope across a nested module

Re-running `graph impact` on `AgentEvent.Validate` — the symbol the §1 decision rested on — gives
**two different answers depending on which directory you point `--repo` at**.

From the repository root:

```
$ cd external-agents
$ entire graph impact --symbol AgentEvent.Validate --repo . --format text

Impact: AgentEvent.Validate (agents/entire-agent-acmecode/internal/continuity/model/event.go:236)
Blast radius: 1 caller (1 direct, 0 transitive), ...
Callers (1 direct, 0 transitive):
- TestAgentEventValidate (.../model/model_test.go:172) [+2 more call sites]
```

From the module root, same symbol, same command:

```
$ cd agents/entire-agent-acmecode
$ entire graph impact --symbol AgentEvent.Validate --repo . --format text

Impact: AgentEvent.Validate (internal/continuity/model/event.go:236)
Blast radius: 14 callers (4 direct, 10 transitive), ...
Callers (4 direct, 10 transitive):
- selectEvents   (internal/continuity/derive/derive.go:243)
- finalize       (internal/continuity/normalize/normalize.go:234)
- FS.AppendEvent (internal/continuity/store/events.go:37)
- TestAgentEventValidate (internal/continuity/model/model_test.go:172)
- State, harness.seed, ... [transitive]
```

**One caller versus fourteen.** The second answer is the correct one — the four direct callers match
what `Select-String '\.Validate\(\)'` finds in the source, and they are exactly the three packages
(`derive`, `normalize`, `store`) the §1 design decision was based on.

`external-agents` is a repository of *independent Go modules*: each `agents/entire-agent-*/`
carries its own `go.mod`. Indexed from the repository root, call edges inside a nested module do
not resolve, and the blast radius collapses to whatever the root-level index can see. Both runs
above report `cache-miss` and rebuild, so this is not a stale cache — it reproduces.

An earlier revision of this document attributed the miss to overloaded method names and
loop-variable receivers. **That hypothesis was wrong**, and it is corrected here rather than
quietly deleted: the deciding variable is the `--repo` root, not the call shape. It was found by
re-running the command from a different working directory while preparing a demo.

### What to do about it

- **Point `--repo` at the module root**, not the repository root, in a multi-module repo.
- **Never read a low caller count as "safe to change."** `1 caller` here did not mean one caller;
  it meant the analysis could not see the rest.
- Cross-check any impact result that will gate a decision against a plain source search.

### Why this belongs in the submission

The product's own rule is that a claim can never outrank its evidence, and that historical context
must be verified before it is relied on. That rule applies to Graph output about this repository,
and it applies to *this document*: a finding recorded four hours ago turned out to be wrong the
moment it was re-run under different conditions.

That is precisely the failure mode the continuity layer exists to catch — which is why
`graph.Verification` carries `Available` and `Resolved` as separate fields, and why every next
action renders `graph verified: no — blast radius is unconfirmed` when Graph did not run. "We did
not look" and "there is nothing there" must never be the same answer.
