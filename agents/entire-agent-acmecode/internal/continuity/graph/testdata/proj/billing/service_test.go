package billing

import "testing"

// TestBillingResume never names Process. It is a candidate test only because it
// exercises Handle, which is a direct caller of Process.
func TestBillingResume(t *testing.T) {
	s := &BillingService{proc: &WebhookProcessor{}}
	if err := s.Handle("resume"); err != nil {
		t.Fatal(err)
	}
}
