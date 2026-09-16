package sink_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qoryai/runner/internal/event"
	"github.com/qoryai/runner/internal/receiver"
	"github.com/qoryai/runner/internal/sink"
	"github.com/qoryai/runner/internal/webhook"
)

// TestFileSinkWritesBothRecords pins events.jsonl as one line per event and
// output.log as the log chunks' bytes, and that a run directory is never reused.
func TestFileSinkWritesBothRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	f, err := sink.NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := event.NewEmitter(event.NewRunID(), nil)
	f.Write(e.Make(event.RunStarted, map[string]any{"runtime": "x"}))
	f.Write(e.Make(event.RunLog, map[string]any{"stream": "terminal", "bytes": base64.StdEncoding.EncodeToString([]byte("hi\n"))}))
	f.Write(e.Make(event.RunLog, map[string]any{"stream": "terminal", "bytes": base64.StdEncoding.EncodeToString([]byte("there"))}))
	if err := f.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := os.ReadFile(filepath.Join(dir, sink.EventsFile))
	if n := strings.Count(string(events), "\n"); n != 3 || !strings.Contains(string(events), `"sequence":"0000000002"`) {
		t.Errorf("events.jsonl:\n%s", events)
	}
	out, _ := os.ReadFile(filepath.Join(dir, sink.OutputFile))
	if string(out) != "hi\nthere" {
		t.Errorf("output.log %q", out)
	}
	if _, err := sink.NewFile(dir); err == nil {
		t.Error("a run directory was reused")
	}
}

// station is a receiver in front of a store, with a failure mode the test flips.
type station struct {
	srv   *httptest.Server
	store *receiver.File
	fail  atomic.Int32
	hits  atomic.Int32
}

func newStation(t *testing.T, stop func(string) bool) *station {
	t.Helper()
	store, err := receiver.OpenFile(filepath.Join(t.TempDir(), "received.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := &station{store: store}
	h := &receiver.Handler{Secret: "fixture-secret-not-a-real-one", Store: store, Stop: stop}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if code := s.fail.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() { store.Close() })
	return s
}

func client(s *station, events ...string) *webhook.Client {
	return &webhook.Client{Config: &webhook.Config{URL: s.srv.URL, Secret: "fixture-secret-not-a-real-one", Events: events}, UserAgent: "qory-runner/test"}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWebhookDeliversBatchesTheReceiverStores pins the sink against the reference
// receiver: events arrive signed, in batches, and are stored once each; a filtered type
// is never sent; a batch cut by count is one delivery.
func TestWebhookDeliversBatchesTheReceiverStores(t *testing.T) {
	s := newStation(t, nil)
	w := sink.NewWebhook(client(s, "ai.qory.run.started", "ai.qory.run.heartbeat"), t.TempDir(), nil)
	e := event.NewEmitter(event.NewRunID(), nil)
	w.Write(e.Make(event.RunStarted, map[string]any{"runtime": "x"}))
	w.Write(e.Make(event.RunLog, map[string]any{"stream": "terminal", "bytes": ""}))
	for i := 0; i < sink.BatchEvents+5; i++ {
		w.Write(e.Make(event.RunHeartbeat, map[string]any{"elapsed_seconds": i, "interval_seconds": 30}))
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.store.Count(); got != sink.BatchEvents+6 {
		t.Errorf("stored %d events", got)
	}
	if hits := s.hits.Load(); hits < 2 || hits > 3 {
		t.Errorf("%d deliveries for %d events", hits, sink.BatchEvents+6)
	}
	if w.Undelivered() != 0 {
		t.Errorf("%d undelivered", w.Undelivered())
	}
}

// TestWebhookRetriesUntilAcceptedAndSpoolsTheRest pins the retry and the spool: a
// receiver that fails then recovers gets the batch once it recovers, under the same
// delivery id; a receiver that stays down leaves the batch in undelivered/ with the
// count reported at close.
func TestWebhookRetriesUntilAcceptedAndSpoolsTheRest(t *testing.T) {
	s := newStation(t, nil)
	s.fail.Store(500)
	var notes []string
	dir := t.TempDir()
	w := sink.NewWebhook(client(s), dir, func(l string) { notes = append(notes, l) })
	e := event.NewEmitter(event.NewRunID(), nil)
	w.Write(e.Make(event.RunStarted, map[string]any{"runtime": "x"}))
	waitFor(t, func() bool { return s.hits.Load() >= 1 })
	s.fail.Store(0)
	waitFor(t, func() bool { return s.store.Count() == 1 })

	s.fail.Store(503)
	w.Write(e.Make(event.RunExited, map[string]any{"state": "failed"}))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Undelivered() != 1 {
		t.Errorf("%d undelivered", w.Undelivered())
	}
	files, _ := filepath.Glob(filepath.Join(dir, sink.UndeliveredDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("spooled files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), `"ai.qory.run.exited"`) || !strings.HasPrefix(string(b), "[") {
		t.Errorf("spooled batch %s", b)
	}
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "1 events were not accepted") {
		t.Errorf("notes %v", notes)
	}
}

// TestReceiverStopEndsDeliveries pins 410: the sink sends nothing more for the run and
// drops what follows without spooling it.
func TestReceiverStopEndsDeliveries(t *testing.T) {
	s := newStation(t, func(string) bool { return true })
	w := sink.NewWebhook(client(s), t.TempDir(), nil)
	e := event.NewEmitter(event.NewRunID(), nil)
	w.Write(e.Make(event.RunStarted, map[string]any{"runtime": "x"}))
	waitFor(t, w.Stopped)
	w.Write(e.Make(event.RunExited, map[string]any{"state": "failed"}))
	w.Close(context.Background())
	if s.hits.Load() != 1 || w.Undelivered() != 0 {
		t.Errorf("%d deliveries, %d undelivered after stop", s.hits.Load(), w.Undelivered())
	}
}
