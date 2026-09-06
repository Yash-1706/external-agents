# Demo domain: subscription billing

The scenario from plan §52 — a small, realistic service with several
requirements, one genuine edge case, one rejected approach, and work that stops
before completion.

> **Task:** Implement subscription pause without changing the current billing cycle.

| | Requirement | State |
|---|---|---|
| R1 | Pause API | ✓ complete |
| R2 | Authorization | ✓ complete |
| R3 | Preserve the current billing cycle | ✓ complete |
| R4 | Prevent duplicate webhook processing | ✗ **unresolved** |
| R5 | Regression tests | ⚠ partial |

## `TestDuplicateWebhook` fails on purpose

This is the point of the demo, not an oversight.

```bash
cd examples/billing && go test ./...
--- FAIL: TestDuplicateWebhook
    event evt_dup_1 was applied 2 times, want exactly 1
```

The payment provider delivers at-least-once, so the same event id arrives twice.
`WebhookProcessor.Process` has no idempotency guard, so the second delivery is
applied again.

That failure is the artifact the whole product exists to carry across a session
boundary. It is not a vague note that webhooks "need attention" — it is a named
test, a reproducible command, and a file and line range. A fresh worker inherits
it as evidence instead of rediscovering it.

**It is a separate Go module**, so the repository's own `go test ./...` stays
green. A deliberately failing demo must not make the product look broken.

## The rejected approach

An earlier attempt implemented pause by writing the renewal fields directly.
That moved `RenewalPolicy.NextRenewal` and silently changed the cycle the
customer had already paid for — breaking R3 and the legacy renewal path.

It was rejected in favour of an explicit `Paused` status. `TestPausePreservesCurrentCycle`
is the regression guard.

This is the second thing continuity has to carry: without it, the next worker
reads the code, sees the obvious direct-mutation fix, and re-derives a broken
approach that has already been tried once.

## Using it with the continuity layer

```bash
# from the repository root
go build -o entire-continuity ./cmd/entire-continuity

./entire-continuity task init \
  --title "Implement subscription pause" \
  --intent "Pause future billing without changing the current billing cycle. Must add a pause API. Must preserve the current billing cycle. Must prevent duplicate webhook processing. Needs regression tests."

# before changing the webhook path, ask what it touches
./entire-continuity --repo examples/billing graph impact WebhookProcessor

./entire-continuity checkpoint create --label "CP2 pre-interruption"
./entire-continuity task handoff
```

`graph impact WebhookProcessor` reports its callers, the components downstream
(`BillingService`, `RetryScheduler`) and the tests that exercise them — which is
the §35 "next action validation" the plan asks for, run *before* the edit rather
than after it.
