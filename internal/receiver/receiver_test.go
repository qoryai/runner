package receiver_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qoryai/runner/internal/event"
	"github.com/qoryai/runner/internal/receiver"
	"github.com/qoryai/runner/internal/webhook"
)

// TestReceiverRefusesAndDeduplicates pins the receiver alone: a bad signature is 401,
// the wrong content type 415, a duplicate event is not stored twice, and a reopened
// store still knows its ids.
func TestReceiverRefusesAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "received.jsonl")
	store, err := receiver.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &receiver.Handler{Secret: "fixture-secret-not-a-real-one", Store: store}
	e := event.NewEmitter(event.NewRunID(), nil)
	line, _ := e.Make(event.RunStarted, map[string]any{"runtime": "x"}).JSON()
	body := []byte("[" + string(line) + "," + string(line) + "]")
	post := func(sig, ct string) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.Header.Set(webhook.HeaderSignature, sig)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post("sha256=00", webhook.ContentType); got != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", got)
	}
	if got := post(webhook.Sign("fixture-secret-not-a-real-one", body), "application/json"); got != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type: %d", got)
	}
	if got := post(webhook.Sign("fixture-secret-not-a-real-one", body), webhook.ContentType); got != http.StatusAccepted {
		t.Errorf("good delivery: %d", got)
	}
	if store.Count() != 1 {
		t.Errorf("stored %d, want the duplicate dropped", store.Count())
	}
	store.Close()
	reopened, err := receiver.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Count() != 1 {
		t.Errorf("reopened store holds %d ids", reopened.Count())
	}
	f, _ := os.Open(path)
	defer f.Close()
	n := 0
	for s := bufio.NewScanner(f); s.Scan(); {
		n++
	}
	if n != 1 {
		t.Errorf("%d lines in the file", n)
	}
}
