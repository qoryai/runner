package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qoryai/runner/internal/credential"
	"github.com/qoryai/runner/internal/descriptor"
	"github.com/qoryai/runner/internal/event"
	"github.com/qoryai/runner/internal/policy"
	"github.com/qoryai/runner/internal/proxy"
	"github.com/qoryai/runner/internal/sink"
	"github.com/qoryai/runner/internal/socket"
	"github.com/qoryai/runner/internal/webhook"
	"github.com/qoryai/runner/wall"
)

// Spec is what one run is given.
type Spec struct {
	// Runtime names the descriptor: claude, codex.
	Runtime string
	// Command, Args, Env and Dir are what to start. A nil Env is the process's own, or
	// nothing under a Wall, where only what Env lists goes in; an empty Dir is the
	// working directory.
	Command string
	Args    []string
	Env     []string
	Dir     string
	// Interactive runs the session on a pseudo-terminal attached to Stdin and Stdout;
	// otherwise it runs on pipes with Stdin as its input and its output copied to
	// Stdout and Stderr. A nil stream is the process's own.
	Interactive bool
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	// Policy is the run's policy; nil means no policy, mode observe.
	Policy *Policy
	// Webhook is where the events are posted as well; nil means files only. Local
	// ignores it.
	Webhook *Webhook
	Local   bool
	// Events, when not nil, gets every event as one JSON line as well, the line
	// events.jsonl holds: a run with no receiver is followed on standard output this
	// way. Local does not silence it.
	Events io.Writer
	// ProxyBind is the address the proxy listens on, host:port; empty means a loopback
	// port. A caller that builds an enclosure of its own names the address the
	// enclosure reaches here. It is not set together with Wall, which names its own.
	ProxyBind string
	// Wall, when not nil, encloses the runtime: the command is started inside an
	// enclosure whose only route out leads to the proxy, in Image, with Dir as its
	// workspace. Command, Args and Forwarder are then paths inside the enclosure. Nil
	// means no wall: the runtime is this machine's process, and enforcement is
	// cooperative.
	Wall wall.Wall
	// Image is the agent's image under a Wall.
	Image string
	// Mounts are what the enclosure shows of this machine beside Dir, each at its own
	// path: the checkout around Dir, a composed home outside it. The runner adds the
	// run directory, read-only. Without a Wall they mean nothing.
	Mounts []wall.Mount
	// Credentials are the credentials this machine defines; the run's policy selects
	// among them by name. A selected credential, like a path rule, needs a Wall: the
	// proxy then terminates TLS for the hosts concerned, with an authority made for the
	// run whose certificate the enclosure is given to trust.
	Credentials []Credential
	// Declared is the egress the harness declared, nil when nothing was.
	Declared []string
	// RunsDir holds the run directories; empty means Dir/.qory/runs.
	RunsDir string
	// Descriptors is a directory of descriptor overrides, <runtime>.yaml; may be empty.
	Descriptors string
	// Forwarder is the command the runner installs as the runtime's hook: it reads the
	// hook's input and forwards it to the socket. Empty means no hooks are installed.
	Forwarder []string
	// RunnerVersion is reported in the events.
	RunnerVersion string
	// RunID is the run's id when a parent already made one; empty means a new one. It is
	// a UUID in the canonical lower-case form, because it is the events' subject and names
	// the run directory; anything else is refused.
	RunID string
	// Labels are the caller's own names for the run, its key in a queue, a repository, an
	// issue: reported in run.started and nowhere else, so a receiver ties the run id to
	// what it knows. At most MaxLabels; a key is 1 to 64 of a-z, 0-9, underscore, dot and
	// dash, a value at most 256 bytes.
	Labels map[string]string
	// Timeout is how long the runtime may run; zero means no limit. At the limit the
	// runtime is stopped the way the context ending stops it, and run.exited carries
	// the reason.
	Timeout time.Duration
	// StopGrace is how long the runtime gets between SIGTERM and SIGKILL when the runner
	// stops it, at the Timeout or the context's end: the time a session needs to close
	// what it has open. Zero means DefaultStopGrace.
	StopGrace time.Duration
	// Limits are the resources the enclosure gives the runtime. Without a Wall they mean
	// nothing.
	Limits wall.Limits
	// Heartbeat is the interval between heartbeats; zero means 30 seconds.
	Heartbeat time.Duration
	// Report receives one line per thing the runner tells its user; nil means Stderr.
	Report func(string)
}

// Result is what a run came to.
type Result struct {
	RunID string
	// Dir is the run directory holding events.jsonl and output.log.
	Dir string
	// ExitCode is the runtime's, or -1 when a signal killed it.
	ExitCode int
	// Signal names the signal that killed the runtime, if one did.
	Signal string
	// State is succeeded or failed.
	State string
	// TimedOut says the runtime was stopped at the spec's Timeout.
	TimedOut bool
	// Undelivered is how many events the webhook did not accept.
	Undelivered int
}

// The environment variables the session gets from the runner.
const (
	EnvRunID  = "QORY_RUN_ID"
	EnvSocket = socket.Env
)

// MaxLabels is how many labels a run may carry.
const MaxLabels = 16

// errTimeout is the cause of the runtime's context ending at the spec's Timeout.
var errTimeout = errors.New("the run's time limit")

var (
	runIDShape    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	labelKeyShape = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
)

// CheckRunID refuses a caller's run id that is not a UUID in the canonical lower-case
// form. [Run] checks it; a command checks it first to call it a mistake of its user's.
func CheckRunID(id string) error {
	if !runIDShape.MatchString(id) {
		return fmt.Errorf("the run id %q is not a UUID in the canonical lower-case form", id)
	}
	return nil
}

// CheckLabels refuses labels the contract's schema would: too many, a key outside its
// grammar, a value too long. [Run] checks them; a command may first.
func CheckLabels(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("%d labels; a run carries at most %d", len(labels), MaxLabels)
	}
	for k, v := range labels {
		if !labelKeyShape.MatchString(k) {
			return fmt.Errorf("the label key %q is not 1 to 64 of a-z, 0-9, underscore, dot and dash", k)
		}
		if len(v) > 256 || !utf8.ValidString(v) {
			return fmt.Errorf("the value of the label %s is longer than 256 bytes or not UTF-8", k)
		}
	}
	return nil
}

// closeWait is how long the sinks get to flush after the runtime exits.
const closeWait = 15 * time.Second

// Run runs one session and returns when the runtime has exited and the sinks are
// flushed. An error means the run did not start: the policy or the webhook could not
// be read, the receiver did not accept the ping, the descriptor is unknown, the wall
// could not be built, or the program could not be started. Once the runtime runs, its exit is the result and not
// an error. The context ending stops the runtime.
func Run(ctx context.Context, spec Spec) (*Result, error) {
	spec = withDefaults(spec)
	pol := policy.None()
	var err error
	if spec.Policy != nil {
		b, _ := json.Marshal(spec.Policy)
		if pol, err = policy.Read("policy", b); err != nil {
			return nil, err
		}
	}
	var hook *webhook.Config
	if !spec.Local && spec.Webhook != nil {
		b, _ := json.Marshal(spec.Webhook)
		if hook, err = webhook.Read("webhook", b); err != nil {
			return nil, err
		}
	}
	desc, err := descriptor.Load(spec.Runtime, spec.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Wall != nil && spec.ProxyBind != "" {
		return nil, errors.New("the spec names a wall and a proxy address; the wall names its own")
	}
	runID := spec.RunID
	if runID == "" {
		runID = event.NewRunID()
	} else if err := CheckRunID(runID); err != nil {
		return nil, err
	}
	if spec.Timeout < 0 || spec.StopGrace < 0 {
		return nil, errors.New("the timeout or the stop grace is negative")
	}
	if err := CheckLabels(spec.Labels); err != nil {
		return nil, err
	}
	dir := filepath.Join(spec.RunsDir, runID)
	emit := event.NewEmitter(runID, nil)
	files, err := sink.NewFile(dir)
	if err != nil {
		return nil, err
	}
	allow := pol.Narrow(spec.Declared)
	if (len(pol.Policy.Credentials) > 0 || len(pol.Policy.Egress.Paths) > 0) && spec.Wall == nil {
		files.Close(ctx)
		return nil, errors.New("the policy selects credentials or has path rules, which need a wall: without one a program that ignores the proxy is bound by neither")
	}
	defs := make([]credential.Definition, len(spec.Credentials))
	for i, c := range spec.Credentials {
		defs[i] = credential.Definition(c)
	}
	held, err := credential.Resolve(ctx, defs, pol.Policy.Credentials, pol.Policy.Egress.Mode, allow, spec.Report)
	if err != nil {
		files.Close(ctx)
		return nil, err
	}
	defer held.Close()
	for _, name := range held.Placeholders {
		if slices.ContainsFunc(spec.Env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") }) {
			files.Close(ctx)
			return nil, fmt.Errorf("%s is a placeholder of a credential the runner holds outside the enclosure, and the run passes a value for it inside", name)
		}
	}
	unlock, err := lock(dir)
	if err != nil {
		files.Close(ctx)
		return nil, err
	}
	defer unlock()
	sinks := sink.Multi{files}
	if spec.Events != nil {
		sinks = append(sinks, sink.NewWriter(spec.Events))
	}
	var posts *sink.Webhook
	if hook != nil {
		client := &webhook.Client{Config: hook, UserAgent: "qory-runner/" + spec.RunnerVersion}
		ping := emit.Make(event.Ping, map[string]any{"runner_version": spec.RunnerVersion, "events": filter(hook)})
		sinks.Write(ping)
		body, _ := ping.JSON()
		pingID := event.NewID()
		if err := client.Ping(ctx, pingID, []byte("["+string(body)+"]")); err != nil {
			sinks.Close(ctx)
			return nil, err
		}
		posts = sink.NewWebhook(client, dir, spec.Report)
		posts.Accepted(pingID, ping.Sequence)
		sinks = append(sinks, posts)
	}
	// write numbers and writes under one lock, so the order in the sinks is the order of
	// the sequence whichever goroutine emits: the proxy, the socket, the heartbeat.
	var mu sync.Mutex
	write := func(typ string, data any) {
		mu.Lock()
		defer mu.Unlock()
		sinks.Write(emit.Make(typ, data))
	}

	// The wall comes before the proxy, because it says where the proxy must listen, and
	// goes after everything else, because the proxy outlives the last connection.
	var enclosure wall.Enclosure
	bind := spec.ProxyBind
	if spec.Wall != nil {
		if enclosure, err = spec.Wall.Prepare(ctx, wall.Request{RunID: runID, Image: spec.Image}); err != nil {
			sinks.Close(ctx)
			return nil, err
		}
		defer func() {
			// The run's context may be what ended the run; the wall is removed regardless.
			if err := enclosure.Close(context.WithoutCancel(ctx)); err != nil {
				spec.Report("removing the wall: " + err.Error())
			}
		}()
		bind = enclosure.ProxyAddr()
	}

	px, err := proxy.Listen(bind, pol.Policy.Egress.Mode, allow, func(d proxy.Decision) {
		decision := "denied"
		if d.Allowed {
			decision = "allowed"
		}
		egress := map[string]any{"host": d.Host, "port": d.Port, "method": d.Method, "decision": decision, "mode": string(pol.Policy.Egress.Mode), "rule": d.Rule}
		if d.Path != "" {
			egress["request_method"], egress["path"], egress["path_rule"] = d.RequestMethod, d.Path, d.PathRule
		}
		if d.Credential != "" {
			egress["credential"] = d.Credential
		}
		write(event.RunEgress, egress)
	})
	if err != nil {
		sinks.Close(ctx)
		return nil, err
	}
	defer px.Close()
	if spec.Wall != nil || spec.ProxyBind != "" {
		// The proxy serves something that is not on this machine, so this machine's own
		// addresses are not its to reach.
		px.Guard(pol.Policy.Egress.Allow)
	}
	var authority []byte
	if len(held.Uses) > 0 || len(pol.Policy.Egress.Paths) > 0 {
		ca, err := proxy.NewCA(runID)
		if err != nil {
			sinks.Close(ctx)
			return nil, err
		}
		uses := make([]proxy.Credential, len(held.Uses))
		for i, u := range held.Uses {
			uses[i] = proxy.Credential{Name: u.Name, Hosts: u.Hosts, Scheme: u.Scheme, Username: u.Username, Header: u.Header, Paths: u.Paths, Token: u.Token, Rejected: u.Rejected}
		}
		px.Terminate(ca, uses, pol.Policy.Egress.Paths)
		authority = ca.PEM()
	}
	// Behind a wall the proxy listens where other containers of the engine, or other
	// processes of the machine, may reach it. It serves the run's relay alone.
	token := ""
	if spec.Wall != nil {
		token = event.NewID() + event.NewID()
		var once sync.Once
		px.Require(token, func() {
			once.Do(func() { spec.Report("a connection to the proxy that was not the run's relay was refused") })
		})
	}

	sock, err := socket.Listen()
	if err != nil {
		sinks.Close(ctx)
		return nil, err
	}
	records := func(r descriptor.Record) {
		if typ, data, ok := desc.Map(r); ok {
			write(typ, data)
		}
	}
	go sock.Serve(records, func(err error) { spec.Report("socket: " + err.Error()) })
	closeSocket := sync.OnceFunc(func() { sock.Close() })
	defer closeSocket()

	args := spec.Args
	// The run directory goes in read-only, over whatever mount holds it: the settings
	// are read from it, and the record in it is not the agent's to rewrite.
	mounts := append(append([]wall.Mount(nil), spec.Mounts...), wall.Mount{Path: dir, ReadOnly: true})
	if desc.Sources.Hooks != nil && len(spec.Forwarder) > 0 {
		if args, _, err = installHooks(desc.Sources.Hooks, args, dir, spec.Forwarder); err != nil {
			sinks.Close(ctx)
			return nil, err
		}
	}
	// What is started: the runtime itself, or under a wall the adapter's command that
	// starts it inside, which sets the proxy and socket variables by the addresses the
	// enclosure reaches them on.
	launch := wall.Launch{
		Command: spec.Command, Args: args, Dir: spec.Dir,
		Env: environment(spec.Env, px.Env(), []string{EnvSocket + "=" + sock.Path(), EnvRunID + "=" + runID}),
	}
	if enclosure != nil {
		launch, err = enclosure.Wrap(ctx, wall.Launch{
			Command: spec.Command, Args: args, Dir: spec.Dir, Interactive: spec.Interactive,
			Env:   environment(spec.Env, []string{EnvRunID + "=" + runID}, placeholders(held.Placeholders)),
			CA:    authority,
			Proxy: px.Addr(), Socket: sock.Path(), Mounts: mounts, Limits: spec.Limits, ProxyToken: token,
		})
		if err != nil {
			sinks.Close(ctx)
			return nil, err
		}
	}

	start := time.Now()
	started := map[string]any{
		"runtime": spec.Runtime, "runtime_version": desc.RuntimeVersion, "command": spec.Command, "args": args,
		"dir": spec.Dir, "interactive": spec.Interactive, "runner_version": spec.RunnerVersion, "host": hostname(),
	}
	if spec.Wall != nil {
		started["wall"] = spec.Wall.Name()
		started["image"] = spec.Image
	}
	if len(spec.Labels) > 0 {
		started["labels"] = spec.Labels
	}
	write(event.RunStarted, started)
	applied := map[string]any{"mode": string(pol.Policy.Egress.Mode), "allow": allow, "source": pol.Source}
	if pol.Source == "config" {
		applied["digest"] = pol.Digest
	}
	if spec.Declared != nil {
		applied["declared"] = spec.Declared
	}
	if len(pol.Policy.Egress.Paths) > 0 {
		applied["paths"] = pol.Policy.Egress.Paths
	}
	if len(held.Uses) > 0 {
		uses := make([]map[string]any, len(held.Uses))
		for i, u := range held.Uses {
			uses[i] = map[string]any{"name": u.Name, "hosts": u.Hosts, "scheme": u.Scheme}
			if u.Paths != nil {
				uses[i]["paths"] = u.Paths
			}
		}
		applied["credentials"] = uses
	}
	if hosts := px.Terminated(); len(hosts) > 0 {
		applied["terminated"] = hosts
	}
	write(event.PolicyApplied, applied)

	logs := func(stream string) func([]byte) {
		return func(b []byte) { write(event.RunLog, map[string]any{"stream": stream, "bytes": encode(b)}) }
	}
	output := func(r map[string]any) { records(descriptor.Record{Source: descriptor.SourceOutput, Record: r}) }
	if desc.Sources.Output == nil {
		output = nil
	}
	proc := &process{grace: spec.StopGrace, command: launch.Command, args: launch.Args, env: launch.Env, dir: launch.Dir, stdin: spec.Stdin, stdout: spec.Stdout, stderr: spec.Stderr, logs: logs, output: output}
	stop := heartbeat(ctx, spec.Heartbeat, start, write)
	// The limit ends the runtime and nothing else: the sinks and the wall are closed on
	// the caller's context, as after any exit.
	limited, cancelLimit := ctx, context.CancelFunc(func() {})
	if spec.Timeout > 0 {
		limited, cancelLimit = context.WithTimeoutCause(ctx, spec.Timeout, errTimeout)
	}
	var exit exitStatus
	if spec.Interactive {
		exit, err = proc.runPTY(limited)
	} else {
		exit, err = proc.runPipes(limited)
	}
	// A runtime that exited with 0 as the limit fell finished; the limit was not why.
	timedOut := exit.code != 0 && errors.Is(context.Cause(limited), errTimeout)
	cancelLimit()
	stop()
	closeSocket()
	if err != nil {
		sinks.Close(ctx)
		return nil, err
	}
	state := "failed"
	if exit.code == 0 {
		state = "succeeded"
	}
	exited := map[string]any{"state": state, "exit_code": exit.code, "duration_ms": time.Since(start).Milliseconds()}
	if exit.signal != "" {
		exited["signal"] = exit.signal
	}
	if timedOut {
		exited["reason"] = "timeout"
		spec.Report(fmt.Sprintf("the runtime was stopped at the limit of %s", spec.Timeout))
	}
	write(event.RunExited, exited)
	closeCtx, cancel := context.WithTimeout(context.Background(), closeWait)
	defer cancel()
	if err := sinks.Close(closeCtx); err != nil {
		spec.Report("closing the sinks: " + err.Error())
	}
	res := &Result{RunID: runID, Dir: dir, ExitCode: exit.code, Signal: exit.signal, State: state, TimedOut: timedOut}
	if posts != nil {
		res.Undelivered = posts.Undelivered()
	}
	return res, nil
}

// withDefaults fills what the spec left empty.
func withDefaults(spec Spec) Spec {
	if spec.Dir == "" {
		spec.Dir, _ = os.Getwd()
	}
	if spec.RunsDir == "" {
		spec.RunsDir = filepath.Join(spec.Dir, ".qory", "runs")
	}
	if spec.Env == nil && spec.Wall == nil {
		spec.Env = os.Environ()
	}
	if spec.StopGrace == 0 {
		spec.StopGrace = DefaultStopGrace
	}
	if spec.Heartbeat == 0 {
		spec.Heartbeat = 30 * time.Second
	}
	if spec.Stdin == nil {
		spec.Stdin = os.Stdin
	}
	if spec.Stdout == nil {
		spec.Stdout = os.Stdout
	}
	if spec.Stderr == nil {
		spec.Stderr = os.Stderr
	}
	if spec.Report == nil {
		stderr := spec.Stderr
		spec.Report = func(line string) { fmt.Fprintln(stderr, "qory run:", line) }
	}
	if spec.RunnerVersion == "" {
		spec.RunnerVersion = "dev"
	}
	return spec
}

// placeholders are the variables a program wants set before it starts, with a value
// that is no credential.
func placeholders(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + "=" + credential.Placeholder
	}
	return out
}

// environment is the session's environment: base with the runner's variables set,
// replacing any of the same names.
func environment(base []string, sets ...[]string) []string {
	var extra []string
	for _, s := range sets {
		extra = append(extra, s...)
	}
	names := map[string]bool{}
	for _, kv := range extra {
		name, _, _ := strings.Cut(kv, "=")
		names[name] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !names[name] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// heartbeat emits run.heartbeat every interval until the returned function is called.
func heartbeat(ctx context.Context, interval time.Duration, start time.Time, write func(string, any)) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				write(event.RunHeartbeat, map[string]any{"elapsed_seconds": int(time.Since(start).Seconds()), "interval_seconds": int(math.Ceil(interval.Seconds()))})
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() { close(done); <-stopped }
}

func filter(c *webhook.Config) []string {
	if len(c.Events) == 0 {
		return []string{"*"}
	}
	return c.Events
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// ErrNotStarted wraps a failure to start the program, so a caller tells it from the
// runtime's own failure.
var ErrNotStarted = errors.New("the runtime did not start")
