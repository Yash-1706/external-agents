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
