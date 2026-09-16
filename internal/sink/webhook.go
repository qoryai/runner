package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/qoryai/runner/internal/event"
	"github.com/qoryai/runner/internal/webhook"
)

// The batching rule: a batch is cut at BatchEvents events, at BatchBytes of encoded
// events, or BatchWait after its first event, whichever comes first.
const (
	BatchEvents = 100
	BatchBytes  = 1 << 20
	BatchWait   = time.Second
)

// The retry rule: a delivery the receiver did not accept is retried after Backoff,
// doubling to MaxBackoff, until the run ends.
const (
	Backoff    = time.Second
	MaxBackoff = time.Minute
)

// QueueSize is how many events the sink holds before it spools instead of blocking.
const QueueSize = 10000

// UndeliveredDir is the directory under the run directory that holds the batches the
// receiver did not accept, one file per delivery id.
const UndeliveredDir = "undelivered"

// Webhook posts a run's events to one receiver.
type Webhook struct {
	client *webhook.Client
	dir    string
	report func(string)
	sleep  func(context.Context, time.Duration) bool

	queue   chan []byte
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	stopped bool
	closed  bool
	lost    int
}

// NewWebhook returns a sink posting to the client's webhook. spool is the run
// directory, under which undelivered batches are written; report receives one line per
// thing worth telling the user, a stop or a spool, and may be nil. The worker runs
// until Close.
func NewWebhook(client *webhook.Client, spool string, report func(string)) *Webhook {
	if report == nil {
		report = func(string) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Webhook{client: client, dir: spool, report: report, queue: make(chan []byte, QueueSize), ctx: ctx, cancel: cancel, done: make(chan struct{}), sleep: sleep}
	go w.run()
	return w
}

// Write queues the event when the configuration wants its type. A full queue spools
// the event as a batch of one rather than blocking; a sink told to stop drops it.
func (w *Webhook) Write(ev *event.Event) error {
	if !w.client.Config.Wants(ev.Type) {
		return nil
	}
	line, err := ev.JSON()
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.closed {
		return nil
	}
	select {
	case w.queue <- line:
	default:
		w.spool([][]byte{line}, event.NewID())
	}
	return nil
}

// run is the worker: it forms batches from the queue and delivers each with retries.
func (w *Webhook) run() {
	defer close(w.done)
	for {
		batch, ok := w.next()
		if len(batch) > 0 {
			w.deliver(batch)
		}
		if !ok {
			return
		}
	}
}

// next collects one batch. It returns ok false once the queue is closed and drained.
func (w *Webhook) next() ([][]byte, bool) {
	var batch [][]byte
	size := 0
	var timer <-chan time.Time
	for {
		select {
		case line, ok := <-w.queue:
			if !ok {
				return batch, false
			}
			batch = append(batch, line)
			size += len(line)
			if len(batch) >= BatchEvents || size >= BatchBytes {
				return batch, true
			}
			if timer == nil {
				timer = time.After(BatchWait)
			}
		case <-timer:
			return batch, true
		}
	}
}

// deliver posts one batch until it is accepted, the receiver says stop, or the sink's
// context ends; then it spools what was not accepted.
func (w *Webhook) deliver(batch [][]byte) {
	body := encode(batch)
	id := event.NewID()
	backoff := Backoff
	for {
		ctx, cancel := context.WithTimeout(w.ctx, webhook.Timeout)
		status, err := w.client.Deliver(ctx, id, body)
		cancel()
		switch {
		case err == nil && webhook.Accepted(status):
			return
		case err == nil && webhook.Stop(status):
			w.mu.Lock()
			w.stopped = true
			w.mu.Unlock()
			w.report(fmt.Sprintf("the receiver answered %d; no further batch is sent for this run", status))
			return
		}
		if w.ctx.Err() != nil || !w.sleep(w.ctx, backoff) {
			w.mu.Lock()
			w.spool(batch, id)
			w.mu.Unlock()
			return
		}
		if backoff *= 2; backoff > MaxBackoff {
			backoff = MaxBackoff
		}
	}
}

// spool writes a batch the receiver did not accept under the undelivered directory,
// named by its delivery id, and counts its events. Called with the lock held.
func (w *Webhook) spool(batch [][]byte, id string) {
	w.lost += len(batch)
	dir := filepath.Join(w.dir, UndeliveredDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.report("spool: " + err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), encode(batch), 0o644); err != nil {
		w.report("spool: " + err.Error())
	}
}

// encode renders a batch as the JSON array of its events.
func encode(batch [][]byte) []byte {
	raw := make([]json.RawMessage, len(batch))
	for i, b := range batch {
		raw[i] = b
	}
	body, _ := json.Marshal(raw)
	return body
}

// Close stops taking events and lets the worker deliver what is queued until the
// context ends; what is still undelivered then is spooled. It reports the count and
// returns nil: an undelivered copy is not a failed run.
func (w *Webhook) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-ctx.Done():
		w.cancel()
		<-w.done
	}
	w.cancel()
	if n := w.Undelivered(); n > 0 {
		w.report(fmt.Sprintf("%d events were not accepted by the webhook; see %s", n, filepath.Join(w.dir, UndeliveredDir)))
	}
	return nil
}

// Undelivered is the number of events spooled so far.
func (w *Webhook) Undelivered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lost
}

// Stopped reports whether the receiver asked for nothing more.
func (w *Webhook) Stopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

// sleep waits d or until the context ends, reporting whether it waited the whole d.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
