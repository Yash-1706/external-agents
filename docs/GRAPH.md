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

## 4. A limitation, found by re-running the same query post-implementation

Re-running `graph impact` on `AgentEvent.Validate` after the curveball work landed — the same
symbol the §1 decisive finding was about — turned up a gap in Graph's own caller resolution.

```
$ entire graph impact --symbol AgentEvent.Validate --repo . --format text
```

```
Impact: AgentEvent.Validate (.../model/event.go:236) def=236 span=236-256 [method in AgentEvent]
Blast radius: 1 caller (1 direct, 0 transitive), 2 callees, 1 type consumer, 0 data flows,
              0 co-change files, 14 siblings.

Callers (1 direct, 0 transitive; who breaks if behavior changes):
- TestAgentEventValidate (.../model/model_test.go:172) [+2 more call sites]
```

Graph reports exactly **one caller, a test**. Grepping the source directly turns up production
callers Graph did not list:

- `.../derive/derive.go:243` — `ev.Validate()` where `ev model.AgentEvent`, inside a `for _, ev :=
  range in` loop
- `.../normalize/format.go:355`
- `.../normalize/normalize.go:234`
- `.../store/events.go:37`

That is the same shape of caller set the original design analysis in §1/§2 relied on (`normalize`,
`derive`, `store` — three packages). The live index is under-reporting it as one.

**Working hypothesis, not confirmed:** `Validate` is an overloaded method name — three distinct
types in this codebase define a method called `Validate` (`AgentEvent`, `Lineage`, and a
package-private `validate` function in a test fixture). The missed call sites are all on a
loop-variable value (`ev` from `for _, ev := range in`) rather than a directly-declared-type
reference, which is exactly the case where a resolver has to disambiguate an overloaded method by
inferred type instead of by literal declaration. That combination — overload plus loop-variable
receiver — is the most plausible reason the real callers dropped out of the CALLS edge set. This
was not root-caused inside Graph itself; it is a source-level observation about which call shapes
correlate with the miss.

**Why this matters for how we use Graph.** The whole product's evidence rule (`model.ConfidenceFor`)
says a claim with no evidence can never be better than `UNKNOWN`. The same rule applies to Graph
output about the repository itself: an impact result is evidence to verify, not a ground truth to
forward uncritically. Concretely, this means:

- Never present `graph impact` caller counts to a next-worker as a completeness guarantee ("only 1
  caller" must not be read as "safe to change with no other blast radius") — cross-check with a
  grep for the symbol name before treating a low caller count as license to skip validation.
- The §2 impact-analysis-before-a-high-risk-change step should not stop at the tool's caller list
  when the symbol name is a common one (`Validate`, `Run`, `Handle`, etc.) likely to be overloaded
  across types — that is precisely the condition under which this gap appeared.
- This does not retract the §1 finding. The `FormatAuto` closed-set defect was found by a different
  analysis (const-group exhaustiveness over `DetectFormat`'s switch), not by caller counting, and
  remains verified against source. The two findings are independent; only the caller-count query
  is now known to be unreliable for overloaded method names on loop-variable receivers.

No fix is proposed here beyond the workaround above (grep alongside `graph impact` for common
method names) — this is Graph's own resolver, not this repository's code, so there is nothing in
this codebase to patch. Recording it is the honest-degradation rule (plan §33/§47) applied to the
tool itself, not just to transcript capture.
