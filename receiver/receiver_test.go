package receiver_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/event"
	"github.com/qoryai/runner/internal/server"
	"github.com/qoryai/runner/receiver"
)

const (
	secret   = "fixture-secret-not-a-real-one"
	previous = "fixture-secret-before-rotation"
	key      = "ak_f1xt0re000000000"
)

// keys is a lookup that knows the fixture key with two secrets, after a rotation, and
// counts how often it is asked.
func keys(asked *int) func(string) ([]string, bool) {
	return func(k string) ([]string, bool) {
		*asked++
		if k == key {
			return []string{secret, previous}, true
		}
		return nil, false
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(contracts.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// digestOf is a receiver's digest of a document: any string, opaque to the runner;
// here the hex sha256 of the bytes.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256=" + hex.EncodeToString(sum[:])
}

// handler is a receiver over a fresh store, serving the contract's fixtures as its
// documents, with its clock where the test puts it.
func handler(t *testing.T, now int64) (*receiver.Handler, *receiver.File, *int) {
	t.Helper()
	store, err := receiver.OpenFile(filepath.Join(t.TempDir(), "received.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	asked := new(int)
	conf := fixture(t, "fixtures/configuration/with-run.json")
	run := fixture(t, "fixtures/run-configuration/enforce.json")
	h := &receiver.Handler{
		Keys:          keys(asked),
		Store:         store,
		Now:           func() time.Time { return time.Unix(now, 0) },
		Configuration: func() ([]byte, string) { return conf, digestOf(conf) },
		RunConfiguration: func(forge, repository string) ([]byte, string, bool) {
			if forge == "none.example" {
				return nil, "", false
			}
			return run, digestOf(run), true
		},
	}
	return h, store, asked
}

// signedGET makes a GET signed under the secret at the receiver's clock.
func signedGET(target, sec string, ts string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(server.HeaderAccessKey, key)
	req.Header.Set(server.HeaderTimestamp, ts)
	req.Header.Set(server.HeaderSignature, server.SignGET(sec, http.MethodGet, target, ts))
	return req
}

// signedPOST makes a delivery signed under the secret.
func signedPOST(target, sec string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(body)))
	req.Header.Set(server.HeaderAccessKey, key)
	req.Header.Set("Content-Type", server.ContentType)
	req.Header.Set(server.HeaderDelivery, event.NewID())
	req.Header.Set(server.HeaderSignature, server.Sign(sec, body))
	return req
}

func serve(h *receiver.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSignedFixturesReplay pins the receiver against the contract's signed fixtures:
// each, sent as recorded with the receiver's clock at the fixture time, gets the
// status the fixture expects, and the replayed batch stores nothing twice.
func TestSignedFixturesReplay(t *testing.T) {
	h, store, _ := handler(t, 1700000000)
	names, err := fs.Glob(contracts.FS, "fixtures/signed/*.json")
	if err != nil || len(names) < 8 {
		t.Fatalf("signed fixtures: %v, %v", names, err)
	}
	sort.Strings(names)
	var first []json.RawMessage
	json.Unmarshal(fixture(t, "fixtures/batch/first.json"), &first)
	for _, name := range names {
		var f struct {
			Method  string            `json:"method"`
			Target  string            `json:"target"`
			Headers map[string]string `json:"headers"`
			Body    *string           `json:"body"`
			Expect  int               `json:"expect"`
			Note    string            `json:"note"`
		}
		if err := json.Unmarshal(fixture(t, name), &f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var body io.Reader
		if f.Body != nil {
			body = strings.NewReader(*f.Body)
		}
		req := httptest.NewRequest(f.Method, f.Target, body)
		for k, v := range f.Headers {
			req.Header.Set(k, v)
		}
		rec := serve(h, req)
		if rec.Code != f.Expect {
			t.Errorf("%s: %d, want %d (%s): %s", name, rec.Code, f.Expect, f.Note, rec.Body.String())
		}
		if rec.Code == http.StatusUnauthorized && rec.Body.String() != `{"error":"unauthorized"}` {
			t.Errorf("%s: a 401 with the body %q", name, rec.Body.String())
		}
		if rec.Code == http.StatusOK && strings.HasPrefix(f.Target, server.WellKnown) && rec.Header().Get(server.HeaderConfiguration) == "" {
			t.Errorf("%s: no configuration digest on the answer", name)
		}
		if rec.Code == http.StatusOK && strings.HasPrefix(f.Target, receiver.DefaultRunPath) && (rec.Header().Get(server.HeaderRunConfiguration) == "" || rec.Header().Get("ETag") != `"`+rec.Header().Get(server.HeaderRunConfiguration)+`"`) {
			t.Errorf("%s: the run configuration answer's headers %v", name, rec.Header())
		}
	}
	if store.Count() != len(first) {
		t.Errorf("the store holds %d events after the valid and the replayed batch of %d", store.Count(), len(first))
	}
}

// TestEveryFailureIsOneUnauthorized pins the failure rule: a malformed key is refused
// before any lookup, an unknown key, a header sent twice, a timestamp that is not an
// integer, one outside the window either way, and a signature that does not verify all
// get the same 401 and the same body; a signature under the key's previous secret
// verifies.
func TestEveryFailureIsOneUnauthorized(t *testing.T) {
	h, _, asked := handler(t, 1700000000)
	ok := signedGET(server.WellKnown, secret, "1700000000")
	if rec := serve(h, ok); rec.Code != 200 || rec.Header().Get(server.HeaderConfiguration) == "" {
		t.Fatalf("a valid GET: %d %v", rec.Code, rec.Header())
	}
	if rec := serve(h, signedGET(server.WellKnown, previous, "1700000100")); rec.Code != 200 {
		t.Errorf("the previous secret: %d", rec.Code)
	}
	*asked = 0
	for name, req := range map[string]*http.Request{
		"a malformed key": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Set(server.HeaderAccessKey, "ak_F1XT0RE000000000")
			return r
		}(),
		"an empty key": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Set(server.HeaderAccessKey, "")
			return r
		}(),
		"no key at all": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Del(server.HeaderAccessKey)
			return r
		}(),
	} {
		if rec := serve(h, req); rec.Code != 401 || rec.Body.String() != `{"error":"unauthorized"}` {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if *asked != 0 {
		t.Errorf("the lookup was asked %d times for keys without the shape", *asked)
	}
	for name, req := range map[string]*http.Request{
		"an unknown key": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Set(server.HeaderAccessKey, "ak_0000000000000000")
			return r
		}(),
		"a key sent twice": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Add(server.HeaderAccessKey, key)
			return r
		}(),
		"a timestamp sent twice": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Add(server.HeaderTimestamp, "1700000000")
			return r
		}(),
		"a timestamp that is not an integer": signedGET(server.WellKnown, secret, "1700000000.5"),
		"a signed timestamp":                 signedGET(server.WellKnown, secret, "+1700000000"),
		"a stale timestamp":                  signedGET(server.WellKnown, secret, "1699999699"),
		"a timestamp from the future":        signedGET(server.WellKnown, secret, "1700000301"),
		"a bad signature": func() *http.Request {
			r := signedGET(server.WellKnown, secret, "1700000000")
			r.Header.Set(server.HeaderSignature, server.SignGET("other-secret-of-16-chars", http.MethodGet, server.WellKnown, "1700000000"))
			return r
		}(),
		"a signature over another target": func() *http.Request {
			r := signedGET(server.WellKnown+"?x=1", secret, "1700000000")
			r.Header.Set(server.HeaderSignature, server.SignGET(secret, http.MethodGet, server.WellKnown, "1700000000"))
			return r
		}(),
		"a delivery under the wrong secret": signedPOST(receiver.DefaultEventsPath, "other-secret-of-16-chars", []byte("[]")),
		"a delivery with no signature": func() *http.Request {
			r := signedPOST(receiver.DefaultEventsPath, secret, []byte("[]"))
			r.Header.Del(server.HeaderSignature)
			return r
		}(),
	} {
		if rec := serve(h, req); rec.Code != 401 || rec.Body.String() != `{"error":"unauthorized"}` || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	for _, edge := range []string{"1699999700", "1700000300"} {
		if rec := serve(h, signedGET(server.WellKnown, secret, edge)); rec.Code != 200 {
			t.Errorf("a timestamp at the edge of the window, %s: %d", edge, rec.Code)
		}
	}
}

// TestDeliveriesAreStoredOnceAndAnsweredWithTheDigests pins the events endpoint: the
// wrong content type is 415, a duplicate is not stored twice, a reopened store still
// knows its ids, and the answer carries the configuration digest and, once the run's
// run.started named its labels, the run configuration digest for them.
func TestDeliveriesAreStoredOnceAndAnsweredWithTheDigests(t *testing.T) {
	h, store, _ := handler(t, 1700000000)
	e := event.NewEmitter(event.NewRunID(), nil)
	started, _ := e.Make(event.RunStarted, map[string]any{"runtime": "x", "labels": map[string]string{"forge": "none.example", "repository": "acme/shop"}}).JSON()
	body := []byte("[" + string(started) + "," + string(started) + "]")
	wrong := signedPOST(receiver.DefaultEventsPath, secret, body)
	wrong.Header.Set("Content-Type", "application/json")
	if rec := serve(h, wrong); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type: %d", rec.Code)
	}
	rec := serve(h, signedPOST(receiver.DefaultEventsPath, secret, body))
	if rec.Code != http.StatusAccepted || rec.Header().Get(server.HeaderConfiguration) == "" || rec.Header().Get(server.HeaderRunConfiguration) != "" {
		t.Errorf("a delivery for a forge with no run configuration: %d %v", rec.Code, rec.Header())
	}
	if store.Count() != 1 {
		t.Errorf("stored %d, want the duplicate dropped", store.Count())
	}
	other := event.NewEmitter(event.NewRunID(), nil)
	line, _ := other.Make(event.RunStarted, map[string]any{"runtime": "x", "labels": map[string]string{"forge": "github.com", "repository": "acme/shop"}}).JSON()
	rec = serve(h, signedPOST(receiver.DefaultEventsPath, secret, []byte("["+string(line)+"]")))
	run := fixture(t, "fixtures/run-configuration/enforce.json")
	if rec.Code != http.StatusAccepted || rec.Header().Get(server.HeaderRunConfiguration) != digestOf(run) {
		t.Errorf("a delivery for a forge with a run configuration: %d %v", rec.Code, rec.Header())
	}
	if rec := serve(h, signedGET(receiver.DefaultRunPath+"?forge=github.com&repository=acme%2Fshop", secret, "1700000000")); rec.Code != 200 || rec.Header().Get("ETag") != `"`+digestOf(run)+`"` || rec.Body.String() != string(run) {
		t.Errorf("the run configuration: %d %v", rec.Code, rec.Header())
	}
	if rec := serve(h, signedGET(receiver.DefaultRunPath+"?forge=none.example", secret, "1700000000")); rec.Code != 404 {
		t.Errorf("the run configuration of a forge without one: %d", rec.Code)
	}
	if rec := serve(h, signedGET("/elsewhere", secret, "1700000000")); rec.Code != 404 {
		t.Errorf("a path the receiver does not serve: %d", rec.Code)
	}
	if rec := serve(h, signedGET(receiver.DefaultEventsPath, secret, "1700000000")); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("a GET of the events endpoint: %d", rec.Code)
	}
	path := filepath.Join(t.TempDir(), "received.jsonl")
	f, _ := receiver.OpenFile(path)
	f.Append("one", []byte(`{"id":"one"}`))
	f.Close()
	reopened, err := receiver.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Seen("one") || reopened.Count() != 1 {
		t.Errorf("reopened store holds %d ids", reopened.Count())
	}
	fh, _ := os.Open(path)
	defer fh.Close()
	n := 0
	for s := bufio.NewScanner(fh); s.Scan(); {
		n++
	}
	if n != 1 {
		t.Errorf("%d lines in the file", n)
	}
}
