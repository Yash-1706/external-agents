package billing

import "time"

// Event is an inbound billing webhook from the payment provider.
type Event struct {
	// ID is the provider's event id. A provider guarantees at-least-once
	// delivery, so the same ID can and does arrive more than once.
	ID             string
	Type           string
	SubscriptionID string
	ActorID        string
	At             time.Time
}

// RetryScheduler re-queues an event whose handling failed.
type RetryScheduler struct{ queued []Event }

// Schedule enqueues an event for another attempt.
func (r *RetryScheduler) Schedule(e Event) { r.queued = append(r.queued, e) }

// Pending returns the events waiting to be retried.
func (r *RetryScheduler) Pending() []Event { return r.queued }

// WebhookProcessor applies provider events to subscriptions.
//
// R4 — prevent duplicate webhook processing — is UNRESOLVED here. The provider
// delivers at-least-once, so Process can be called twice with the same Event.ID;
// nothing below detects that, so the second delivery is applied again.
//
// TestDuplicateWebhook fails against this file. That failure is the demo: it is
// the concrete, evidence-backed piece of unfinished work a fresh worker inherits
// instead of rediscovering, and it is what Graph impact analysis is pointed at
// before the fix is attempted.
type WebhookProcessor struct {
	Billing *BillingService
	Retry   *RetryScheduler
	// Applied counts how many times an event was actually applied, keyed by
	// event id. The test asserts this never exceeds one per id.
	Applied map[string]int
}

// NewWebhookProcessor wires a processor onto a billing service.
func NewWebhookProcessor(b *BillingService) *WebhookProcessor {
	return &WebhookProcessor{Billing: b, Retry: &RetryScheduler{}, Applied: map[string]int{}}
}

// Process applies one webhook event.
//
// TODO(R4): this is not idempotent. A duplicate delivery of the same Event.ID
// is applied a second time. The fix is a seen-set keyed on Event.ID, checked
// before the switch below and recorded only after a successful apply.
func (w *WebhookProcessor) Process(e Event) error {
	switch e.Type {
	case "subscription.pause":
		if _, err := w.Billing.Pause(e.ActorID, e.SubscriptionID, e.At); err != nil {
			w.Retry.Schedule(e)
			return err
		}
	case "subscription.resume":
		if _, err := w.Billing.Resume(e.ActorID, e.SubscriptionID, e.At); err != nil {
			w.Retry.Schedule(e)
			return err
		}
	default:
		return nil
	}
	w.Applied[e.ID]++
	return nil
}
