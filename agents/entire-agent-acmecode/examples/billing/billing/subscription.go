// Package billing is the demo domain from plan §52: a subscription service
// with several requirements, one real edge case, and a task deliberately
// stopped before completion.
//
// The task under way here is:
//
//	"Implement subscription pause without changing the current billing cycle."
//
//	R1 Pause API                          complete
//	R2 Authorization                      complete
//	R3 Preserve the current billing cycle partial
//	R4 Prevent duplicate webhook processing  UNRESOLVED  (TestDuplicateWebhook fails)
//	R5 Regression tests                   unresolved
//
// The failing test is intentional and is the point of the demo: it is the
// concrete, evidence-backed failure a fresh worker has to inherit rather than
// rediscover. See README.md.
package billing

import (
	"errors"
	"time"
)

// Status is the lifecycle of a subscription.
//
// Pausing is modelled as an explicit state rather than by mutating renewal
// fields. That choice is load-bearing: the direct-mutation approach was tried
// and rejected because it broke the legacy renewal path (see RenewalPolicy).
type Status string

const (
	Active Status = "active"
	Paused Status = "paused"
	Ended  Status = "ended"
)

var (
	ErrNotFound     = errors.New("subscription not found")
	ErrForbidden    = errors.New("not authorized to modify this subscription")
	ErrAlreadyEnded = errors.New("subscription has ended")
)

// Subscription is one customer's plan.
type Subscription struct {
	ID      string
	OwnerID string
	Status  Status
	// CycleStart and CycleEnd bound the cycle the customer has already paid
	// for. Pausing must not move either (R3).
	CycleStart time.Time
	CycleEnd   time.Time
	// PausedAt records when a pause took effect, for reporting.
	PausedAt time.Time
}

// InCurrentCycle reports whether t falls inside the paid-for cycle.
func (s *Subscription) InCurrentCycle(t time.Time) bool {
	return !t.Before(s.CycleStart) && t.Before(s.CycleEnd)
}

// Store is the persistence port for subscriptions.
type Store interface {
	Get(id string) (*Subscription, error)
	Put(s *Subscription) error
}

// MemoryStore is an in-memory Store for the demo and its tests.
type MemoryStore struct{ items map[string]*Subscription }

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: map[string]*Subscription{}}
}

// Get returns a copy so callers cannot mutate stored state by accident.
func (m *MemoryStore) Get(id string) (*Subscription, error) {
	s, ok := m.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	clone := *s
	return &clone, nil
}

// Put stores a subscription.
func (m *MemoryStore) Put(s *Subscription) error {
	clone := *s
	m.items[s.ID] = &clone
	return nil
}
