package billing

import "testing"

// TestDuplicateWebhook references Process directly.
func TestDuplicateWebhook(t *testing.T) {
	p := &WebhookProcessor{}
	if err := p.Process("dup"); err != nil {
		t.Fatal(err)
	}
}

// TestingHelperIsNotATest is named like a test but is not one: the rune after
// "Test" is lower case. It must never appear in a candidate test list.
func TestingHelperIsNotATest() {}
