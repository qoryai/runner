# Qory runner

The security boundary around one coding agent session. The runner starts the agent on a
composed harness, pins a policy that can only narrow what the binary allows, observes and
enforces egress through a loopback proxy it owns, keeps the runner's credentials out of
the session, heartbeats while the session runs, and reports the session's output and its
own observations as CloudEvents: to files always, to a webhook when one is configured.

This is a Go module, `github.com/qoryai/runner`, imported by the [`qory`](https://github.com/qoryai/qory)
command, which ships `qory run` and `qory receive` in front of it. It has no command of
its own.

## Layout

| Path | What |
|---|---|
| `contracts/runner/v1/` | the contract: the documents, a JSON schema each, the runtime descriptors and the fixtures. [Its README](contracts/runner/v1/README.md) is the specification |
| `contracts/` | the Go package that embeds the contract and validates every fixture |
| `session/` | the session runner: `session.Run` takes a launch spec, a policy path and a webhook path and returns the exit status; `session.Forward` is the hook forwarder behind it |
| `receiver/` | the reference webhook receiver behind `qory receive`: a handler that verifies, deduplicates and stores, and a file store |
| `internal/` | what the layers share: `policy`, `proxy`, `event`, `sink`, `webhook`, `descriptor`, `socket`, `chunk` |
| `node/` | the node runner, not built yet: it will register, heartbeat, take a dispatched task, hold the run's credentials and start a session through `session` |

`qory run` calls `session.Run` with the spec it builds from the composed home and the
launch template, and `session.Forward` from the hook command it installs. `qory receive`
serves a `receiver.Handler` over a `receiver.File`.

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
