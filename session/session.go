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
	"strings"
	"sync"
	"time"

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
	// RunID is the run's id when a parent already made one; empty means a new one.
	RunID string
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
	// Undelivered is how many events the webhook did not accept.
	Undelivered int
}

// The environment variables the session gets from the runner.
const (
	EnvRunID  = "QORY_RUN_ID"
	EnvSocket = socket.Env
)

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
	}
	dir := filepath.Join(spec.RunsDir, runID)
	emit := event.NewEmitter(runID, nil)
	files, err := sink.NewFile(dir)
	if err != nil {
		return nil, err
	}
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
		if err := client.Ping(ctx, event.NewID(), []byte("["+string(body)+"]")); err != nil {
			sinks.Close(ctx)
			return nil, err
		}
		posts = sink.NewWebhook(client, dir, spec.Report)
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

	allow := pol.Narrow(spec.Declared)
	px, err := proxy.Listen(bind, pol.Policy.Egress.Mode, allow, func(d proxy.Decision) {
		decision := "denied"
		if d.Allowed {
			decision = "allowed"
		}
		write(event.RunEgress, map[string]any{"host": d.Host, "port": d.Port, "method": d.Method, "decision": decision, "mode": string(pol.Policy.Egress.Mode), "rule": d.Rule})
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
			Env:   environment(spec.Env, []string{EnvRunID + "=" + runID}),
			Proxy: px.Addr(), Socket: sock.Path(), Mounts: mounts,
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
	write(event.RunStarted, started)
	applied := map[string]any{"mode": string(pol.Policy.Egress.Mode), "allow": allow, "source": pol.Source}
	if pol.Source == "config" {
		applied["digest"] = pol.Digest
	}
	if spec.Declared != nil {
		applied["declared"] = spec.Declared
	}
	write(event.PolicyApplied, applied)

	logs := func(stream string) func([]byte) {
		return func(b []byte) { write(event.RunLog, map[string]any{"stream": stream, "bytes": encode(b)}) }
	}
	output := func(r map[string]any) { records(descriptor.Record{Source: descriptor.SourceOutput, Record: r}) }
	if desc.Sources.Output == nil {
		output = nil
	}
	proc := &process{command: launch.Command, args: launch.Args, env: launch.Env, dir: launch.Dir, stdin: spec.Stdin, stdout: spec.Stdout, stderr: spec.Stderr, logs: logs, output: output}
	stop := heartbeat(ctx, spec.Heartbeat, start, write)
	var exit exitStatus
	if spec.Interactive {
		exit, err = proc.runPTY(ctx)
	} else {
		exit, err = proc.runPipes(ctx)
	}
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
	write(event.RunExited, exited)
	closeCtx, cancel := context.WithTimeout(context.Background(), closeWait)
	defer cancel()
	if err := sinks.Close(closeCtx); err != nil {
		spec.Report("closing the sinks: " + err.Error())
	}
	res := &Result{RunID: runID, Dir: dir, ExitCode: exit.code, Signal: exit.signal, State: state}
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
