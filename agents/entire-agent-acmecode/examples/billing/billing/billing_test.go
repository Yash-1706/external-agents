package billing

import (
	"errors"
	"testing"
	"time"
)

var (
	cycleStart = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	cycleEnd   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	midCycle   = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
)

func fixture(t *testing.T) (*BillingService, *Subscription) {
	t.Helper()
	store := NewMemoryStore()
	sub := &Subscription{
		ID: "sub_1", OwnerID: "user_1", Status: Active,
		CycleStart: cycleStart, CycleEnd: cycleEnd,
	}
	if err := store.Put(sub); err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	return NewBillingService(store), sub
}

// R1 — the pause API exists and suspends future billing.
func TestPauseSuspendsFutureBilling(t *testing.T) {
	svc, sub := fixture(t)

	paused, err := svc.Pause("user_1", sub.ID, midCycle)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if paused.Status != Paused {
		t.Errorf("Status = %q, want %q", paused.Status, Paused)
	}
	if svc.ShouldCharge(paused) {
		t.Error("a paused subscription would still be charged")
	}
	if !paused.PausedAt.Equal(midCycle) {
		t.Errorf("PausedAt = %s, want %s", paused.PausedAt, midCycle)
	}
}

// R2 — only the owner may pause.
func TestPauseRequiresAuthorization(t *testing.T) {
	svc, sub := fixture(t)

	if _, err := svc.Pause("someone_else", sub.ID, midCycle); !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	stored, err := svc.Store.Get(sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != Active {
		t.Errorf("an unauthorized pause still changed the subscription: %q", stored.Status)
	}
}

// R3 — the current billing cycle must be untouched. This is the requirement the
// rejected approach broke: mutating renewal fields moved the cycle the customer
// had already paid for.
func TestPausePreservesCurrentCycle(t *testing.T) {
	svc, sub := fixture(t)
	before := svc.Policy.NextRenewal(sub)

	paused, err := svc.Pause("user_1", sub.ID, midCycle)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if !paused.CycleStart.Equal(cycleStart) || !paused.CycleEnd.Equal(cycleEnd) {
		t.Errorf("cycle moved: %s..%s, want %s..%s",
			paused.CycleStart, paused.CycleEnd, cycleStart, cycleEnd)
	}
	if after := svc.Policy.NextRenewal(paused); !after.Equal(before) {
		t.Errorf("NextRenewal moved from %s to %s; the legacy renewal path is broken", before, after)
	}
	if !paused.InCurrentCycle(midCycle) {
		t.Error("the customer lost the cycle they had already paid for")
	}
}

func TestResumeReturnsToActive(t *testing.T) {
	svc, sub := fixture(t)
	if _, err := svc.Pause("user_1", sub.ID, midCycle); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	resumed, err := svc.Resume("user_1", sub.ID, midCycle)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != Active || !svc.ShouldCharge(resumed) {
		t.Errorf("Status = %q, ShouldCharge = %v", resumed.Status, svc.ShouldCharge(resumed))
	}
}

// TestDuplicateWebhook is R4, and it FAILS ON PURPOSE.
//
// The payment provider delivers at-least-once, so the same event id arrives
// twice. WebhookProcessor.Process has no idempotency guard, so the second
// delivery is applied again. This is the unfinished work the continuity demo
// hands to a fresh worker: a named, reproducible failure with a command behind
// it, rather than a vague note that webhooks "need attention".
//
// Fixing it means adding a seen-set keyed on Event.ID. Run Graph impact
// analysis on WebhookProcessor before doing so — it has callers.
func TestDuplicateWebhook(t *testing.T) {
	svc, sub := fixture(t)
	proc := NewWebhookProcessor(svc)

	event := Event{
		ID: "evt_dup_1", Type: "subscription.pause",
		SubscriptionID: sub.ID, ActorID: "user_1", At: midCycle,
	}

	// At-least-once delivery: the provider sends the same event twice.
	for i := 0; i < 2; i++ {
		if err := proc.Process(event); err != nil {
			t.Fatalf("Process (delivery %d): %v", i+1, err)
		}
	}

	if got := proc.Applied[event.ID]; got != 1 {
		t.Errorf("event %s was applied %d times, want exactly 1: "+
			"duplicate webhook delivery is not guarded (R4 unresolved)", event.ID, got)
	}
}

// R5 — regression coverage for the retry path. Present but thin; the scenario
// treats R5 as unresolved because the duplicate case above is not yet covered
// by a passing test.
func TestFailedWebhookIsScheduledForRetry(t *testing.T) {
	svc, sub := fixture(t)
	proc := NewWebhookProcessor(svc)

	err := proc.Process(Event{
		ID: "evt_forbidden", Type: "subscription.pause",
		SubscriptionID: sub.ID, ActorID: "intruder", At: midCycle,
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	if len(proc.Retry.Pending()) != 1 {
		t.Errorf("pending retries = %d, want 1", len(proc.Retry.Pending()))
	}
}
