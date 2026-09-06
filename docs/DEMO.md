# Demo — captured run

A real end-to-end run, captured verbatim, as the fallback demo asset the Participant Guide asks
for. Reproduce with:

```bash
cd agents/entire-agent-acmecode
go build -o entire-agent-acmecode ./cmd/entire-agent-acmecode
go build -o entire-continuity    ./cmd/entire-continuity
```

## What this shows

A transcript in a format this build had **never seen**, emitted by a runtime it had **never heard
of** (`AcmeCode 1.4.2`), becoming usable engineering state:

- **`17 record(s): 16 event(s), 1 ignored [lifecycle/v2]`** — format detected per record; the
  `usage` line is recognised and deliberately not modelled, which is reported separately from
  "unrecognised" because the two mean different things to a reader.
- **Original intent recovered** from the transcript — the one field a later worker cannot
  reconstruct from a diff.
- **`tests: 1 passed`** — the fixture runs one test twice, failing then passing. The §32 merge rule
  resolved the failure because both runs carry the same test name. Note the evidence line still
  cites the failing run: the resolution is recorded, not erased.
- **`STATE PARTIALLY RECOVERED`** — every capture gap named rather than glossed. The product never
  presents incomplete context as complete.
- **`[OBSERVED]` / `[INFERRED]` / `[RECOMMENDED]`** on every claim, and
  `graph verified: no — blast radius is unconfirmed` where Graph did not run.
- **`checkpoint backend: entire CLI`** — talking to real Entire, and it says so.

---

## Captured output

```console
$ ./entire-agent-acmecode info
{"protocol_version":1,"name":"acmecode","type":"AcmeCode","description":"AcmeCode - External agent plugin for Entire CLI, reading both the original and the v2 lifecycle transcript formats","is_preview":true,"protected_dirs":[".acmecode"],"hook_names":[],"capabilities":{"hooks":false,"transcript_analyzer":true,"transcript_preparer":false,"token_calculator":false,"compact_transcript":false,"text_generator":false,"hook_response_writer":false,"subagent_aware_extractor":false,"uses_terminal":false}}

$ ./entire-continuity ingest --file internal/continuity/normalize/testdata/lifecycle_v2_acmecode.jsonl --quiet
17 record(s): 16 event(s), 1 ignored [lifecycle/v2]
Every record was understood.

Ingested 16 event(s) into 1 task(s).
  task task_fb86c4d96b3163f1

$ ./entire-continuity task status task_fb86c4d96b3163f1
TASK
task_fb86c4d96b3163f1 — Ingested from Acmecode

INTENT
Reject expired, disabled, or minimum-cart-value-ineligible coupons
during checkout.

STATUS
recorded: partial
0 / 2 requirements complete
✓ [OBSERVED] tests: 1 passed, 0 failed, 0 skipped, 0 unknown
[OBSERVED] repo: yash-1706/external-agents branch
  buildathon/entire-continuity @
  a553cb91a34bb5e0dca55f2712a5e16806366b82 (working tree dirty)

STATE PARTIALLY RECOVERED
Capture was incomplete. Everything below is reported at the confidence
its evidence supports, and the gaps are named rather than filled in
(plan §33, §47).

Verified:
✓ git repository state
✓ test results
✓ entire checkpoints
✓ agent events
✓ agent transcript
✓ semantic extraction

Unavailable:
✗ entire graph

Unknown:
? [UNKNOWN] nothing beyond the unavailable inputs above was recorded as
  missing — whatever those inputs would have carried is unknown, not
  absent

Safe next action:
→ [RECOMMENDED] Inspect the current repository, and ask for human
  confirmation before relying on anything above that is not marked
  OBSERVED.

REQUIREMENTS
0 / 2 complete
? [INFERRED] R1 Add coupon validation to checkout
    source: original_prompt
    evidence: prompt original_prompt
? [INFERRED] R2 Coupons should be rejected if expired, disabled, or
  below the minimum cart value
    source: original_prompt
    evidence: prompt original_prompt

FILES
[OBSERVED] no files changed

TESTS
✓ [OBSERVED] apply_coupon.test.ts — passed
    command: npm test -- apply_coupon.test.ts
    ran at: 2026-09-06T03:34:18Z
    evidence: test apply_coupon.test.ts (failed)

NEXT ACTION
→ [RECOMMENDED] Resolve R1: Add coupon validation to checkout
    why: R1 has no repository evidence yet, and a heuristic cannot
    certify completion (plan §32).
    priority: 0
    graph verified: no — blast radius is unconfirmed
    evidence: prompt original_prompt
→ [RECOMMENDED] Resolve R2: Coupons should be rejected if expired,
  disabled, or below the minimum cart value
    why: R2 has no repository evidence yet, and a heuristic cannot
    certify completion (plan §32).
    priority: 1
    graph verified: no — blast radius is unconfirmed
    evidence: prompt original_prompt

LINEAGE SUMMARY
[OBSERVED] agents: Acmecode
[OBSERVED] sessions: 1
[OBSERVED] checkpoints: 1 — latest cp-001 (2026-09-06T03:34:49Z)
[OBSERVED] handoffs: 0

EVIDENCE
[L1] commit a553cb91a34bb5e0dca55f2712a5e16806366b82
[L1] test apply_coupon.test.ts (failed)
[L2] checkpoint cp-001
[L2] session btw-track3-demo-001 (ended)
[L3] prompt original_prompt

checkpoint backend: entire CLI "entire" in .

$ ./entire-continuity task explain task_fb86c4d96b3163f1
WHY IS THIS TASK PARTIAL?

REQUIREMENTS NOT YET COMPLETE
  ? R1   Add coupon validation to checkout [INFERRED]
  ? R2   Coupons should be rejected if expired, disabled, or below the minimum cart value [INFERRED]

RECOMMENDED NEXT ACTION
  → [RECOMMENDED] Resolve R1: Add coupon validation to checkout
      because R1 has no repository evidence yet, and a heuristic cannot certify completion (plan §32).

EVIDENCE
[L3] prompt original_prompt


$ ./entire-continuity task lineage task_fb86c4d96b3163f1
LINEAGE
task_fb86c4d96b3163f1  Ingested from Acmecode
│
└── Acmecode
    └── btw-track3-demo-001  [unknown role]  2026-09-06T03:30:00Z → 2026-09-06T03:35:02Z
        └── cp-001  @8d34f70  2026-09-06T03:34:49Z

LINEAGE SUMMARY
[OBSERVED] agents: Acmecode
[OBSERVED] sessions: 1
[OBSERVED] checkpoints: 1 — latest cp-001 (2026-09-06T03:34:49Z)
[OBSERVED] handoffs: 0

```
