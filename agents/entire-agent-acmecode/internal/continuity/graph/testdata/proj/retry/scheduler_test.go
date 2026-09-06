package retry

import "testing"

// TestRetry reaches the target only through Reschedule.
func TestRetry(t *testing.T) {
	r := &RetryScheduler{}
	if err := r.Reschedule("x"); err != nil {
		t.Fatal(err)
	}
}
