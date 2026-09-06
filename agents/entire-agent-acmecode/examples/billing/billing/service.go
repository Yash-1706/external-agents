package billing

import "time"

// RenewalPolicy decides when a subscription next renews.
//
// This is the legacy path the rejected approach broke. An earlier attempt
// implemented pause by writing the renewal fields directly, which made
// NextRenewal return a moved date and silently changed the cycle the customer
// had already paid for. Pause must leave this untouched.
type RenewalPolicy struct{}

// NextRenewal returns the instant the subscription renews.
func (RenewalPolicy) NextRenewal(s *Subscription) time.Time { return s.CycleEnd }

// Authorizer decides whether an actor may modify a subscription (R2).
type Authorizer interface {
	CanModify(actorID string, s *Subscription) bool
}

// OwnerOnly permits only the subscription's owner.
type OwnerOnly struct{}

// CanModify reports whether the actor owns the subscription.
func (OwnerOnly) CanModify(actorID string, s *Subscription) bool { return actorID == s.OwnerID }

// BillingService is the entry point for subscription operations.
type BillingService struct {
	Store  Store
	Auth   Authorizer
	Policy RenewalPolicy
}

// NewBillingService wires a service with owner-only authorization.
func NewBillingService(store Store) *BillingService {
	return &BillingService{Store: store, Auth: OwnerOnly{}}
}

// Pause suspends future billing (R1) without moving the current cycle (R3).
//
// The cycle fields are deliberately not touched. That is the whole requirement:
// the customer keeps the period they already paid for, and only the *next*
// charge is suppressed.
func (b *BillingService) Pause(actorID, id string, now time.Time) (*Subscription, error) {
	s, err := b.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if !b.Auth.CanModify(actorID, s) {
		return nil, ErrForbidden
	}
	if s.Status == Ended {
		return nil, ErrAlreadyEnded
	}
	if s.Status == Paused {
		return s, nil // pausing twice is not an error
	}

	s.Status = Paused
	s.PausedAt = now
	if err := b.Store.Put(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Resume returns a paused subscription to active billing.
func (b *BillingService) Resume(actorID, id string, now time.Time) (*Subscription, error) {
	s, err := b.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if !b.Auth.CanModify(actorID, s) {
		return nil, ErrForbidden
	}
	if s.Status != Paused {
		return s, nil
	}
	s.Status = Active
	s.PausedAt = time.Time{}
	if err := b.Store.Put(s); err != nil {
		return nil, err
	}
	return s, nil
}

// ShouldCharge reports whether the next cycle should be billed.
func (b *BillingService) ShouldCharge(s *Subscription) bool { return s.Status == Active }
