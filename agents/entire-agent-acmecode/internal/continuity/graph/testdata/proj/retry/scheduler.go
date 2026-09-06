package retry

import "example.com/proj/billing"

// RetryScheduler re-delivers webhooks that failed the first time.
type RetryScheduler struct {
	proc *billing.WebhookProcessor
}

// Reschedule retries one delivery.
func (r *RetryScheduler) Reschedule(id string) error {
	return r.proc.Process(id)
}

// Run drives a single scheduling pass. It is a package-level caller with no
// receiver, so the package itself is what depends on the target.
func Run(p *billing.WebhookProcessor, id string) error {
	return p.Process(id)
}
