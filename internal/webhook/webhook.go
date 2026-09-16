// Package webhook reads the webhook configuration and signs and verifies deliveries.
//
// The configuration is the document of contracts/runner/v1/webhook.schema.json: a URL,
// a secret and the event types to send. [Load] reads it from a file outside the
// checkout; the schema is the reader. A delivery is one POST of a batch with the
// headers the contract names; [Sign] makes the signature of a body and [Verify] checks
// one in constant time, so the runner's sink and the reference receiver share one
// definition of what a valid delivery is.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/qoryai/runner/contracts"
)

// The headers of a delivery and its content type.
const (
	HeaderDelivery  = "X-Qory-Delivery"
	HeaderSignature = "X-Qory-Signature-256"
	ContentType     = "application/cloudevents-batch+json"
)

// Timeout is how long a receiver has to answer one delivery.
const Timeout = 10 * time.Second

// Config is the webhook configuration.
type Config struct {
	Version int      `json:"version"`
	URL     string   `json:"url"`
	Secret  string   `json:"secret"`
	Events  []string `json:"events"`
}

// Error is a configuration that could not be read or is not a webhook configuration.
// A run does not start on it.
type Error struct {
	Path string
	Err  error
}

func (e *Error) Error() string { return "webhook " + e.Path + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Load reads the configuration at path. An empty path is no webhook and returns nil.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Path: path, Err: err}
	}
	c, err := Parse(path, b)
	if err != nil {
		return nil, &Error{Path: path, Err: err}
	}
	return c, nil
}

// Parse validates the bytes of a configuration against the schema and decodes it.
func Parse(name string, b []byte) (*Config, error) {
	doc, err := contracts.Decode(name, b)
	if err != nil {
		return nil, err
	}
	schema, err := contracts.Compile("webhook.schema.json")
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(doc); err != nil {
		return nil, err
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(j, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Wants reports whether the configuration asks for events of the type: every type when
// Events is absent or holds "*", else the listed ones. The ping is always wanted.
func (c *Config) Wants(typ string) bool {
	if typ == "ai.qory.ping" || len(c.Events) == 0 {
		return true
	}
	for _, e := range c.Events {
		if e == "*" || e == typ {
			return true
		}
	}
	return false
}

// Sign returns the signature header value of a body: "sha256=" and the hex HMAC
// SHA-256 of the body keyed with the secret.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify reports whether header is the signature of body under the secret, compared in
// constant time.
func Verify(secret string, body []byte, header string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(header))
}

// Client delivers batches to one webhook.
type Client struct {
	Config *Config
	// UserAgent is sent as User-Agent: qory-runner/<version>.
	UserAgent string
	// HTTP is the client used; nil means one with Timeout.
	HTTP *http.Client
}

// Deliver posts one body as the delivery with the given id and returns the receiver's
// status. A transport failure or no answer within Timeout is an error and no status.
func (c *Client) Deliver(ctx context.Context, deliveryID string, body []byte) (int, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: Timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.URL, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set(HeaderDelivery, deliveryID)
	req.Header.Set(HeaderSignature, Sign(c.Config.Secret, body))
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// Accepted reports whether a status accepts a delivery.
func Accepted(status int) bool { return status >= 200 && status < 300 }

// Stop reports whether a status tells the runner to send nothing more for the run.
func Stop(status int) bool { return status == http.StatusGone }

// ErrNotAccepted is the error of a ping the receiver did not accept.
var ErrNotAccepted = errors.New("the receiver did not accept the ping")

// Ping delivers a batch of one ping event and returns nil only on a 2xx. It is what
// makes a configured webhook fail closed: the run does not start otherwise.
func (c *Client) Ping(ctx context.Context, deliveryID string, body []byte) error {
	status, err := c.Deliver(ctx, deliveryID, body)
	if err != nil {
		return fmt.Errorf("ping %s: %w", c.Config.URL, err)
	}
	if !Accepted(status) {
		return fmt.Errorf("ping %s: status %d: %w", c.Config.URL, status, ErrNotAccepted)
	}
	return nil
}
