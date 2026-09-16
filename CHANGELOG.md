# Changelog

Every release of the runner, newest first, in the shape of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The version numbers follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html); before 1.0 a minor
release may change what an existing document does, and says so under Upgrading.

## [0.1.0] - 2026-09-16

### Added

- The runner contract, `contracts/runner/v1/`: the policy document, the webhook
  configuration, the event types with a JSON schema per data type, the batch and
  signature rules, the runtime descriptor schema, and the Claude Code descriptor with its
  fixtures. The `contracts` package embeds the directory and its tests validate every
  fixture against the schemas.
- The session runner, `session.Run`: the policy read once and pinned, or observe with
  no file; the loopback proxy in observe and enforce modes, one `run.egress` event per
  connection and a 403 for a denied one; the session on a pseudo-terminal when
  interactive and on pipes otherwise, its output chunked into `run.log`; the runtime's
  descriptor mapping its JSON lines and its hook calls to session events; the hook
  forwarder installed into the settings the launch passes, reporting over a local
  socket; a heartbeat; `run.exited` as the result; `events.jsonl` and `output.log`
  under `.qory/runs/<id>/`.
- The webhook sink: a ping the receiver must accept before the run starts when a
  webhook is configured, and none when it is not; signed batches with a delivery id;
  retries with backoff; 410 as stop; what is not accepted spooled under `undelivered/`
  and counted at exit.
- The `receiver` package behind `qory receive`: a handler that verifies the signature
  in constant time, deduplicates on event id and appends to a file that remembers its
  ids across restarts, and `LoadWebhook`, so one webhook file configures both ends.
- The contract states that no declaration and an empty declaration differ: no list
  leaves the policy's allow list as it is, an empty list under enforce reaches nothing.
  It names the policy's `egress.allow` grammar as the one definition of a declared host,
  which the harness contract copies.
