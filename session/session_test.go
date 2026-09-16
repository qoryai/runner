package session_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/runner/contracts"
	"github.com/qoryai/runner/internal/policy"
	"github.com/qoryai/runner/internal/receiver"
	"github.com/qoryai/runner/internal/socket"
	"github.com/qoryai/runner/session"
)

// TestMain lets the test binary stand in for a runtime and for the hook forwarder, so
// no real runtime and no shell script are needed: with QORY_TEST_RUNTIME set it acts as
// a runtime, with QORY_TEST_FORWARD set as the forwarder.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("QORY_TEST_FORWARD") != "":
		if err := socket.Forward(context.Background(), os.Getenv(session.EnvSocket), os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case os.Getenv("QORY_TEST_RUNTIME") != "":
		os.Exit(fakeRuntime())
	}
	os.Exit(m.Run())
}

// fakeRuntime prints what a headless runtime prints, reaches two hosts through the
// proxy, calls its hook the way a runtime calls a command hook, and exits as told. It
// sends its requests through HTTP_PROXY explicitly, because the origins are on
// loopback, which the runner's NO_PROXY exempts and Go never proxies.
func fakeRuntime() int {
	fmt.Println(`{"type":"system","subtype":"init","session_id":"fake","model":"m"}`)
	fmt.Fprintln(os.Stderr, "fake runtime: starting")
	proxyURL, _ := url.Parse(os.Getenv("HTTP_PROXY"))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	for _, u := range []string{os.Getenv("FAKE_ALLOWED_URL"), os.Getenv("FAKE_DENIED_URL")} {
		if u == "" {
			continue
		}
		resp, err := client.Get(u)
		if err != nil {
			fmt.Fprintln(os.Stderr, "get:", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "get %s: %d\n", u, resp.StatusCode)
	}
	if settings := os.Getenv("FAKE_SETTINGS"); settings != "" {
		// Call the hook the settings name, as the runtime would, with a SessionEnd input.
		b, _ := os.ReadFile(settings)
		var s struct {
			Hooks map[string][]struct {
				Hooks []struct{ Command string } `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(b, &s); err != nil || len(s.Hooks["SessionEnd"]) == 0 {
			fmt.Fprintln(os.Stderr, "no SessionEnd hook installed:", err)
			return 90
		}
		// Every group's every hook runs, as the runtime runs them: the launch's own and
		// the runner's forwarder.
		for _, group := range s.Hooks["SessionEnd"] {
			for _, h := range group.Hooks {
				cmd := shell(h.Command)
				cmd.Stdin = strings.NewReader(`{"session_id":"fake","hook_event_name":"SessionEnd","reason":"other","cwd":"/"}`)
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					fmt.Fprintln(os.Stderr, "hook:", err)
					return 91
				}
			}
		}
	}
	fmt.Println(`{"type":"result","subtype":"success","session_id":"fake","is_error":false,"num_turns":1,"duration_ms":5,"total_cost_usd":0.01,"result":"done"}`)
	time.Sleep(60 * time.Millisecond)
	code := 0
	fmt.Sscan(os.Getenv("FAKE_EXIT"), &code)
	return code
}

// spec returns a spec running the fake runtime with the given policy path and
// environment, in a fresh checkout directory.
func spec(t *testing.T, pol *session.Policy, env ...string) session.Spec {
	t.Helper()
	dir := t.TempDir()
	var out, errs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stdout:\n%s\nstderr:\n%s", out.String(), errs.String())
		}
	})
	return session.Spec{
		Runtime:       "claude",
		Command:       os.Args[0],
		Args:          []string{"--settings", writeSettings(t, dir)},
		Env:           append([]string{"QORY_TEST_RUNTIME=1", "PATH=" + os.Getenv("PATH")}, env...),
		Dir:           dir,
		Stdin:         strings.NewReader(""),
		Stdout:        &out,
		Stderr:        &errs,
		Policy:        pol,
		Forwarder:     []string{"env", "QORY_TEST_FORWARD=1", os.Args[0]},
		RunnerVersion: "test",
		Heartbeat:     10 * time.Millisecond,
		Report:        func(l string) { t.Log("report:", l) },
	}
}

func writeSettings(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "launch-settings.json")
	if err := os.WriteFile(p, []byte(`{"permissions":{"allow":["Bash"]},"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"echo existing"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// events reads and validates the run's events.jsonl and returns them by index.
func events(t *testing.T, res *session.Result) []map[string]any {
	t.Helper()
	schema, err := contracts.Compile("event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(res.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	s := bufio.NewScanner(f)
	s.Buffer(nil, 1<<20)
	for n := 1; s.Scan(); n++ {
		doc, err := contracts.Decode("events.jsonl", s.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(doc); err != nil {
			t.Errorf("events.jsonl:%d: %v\n%s", n, err, s.Bytes())
		}
		var m map[string]any
		json.Unmarshal(s.Bytes(), &m)
		if want := fmt.Sprintf("%010d", n); m["sequence"] != want {
			t.Errorf("events.jsonl:%d: sequence %v", n, m["sequence"])
		}
		out = append(out, m)
	}
	return out
}

func ofType(evs []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func data(e map[string]any) map[string]any { return e["data"].(map[string]any) }

// TestRunEnforcesRecordsAndExitsWithTheRuntimesStatus is the run end to end on pipes:
// the policy is pinned and applied, the allowed host goes through and the denied one
// does not, both are recorded, the runtime's output is logged per stream and mapped to
// session.result, the installed hook reaches the socket and becomes session.ended, the
// heartbeat ticks, and the exit status is the runtime's.
func TestRunEnforcesRecordsAndExitsWithTheRuntimesStatus(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer origin.Close()
	pol := &session.Policy{Version: 1, Egress: session.PolicyEgress{Mode: "enforce", Allow: []string{"127.0.0.1", "api.anthropic.com"}}}
	sp := spec(t, pol, "FAKE_ALLOWED_URL="+origin.URL+"/allowed", "FAKE_DENIED_URL="+strings.Replace(origin.URL, "127.0.0.1", "localhost", 1)+"/denied", "FAKE_EXIT=3")
	sp.Declared = []string{"127.0.0.1", "registry.npmjs.org"}
	res, err := runWithSettingsEnv(t, sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.State != "failed" || res.Signal != "" {
		t.Errorf("result %+v", res)
	}
	evs := events(t, res)
	if len(evs) < 8 || evs[0]["type"] != "ai.qory.run.started" || evs[1]["type"] != "ai.qory.run.policy_applied" || evs[len(evs)-1]["type"] != "ai.qory.run.exited" {
		t.Fatalf("event order: %v", types(evs))
	}
	applied := data(evs[1])
	if applied["mode"] != "enforce" || applied["source"] != "config" || fmt.Sprint(applied["allow"]) != "[127.0.0.1]" || fmt.Sprint(applied["declared"]) != "[127.0.0.1 registry.npmjs.org]" || applied["digest"] == nil {
		t.Errorf("policy_applied %v", applied)
	}
	egress := ofType(evs, "ai.qory.run.egress")
	if len(egress) != 2 || data(egress[0])["decision"] != "allowed" || data(egress[0])["rule"] != "127.0.0.1" || data(egress[1])["decision"] != "denied" || data(egress[1])["host"] != "localhost" {
		t.Errorf("egress %v", egress)
	}
	streams := map[string]bool{}
	for _, l := range ofType(evs, "ai.qory.run.log") {
		streams[data(l)["stream"].(string)] = true
	}
	if !streams["stdout"] || !streams["stderr"] {
		t.Errorf("log streams %v", streams)
	}
	if r := ofType(evs, "ai.qory.session.result"); len(r) != 1 || data(r[0])["outcome"] != "success" || data(r[0])["result"] != "done" {
		t.Errorf("session.result %v", r)
	}
	if e := ofType(evs, "ai.qory.session.ended"); len(e) != 1 || data(e[0])["reason"] != "other" {
		t.Errorf("session.ended %v", e)
	}
	if len(ofType(evs, "ai.qory.run.heartbeat")) == 0 {
		t.Error("no heartbeat")
	}
	exited := data(evs[len(evs)-1])
	if exited["state"] != "failed" || exited["exit_code"] != 3.0 {
		t.Errorf("exited %v", exited)
	}
	out, _ := os.ReadFile(filepath.Join(res.Dir, "output.log"))
	if !strings.Contains(string(out), `"type":"result"`) || !strings.Contains(string(out), "fake runtime: starting") {
		t.Errorf("output.log %q", out)
	}
	settings, _ := os.ReadFile(filepath.Join(res.Dir, "settings.json"))
	if !strings.Contains(string(settings), `"echo existing"`) || !strings.Contains(string(settings), `"permissions"`) || strings.Count(string(settings), os.Args[0]) != 11 {
		t.Errorf("settings.json:\n%s", settings)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "undelivered")); !os.IsNotExist(err) {
		t.Error("an undelivered directory exists with no webhook")
	}
}

// runWithSettingsEnv runs the spec with FAKE_SETTINGS pointing at the settings file
// the runner writes, which is only known once the run directory exists: the run id is
// fixed in advance so the path is known.
func runWithSettingsEnv(t *testing.T, sp session.Spec) (*session.Result, error) {
	t.Helper()
	sp.RunID = "0191f2a4-3c5e-7b8d-9e0f-1a2b3c4d5e6f"
	sp.Env = append(sp.Env, "FAKE_SETTINGS="+filepath.Join(sp.Dir, ".qory", "runs", sp.RunID, "settings.json"))
	return session.Run(context.Background(), sp)
}

func types(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e["type"].(string))
	}
	return out
}

// TestNoPolicyObservesAndNoWebhookNeedsNoPing pins the defaults: no policy is
// observe with source none, a denied host is not denied, and nothing is posted.
func TestNoPolicyObservesAndNoWebhookNeedsNoPing(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer origin.Close()
	sp := spec(t, nil, "FAKE_DENIED_URL="+strings.Replace(origin.URL, "127.0.0.1", "localhost", 1))
	sp.Forwarder = nil
	res, err := session.Run(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	evs := events(t, res)
	if a := data(evs[1]); a["mode"] != "observe" || a["source"] != "none" || a["digest"] != nil {
		t.Errorf("policy_applied %v", a)
	}
	if e := ofType(evs, "ai.qory.run.egress"); len(e) != 1 || data(e[0])["decision"] != "allowed" || data(e[0])["rule"] != "" {
		t.Errorf("egress %v", e)
	}
	if len(ofType(evs, "ai.qory.ping")) != 0 || res.ExitCode != 0 || res.State != "succeeded" {
		t.Errorf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "settings.json")); !os.IsNotExist(err) {
		t.Error("hooks were installed with no forwarder")
	}
}

// TestInvalidPolicyMeansNoRun pins the rule: a policy the schema refuses is an error
// before anything starts, and no run directory is made.
func TestInvalidPolicyMeansNoRun(t *testing.T) {
	sp := spec(t, &session.Policy{Version: 1, Egress: session.PolicyEgress{Mode: "log"}})
	_, err := session.Run(context.Background(), sp)
	var pe *policy.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(sp.Dir, ".qory")); len(entries) != 0 {
		t.Error("a run directory was made")
	}
}

// TestWebhookPingsFailsClosedAndDelivers pins the webhook side: with a receiver that
// answers, the ping is the first event and every event reaches the store; with one that
// refuses, no run starts; with --local, the same configuration is ignored.
func TestWebhookPingsFailsClosedAndDelivers(t *testing.T) {
	store, err := receiver.OpenFile(filepath.Join(t.TempDir(), "received.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse {
			w.WriteHeader(500)
			return
		}
		(&receiver.Handler{Secret: "fixture-secret-not-a-real-one", Store: store}).ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg := &session.Webhook{Version: 1, URL: srv.URL + "/events", Secret: "fixture-secret-not-a-real-one"}

	sp := spec(t, nil)
	sp.Forwarder = nil
	sp.Webhook = cfg
	res, err := session.Run(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	evs := events(t, res)
	if evs[0]["type"] != "ai.qory.ping" || fmt.Sprint(data(evs[0])["events"]) != "[*]" {
		t.Errorf("first event %v", evs[0])
	}
	if store.Count() != len(evs) || res.Undelivered != 0 {
		t.Errorf("store holds %d of %d events, %d undelivered", store.Count(), len(evs), res.Undelivered)
	}

	refuse = true
	sp = spec(t, nil)
	sp.Forwarder = nil
	sp.Webhook = cfg
	if _, err := session.Run(context.Background(), sp); err == nil || !strings.Contains(err.Error(), "ping") {
		t.Errorf("refused ping: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(sp.Dir, ".qory", "runs")); len(entries) != 1 {
		t.Errorf("run directories after a refused ping: %d", len(entries))
	} else if evs, _ := os.ReadFile(filepath.Join(sp.Dir, ".qory", "runs", entries[0].Name(), "events.jsonl")); strings.Count(string(evs), "\n") != 1 {
		t.Errorf("the refused run's file holds more than the ping:\n%s", evs)
	}

	sp = spec(t, nil)
	sp.Forwarder = nil
	sp.Webhook = cfg
	sp.Local = true
	if res, err := session.Run(context.Background(), sp); err != nil || len(ofType(events(t, res), "ai.qory.ping")) != 0 {
		t.Errorf("local run: %v", err)
	}
}

// TestInteractiveRunsOnAPseudoTerminal pins the PTY path: the output is one terminal
// stream with the terminal's line endings, and the exit status is the program's.
func TestInteractiveRunsOnAPseudoTerminal(t *testing.T) {
	sp := spec(t, nil)
	sp.Forwarder = nil
	sp.Interactive = true
	sp.Command = "sh"
	sp.Args = []string{"-c", "echo hello; exit 4"}
	res, err := session.Run(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 4 {
		t.Errorf("exit %d", res.ExitCode)
	}
	out, _ := os.ReadFile(filepath.Join(res.Dir, "output.log"))
	if string(out) != "hello\r\n" {
		t.Errorf("output.log %q", out)
	}
	if l := ofType(events(t, res), "ai.qory.run.log"); len(l) != 1 || data(l[0])["stream"] != "terminal" {
		t.Errorf("log %v", l)
	}
}

// TestContextEndStopsTheRuntime pins that a cancelled context ends the session with a
// signal, recorded as such.
func TestContextEndStopsTheRuntime(t *testing.T) {
	sp := spec(t, nil)
	sp.Forwarder = nil
	sp.Command = "sh"
	sp.Args = []string{"-c", "sleep 30"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := session.Run(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != -1 || res.Signal != "SIGTERM" || res.State != "failed" {
		t.Errorf("result %+v", res)
	}
}
