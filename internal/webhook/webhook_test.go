package webhook_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/webhook"
)

// fixture is the bytes of a contract fixture.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(contracts.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestConfigurationReadsAndFilters pins the fixtures reading, the filter, and that a
// refused document is an error naming it.
func TestConfigurationReadsAndFilters(t *testing.T) {
	all, err := webhook.Read("all-events.yaml", fixture(t, "fixtures/webhook/all-events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	some, err := webhook.Read("some-events.yaml", fixture(t, "fixtures/webhook/some-events.yaml"))
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
		_, err := webhook.Read(f, fixture(t, f))
		var we *webhook.Error
		if !errors.As(err, &we) || we.Name != f {
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
