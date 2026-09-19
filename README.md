# Qory runner

The security boundary around one coding agent session. The runner starts the agent on a
composed harness, pins a policy that can only narrow what the binary allows, observes and
enforces egress through a proxy it owns, keeps the runner's credentials out of the
session, heartbeats while the session runs, and reports the session's output and its own
observations as CloudEvents: to files always, to a webhook when one is configured.
Optionally it starts the agent behind a wall, a container with no route out except to
that proxy, so a program that ignores the proxy reaches nothing.

This is a Go module, `github.com/qoryai/runner`, imported by the [`qory`](https://github.com/qoryai/qory)
command, which ships `qory run` in front of it. It has no command of
its own.

## Using it

**From the command.** Install [`qory`](https://github.com/qoryai/qory), compose a harness,
and run the runtime through it instead of starting it yourself:

```sh
qory harness compose
qory run                                # the composed runtime, at your terminal
qory run claude -- -p "Reply pong"      # one headless turn
```

Every connection the runtime makes goes through the runner's proxy and is recorded; the
session is written to `.qory/runs/<id>/` in the checkout as `events.jsonl`, one
CloudEvent per line, beside `output.log`, the session's bytes. One optional file,
`~/.config/qory/runner.yaml`, never in a repository, changes what the runner does.
`egress` is the ceiling on what the runtime may reach; without it everything is allowed
and recorded. `webhook` posts every event somewhere as well, signed; with it configured
the runner does not start unless the receiver answers, and `qory run --local` runs with
the files alone:

```yaml
apiVersion: qory.dev/v1alpha1
egress:
  mode: enforce                         # or observe: record everything, deny nothing
  allow: [api.anthropic.com, "*.github.com"]
webhook:                                # optional
  url: https://example.com/qory/events
  secret: sixteen-characters-at-least   # or QORY_WEBHOOK_SECRET in the environment
```

A denied connection is one `403` to the runtime and one `ai.qory.run.egress` event with
`decision: denied`; the session goes on. Nothing the runner does ends a session.

The proxy sees only programs that honour it. A `wall` section, or
`qory run --wall docker --image <image>`, starts the runtime in a container with no
route out except to the proxy, so the rest fails instead of going unseen:

```yaml
wall:
  adapter: docker
  image: example.com/agent:1            # yours: the runtime and your toolchain
  env: [ANTHROPIC_API_KEY]              # names; nothing else of your environment goes in
```

**From Go.** The module is a library with one entry point. A caller builds a
[`session.Spec`](session/session.go), the program to start and how, and gets a
[`session.Result`](session/session.go) back when the runtime has exited and the sinks
are flushed:

```go
rt, err := catalog.Lookup("claude", "")           // a runtime by its name, see below
res, err := session.Run(ctx, session.Spec{
	Runtime:   rt,
	Command:   "claude",
	Args:      []string{"--settings", settings, "-p", "Reply pong."},
	Policy:    &session.Policy{Version: 1, Egress: session.PolicyEgress{Mode: "enforce", Allow: hosts}},
	Webhook:   nil,                               // files only; a *session.Webhook posts as well
	Forwarder: []string{exe, "forward"},          // the hook command, see below
})
if err != nil {                                   // the run did not start
	return err
}
os.Exit(res.ExitCode)                             // the runtime's status; res.Dir is the record
```

`Runtime` is the program as the runner needs to know it, a
[`runtimes.Runtime`](runtimes/runtimes.go): how its launch is prepared, what its records
mean, how it is asked to leave. `catalog.Lookup(name, dir)` resolves a name: a
descriptor `<name>.yaml` in `dir`, which is how a machine describes a runtime nothing
ships for, then the contract's own, Claude Code's today, and for a name with neither a
bare runtime, run and recorded with no session events. A program that needs code of its
own implements the interface, and `runtimes/runtimetest` holds it to the same checks.

The spec's `Declared` is the egress the harness declared, which the policy narrows;
nil means no declaration. `Interactive` runs the session on a pseudo-terminal, else on
pipes, where the runtime's structured output is read. The command named in `Forwarder`
is installed as the runtime's hook and must call `session.Forward(ctx, os.Stdin)`, which
hands the hook's input to the run over a socket named in the environment. The whole
sequence, every event type and every file is in the
[contract](contracts/runner/v1/README.md); [`session/example_test.go`](session/example_test.go)
is the example above, compiled with the tests.

**Behind a wall.** Without one, enforcement is cooperative: a program that ignores the
proxy variables is not seen. A [`wall.Wall`](wall/wall.go) in the spec starts the runtime
in an enclosure whose only route out leads to the proxy; the session runner, the policy
and the webhook's secret stay outside, and the run's record is read-only inside. The Docker adapter uses the `docker`
command and whatever engine it reaches:

```go
res, err := session.Run(ctx, session.Spec{
	Runtime: rt,
	Command: "claude",                            // a path inside the image
	Args:    []string{"-p", "Reply pong."},
	Env:     []string{"ANTHROPIC_API_KEY=" + key}, // under a wall, nothing else goes in
	Dir:     checkout,                            // the workspace, mounted at its own path
	Mounts:  []wall.Mount{{Path: home, ReadOnly: true}}, // what else of this machine it sees
	Image:   "example.com/agent:1",               // yours: the runtime and the toolchain
	Limits:  wall.Limits{Memory: "8g", ShmSize: "2g"},  // what the agent may use; zero is the engine's default
	Timeout: 5 * time.Hour,                       // the runtime is stopped at it; run.exited says so
	Labels:  map[string]string{"issue": "77"},    // the caller's names for the run, in run.started
	Wall: &wall.Docker{
		Helper:    linuxBuild,                    // a static Linux build of this program
		RelayArgs: []string{"relay"},             // the mode of it that calls wall.Relay
	},
	Forwarder: []string{wall.HelperPath, "forward"},
	Events:    os.Stdout,                         // every event as a JSON line, as well
})
```

The helper is the caller's own binary, built static for Linux and mounted read-only into
the enclosure, where it runs as the relay the agent reaches the proxy through and as the
hook forwarder; the wall needs no image of its own. What every wall guarantees, what
crosses it and its limits are the contract's [wall section](contracts/runner/v1/README.md#the-wall),
and the [`wall/walltest`](wall/walltest/walltest.go) suite checks the list from inside
the enclosure. `Events` is any stream: a run with no receiver is followed on standard
output with the lines `events.jsonl` holds.

**Receiving the webhook.** A receiver is any HTTPS endpoint that verifies the
`X-Qory-Signature-256` header over the raw body, deduplicates on the event `id` and
answers `2xx`; the contract's [webhook section](contracts/runner/v1/README.md#the-webhook)
has the rules and `internal/receiver` is a worked example.

## The node runner

A node runner is a machine that runs agents for someone else: it takes work, starts each
run behind a wall, and reports what happened. It is two halves, and one of them ships.

| Half | What it does | State |
|---|---|---|
| **The wall** | starts the agent in a container with no route out except to the session runner's proxy; the policy, the record and the webhook's secret stay on the node | ships in 0.2.0, as `qory run --wall docker` |
| **The fleet layer** | registers the node with a control plane, heartbeats, claims work, and gets the run's policy in the answer | not built; no command starts it, and nothing here describes it as if one did |

So today a node is a machine with Docker on which `qory run --wall docker` is started,
by a person, a CI job or a scheduler of your own. Reporting to a control plane already
works without the fleet layer, because it is the same webhook every run has: the receiver
creates the run from the first event it sees.

### How it works

```
 the node                                              elsewhere
┌─────────────────────────────────────────────────────┐
│ qory run: the session runner, outside the wall      │
│   policy ─▶ proxy ─▶ decides, records, dials ───────┼──▶ the hosts the policy allows
│   events ─▶ .qory/runs/<id>/events.jsonl            │
│          └▶ webhook, signed ────────────────────────┼──▶ a receiver: your control plane
│      ▲ proxy      ▲ hooks       ▲ terminal          │
│══════╪════════════╪═════════════╪════ the wall ═════│
│  ┌───┴───┐   ┌────┴─────────────┴────────────────┐  │
│  │ relay │◀──│ the agent's container: your image │  │
│  └───────┘   │ no route · no resolver · not root │  │
│              └───────────────────────────────────┘  │
└─────────────────────────────────────────────────────┘
```

The agent's container is on a network with no route out. The one peer it reaches is the
relay, which copies a fixed port to the proxy and decides nothing. Every connection is
therefore the proxy's to decide and record, and a program that ignores the proxy reaches
nothing. The checkout and the composed home are mounted at their own paths, the run's
record read-only; nothing else of the node is visible inside. What every wall guarantees
is the contract's [wall section](contracts/runner/v1/README.md#the-wall), and the
[conformance suite](wall/walltest/walltest.go) checks it from inside the container in
this repository's CI.

### Starting it

You need the `docker` command with an engine behind it, [`qory`](https://github.com/qoryai/qory)
0.7.0 or later, and an image of yours that holds the runtime and your toolchain; the
wall builds none. A minimal one for Claude Code:

```dockerfile
FROM node:22-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* && npm install -g @anthropic-ai/claude-code
# The container runs as the node's user, who has no home in the image.
ENV HOME=/tmp
```

```sh
docker build -t agent:1 .
cd your-checkout && qory harness compose
export ANTHROPIC_API_KEY=...            # a key, or CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`
qory run --wall docker --image agent:1 --env ANTHROPIC_API_KEY claude -- -p "Reply pong"
```

The run is recorded in `.qory/runs/<id>/` as without a wall, `ai.qory.run.started` names
the wall and the image, and the exit status is the agent's. Two things differ by machine:

- **On Linux** `qory` mounts itself into the container as the relay and the hook
  forwarder, and the agent's hooks reach the runner.
- **On a Mac** the container cannot run the Mac's binary: download the Linux archive of
  the same `qory` release for your engine's architecture and name the binary as
  `wall.helper`. The hook socket does not cross the engine's virtual machine, so a walled
  run there has the log, the egress record and the structured output, and no hook events.

### Configuring it

One file on the node, `~/.config/qory/runner.yaml`, never in a repository, so a checkout
cannot set what it runs under. With it, a bare `qory run` is walled:

```yaml
apiVersion: qory.dev/v1alpha1
egress:                         # what the agent may reach; without it, everything, recorded
  mode: enforce                 # or observe: record everything, deny nothing
  allow: [api.anthropic.com, github.com, "*.githubusercontent.com"]
webhook:                        # how the node reports; without it, files only
  url: https://control-plane.example.com/qory/events
  secret: sixteen-characters-at-least   # or QORY_WEBHOOK_SECRET in the environment
wall:
  adapter: docker
  image: agent:1                # or --image
  env: [ANTHROPIC_API_KEY]      # names; the values come from qory run's environment
  user: "1000:1000"             # only where qory runs as root, which a wall refuses
  helper: /opt/qory/qory-linux  # only where qory is not a Linux build
  command: podman               # only for another command than docker
```

- **`egress`** is the policy. Hosts only: a name, or `*.` and a name for every host
  below it. When the harness's modules declare the hosts they reach, the agent reaches
  the declared hosts this list covers; the list is the ceiling.
- **Behind a wall the proxy is guarded.** It never dials link-local addresses, the cloud
  metadata service among them, and it dials the node itself only for a host this list
  names, not for one under a `*.` entry. A model endpoint or MCP server on the node is
  reached through the proxy by the node's host name, listed here; `localhost` inside the
  container is the container.
- **`webhook`** posts every event, signed, to a receiver. With it set the run does not
  start unless the receiver answers a ping, so a run meant to be observed is not run
  unobserved; `--local` runs with the files alone.
- **`wall.env`** is the whole of the node's environment that goes in, by name. The model
  credential is among it and is then the agent's; keeping it outside, injected by the
  proxy, is not built.
- `--wall none` runs once without the wall, `--wall docker --image ...` once with one on
  a node that has no `wall` section.

### What is not there yet

- The fleet layer: register, heartbeat, claim, the policy in the run start answer.
- Hook events on an engine inside a virtual machine, until the forwarder has a network
  transport through the relay.
- Git inside the container when the checkout is a git worktree, whose repository data
  lies outside the mounts, unless the run lists that directory among them.

## Layout

| Path | What |
|---|---|
| `contracts/runner/v1/` | the contract: the documents, a JSON schema each, the runtime descriptors and the fixtures. [Its README](contracts/runner/v1/README.md) is the specification |
| `contracts/` | the Go package that embeds the contract and validates every fixture |
| `session/` | the session runner: `session.Run` takes a launch spec, with the policy, the webhook and the wall as values, and returns the exit status; `session.Forward` is the hook forwarder behind it |
| `runtimes/` | the runtime: `runtimes.Runtime`, the interface between the runner and the program it runs, how a launch is prepared, what the program's records mean, how it is asked to leave. `Described` is a runtime written as a descriptor, `Bare` a program the runner runs and does not read, `runtimes/claude` Claude Code, `runtimes/catalog` a name resolved to one, and `runtimes/runtimetest` the conformance suite every runtime passes |
| `wall/` | the wall: the adapter interface, the Docker adapter, and `wall.Relay`, the one peer an enclosure reaches. `wall/walltest` is the conformance suite every adapter passes before it ships |
| `internal/` | what the layers share: `policy`, `proxy`, `event`, `sink`, `webhook`, `descriptor`, `socket`, `chunk`; and `receiver`, the receiving side of the webhook the tests run the sink against, a worked example of the contract's receiving rules |
| `node/` | the node runner's fleet layer, not built yet: it will register, heartbeat, take a dispatched task, hold the run's credentials and start a session through `session`, behind a wall ([§The node runner](#the-node-runner)) |

`qory run` calls `session.Run` with the spec it builds from the composed home and the
launch template, and `session.Forward` from the hook command it installs.

## Two invariants

`qory` imports `runner`; `runner` imports nothing of `qory`. The runner knows nothing of
stacks, modules, homes or reports; it takes a spec. Inside the module, `node` imports
`session` and `session` never imports `node`; `session` imports `wall` for the
interface, and only `wall/walltest` imports `session`.

A policy, a webhook configuration and a descriptor select among what the binary does.
They never add to it. New behaviour arrives only in a release.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). `go test ./...` validates every fixture against
the schemas.

## Licence

Apache License, Version 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). Qory is a
trademark of 8wonders GmbH; see [TRADEMARKS.md](TRADEMARKS.md).
