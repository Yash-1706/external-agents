package billing

// BillingService owns webhook delivery.
type BillingService struct {
	proc *WebhookProcessor
}

// Handle forwards a webhook to the processor. It is a method caller, so the
// receiver type is what shows up as a dependent.
func (s *BillingService) Handle(id string) error {
	return s.proc.Process(id)
}
