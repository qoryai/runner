# Qory runner

The security boundary around one coding agent session. The runner starts the agent on a
composed harness, pins a policy that can only narrow what the binary allows, observes and
enforces egress through a loopback proxy it owns, keeps the runner's credentials out of
the session, heartbeats while the session runs, and reports the session's output and its
own observations as CloudEvents: to files always, to a webhook when one is configured.

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
CloudEvent per line, beside `output.log`, the session's bytes. Two optional files in
`~/.config/qory` change what the runner does. `policy.yaml` is the ceiling on what the
runtime may reach; without it everything is allowed and recorded:

```yaml
version: 1
egress:
  mode: enforce                         # or observe: record everything, deny nothing
  allow: [api.anthropic.com, "*.github.com"]
```

`webhook.yaml` posts every event somewhere as well, signed; with it configured the
runner does not start unless the receiver answers, and `qory run --local` runs with the
files alone:

```yaml
version: 1
url: https://example.com/qory/events
secret: sixteen-characters-at-least    # a placeholder; the real one is not in a repository
```

A denied connection is one `403` to the runtime and one `ai.qory.run.egress` event with
`decision: denied`; the session goes on. Nothing the runner does ends a session.

**From Go.** The module is a library with one entry point. A caller builds a
[`session.Spec`](session/session.go), the program to start and how, and gets a
[`session.Result`](session/session.go) back when the runtime has exited and the sinks
are flushed:

```go
res, err := session.Run(ctx, session.Spec{
	Runtime:     "claude",                        // names the runtime descriptor
	Command:     "claude",
	Args:        []string{"--settings", settings, "-p", "Reply pong."},
	PolicyPath:  policy,                          // "" is observe everything
	WebhookPath: webhook,                         // "" is files only
	Forwarder:   []string{exe, "forward"},        // the hook command, see below
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

**Receiving the webhook.** A receiver is any HTTPS endpoint that verifies the
`X-Qory-Signature-256` header over the raw body, deduplicates on the event `id` and
answers `2xx`; the contract's [webhook section](contracts/runner/v1/README.md#the-webhook)
has the rules and `internal/receiver` is a worked example.

## Layout

| Path | What |
|---|---|
| `contracts/runner/v1/` | the contract: the documents, a JSON schema each, the runtime descriptors and the fixtures. [Its README](contracts/runner/v1/README.md) is the specification |
| `contracts/` | the Go package that embeds the contract and validates every fixture |
| `session/` | the session runner: `session.Run` takes a launch spec, a policy path and a webhook path and returns the exit status; `session.Forward` is the hook forwarder behind it |
| `internal/` | what the layers share: `policy`, `proxy`, `event`, `sink`, `webhook`, `descriptor`, `socket`, `chunk`; and `receiver`, the receiving side of the webhook the tests run the sink against, a worked example of the contract's receiving rules |
| `node/` | the node runner, not built yet: it will register, heartbeat, take a dispatched task, hold the run's credentials and start a session through `session` |

`qory run` calls `session.Run` with the spec it builds from the composed home and the
launch template, and `session.Forward` from the hook command it installs.

## Two invariants

`qory` imports `runner`; `runner` imports nothing of `qory`. The runner knows nothing of
stacks, modules, homes or reports; it takes a spec. Inside the module, `node` imports
`session` and `session` never imports `node`.

A policy, a webhook configuration and a descriptor select among what the binary does.
They never add to it. New behaviour arrives only in a release.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). `go test ./...` validates every fixture against
the schemas.

## Licence

Apache License, Version 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). Qory is a
trademark of 8wonders GmbH; see [TRADEMARKS.md](TRADEMARKS.md).
