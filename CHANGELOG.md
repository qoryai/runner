# Changelog

Every release of the runner, newest first, in the shape of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The version numbers follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html); before 1.0 a minor
release may change what an existing document does, and says so under Upgrading.

## [0.4.0] - 2026-09-21

### Upgrading

- The server replaces the webhook. `session.Spec.Webhook` is gone; `Spec.Server` is a
  `*session.Server` with `Version`, `URL`, `AccessKey` and `Secret`, the document of
  `server.schema.json`: the server's origin and nothing after it, the key the server
  knows the runner by, and the secret that signs. `ResendSpec.Server` likewise. For
  `qory`, the file's `server` section replaces `webhook`, and `QORY_SERVER_SECRET`
  replaces `QORY_WEBHOOK_SECRET`. Where a webhook took an endpoint, a server is an
  origin: the runner fetches `/.well-known/qory-configuration` under it, signed, and
  learns the events URL and the filter there.
- A receiver of your own implements the contract's server section: discovery, the
  events endpoint, and the two headers on every request, `X-Qory-Access-Key` and
  `X-Qory-Contract-Version`. A `GET` is signed over a canonical string with a
  timestamp; a `POST` over its body as before. Every authentication failure is `401`
  with `{"error":"unauthorized"}`. `internal/receiver` is now the public package
  `receiver`; its `Handler` takes `Keys`, a lookup of an access key to its secrets,
  in place of `Secret`, and is replayed against `contracts/runner/v1/fixtures/signed/`.
- The contract is `v1` revision 1: `X-Qory-Contract-Version: 1` on every request and
  `contract_version: 1` in the ping's data, required. A runner that sends neither is
  revision 0.
- `ai.qory.run.policy_applied`: `declared` is renamed `harness_hosts`, and the narrowing
  is gone: `allow` is the policy's list, and a host the harness declared that the policy
  does not cover is denied under `enforce` like any other. `source` gains `fetched`,
  with `url` and `run_configuration`.
- `ai.qory.run.egress` requires `outcome`: `connected`, `dial_failed` or `refused`.
- A name that resolves to the runner's own machine or to the link-local range, through
  a guarded proxy, is now answered `403` and recorded as denied with the rule
  `wall:own-address`; it was a `502` recorded as allowed.
- `runtimes.Runtime` gains `Headless(args []string) bool`, so a `Runtime` of one's own
  adds the method; returning false keeps what the caller asked for.
- A receiver accepts one more type, `ai.qory.run.resized`, with `cols` and `rows`, and
  `terminal`, an object of `cols` and `rows`, on `ai.qory.run.started` when
  `interactive` is true. A receiver that replays the terminal takes the size from
  `run.started` and changes it at each `run.resized`. A receiver that cut the terminal
  stream into lines itself finds a chunk is now a redraw, not a line: it splits on
  nothing, or on what it wants to.

### Added

- The server, `session.Server`: the runner as a client of one server contract. At start
  it fetches the server's configuration document with a signed `GET`, posts the ping and
  every batch to the events URL the document names, with the access key beside the
  signature, and when the document names a `run` section, fetches the run configuration
  for the run's `forge` and `repository` labels and takes its `security_policy` as the
  run's policy, source `fetched`. A fetch that fails, a document the schema refuses or
  a ping not accepted is no run, and the error names the URL and the status. `--local`
  contacts no server.
- Reload. Every answer to a batch may carry `X-Qory-Configuration` and
  `X-Qory-Run-Configuration`; a digest that differs from what the run holds has the
  runner fetch the document again. A new run configuration takes effect for new
  connections at once, the record gets a second `ai.qory.run.policy_applied` at the
  sequence where it took effect, a tunnel open to a host the new policy denies is
  closed and recorded as a denied `ai.qory.run.egress` with `outcome: refused`, and a
  host the new policy terminates TLS for is terminated on its next connection. A new
  configuration document swaps the events URL and the filter for later batches. One
  reload runs at a time; answers during one are coalesced into the next.
- A reload is as strict as a start. The fetched policy's `credentials` are resolved
  again, as at the start, and go to the proxy in one step with the policy; a terminated
  connection whose credential changed is closed, so no later request carries one the
  new policy does not select. What a start refuses, a credential that does not resolve
  or one selected without a wall, fails the reload: it is reported and the policy in
  force stays. A run configuration the server answered is fetched once per answered
  digest: a document that is the one in force changes nothing, and one that was
  refused is not asked for again until the answer changes; a fetch the server did not
  answer is tried again on the next answer.
- The client follows no redirect, its own or a caller's `http.Client` alike: a 3xx is
  a status like any other, no run at discovery and a retry for a delivery, so the
  access key, the signature and the timestamp never reach a host a redirect names.
- `receiver.Handler` serves the three endpoints with `Configuration`,
  `RunConfiguration`, `Now`, `Window`, `EventsPath` and `RunPath`, answers the digest
  headers, verifies in constant time, looks a key up only after its shape is checked
  and logs nothing about the headers.
- The contract: `server.schema.json`, `configuration.schema.json`,
  `run-configuration.schema.json`; fixtures under `fixtures/server/`,
  `fixtures/configuration/`, `fixtures/run-configuration/` and `fixtures/signed/`, the
  last with real signatures under the published key `ak_f1xt0re000000000` and secret
  `fixture-secret-not-a-real-one`; the section §The server with the canonical string,
  its known answers, the failure rule, the documents, the reload rules and the modes
  of a run; `contracts.Revision`.
- The pseudo-terminal's size in the record, so a replay can lay the redraws of a
  full-screen program over each other: `terminal` on `ai.qory.run.started`, the columns
  and rows the runtime started on, the runner's own terminal's when it has one and 80 by
  24 otherwise; and `ai.qory.run.resized` with the new size at the sequence where the
  runner's terminal was resized and the pseudo-terminal followed, so the chunks after it
  were drawn on the new size. Neither on pipes. Behind the docker wall a resize reaches
  the container's terminal: the docker command runs on the runner's pseudo-terminal,
  takes the resize signal and resizes the container's. The schema
  `events/run.resized.schema.json`; the recorded run under `fixtures/run/` carries both.
- `egress.deny` in the policy: hosts the session may not reach, in `allow`'s grammar,
  in either mode. The proxy decides it first, after the wall's guard and before the
  mode and the allow list: a host an entry covers is refused with a `403`, not
  dialled, recorded as denied with the entry as its rule, under `observe` as under
  `enforce`, whatever `allow` says of it; `allow: ["*.example"]` with
  `deny: ["tracker.example"]` denies `tracker.example` and reaches `api.example`.
  Observe records every connection and denies only what `deny` names. A reload
  carries the list and closes an open tunnel to a host the new one names.
  `ai.qory.run.policy_applied` carries `deny` beside `allow`, the policy's entries.
  `session.PolicyEgress.Deny`; `Under` keeps the deny lists of a policy and its
  ceiling both, whatever their modes, since a deny narrows. Fixtures
  `fixtures/policy/observe-deny.yaml` and `fixtures/run-configuration/observe-deny.json`.
  Revision 1 of the contract is amended in place; it had not shipped.
- `headless` in the descriptor: `args`, the arguments that mean the runtime runs
  without an interface. When one of them is among the arguments the runtime is started
  with, the session runs on pipes even at a terminal, exactly as if the caller had
  asked for that: `ai.qory.run.started` carries `interactive: false` and no `terminal`,
  and the descriptor's `output` source is read. A short argument matches the whole
  token, a long one the token or its `--name=value` form, and nothing else is
  inferred; absent, the caller alone decides. Runtimes differ in how they say "no
  interface", so the descriptor defines the inference and not the command. The Claude
  Code descriptor names `-p` and `--print`, so `claude -p "…"` at a terminal is a
  headless session with no flag to say so. `session.Spec.Interactive` keeps its
  meaning, what the caller has; `runtimes.Runtime.Headless` is what the runtime says
  of the arguments, false from a bare runtime. The invalid fixture
  `descriptor-headless-empty.yaml`.

### Changed

- The terminal stream is chunked at 4096 bytes or at a quiet gap of 50 ms after the
  runtime's last write, whichever comes first, never inside a multibyte character, and a
  line break no longer cuts it. A full-screen program redraws on every keypress, and cut
  at line breaks an interactive session was thousands of `ai.qory.run.log` chunks of a
  few bytes each; now one redraw is one chunk. A stream that never pauses is cut at 4096
  bytes, and what is held when the runtime exits is the last chunk. Pipes keep the rule
  of one line or 4096 bytes. `chunk.NewTerminal` is the writer; `chunk.New` is the
  pipes'.

- The proxy records a connection once its outcome is known: an allowed connection when
  the dial succeeded or failed, a denied one at once. A request inside a terminated
  connection is recorded when its response's headers arrive.
- Behind a wall a run with a server always has its authority, made at the start and
  given to the enclosure, so a reload that brings path rules or credentials can
  terminate the hosts concerned. Without a wall the proxy cannot hold path rules: the
  hosts a reloaded policy holds to paths are taken out of its allow list, denied under
  `enforce` and recorded like any other, the event carries no `paths`, and a report
  line says why.
- `receiver.MaxBody` is 2 MiB, twice the size the contract cuts a batch at, since it
  is what the handler reads before it has verified anything; it was 16 MiB.
- The secret of a server document, and the key, never appear in an event or a log; the
  ping and the run's record hold the events URL and the run configuration URL only.

### Removed

- `webhook.schema.json`, `fixtures/webhook/`, `session.Webhook`, `Spec.Webhook`,
  `ResendSpec.Webhook`, `internal/webhook` and `internal/receiver`.
- The narrowing of the allow list by the harness's declared hosts, and
  `policy.Loaded.Narrow`.

## [0.3.0] - 2026-09-19

### Upgrading

- A caller in Go resolves the runtime before the run: `session.Spec.Runtime` is a
  `runtimes.Runtime`, not a name, and `Spec.Descriptors` is gone. Where a spec had
  `Runtime: "claude"` and `Descriptors: dir`, call `catalog.Lookup("claude", dir)` from
  `github.com/qoryai/runner/runtimes/catalog` and give the spec what it returns. A name
  nothing describes was an error and is now a bare runtime, run and recorded with no
  session events.
- A receiver that requires `runtime_version` in `ai.qory.run.started` no longer finds it
  for a bare runtime; for a described one it is there as before.
- Nothing else changes for a run that uses none of what this release adds: a run whose
  policy selects no credential and has no path rule makes no authority and terminates no
  TLS, and the events it produces only gained optional fields.

### Added

- `session.Spec.Timeout`: a time limit for the runtime. At the limit it is stopped as
  the context ending stops it, `ai.qory.run.exited` carries `reason: timeout`, and
  `Result.TimedOut` is set.
- `runtimes.Runtime`: the boundary between the runner and the program it runs, as
  `wall.Wall` is for an enclosure. A runtime says how a launch is prepared, what its
  records mean and how it is asked to leave; the session package knows no program.
  `runtimes.Described` is a runtime written as a descriptor, `runtimes.Bare` a program
  the runner runs and does not read, `runtimes/claude` Claude Code, and
  `runtimes/catalog.Lookup` resolves a name: the machine's descriptor, the contract's,
  or bare, so any program runs behind a wall. `runtimes/runtimetest` is the conformance
  suite, `Conforms` and `Replays`.
- The descriptor's `stop` section, `signal` and `grace`: how a runtime is asked to
  leave. A run's own `StopSignal` and `StopGrace` override it.
- `session.Spec.StopSignal` and `Spec.StopGrace`: how the runner stops a runtime, at the
  limit or when its context ends. The signal is one of SIGTERM, SIGINT, SIGHUP, SIGQUIT,
  SIGUSR1 and SIGUSR2, SIGTERM unless named, since a runtime may close its session on one
  and drop it on another; the grace is the time until SIGKILL, ten seconds unless named.
  `session.CheckStopSignal` is the check.
- `session.Spec.Labels`: the caller's own names for the run, reported as `labels` in
  `ai.qory.run.started` and nowhere else. At most 16, keys of `a-z`, `0-9`, `_`, `.`
  and `-`, values of at most 256 bytes.
- `session.Spec.Limits` and `wall.Limits`: processors, memory, processes and the size
  of `/dev/shm` for the agent's container, as `--cpus`, `--memory`, `--pids-limit` and
  `--shm-size` with the Docker adapter. The relay gets none.
- `session.ReadPolicy` and `Policy.Under`: a command reads a run's own policy file and
  puts it under the machine's, which it can only narrow.

- Credentials the session never holds. `Spec.Credentials` are the machine's: a token
  from a variable of the runner's environment, from a file, or from an adapter, a
  program of the machine's that knows one kind of host and prints, as
  `credential.schema.json`, the token, its expiry, and the hosts, the scheme and the
  paths it is for. A policy's new `credentials` selects among them by name, with an
  argument for an adapter, and defines none. Behind a wall the proxy sets each on the
  requests to its hosts; the enclosure gets placeholders, never a token. An adapter is
  asked again before its token expires and when a host answers 401.
- Path rules: `egress.paths` in the policy, and the `paths` of a credential. Of a host
  with paths the run reaches those and no other, so a repository's credential does not
  open another organization's on the same host. A path that could be read two ways is
  denied in either mode.
- TLS termination, for the hosts a credential is for and the hosts with path rules, and
  no other: the proxy answers as the host with a certificate of an authority made for
  the run, whose key never leaves the runner's memory. `wall.Launch.CA` gives a wall the
  certificate; the Docker adapter shows the enclosure one bundle, the image's own
  authorities and the run's, and sets `SSL_CERT_FILE`, `GIT_SSL_CAINFO`,
  `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE`, or `Docker.CAEnv`.
  `ai.qory.run.policy_applied` lists `credentials`, `paths` and the `terminated` hosts,
  and on a terminated host `ai.qory.run.egress` is one event per request with
  `request_method`, `path`, `path_rule` and `credential`.
- Work in the background, in the Claude Code descriptor: `ai.qory.session.turn_finished`
  and `ai.qory.session.subagent_finished` carry `background_tasks`, the runtime's own
  list of what is still running, each with its id, type, status, description, and a
  shell's command or a subagent's type. A background command's start was already a
  `tool_started` with `run_in_background` in its input. The runtime reports no exit
  status and no duration for such a task, so the record has neither.
- The conformance suite checks, from inside the enclosure, that a host held to paths is
  held to them, that a terminated host is answered with the run's authority and held to
  its credential's paths, that the credential is set outside, and that no token and no
  key is inside: not in the environment, not in the bundle, not in the record.
- `session.Resend`: completes and delivers the record of a run that is over, for a
  job's last step after a runner that died or a receiver that was away. The run
  directory gains `delivered.log`, a line per accepted batch written as the answer
  comes, and `lock`, held while the runner lives; a run that still goes is
  `ErrRunning`. A record with no `ai.qory.run.exited` gets one with `reason:
  runner_lost`, and the events no accepted batch named are posted in order.
- `wall.Reaper`, and `Docker.Reap`: removes the containers and networks that carry a
  run's label, what a runner that died left behind. `Resend` asks for it.

### Requirements

- The Docker adapter is tested on Linux with Docker Engine 28, in CI, and with Docker
  Engine 29 on OrbStack; the conformance suite passes on both. It may work on an earlier
  engine, and that is not tested. It depends on the bridge option
  `com.docker.network.bridge.inhibit_ipv4`, which is in the engine's source at 24.0 and
  was not looked for before it, and on the `host-gateway` address. On an engine nobody
  has tried, run the suite: `go test ./wall/walltest` with `QORY_WALL_HELPER` naming its
  Linux build.

### Changed

- **Breaking for a caller in Go.** `session.Spec.Runtime` is a `runtimes.Runtime`, not a
  name, and `Spec.Descriptors` is gone: `catalog.Lookup(name, dir)` gives the runtime a
  name and a descriptor directory gave before. A nil `Runtime` is a bare one named after
  the command. A name nothing describes was an error and is now a bare runtime, and
  `runtime_version` in `ai.qory.run.started` is absent for one.
- The contract's limit that the proxy never reads a TLS connection now has its one
  exception, stated in every run's record: a terminated host. A run whose policy selects
  no credential and has no path rule is as before, with no authority made at all.
- Behind a wall the proxy serves the run's relay alone. Its address was reached by
  other containers of the same engine, on a Linux host, and by other processes of the
  machine; the run's policy bounded what they did with it. Now the relay opens every
  connection it forwards with a token of the run's, `Launch.ProxyToken`, given to the
  relay through a file and to nothing inside the enclosure, and the proxy closes
  unanswered whatever opens otherwise. An adapter of your own passes the token to its
  relay, which is `wall.Relay` with `QORY_RELAY_TOKEN` in its environment.
- A `Spec.RunID` that is not a UUID in the canonical lower-case form is refused. It
  went unchecked into the run directory's path and into the events' `subject`, which
  the envelope's schema holds to a UUID.
- The Docker adapter refuses a mount that is a socket, or a directory holding a
  container runtime's socket.

### Fixed

- The contract's event table listed `path` in `ai.qory.run.policy_applied`, which no
  schema and no runner has had since the policy became a value, and said every session
  event may carry `agent_id`, which `ai.qory.session.result` cannot.

## [0.2.0] - 2026-09-17

### Added

- The wall, `wall.Wall`: an optional enclosure for the runtime whose only route out
  leads to the session runner's proxy, so a program that ignores the proxy variables
  reaches nothing instead of going unseen. The session runner stays outside with the
  policy and the webhook's secret, and the enclosure sees the run directory read-only. `session.Spec` gains `Wall`, `Image` and `Mounts`;
  under a wall a nil `Env` is nothing, not the process's own, and
  `ai.qory.run.started` carries `wall` and `image`.
- The Docker adapter, `wall.Docker`, through the `docker` command and no library: an
  `--internal` network for the agent whose bridge holds no address of the host's, a relay container on that network and an ordinary
  one, both containers as the caller's user with every capability dropped and no new
  privileges, the workspace at its own path, the runner's settings read-only, the
  environment through a file so no value is on a command line, and everything removed at
  exit. The proxy binds the network's gateway on a Linux host and stays on loopback where
  the engine is in a virtual machine.
- The guard: behind a wall, or with `ProxyBind` set, the proxy refuses the link-local
  range always, and the runner's own machine, loopback and every address it holds,
  unless an allow entry names the host itself, in either mode. The way around the wall
  is not through the proxy, and a local MCP server or model endpoint is reached through
  it when the policy names it. A refused literal address or `localhost` is a denied
  `ai.qory.run.egress` with the rule `wall:own-address`; a name that resolves to one is
  refused when dialled.
- `wall.Relay`, the one peer an enclosure reaches: it copies a fixed port to one address
  fixed when it starts. The caller's binary runs it in a mode of its own and is mounted
  into the enclosure as the relay and the hook forwarder, so the wall needs no image.
- The conformance suite, `wall/walltest`: the contract's guarantees checked from inside
  the enclosure with a real session behind the adapter. CI runs it against Docker on a
  Linux machine, where a skip is a failure; golden files pin the adapter's command lines
  everywhere else.
- `session.Spec.ProxyBind`, the address the proxy listens on, for a caller that builds
  an enclosure of its own; loopback stays the default.
- `session.Spec.Events`, a stream that gets every event as the JSON line `events.jsonl`
  holds: a run with no receiver is followed on standard output.
- The contract gains §The wall: the guarantees, what crosses, the relay, the suite and
  what ships; and under §Limits the three outcomes of a connection, the model credential
  inside the enclosure, the proxy's address on a Linux host, and hooks on an engine in a
  virtual machine.
- `QORY_RUN_SOCKET` is read as an address: a path, or `unix:` and a path, is the local
  socket, and another scheme is a transport the forwarder refuses by name, so a network
  transport can be added without an old forwarder misreading it.

## [0.1.0] - 2026-09-16

### Added

- The runner contract, `contracts/runner/v1/`: the policy document, the webhook
  configuration, the event types with a JSON schema per data type, the batch and
  signature rules, the runtime descriptor schema, and the Claude Code descriptor with its
  fixtures. The `contracts` package embeds the directory and its tests validate every
  fixture against the schemas.
- The session runner, `session.Run`: the policy, given by the caller as a value and
  validated against the schema, pinned with the digest of its canonical JSON, or observe
  with none; the webhook configuration given the same way; the loopback proxy in observe and enforce modes, one `run.egress` event per
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
- `internal/receiver`, the receiving side the tests run the webhook sink against: a
  handler that verifies the signature in constant time, deduplicates on event id and
  appends to a file that remembers its ids across restarts.
- The contract states that no declaration and an empty declaration differ: no list
  leaves the policy's allow list as it is, an empty list under enforce reaches nothing.
  It names the policy's `egress.allow` grammar as the one definition of a declared host,
  which the harness contract copies.

[Unreleased]: https://github.com/qoryai/runner/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/qoryai/runner/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/qoryai/runner/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/qoryai/runner/compare/v0.1.0...v0.2.0
