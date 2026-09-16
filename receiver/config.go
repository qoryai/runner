package receiver

import (
	"errors"

	"github.com/qoryai/runner/internal/webhook"
)

// Webhook is the receiving side's view of a webhook configuration: the URL the runner
// posts to, which says where a receiver listens and on which path, and the secret both
// sides sign with. Events is the runner's filter and carries no meaning here beyond
// being shown.
type Webhook struct {
	URL    string
	Secret string
	Events []string
}

// LoadWebhook reads the webhook configuration the runner reads, so one file
// configures both ends. An empty path is an error: a receiver without a secret would
// accept anything.
func LoadWebhook(path string) (*Webhook, error) {
	if path == "" {
		return nil, errors.New("no webhook configuration to receive for")
	}
	c, err := webhook.Load(path)
	if err != nil {
		return nil, err
	}
	return &Webhook{URL: c.URL, Secret: c.Secret, Events: append([]string{}, c.Events...)}, nil
}
