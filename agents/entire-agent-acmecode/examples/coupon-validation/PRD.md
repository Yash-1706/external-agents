# PRD — Coupon validation at checkout

**Owner:** Payments · **Status:** in build · **Ticket:** CHK-2291

## Problem

Customers can currently apply coupons that should not be accepted: expired ones,
ones an operator has disabled, and ones below the cart minimum they were issued
for. Support is manually reversing roughly 40 orders a week because of it.

## Goal

Reject an ineligible coupon at the point it is applied, with an error the
customer can act on, and do it the same way every time.

## Requirements

R1 — Reject a coupon whose expiry date has passed
R2 — Reject a coupon an operator has disabled
R3 — Reject a coupon when the cart total is below its minimum
R4 — Return one deterministic error when a coupon fails more than one rule
R5 — Cover every rejection reason with a regression test

## Out of scope

- Partial or percentage-based coupon stacking
- Changing how coupons are issued
- Retroactive validation of coupons already applied to open orders

## Acceptance

A coupon failing several rules at once must produce the **same** error on every
run. Support cannot write a runbook against an error that changes between
attempts, and that ambiguity is what R4 exists to remove.

## Open question for engineering

When a coupon is both expired *and* disabled, which error should the customer
see? Support argues expiry, because a customer can act on it — they can ask for
a new coupon. A disabled coupon is an internal state they can do nothing about.
**This needs an engineering decision before R4 can be called done.**
