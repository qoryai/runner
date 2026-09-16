// Package receiver is the receiving side of the webhook that this module's tests run
// the webhook sink against, and a worked example of the contract's receiving rules.
//
// A [Handler] answers deliveries the way the contract asks a receiver to: it verifies
// the signature over the raw body in constant time before parsing, refuses what does
// not verify, deduplicates on each event's id, hands every new event to a [Store], and
// answers 202. [File] is a store that appends events to one JSON lines file and
// remembers the ids it holds.
package receiver

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/qoryai/runner/internal/webhook"
)

// MaxBody is the largest delivery accepted.
const MaxBody = 16 << 20

// Store keeps what a receiver accepted.
type Store interface {
	// Seen reports whether an event id was stored before.
	Seen(id string) bool
	// Append stores one event, given as its JSON line.
	Append(id string, line []byte) error
}

// Handler is the receiving endpoint.
type Handler struct {
	Secret string
	Store  Store
	// Log receives one line per delivery, and may be nil.
	Log func(string)
	// Stop, when set, is asked per run id whether the receiver wants nothing more; a
	// true answer is a 410. Nil means never.
	Stop func(runID string) bool
}

// ServeHTTP answers one delivery.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST a batch", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		http.Error(w, "body unreadable or over 16 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	if !webhook.Verify(h.Secret, body, r.Header.Get(webhook.HeaderSignature)) {
		http.Error(w, "signature does not verify", http.StatusUnauthorized)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), webhook.ContentType) {
		http.Error(w, "content type is not "+webhook.ContentType, http.StatusUnsupportedMediaType)
		return
	}
	var batch []json.RawMessage
	if err := json.Unmarshal(body, &batch); err != nil || len(batch) == 0 {
		http.Error(w, "body is not a non-empty batch", http.StatusBadRequest)
		return
	}
	stored, dup := 0, 0
	for _, raw := range batch {
		var head struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.ID == "" || head.Subject == "" || head.Type == "" {
			http.Error(w, "an event lacks id, subject or type", http.StatusBadRequest)
			return
		}
		if h.Stop != nil && h.Stop(head.Subject) {
			w.WriteHeader(http.StatusGone)
			return
		}
		if h.Store.Seen(head.ID) {
			dup++
			continue
		}
		if err := h.Store.Append(head.ID, raw); err != nil {
			http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		stored++
	}
	if h.Log != nil {
		h.Log("delivery " + r.Header.Get(webhook.HeaderDelivery) + ": " + itoa(stored) + " stored, " + itoa(dup) + " duplicates")
	}
	w.WriteHeader(http.StatusAccepted)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// File is a store appending to one JSON lines file.
type File struct {
	mu   sync.Mutex
	f    *os.File
	seen map[string]bool
}

// OpenFile opens or creates the file and reads the ids it already holds, so a
// restarted receiver still deduplicates.
func OpenFile(path string) (*File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	s := bufio.NewScanner(f)
	s.Buffer(nil, MaxBody)
	for s.Scan() {
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(s.Bytes(), &head) == nil && head.ID != "" {
			seen[head.ID] = true
		}
	}
	if err := s.Err(); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	return &File{f: f, seen: seen}, nil
}

// Seen reports whether the id was stored.
func (s *File) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[id]
}

// Append writes the line and remembers the id.
func (s *File) Append(id string, line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		return err
	}
	s.seen[id] = true
	return nil
}

// Count is how many events the store holds.
func (s *File) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// Close syncs and closes the file.
func (s *File) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.f.Sync(), s.f.Close())
}
