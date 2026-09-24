// Package walltest is the conformance suite of the wall: one list of guarantees,
// contracts/runner/v1/README.md §The wall, checked from inside the enclosure, the same
// for every adapter. An adapter ships when the suite passes for it.
//
// The suite runs a real session behind the adapter with a probe as its runtime. The
// probe, the relay and the hook forwarder are all the test binary itself, mounted into
// the enclosure as the adapter's helper, so the suite needs no image of its own beyond
// one with a shell. A test binary that runs the suite calls [Main] first in its
// TestMain:
//
//	func TestMain(m *testing.M) {
//		walltest.Main()
//		os.Exit(m.Run())
//	}
//
// The enclosure runs Linux, so the helper is the test binary only on Linux, built with
// CGO_ENABLED=0; elsewhere QORY_WALL_HELPER names a test binary built for Linux, go
// test -c with GOOS=linux, and without it the suite is skipped.
package walltest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/qoryai/runner/internal/credential"
	"github.com/qoryai/runner/runtimes/claude"
	"github.com/qoryai/runner/session"
	"github.com/qoryai/runner/wall"
)

// The modes of the helper, its first argument.
const (
	modeRelay   = "relay"
	modeForward = "forward"
	modeProbe   = "probe"
)

// RelayArgs are the arguments that make the helper run the relay.
var RelayArgs = []string{modeRelay}

// EnvHelper names a Linux build of the test binary, for a machine that is not Linux.
const EnvHelper = "QORY_WALL_HELPER"

// EnvRequire names the variable that, when set, turns every skip of the suite into a
// failure, so the machine that is meant to prove an adapter cannot pass by proving
// nothing.
const EnvRequire = "QORY_WALL_REQUIRE"

// What the suite's credential is: a token in the suite's own environment, for a host
// that does not exist, on one path, with a placeholder inside. The host never needs to
// answer: what is checked happens between the enclosure and the proxy.
const (
	tokenVar       = "QORY_WALLTEST_TOKEN"
	tokenMark      = "walltest-token-held-outside"
	placeholderVar = "PROBE_TOKEN"
	credentialHost = "credential.invalid"
	credentialPath = "/inside-the-paths"
)

// hostOnly is a variable the suite sets in its own environment and must not find
// inside.
const hostOnly = "QORY_WALLTEST_HOST_ONLY"

// Main makes the test binary the helper when it was started as one: the relay, the hook
// forwarder or the probe. It returns at once otherwise.
func Main() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case modeRelay:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := wall.Relay(ctx, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case modeForward:
		if err := session.Forward(context.Background(), os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "forward:", err)
		}
		os.Exit(0)
	case modeProbe:
		os.Exit(probe(os.Args[2:]))
	}
}

// Skip skips the test, or fails it when [EnvRequire] is set.
func Skip(t *testing.T, why string) {
	t.Helper()
	if os.Getenv(EnvRequire) != "" {
		t.Fatalf("%s is set and the suite cannot run: %s", EnvRequire, why)
	}
	t.Skip(why)
}

// Helper returns the path of the helper to give the adapter, or skips.
func Helper(t *testing.T) string {
	t.Helper()
	if h := os.Getenv(EnvHelper); h != "" {
		return h
	}
	if runtime.GOOS != "linux" {
		Skip(t, "the enclosure runs Linux and this test binary does not; name a Linux build of it in "+EnvHelper)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// Options are what the suite is told about the adapter under test.
type Options struct {
	// Wall is the adapter, built with [Helper] as its helper and [RelayArgs].
	Wall wall.Wall
	// Image is an image with a shell at sh; its user and entry point do not matter.
	Image string
	// Origin is the URL of an HTTP server that answers 200 and is not on this machine,
	// a container's say: the proxy never dials this machine for an enclosure, so the
	// suite's own listener cannot be what an allowed request reaches.
	Origin string
	// Forwarder is the hook forwarder's command inside the enclosure: the adapter's
	// helper path, then "forward".
	Forwarder []string
	// Probe is the helper's path inside the enclosure.
	Probe string
	// Hooks says the adapter carries the hook socket across on this machine. Where it
	// does not, an engine in a virtual machine, the hook check is skipped and said so.
	Hooks bool
	// Leftovers lists what the adapter left behind for a run, for the check that Close
	// removes everything; nil skips that check.
	Leftovers func(runID string) ([]string, error)
}

// Run checks the adapter against the guarantees.
func Run(t *testing.T, o Options) {
	// This machine's own listener, on every address: what no path from inside may reach,
	// around the proxy or through it.
	var own atomic.Int32
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	host := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		own.Add(1)
		io.WriteString(w, "this machine")
	})}}
	host.Start()
	defer host.Close()
	origin := hosts{origin: o.Origin, own: fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port), port: ln.Addr().(*net.TCPAddr).Port}
	t.Setenv(hostOnly, "1")
	t.Setenv(tokenVar, tokenMark+"-"+strconv.Itoa(os.Getpid()))
	outside := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(outside, []byte("the host's"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := run(t, o, false, origin, outside)
	p := r.probe
	check := func(name string, ok bool, detail any) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			t.Helper()
			if !ok {
				t.Errorf("%v", detail)
			}
		})
	}
	check("no route out by address", p.OutsideAddress != "", "the probe connected to an outside address")
	check("no name resolved outside", p.OutsideName != "", "the probe resolved an outside name")
	check("no metadata address", p.Metadata != "", "the probe connected to the metadata address")
	check("the proxy is reached and decides", p.Allowed == 200 && p.Denied == 403, fmt.Sprintf("allowed answered %d (%s), denied answered %d (%s)", p.Allowed, p.AllowedErr, p.Denied, p.DeniedErr))
	var egress []string
	for _, e := range r.events {
		if e["type"] == "dev.qory.run.egress" {
			d := e["data"].(map[string]any)
			egress = append(egress, fmt.Sprint(d["host"], " ", d["decision"], " ", d["rule"]))
		}
	}
	originHost := mustHost(t, o.Origin)
	check("what went through the proxy is recorded", fmt.Sprint(egress) == "["+originHost+" allowed "+originHost+" denied.invalid denied  127.0.0.1 denied wall:own-address 169.254.169.254 denied wall:own-address "+originHost+" denied "+originHost+" "+credentialHost+" denied "+credentialHost+" "+credentialHost+" allowed "+credentialHost+"]", egress)
	check("a host held to paths is held to them", p.PathDenied == 403, fmt.Sprintf("a path outside the host's answered %d", p.PathDenied))
	check("a terminated host is answered with the run's authority, held to the credential's paths", p.TLSDenied == 403 && p.TLSAllowed == 502,
		fmt.Sprintf("outside the paths answered %d (%s), inside them %d (%s), want the proxy's 403 and, with nothing upstream, its 502; the bundle is %q with %d certificates", p.TLSDenied, p.TLSDeniedErr, p.TLSAllowed, p.TLSAllowedErr, p.Bundle, p.BundleCerts))
	set := ""
	for _, e := range r.events {
		if d, _ := e["data"].(map[string]any); e["type"] == "dev.qory.run.egress" && d["path"] == credentialPath {
			set, _ = d["credential"].(string)
		}
	}
	check("the credential is set outside, on its own paths", set == "suite", fmt.Sprintf("the request inside the credential's paths is recorded with the credential %q", set))
	record, _ := os.ReadFile(filepath.Join(r.res.Dir, "events.jsonl"))
	check("no credential inside the enclosure", p.Placeholder == credential.Placeholder && len(p.TokenSeen) == 0 && p.BundleCerts > 0 && p.BundleKeys == 0 && !bytes.Contains(record, []byte(tokenMark)),
		fmt.Sprintf("the placeholder is %q; the token was seen in %v; the bundle holds %d certificates and %d keys; the token is in the record: %v", p.Placeholder, p.TokenSeen, p.BundleCerts, p.BundleKeys, bytes.Contains(record, []byte(tokenMark))))
	check("no way to this machine through the proxy unless the policy names it", p.OwnViaProxy == 403 && p.MetaViaProxy == 403 && own.Load() == 0, fmt.Sprintf("this machine's listener answered %d through the proxy and was reached %d times; the metadata address answered %d", p.OwnViaProxy, own.Load(), p.MetaViaProxy))
	check("no way to the engine's host by the network's first address", len(p.HostByGateway) == 0, p.HostByGateway)
	check("the record is read-only", p.RecordWrite != "", "the probe opened events.jsonl for writing")
	check("not root", p.UID != 0 && p.GID != 0, fmt.Sprintf("uid %d gid %d", p.UID, p.GID))
	check("no capabilities and none to gain", zero(p.CapEff) && zero(p.CapPrm) && zero(p.CapBnd) && p.NoNewPrivs == "1", fmt.Sprintf("CapEff %s CapPrm %s CapBnd %s NoNewPrivs %s", p.CapEff, p.CapPrm, p.CapBnd, p.NoNewPrivs))
	check("no container runtime socket", len(p.Sockets) == 0, p.Sockets)
	check("no environment but what the run passes", !p.HostEnv && p.PassedEnv, fmt.Sprintf("the host's variable seen: %v; the run's variable seen: %v; all: %v", p.HostEnv, p.PassedEnv, p.EnvNames))
	check("no file of the host but the mounts", !p.HostFile, "the probe read a file outside the workspace")
	check("the settings are read-only", p.SettingsWrite != "", "the probe opened the runner's settings for writing")
	written, err := os.ReadFile(filepath.Join(r.dir, "probe-was-here"))
	check("the workspace is the run's", err == nil && string(written) == "inside", err)
	if runtime.GOOS == "linux" {
		for _, ns := range []string{"net", "pid", "mnt", "ipc", "uts"} {
			host, _ := os.Readlink("/proc/self/ns/" + ns)
			check("no host namespace: "+ns, p.Namespaces[ns] != "" && p.Namespaces[ns] != host, fmt.Sprintf("inside %q, the host %q", p.Namespaces[ns], host))
		}
	}
	t.Run("the hook reaches the runner", func(t *testing.T) {
		if !o.Hooks {
			t.Skip("the adapter carries no hook socket across on this machine; the forwarder's network transport is not in this release")
		}
		for _, e := range r.events {
			if e["type"] == "dev.qory.session.ended" {
				return
			}
		}
		t.Errorf("no session.ended among the events; the hook said: %s", p.Hook)
	})
	check("the exit status is the runtime's", r.res.ExitCode == 7, r.res.ExitCode)
	for _, e := range r.events {
		if e["type"] == "dev.qory.run.started" {
			d := e["data"].(map[string]any)
			check("the record names the wall", d["wall"] == o.Wall.Name() && d["image"] == o.Image, d)
		}
	}
	if o.Leftovers != nil {
		left, err := o.Leftovers(r.res.RunID)
		check("Close removes everything", err == nil && len(left) == 0, fmt.Sprint(left, err))
	}

	t.Run("the terminal crosses and a named host on this machine is reached", func(t *testing.T) {
		origin.named = true
		r := run(t, o, true, origin, outside)
		if !r.probe.Terminal {
			t.Error("the probe's standard output is not a terminal in an interactive run")
		}
		if r.probe.OwnViaProxy != 200 || own.Load() == 0 || r.probe.MetaViaProxy != 403 {
			t.Errorf("with 127.0.0.1 named in the allow list this machine's listener answered %d through the proxy, and the metadata address %d", r.probe.OwnViaProxy, r.probe.MetaViaProxy)
		}
	})
}

// hosts are the addresses a run's probe is told.
type hosts struct {
	origin string
	own    string
	port   int
	// named puts this machine's loopback in the allow list by name.
	named bool
}

// mustHost is the host of a URL.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		t.Fatalf("the origin %q is not a URL", raw)
	}
	return u.Hostname()
}

func zero(hex string) bool { return hex != "" && strings.Trim(hex, "0") == "" }

// result is one run behind the wall.
type result struct {
	res    *session.Result
	dir    string
	probe  report
	events []map[string]any
}

// run runs the probe as the runtime of a session behind the wall and reads what it
// reported and what the runner recorded.
func run(t *testing.T, o Options, interactive bool, h hosts, outside string) result {
	t.Helper()
	dir := t.TempDir()
	var out, errs bytes.Buffer
	settings := filepath.Join(dir, "launch-settings.json")
	if err := os.WriteFile(settings, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The metadata address is in the list to show that no entry opens it.
	allow := []string{mustHost(t, h.origin), "169.254.169.254", credentialHost}
	if h.named {
		allow = append(allow, "127.0.0.1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rt, err := claude.New()
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.Run(ctx, session.Spec{
		Runtime: rt,
		Command: o.Probe,
		Args:    []string{modeProbe, "--settings", settings},
		Env: []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"PROBE_PASSED=yes", "PROBE_EXIT=7", "PROBE_HOST_FILE=" + outside,
			"PROBE_ALLOWED=" + h.origin,
			"PROBE_DENIED=http://denied.invalid/",
			"PROBE_OWN=" + h.own,
			"PROBE_HOST_PORT=" + strconv.Itoa(h.port),
		},
		Dir:         dir,
		Interactive: interactive,
		Stdin:       strings.NewReader(""),
		Stdout:      &out,
		Stderr:      &errs,
		Policy: &session.Policy{Version: 1,
			Egress:      session.PolicyEgress{Mode: "enforce", Allow: allow, Paths: map[string][]string{mustHost(t, h.origin): {"/"}}},
			Credentials: []session.PolicyCredential{{Name: "suite"}}},
		Credentials:   []session.Credential{{Name: "suite", Env: tokenVar, Hosts: []string{credentialHost}, Scheme: "bearer", Paths: []string{credentialPath}, Placeholders: []string{placeholderVar}}},
		Forwarder:     o.Forwarder,
		Wall:          o.Wall,
		Image:         o.Image,
		RunnerVersion: "walltest",
		Report:        func(l string) { t.Log("report:", l) },
	})
	if err != nil {
		t.Fatalf("the run did not start: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errs.String())
	}
	r := result{res: res, dir: dir}
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), probePrefix); ok {
			if err := json.Unmarshal([]byte(rest), &r.probe); err != nil {
				t.Fatalf("the probe's report: %v\n%s", err, rest)
			}
			found = true
			t.Logf("the probe (interactive %v): %s", interactive, rest)
		}
	}
	if !found {
		t.Fatalf("the probe reported nothing; exit %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, out.String(), errs.String())
	}
	f, err := os.Open(filepath.Join(res.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(nil, 1<<20)
	for s.Scan() {
		var m map[string]any
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		r.events = append(r.events, m)
	}
	return r
}
