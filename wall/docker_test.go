package wall

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// recorder is a machine with no Docker: it records every command and answers the two
// the adapter reads.
type recorder struct {
	t       *testing.T
	gateway string
	local_  bool
	uid     int
	lines   []string
	fail    string
}

func (r *recorder) run(_ context.Context, argv []string) ([]byte, error) {
	line := words(argv)
	r.lines = append(r.lines, line)
	switch {
	case r.fail != "" && strings.Contains(line, r.fail):
		return []byte("Error response from daemon: told to fail"), errors.New("exit status 1")
	case strings.Contains(line, "network inspect"):
		return []byte(r.gateway + " \n"), nil
	case strings.Contains(line, " inspect --format {{.State"):
		return []byte("true\n"), nil
	case strings.Contains(line, " logs "):
		return []byte(RelayReady + "\n"), nil
	}
	return nil, nil
}
func (r *recorder) local(string) bool        { return r.local_ }
func (r *recorder) tempDir() (string, error) { return r.t.TempDir(), nil }
func (r *recorder) ids() (int, int)          { return r.uid, 1000 }
func (r *recorder) checkHelper(string) error { return nil }

const runID = "0191f2a4-3c5e-7b8d-9e0f-1a2b3c4d5e6f"

func launch() Launch {
	return Launch{
		Command: "claude", Args: []string{"--settings", "/work/.qory/runs/" + runID + "/settings.json", "-p", "say hi"},
		Env: []string{"ANTHROPIC_API_KEY=not-a-real-key", "QORY_RUN_ID=" + runID}, Dir: "/work",
		Proxy: "127.0.0.1:50123", Socket: "/tmp/qory-run-1/sock", Mounts: []Mount{{Path: "/work"}, {Path: "/home/dev/.qory/homes/work", ReadOnly: true}, {Path: "/work/.qory/runs/" + runID, ReadOnly: true}},
	}
}

// words writes a command the way a shell would read it back.
func words(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if out[i] = a; strings.ContainsAny(a, " {") {
			out[i] = "'" + a + "'"
		}
	}
	return strings.Join(out, " ")
}

// golden compares got with the file, or rewrites the file under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs; run go test ./wall -update and read the diff\ngot:\n%s\nwant:\n%s", file, got, want)
	}
}

// TestDockerCommandLines pins every command the adapter runs, in order, from Prepare to
// Close, the command it hands back and the environment file, on the two kinds of
// machine: a Linux host that holds the network's gateway address, where the proxy binds
// it, and an engine in a virtual machine, where the proxy stays on loopback and the
// relay reaches it by name.
func TestDockerCommandLines(t *testing.T) {
	for _, c := range []struct {
		name        string
		local       bool
		interactive bool
		proxyAddr   string
	}{
		{"linux-host", true, false, "172.30.0.1:0"},
		{"engine-in-vm", false, true, "127.0.0.1:0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{t: t, gateway: "172.30.0.1", local_: c.local, uid: 1000}
			d := &Docker{Helper: "/opt/qory/qory-linux", RelayArgs: []string{"run", "relay"}, sys: rec}
			e, err := d.Prepare(context.Background(), Request{RunID: runID, Image: "example.com/agent:1"})
			if err != nil {
				t.Fatal(err)
			}
			if e.ProxyAddr() != c.proxyAddr {
				t.Errorf("the proxy is told to listen on %s, want %s", e.ProxyAddr(), c.proxyAddr)
			}
			l := launch()
			l.Interactive = c.interactive
			if c.local {
				l.Proxy = "172.30.0.1:50123"
			}
			wrapped, err := e.Wrap(context.Background(), l)
			if err != nil {
				t.Fatal(err)
			}
			if wrapped.Env != nil {
				t.Errorf("the docker command gets an environment of its own: %v", wrapped.Env)
			}
			envFile := ""
			for i, a := range wrapped.Args {
				if a == "--env-file" {
					envFile = wrapped.Args[i+1]
					wrapped.Args[i+1] = "ENVFILE"
				}
			}
			env, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			if info, _ := os.Stat(envFile); info.Mode().Perm() != 0o600 {
				t.Errorf("the environment file's mode is %v", info.Mode().Perm())
			}
			if err := e.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(envFile); err == nil {
				t.Error("Close left the environment file")
			}
			got := strings.Join(rec.lines, "\n") + "\n\nwrapped:\n" + words(append([]string{wrapped.Command}, wrapped.Args...)) + "\n\nenvironment:\n" + string(env)
			if strings.Contains(strings.Join(rec.lines, "\n")+strings.Join(wrapped.Args, " "), "not-a-real-key") {
				t.Error("a value of the environment is on a command line")
			}
			golden(t, c.name, got)
		})
	}
}

// TestDockerRefuses pins what stops a wall before anything is created.
func TestDockerRefuses(t *testing.T) {
	ok := func() *Docker {
		return &Docker{Helper: "/h", RelayArgs: []string{"relay"}, sys: &recorder{t: t, uid: 1000}}
	}
	req := Request{RunID: runID, Image: "i"}
	for name, c := range map[string]struct {
		d   *Docker
		req Request
	}{
		"no image":             {ok(), Request{RunID: runID}},
		"no helper":            {&Docker{RelayArgs: []string{"relay"}, sys: &recorder{t: t, uid: 1000}}, req},
		"root":                 {&Docker{Helper: "/h", RelayArgs: []string{"relay"}, sys: &recorder{t: t, uid: 0}}, req},
		"root by name":         {&Docker{Helper: "/h", RelayArgs: []string{"relay"}, User: "root", sys: &recorder{t: t, uid: 1000}}, req},
		"not an ELF":           {&Docker{Helper: "docker_test.go", RelayArgs: []string{"relay"}}, req},
		"missing file":         {&Docker{Helper: "testdata/none", RelayArgs: []string{"relay"}}, req},
		"no relay args":        {&Docker{Helper: "/h", sys: &recorder{t: t, uid: 1000}}, req},
		"root as 00":           {&Docker{Helper: "/h", RelayArgs: []string{"relay"}, User: "00:0", sys: &recorder{t: t, uid: 1000}}, req},
		"a flag as the user":   {&Docker{Helper: "/h", RelayArgs: []string{"relay"}, User: "--privileged", sys: &recorder{t: t, uid: 1000}}, req},
		"a flag as the image":  {ok(), Request{RunID: runID, Image: "--privileged"}},
		"a flag as the run id": {ok(), Request{RunID: "-x", Image: "i"}},
	} {
		if _, err := c.d.Prepare(context.Background(), c.req); err == nil {
			t.Errorf("%s: a wall was prepared", name)
		} else if rec, ok := c.d.sys.(*recorder); ok && len(rec.lines) > 0 {
			t.Errorf("%s: commands ran before the refusal: %v", name, rec.lines)
		}
	}
}

// TestDockerRemovesWhatItMadeWhenAStepFails pins that a failure half way leaves
// nothing: Prepare cleans up after itself, and Close after a failed Wrap.
func TestDockerRemovesWhatItMadeWhenAStepFails(t *testing.T) {
	rec := &recorder{t: t, gateway: "172.30.0.1", uid: 1000, fail: "network inspect"}
	d := &Docker{Helper: "/h", RelayArgs: []string{"relay"}, sys: rec}
	if _, err := d.Prepare(context.Background(), Request{RunID: runID, Image: "i"}); err == nil {
		t.Fatal("Prepare succeeded")
	}
	if got := strings.Join(rec.lines, "\n"); !strings.Contains(got, "network rm qory-"+runID+"-out") || !strings.Contains(got, "network rm qory-"+runID+"-in") {
		t.Errorf("the networks were left:\n%s", got)
	}

	rec = &recorder{t: t, gateway: "172.30.0.1", uid: 1000, fail: "start"}
	d = &Docker{Helper: "/h", RelayArgs: []string{"relay"}, sys: rec}
	e, err := d.Prepare(context.Background(), Request{RunID: runID, Image: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wrap(context.Background(), launch()); err == nil {
		t.Fatal("Wrap succeeded")
	}
	e.Close(context.Background())
	if got := strings.Join(rec.lines, "\n"); !strings.Contains(got, "rm --force --volumes qory-"+runID+"-relay") {
		t.Errorf("the relay was left:\n%s", got)
	}
}

// TestDockerRefusesAPathItCannotMount pins that a comma in a path is an error, not a
// mount of something else.
func TestDockerRefusesAPathItCannotMount(t *testing.T) {
	d := &Docker{Helper: "/h", RelayArgs: []string{"relay"}, sys: &recorder{t: t, gateway: "172.30.0.1", uid: 1000}}
	e, err := d.Prepare(context.Background(), Request{RunID: runID, Image: "i"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	l := launch()
	l.Dir = "/work,dst=/etc"
	if _, err := e.Wrap(context.Background(), l); err == nil {
		t.Error("a workspace with a comma was mounted")
	}
	for _, env := range []string{"A=one\nB=two", "AWS_SECRET_ACCESS_KEY", "#A=1", " A=1", "=1"} {
		l = launch()
		l.Env = []string{env}
		if _, err := e.Wrap(context.Background(), l); err == nil {
			t.Errorf("%q was written to the environment file", env)
		}
	}
}
