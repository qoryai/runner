package socket_test

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qoryai/runner/internal/descriptor"
	"github.com/qoryai/runner/internal/socket"
)

// TestForwardedHookInputArrivesAsAHooksRecord pins the forwarder and the listener
// together: what a hook command reads on stdin arrives as one record of the hooks
// source, unchanged inside.
func TestForwardedHookInputArrivesAsAHooksRecord(t *testing.T) {
	l, err := socket.Listen()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []descriptor.Record
	var bad []error
	done := make(chan struct{})
	go func() {
		l.Serve(func(r descriptor.Record) { mu.Lock(); got = append(got, r); mu.Unlock() }, func(err error) { mu.Lock(); bad = append(bad, err); mu.Unlock() })
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := socket.Forward(ctx, l.Path(), strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash"}`)); err != nil {
		t.Fatal(err)
	}
	if err := socket.Forward(ctx, l.Path(), strings.NewReader(`[1,2]`)); err == nil {
		t.Error("a non-object was forwarded")
	}
	conn, err := net.Dial("unix", l.Path())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("not json\n" + `{"source":"transcript","record":{}}` + "\n" + `{"source":"output","record":{"type":"result"}}` + "\n"))
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n, nb := len(got), len(bad)
		mu.Unlock()
		if n == 2 && nb == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Source != "hooks" || got[0].Record["tool_name"] != "Bash" || got[1].Source != "output" {
		t.Errorf("records, in connection order: %+v", got)
	}
	if len(bad) != 2 {
		t.Errorf("bad lines: %v", bad)
	}
	dir := l.Path()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the socket was not removed")
	}
}
