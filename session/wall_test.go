package session_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qoryai/runner/session"
	"github.com/qoryai/runner/wall"
)

// openWall is a wall with nothing in it: it records what the session runner tells it
// and starts the launch as it is, pointed at the proxy and the socket by their own
// addresses.
type openWall struct {
	req     wall.Request
	got     wall.Launch
	wrapped bool
	closed  int
}

func (w *openWall) Name() string { return "open" }
func (w *openWall) Prepare(_ context.Context, req wall.Request) (wall.Enclosure, error) {
	w.req = req
	return w, nil
}
func (w *openWall) ProxyAddr() string { return "127.0.0.1:0" }
func (w *openWall) Wrap(_ context.Context, l wall.Launch) (wall.Launch, error) {
	w.got, w.wrapped = l, true
	env := append(append([]string(nil), l.Env...), "HTTP_PROXY=http://"+l.Proxy, session.EnvSocket+"="+l.Socket)
	return wall.Launch{Command: l.Command, Args: l.Args, Env: env, Dir: l.Dir}, nil
}
func (w *openWall) Close(context.Context) error { w.closed++; return nil }

// TestWallWrapsTheLaunch pins what crosses to a wall and what does not: the enclosure
// is asked where the proxy listens and told where it does, it gets the socket, the
// settings the runner wrote and the run's environment without the proxy variables, a
// nil environment is nothing and not the process's own, the record names the wall and
// the image, and the enclosure is closed once.
func TestWallWrapsTheLaunch(t *testing.T) {
	t.Setenv("QORY_TEST_HOST_ONLY", "1")
	w := &openWall{}
	sp := spec(t, nil, "FAKE_EXIT=0")
	sp.Wall, sp.Image, sp.Mounts = w, "example.com/agent:1", []wall.Mount{{Path: sp.Dir}}
	res, err := runWithSettingsEnv(t, sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !w.wrapped || w.closed != 1 {
		t.Errorf("exit %d, wrapped %v, closed %d times", res.ExitCode, w.wrapped, w.closed)
	}
	if w.req.RunID != res.RunID || w.req.Image != "example.com/agent:1" {
		t.Errorf("request %+v", w.req)
	}
	if !strings.HasPrefix(w.got.Proxy, "127.0.0.1:") || strings.HasSuffix(w.got.Proxy, ":0") || w.got.Socket == "" {
		t.Errorf("proxy %q socket %q", w.got.Proxy, w.got.Socket)
	}
	if want := []wall.Mount{{Path: sp.Dir}, {Path: res.Dir, ReadOnly: true}}; !slices.Equal(w.got.Mounts, want) {
		t.Errorf("mounts %v, want %v", w.got.Mounts, want)
	}
	env := strings.Join(w.got.Env, "\n")
	if strings.Contains(env, "PROXY") || strings.Contains(env, session.EnvSocket) || !strings.Contains(env, session.EnvRunID+"="+res.RunID) {
		t.Errorf("the enclosure's environment:\n%s", env)
	}
	evs := events(t, res)
	if d := data(evs[0]); d["wall"] != "open" || d["image"] != "example.com/agent:1" {
		t.Errorf("run.started %v", d)
	}
	if len(ofType(evs, "ai.qory.session.ended")) != 1 {
		t.Errorf("the hook did not cross: %v", types(evs))
	}

	w = &openWall{}
	sp = spec(t, nil)
	sp.Wall, sp.Image, sp.Env = w, "i", nil
	sp.Command, sp.Args = "/bin/sh", []string{"-c", `test -z "$QORY_TEST_HOST_ONLY"`}
	if res, err := session.Run(context.Background(), sp); err != nil || res.ExitCode != 0 {
		t.Errorf("a nil environment under a wall carried the process's own: %v %+v", err, res)
	}

	sp = spec(t, nil)
	sp.Wall, sp.ProxyBind = &openWall{}, "127.0.0.1:0"
	if _, err := session.Run(context.Background(), sp); err == nil {
		t.Error("a wall and a proxy address were both accepted")
	}
}

// TestEventsWriterGetsTheRecord pins that the writer gets the lines events.jsonl
// holds, local or not.
func TestEventsWriterGetsTheRecord(t *testing.T) {
	var lines bytes.Buffer
	sp := spec(t, nil, "FAKE_EXIT=0")
	sp.Events, sp.Local = &lines, true
	res, err := session.Run(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := os.ReadFile(filepath.Join(res.Dir, "events.jsonl"))
	if lines.String() != string(file) || lines.Len() == 0 {
		t.Errorf("the writer got %d bytes, the file holds %d", lines.Len(), len(file))
	}
}

// TestProxyBindIsWhereTheProxyListens pins the bind address: the session is pointed at
// the address given, and one that cannot be bound means no run.
func TestProxyBindIsWhereTheProxyListens(t *testing.T) {
	sp := spec(t, nil)
	sp.ProxyBind = "127.0.0.1:0"
	sp.Command, sp.Args = "/bin/sh", []string{"-c", `case "$HTTP_PROXY" in http://127.0.0.1:*) exit 0;; esac; exit 9`}
	if res, err := session.Run(context.Background(), sp); err != nil || res.ExitCode != 0 {
		t.Errorf("%v %+v", err, res)
	}
	sp = spec(t, nil)
	sp.ProxyBind = "192.0.2.1:0"
	if _, err := session.Run(context.Background(), sp); err == nil {
		t.Error("the run started with a proxy address this machine does not hold")
	}
}
