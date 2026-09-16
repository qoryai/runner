package webhook_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/webhook"
)

func write(t *testing.T, fixture string) string {
	t.Helper()
	b, err := fs.ReadFile(contracts.FS, fixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), filepath.Base(fixture))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConfigurationLoadsAndFilters pins the fixtures loading, the filter, and that no
// path is no webhook.
func TestConfigurationLoadsAndFilters(t *testing.T) {
	if c, err := webhook.Load(""); c != nil || err != nil {
		t.Errorf("no path: %v %v", c, err)
	}
	all, err := webhook.Load(write(t, "fixtures/webhook/all-events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	some, err := webhook.Load(write(t, "fixtures/webhook/some-events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cfg  *webhook.Config
		typ  string
		want bool
	}{
		{all, "ai.qory.run.log", true}, {all, "ai.qory.ping", true},
		{some, "ai.qory.run.log", false}, {some, "ai.qory.run.egress", true}, {some, "ai.qory.ping", true},
	} {
		if got := c.cfg.Wants(c.typ); got != c.want {
			t.Errorf("%s wants %s = %v", c.cfg.URL, c.typ, got)
		}
	}
	for _, f := range []string{"fixtures/invalid/webhook-no-secret.yaml", "fixtures/invalid/webhook-plain-http.yaml"} {
		_, err := webhook.Load(write(t, f))
		var we *webhook.Error
		if !errors.As(err, &we) {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// TestSignatureRoundTrips pins the header value and constant-time verification.
func TestSignatureRoundTrips(t *testing.T) {
	sig := webhook.Sign("fixture-secret-not-a-real-one", []byte("[]"))
	if len(sig) != len("sha256=")+64 || sig[:7] != "sha256=" {
		t.Errorf("signature %s", sig)
	}
	if !webhook.Verify("fixture-secret-not-a-real-one", []byte("[]"), sig) {
		t.Error("a valid signature was refused")
	}
	if webhook.Verify("fixture-secret-not-a-real-one", []byte("[{}]"), sig) || webhook.Verify("other", []byte("[]"), sig) || webhook.Verify("fixture-secret-not-a-real-one", []byte("[]"), "") {
		t.Error("an invalid signature was accepted")
	}
}

// TestDeliveryCarriesTheHeaders pins one POST: content type, user agent, delivery id
// and a signature the receiver verifies, and the ping's fail-closed rule.
func TestDeliveryCarriesTheHeaders(t *testing.T) {
	var status = 202
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != webhook.ContentType || r.Header.Get("User-Agent") != "qory-runner/test" || r.Header.Get(webhook.HeaderDelivery) != "d1" || !webhook.Verify("fixture-secret-not-a-real-one", body, r.Header.Get(webhook.HeaderSignature)) {
			t.Errorf("headers %v", r.Header)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := &webhook.Client{Config: &webhook.Config{URL: srv.URL, Secret: "fixture-secret-not-a-real-one"}, UserAgent: "qory-runner/test"}
	if err := c.Ping(context.Background(), "d1", []byte("[]")); err != nil {
		t.Error(err)
	}
	status = 500
	if err := c.Ping(context.Background(), "d1", []byte("[]")); !errors.Is(err, webhook.ErrNotAccepted) {
		t.Errorf("ping on 500: %v", err)
	}
	srv.Close()
	if err := c.Ping(context.Background(), "d1", []byte("[]")); err == nil {
		t.Error("ping on a closed server succeeded")
	}
}
