package wall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a buffer the relay writes and the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRelayForwardsToItsOneTarget pins the relay: it says when it listens, what
// connects to its port reaches the target fixed at the start and nothing the client
// names, and it stops with its context.
func TestRelayForwardsToItsOneTarget(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "the target") }))
	defer origin.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var ready syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- Relay(ctx, []string{fmt.Sprintf("%d=%s", port, strings.TrimPrefix(origin.URL, "http://"))}, &ready)
	}()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(ready.String(), RelayReady); {
		if time.Now().After(deadline) {
			t.Fatal("the relay never said it listens")
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "the target" {
		t.Errorf("through the relay: %q", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay outlived its context")
	}
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		t.Error("the relay's port still listens")
	}
}

// TestRelayRefusesWhatIsNotAForward pins the argument's grammar.
func TestRelayRefusesWhatIsNotAForward(t *testing.T) {
	for _, f := range [][]string{nil, {"3128"}, {"x=host:1"}, {"0=host:1"}, {"3128=host"}, {"70000=host:1"}} {
		if err := Relay(context.Background(), f, io.Discard); err == nil {
			t.Errorf("%v was accepted", f)
		}
	}
}

// TestRelayOpensEveryConnectionWithTheRunsToken pins the relay's half of the proxy's
// gate: with the token in its environment, what it forwards starts with the preamble.
func TestRelayOpensEveryConnectionWithTheRunsToken(t *testing.T) {
	t.Setenv(RelayTokenEnv, "the-runs-token")
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	got := make(chan string, 1)
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- string(b)
	}()
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := &syncBuffer{}
	go Relay(ctx, []string{fmt.Sprintf("%d=%s", port, target.Addr())}, ready)
	for range 200 {
		if strings.Contains(ready.String(), RelayReady) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "hello")
	c.(*net.TCPConn).CloseWrite()
	select {
	case s := <-got:
		if s != "QORY-RELAY the-runs-token\nhello" {
			t.Errorf("the target read %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Error("the target read nothing")
	}
	c.Close()
}
