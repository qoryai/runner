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
res, err := session.Run(ctx, session.Spec{
	Runtime:   "claude",                          // names the runtime descriptor
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
	Runtime: "claude",
	Command: "claude",                            // a path inside the image
	Args:    []string{"-p", "Reply pong."},
	Env:     []string{"ANTHROPIC_API_KEY=" + key}, // under a wall, nothing else goes in
	Dir:     checkout,                            // the workspace, mounted at its own path
	Mounts:  []wall.Mount{{Path: home, ReadOnly: true}}, // what else of this machine it sees
	Image:   "example.com/agent:1",               // yours: the runtime and the toolchain
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

## Layout

| Path | What |
|---|---|
| `contracts/runner/v1/` | the contract: the documents, a JSON schema each, the runtime descriptors and the fixtures. [Its README](contracts/runner/v1/README.md) is the specification |
| `contracts/` | the Go package that embeds the contract and validates every fixture |
| `session/` | the session runner: `session.Run` takes a launch spec, with the policy, the webhook and the wall as values, and returns the exit status; `session.Forward` is the hook forwarder behind it |
| `wall/` | the wall: the adapter interface, the Docker adapter, and `wall.Relay`, the one peer an enclosure reaches. `wall/walltest` is the conformance suite every adapter passes before it ships |
| `internal/` | what the layers share: `policy`, `proxy`, `event`, `sink`, `webhook`, `descriptor`, `socket`, `chunk`; and `receiver`, the receiving side of the webhook the tests run the sink against, a worked example of the contract's receiving rules |
| `node/` | the node runner, not built yet: it will register, heartbeat, take a dispatched task, hold the run's credentials and start a session through `session`, behind a wall |

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
