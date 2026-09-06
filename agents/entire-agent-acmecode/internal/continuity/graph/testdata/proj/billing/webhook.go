// This tree is a fixture for the local Go analyser. It lives under testdata so
// the go tool never builds it; the tests read it with go/parser directly.
package billing

import "errors"

var errEmptyID = errors.New("empty webhook id")

// WebhookProcessor consumes inbound billing webhooks.
type WebhookProcessor struct {
	retries int
}

// Process handles one webhook payload. It is the target most of the local
// analyser tests resolve.
func (p *WebhookProcessor) Process(id string) error {
	return validate(id)
}

// validate is called by Process through a plain identifier, which is the other
// call shape the caller walk has to recognise.
func validate(id string) error {
	if id == "" {
		return errEmptyID
	}
	return nil
}
