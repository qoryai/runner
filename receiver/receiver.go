// Package receiver is a server of the contract that is not the control plane: the
// receiving side this module's tests run the runner against, and a worked example of
// the contract's receiving rules, which any receiver may read and is tested against
// the same signed fixtures.
//
// A [Handler] serves the three endpoints of the contract: discovery at the well-known
// path, the events endpoint, and the run configuration, the last two where its fields
// say. It verifies every request the way the contract asks: the key's shape before any
// lookup, either of a key's secrets, the timestamp of a GET within the window, the
// signature in constant time over the raw body or the canonical string, and answers
// every failure alike, 401 with {"error":"unauthorized"} and nothing about the
// headers said or logged. A delivery it verified is deduplicated on each event's id,
// handed to a [Store], and answered 202 with the digests in force. [File] is a store
// that appends events to one JSON lines file and remembers the ids it holds.
package receiver

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qoryai/runner/internal/server"
)

// MaxBody is the largest delivery accepted.
const MaxBody = 16 << 20

// The default paths of the events endpoint and the run configuration.
const (
	DefaultEventsPath = "/v1/events"
	DefaultRunPath    = "/v1/run-configuration"
)

// keyShape is the access key's form: ak_ and 16 lowercase Crockford base32 characters.
var keyShape = regexp.MustCompile(`^ak_[0-9a-hjkmnp-tv-z]{16}$`)

// timestampShape is a decimal integer, and nothing else.
var timestampShape = regexp.MustCompile(`^[0-9]{1,19}$`)

// Store keeps what a receiver accepted.
type Store interface {
	// Seen reports whether an event id was stored before.
	Seen(id string) bool
	// Append stores one event, given as its JSON line.
	Append(id string, line []byte) error
}

// Handler is the receiving endpoint.
type Handler struct {
	// Keys looks an access key up, once its shape is checked: the secrets that
	// verify for it, two after a rotation, and whether the key is known and not
	// revoked. Nil knows no key.
	Keys func(accessKey string) (secrets []string, ok bool)
	// Store keeps the events accepted.
	Store Store
	// Now is the receiver's clock; nil means the wall clock.
	Now func() time.Time
	// Window is how far a GET's timestamp may be from Now, either way; zero means
	// the contract's 300 seconds.
	Window time.Duration
	// Configuration answers discovery with the configuration document and the
	// receiver's digest of it. Nil means discovery is not served.
	Configuration func() (document []byte, digest string)
	// RunConfiguration answers a run configuration for a forge and a repository, each
	// empty when the runner has no such label, with the receiver's digest of it; ok
	// false means none for them, a 404. Nil means the run configuration is not served.
	RunConfiguration func(forge, repository string) (document []byte, digest string, ok bool)
	// EventsPath and RunPath are where the events endpoint and the run configuration
	// are served; empty means DefaultEventsPath and DefaultRunPath.
	EventsPath, RunPath string
	// Log receives one line per delivery, and may be nil. It never sees a header.
	Log func(string)
	// Stop, when set, is asked per run id whether the receiver wants nothing more; a
	// true answer is a 410. Nil means never.
	Stop func(runID string) bool

	// labels are the forge and repository of each run whose run.started passed, so an
	// answer to a later delivery says which run configuration is in force for it.
	mu     sync.Mutex
	labels map[string][2]string
}

// ServeHTTP routes one request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	events, run := h.EventsPath, h.RunPath
	if events == "" {
		events = DefaultEventsPath
	}
	if run == "" {
		run = DefaultRunPath
	}
	switch r.URL.Path {
	case server.WellKnown:
		if h.Configuration == nil {
			http.NotFound(w, r)
			return
		}
		if !allow(w, r, http.MethodGet) {
			return
		}
		if !h.verifyGET(r) {
			unauthorized(w)
			return
		}
		doc, digest := h.Configuration()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(server.HeaderConfiguration, digest)
		w.Write(doc)
	case events:
		if !allow(w, r, http.MethodPost) {
			return
		}
		h.deliver(w, r)
	case run:
		if h.RunConfiguration == nil {
			http.NotFound(w, r)
			return
		}
		if !allow(w, r, http.MethodGet) {
			return
		}
		if !h.verifyGET(r) {
			unauthorized(w)
			return
		}
		q := r.URL.Query()
		doc, digest, ok := h.RunConfiguration(q.Get("forge"), q.Get("repository"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(server.HeaderRunConfiguration, digest)
		w.Header().Set("ETag", strconv.Quote(digest))
		w.Write(doc)
	default:
		http.NotFound(w, r)
	}
}

// allow answers 405 unless the request has the method, and says whether it does.
func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, method+" is the method here", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// unauthorized answers every authentication failure alike.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, `{"error":"unauthorized"}`)
}

// secrets are the secrets of the request's key: none when a header is sent twice, the
// key is not shaped as one, or the lookup does not know it. The lookup is asked only
// for a key that has the shape.
func (h *Handler) secrets(r *http.Request) ([]string, bool) {
	for _, name := range []string{server.HeaderAccessKey, server.HeaderSignature, server.HeaderTimestamp} {
		if len(r.Header.Values(name)) > 1 {
			return nil, false
		}
	}
	key := r.Header.Get(server.HeaderAccessKey)
	if !keyShape.MatchString(key) || h.Keys == nil {
		return nil, false
	}
	return h.Keys(key)
}

// verifyPOST reports whether the delivery's signature over body verifies under one of
// the key's secrets.
func (h *Handler) verifyPOST(r *http.Request, body []byte) bool {
	secrets, ok := h.secrets(r)
	if !ok {
		return false
	}
	header := r.Header.Get(server.HeaderSignature)
	for _, s := range secrets {
		if server.Verify(s, body, header) {
			return true
		}
	}
	return false
}

// verifyGET reports whether the GET's timestamp is within the window and its signature
// over the canonical string verifies under one of the key's secrets. The target is the
// request-target as sent.
func (h *Handler) verifyGET(r *http.Request) bool {
	secrets, ok := h.secrets(r)
	if !ok {
		return false
	}
	ts := r.Header.Get(server.HeaderTimestamp)
	if !timestampShape.MatchString(ts) {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	window := h.Window
	if window == 0 {
		window = server.Window
	}
	if d := now().Unix() - n; d > int64(window/time.Second) || -d > int64(window/time.Second) {
		return false
	}
	target := r.RequestURI
	if target == "" {
		target = r.URL.RequestURI()
	}
	header := r.Header.Get(server.HeaderSignature)
	for _, s := range secrets {
		if server.VerifyGET(s, r.Method, target, ts, header) {
			return true
		}
	}
	return false
}

// deliver answers one delivery.
func (h *Handler) deliver(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		http.Error(w, "body unreadable or over 16 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	if !h.verifyPOST(r, body) {
		unauthorized(w)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), server.ContentType) {
		http.Error(w, "content type is not "+server.ContentType, http.StatusUnsupportedMediaType)
		return
	}
	var batch []json.RawMessage
	if err := json.Unmarshal(body, &batch); err != nil || len(batch) == 0 {
		http.Error(w, "body is not a non-empty batch", http.StatusBadRequest)
		return
	}
	stored, dup := 0, 0
	subject := ""
	for _, raw := range batch {
		var head struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Type    string `json:"type"`
			Data    struct {
				Labels map[string]string `json:"labels"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.ID == "" || head.Subject == "" || head.Type == "" {
			http.Error(w, "an event lacks id, subject or type", http.StatusBadRequest)
			return
		}
		subject = head.Subject
		if h.Stop != nil && h.Stop(head.Subject) {
			h.digests(w, subject)
			w.WriteHeader(http.StatusGone)
			return
		}
		if head.Type == "ai.qory.run.started" {
			h.mu.Lock()
			if h.labels == nil {
				h.labels = map[string][2]string{}
			}
			h.labels[head.Subject] = [2]string{head.Data.Labels["forge"], head.Data.Labels["repository"]}
			h.mu.Unlock()
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
		h.Log("delivery for run " + subject + ": " + itoa(stored) + " stored, " + itoa(dup) + " duplicates")
	}
	h.digests(w, subject)
	w.WriteHeader(http.StatusAccepted)
}

// digests sets on an answer the digests in force: the configuration's, and the run
// configuration's for the run's forge and repository as its run.started said.
func (h *Handler) digests(w http.ResponseWriter, runID string) {
	if h.Configuration != nil {
		_, digest := h.Configuration()
		w.Header().Set(server.HeaderConfiguration, digest)
	}
	if h.RunConfiguration != nil {
		h.mu.Lock()
		labels := h.labels[runID]
		h.mu.Unlock()
		if _, digest, ok := h.RunConfiguration(labels[0], labels[1]); ok {
			w.Header().Set(server.HeaderRunConfiguration, digest)
		}
	}
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
