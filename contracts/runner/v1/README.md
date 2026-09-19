# Runner contract, v1

What the runner reads, and what it emits. The runner is the security boundary around one
coding agent session: the only thing between the agent and the world. This directory is
its contract: the documents, one JSON schema per document, and the fixtures a reader or
a receiver is tested against.

## Versions

Every document here carries `version: 1`, an integer, and its schema is addressed by
URL under `https://qory.dev/contracts/runner/v1/`. The harness contract of `qory`
spells its version differently, `apiVersion: qory.dev/v1alpha1`, and the difference is
the rule, not an accident. A document a person writes and commits, the stack and the
module manifest, carries the group and the version together, the way a Kubernetes
object does, because the file is read on its own and its format evolves with the
product. A document addressed by a schema URL, read or written by a program, carries an
integer that guards its reader, because the URL already names the group and the
generation. The policy, the webhook configuration and the descriptor are on this side:
the objects the command hands the runner are the ones a control plane will one day
deliver over the wire, and the compose report `qory` writes is versioned the same way. CloudEvents adds
its own `specversion: 1.0`, which is not ours to change.

`v1` is the first generation of this namespace, not a stability promise. The runner
module is at `v0`, which under Go's rules promises no compatibility, and until it
reaches `v1` a document or an event here may change in a way that breaks a reader; the
changelog says when one does. A breaking change after that is a new event type or a new
directory, `v2`, never a change in place.

## The boundary

The runner's duties, in the order that matters when they conflict:

1. **Policy.** The runner reads one policy document, pinned for the run, that can only
   narrow what the binary allows: the egress mode, the allow list, the paths of a host,
   and which of the machine's credentials the run may use. A policy that cannot be read means no run. No policy means observe
   everything and deny nothing. Nothing in a policy grants; a stale or failed policy
   degrades toward more restrictive, never toward more permissive.
2. **Egress.** The runner owns an HTTP proxy, on loopback or, behind a wall, on the one
   address the enclosure reaches (§The wall), and starts the session behind it.
   Every connection the session opens through the proxy is observed and recorded as one
   event: the host, the port, whether it was a `CONNECT` tunnel or a plain request, the
   decision, and the rule that made it. In enforce mode a connection to a host outside
   the allow list is denied. A denied attempt is recorded and the session continues; a
   denial never ends a run.
3. **Credentials.** The session holds none of the runner's. On a developer machine the
   session runs with the developer's own environment. Behind a wall the runner holds the
   credentials the run's policy selects, in memory and outside the enclosure, and its
   proxy sets each on the requests to the hosts it is for (§Credentials): the session
   reaches a code host and a model endpoint as itself and never reads what it is.
4. **Liveness.** A heartbeat while the session runs; the exit as the result.
5. **Reporting.** The session's terminal bytes as log chunks, the runner's observations
   as events, the runtime's own output mapped to session events by a descriptor. Every
   event goes to files. When a webhook is configured, every event goes there too. When
   the caller gives a stream, standard output say, every event goes there as well, the
   line `events.jsonl` holds; that is how a run with no receiver is followed.
6. **The harness reports over a local socket**, never over a network. A hook the
   runtime calls forwards what it received to the runner's socket; the runner is the
   only thing that speaks to a receiver.

## Limits

Stated so a receiver reads the record for what it is.

- The proxy sees host names and ports, never the content of a TLS connection: a
  `CONNECT` tunnel is a blind relay once established. The exception is stated in the
  run's record: behind a wall, for a host the run holds a credential for or has path
  rules for, the proxy ends the session's TLS itself and reads each request's method and
  path. `ai.qory.run.policy_applied` lists those hosts as `terminated`, and no other
  host is read.
- On a terminated host the session's side of the connection is HTTP/1.1, so a protocol
  that needs HTTP/2 end to end, gRPC say, does not work there, and a program that pins
  the host's own certificate refuses the run's. A host that sends a request's headers
  back, an echo service, hands the session the credential the proxy set. A path rule
  reads a path and nothing else: where a host takes every request on one path, a
  GraphQL endpoint say, the path is reachable or it is not, and what the request may
  touch behind it is bounded by the credential's own scope, not by the runner.
- Only proxy-aware programs are seen. The agent CLIs, git over HTTPS, curl, the package
  managers and the language runtimes honour the proxy variables; SSH does not, and a
  program that ignores the variables is not seen. Without a wall, enforce mode is
  advisory against such a program. Enforcement against bypass belongs to the wall (§The
  wall): the session runner runs outside and the agent in an enclosure whose only route
  out leads to the proxy. The three outcomes: a connection through the proxy is decided
  by the policy and recorded, on any machine; a connection around the proxy succeeds
  unseen without a wall, and fails unseen behind one. The runner records what came
  through it and nothing else; a record of attempts a wall refused is the container
  layer's own logging, or the network's.
- A wall decides which process may talk, not where the machine may talk. A packet filter
  in front of a node cannot tell the agent from the runner, so its list is the union of
  both; the run's policy can only be held at the proxy. Neither tells two accounts apart
  on one allowed host: a code host, an object store and a model endpoint each carry data
  to whoever owns the account the request names.
- A credential the run passes into the enclosure's environment is the agent's; one the
  policy selects stays outside (§Credentials). What is handed in is the agent's: a checkout that keeps a token
  in the repository's configuration hands the token in with the workspace. The run
  directory is shown read-only, so the agent cannot change `events.jsonl`; when it lies
  inside the workspace the agent can still rename the directory above it, which moves
  the record and does not alter it. The webhook's copy is out of reach either way.
- Behind a wall the exit status is the adapter's command's. With Docker that is the
  runtime's status, except that `125` is the engine failing to start the container,
  `126` and `127` the program not being startable in the image, and a runtime killed by
  a signal arrives as `128` plus the signal's number, with no `signal` named.
- The proxy behind a wall listens where the enclosure reaches it, which other
  containers of the same engine, or other processes of the machine, reach too. It
  serves none of them: the run has a token only its relay is given, every connection
  the relay forwards opens with `QORY-RELAY`, a space, the token and a newline before
  the first byte of HTTP, and a connection that opens otherwise is closed unanswered
  and reported once. The token is never inside the enclosure.
- On an engine inside a virtual machine, a Mac's say, the hook socket does not cross the
  file share, so a walled run there has no session events from hooks; the log, the
  egress record and the structured output are unaffected. The forwarder's network
  transport, through the relay, is not in this version.
- The runtime's session events are what the runtime reports through its hooks and its
  structured output. A runtime that reports nothing produces no session events; the log
  and the egress record are the runner's own and are always there.
- A descriptor matches and copies. It never computes, so a mapping that needs a program
  is a runner change, never a configuration change.
- The receiver is trusted with what it is sent. The webhook's secret proves the sender
  to the receiver; nothing proves the receiver to the sender beyond TLS.

## Sequence

One run, on a developer machine, with a webhook configured:

1. The runner is given a launch spec: the program, its arguments, its environment and
   directory, whether the session is interactive, the policy document, the webhook
   configuration, the egress the harness declared, and the runtime name. The spec comes
   from the `qory` command, which read the policy and the webhook from its own
   configuration; the runner knows nothing of what composed it or where it was read.
2. The runner makes a run id, a UUID version 7, and the run directory
   `.qory/runs/<id>/` in the checkout.
3. It validates the policy once. Refused by the schema: the run does not start. Absent:
   mode `observe`, everything allowed and recorded. Present: pinned, with the digest of
   its canonical JSON as its stamp. The effective allow list is the policy's entries,
   or, when the harness declared egress, the declared entries the policy covers (§The
   policy).
4. It validates the webhook configuration once, when one is given. It posts one
   `ai.qory.ping` and waits for a 2xx. Anything else, or no answer, means the run does
   not start: a run someone asked to have observed is not run unobserved by accident.
   The `--local` flag of the command runs with the file sink alone. With no webhook
   configured there is no ping and the run starts at once, files only.
5. It starts the proxy on a loopback port and sets `HTTP_PROXY`, `HTTPS_PROXY` and
   `NO_PROXY` in the session's environment, in upper and lower case, with
   `NO_PROXY=localhost,127.0.0.1,::1` so a local MCP server or model endpoint still
   answers. It opens the local socket and sets `QORY_RUN_SOCKET` to its path and
   `QORY_RUN_ID` to the run id. Nothing else of the runner's enters the environment.
6. It installs the runtime descriptor's hooks: its forwarder as a command hook for each
   event the descriptor lists, added to a copy of the settings file the launch passes,
   written as `settings.json` in the run directory and named in its place. The composed
   home is not modified.
7. It emits `ai.qory.run.started` and `ai.qory.run.policy_applied`, then starts the
   program: on a pseudo-terminal when interactive, on pipes otherwise.
8. While the program runs: every chunk of output is one `ai.qory.run.log`; every
   connection through the proxy is one `ai.qory.run.egress`; every record the descriptor
   matches is one session event; every thirty seconds one `ai.qory.run.heartbeat`.
9. The program exits. The runner drains the socket, so a hook on the runtime's last
   event is still read, emits `ai.qory.run.exited`, gives the sinks fifteen seconds to
   flush, reports what the webhook did not accept, and returns the program's exit
   status. A runtime killed by a signal exits as `-1` with the signal named.

A run may have a time limit. When the runtime still runs at the limit the runner stops
it, SIGTERM and SIGKILL after a grace the run names, ten seconds unless it does, and `ai.qory.run.exited` carries `reason:
timeout` with the state `failed`; step 9 is otherwise the same. A denied connection
never ends a run; the limit is the one thing of the runner's that does.

The run id is the runner's own, a UUID version 7, unless the caller already holds one: a
caller's id is a UUID in the canonical lower-case form, since it is every event's
`subject` and names the run directory, and anything else is no run. What else the caller
knows the run by, a key in its queue, a repository, an issue, goes in `labels` on
`ai.qory.run.started`: at most 16, a key of 1 to 64 of `a-z`, `0-9`, `_`, `.` and `-`, a
value of at most 256 bytes. The runner copies them and reads nothing into them, and no
other event repeats them: a receiver joins on `subject`.

Behind a wall, three steps differ and no event does. Before step 5 the runner asks the
wall to prepare the enclosure and listens where the enclosure says, not on loopback.
After step 6 it hands the wall the launch, the proxy's address, the socket and the
run directory, read-only, and starts the command the wall returns, on the same pseudo-terminal or
pipes; the proxy and socket variables inside name the addresses the enclosure reaches
them on. After step 9 it closes the wall, which removes everything it created.
`ai.qory.run.started` carries `wall` and `image`.

Under a node runner, step 1 is the node runner handing the same spec down through the
environment, with the run id it already holds; everything after is one code path.

## The policy

`policy.schema.json`. The document the command hands the runner, from the machine's
own configuration, never from inside the checkout, where the agent it constrains could
write it: for `qory`, the `egress` section of `~/.config/qory/runner.yaml`. The same
document a run start answer will carry once a control plane exists, so nothing is
designed twice.

```yaml
version: 1
egress:
  mode: enforce                # or observe
  allow:
    - api.anthropic.com
    - github.com
    - "*.github.com"           # every host below github.com; not github.com itself
```

| Field | Meaning |
|---|---|
| `version` | `1`. A runner refuses a version it does not read, naming it |
| `egress.mode` | `observe`: every connection is allowed and recorded. `enforce`: a connection to a host outside `allow` is denied and recorded |
| `egress.allow` | lower-case host names, or `*.` followed by a name for every host below it. No ports, no paths, no schemes. Absent is empty, and `enforce` with an empty list reaches nothing |
| `egress.paths` | by host, in `allow`'s grammar, the paths the session may ask of it: a path matched whole, or up to a final `*` as a prefix. A host listed is terminated, which needs a wall; a host not listed is reached on every path. An empty list is no path at all |
| `credentials` | the credentials of the machine's the run may use: `name`, and an `argument` for an adapter, a repository say. A policy defines none (§Credentials) |

**Narrowing.** The harness compose reports the hosts its modules declared. The command
hands that list to the runner, and the effective allow list is the declared entries the
policy covers: a name is covered by the same name or by a suffix pattern above it, a
pattern is covered by the same pattern or by a suffix pattern above it. A declared host
the policy does not cover is dropped, and `ai.qory.run.policy_applied` records both
lists. No declaration and an empty declaration are different things: a harness in which
no module declares egress hands over no list, and the policy's list is the effective
list; a harness whose modules declare, and between them name no host, hands over an
empty list, and `enforce` reaches nothing. The policy is the ceiling; a declaration can
only lower it. The grammar of a declared host is that of `egress.allow`, defined here
once; the harness contract copies it and cites this document.

**Matching a connection.** The host of a `CONNECT` request is its authority; the host of
a plain request is the authority of its absolute-form target, never its `Host` header.
Host names are compared lower-case; an IP literal matches only an identical entry. The
first entry that matches is the rule reported. A denial is a `403 Forbidden` with a
one-line text body naming the host and the mode; a tunnel is never opened for it.

**Matching a path.** On a terminated host every request is decided, by the policy's
paths for the host and by the paths of the credential that is for it, and it passes
when every list that exists has an entry that matches. The comparison is exact, case
included: on a host that ignores case this denies a spelling the host would have
taken, never the reverse, so whoever writes a path writes it as the host does. A path
that could be read two ways is denied in either mode, with the rule
`wall:ambiguous-path`: an encoded slash, backslash, dot or percent sign, a backslash, an
empty segment, a dot segment. Under `observe` a path no entry matches is recorded as
denied and let through, as a host is, but the credential is set only where its own paths
match: observing is no reason to hand a token to a path nobody configured. The host
asked of upstream is the one the connection was opened to and decided on, whatever
`Host` a request names. A denial is a `403` naming the method, the host and the path.
A plain request is held to the same paths and never carries a credential.

## Credentials

A credential is a token the runner holds for the session and the session never holds.
The machine defines credentials; the run's policy selects among them by name and
defines none, so whoever writes a policy chooses among the programs the machine's owner
installed and never names one. They need a wall: without one a program that ignores the
proxy is bound by nothing here.

A definition says where the token comes from, exactly one of:

| Source | The token is |
|---|---|
| `env` | a variable of the runner's own environment, read once when the run starts |
| `file` | a file's content, read again whenever it is used, so whatever rotates it tells no one |
| `adapter` | what a program of the machine's prints |

**An adapter** knows one kind of host: a source code host, an artifact store. The runner
knows none, so nothing in it names one. The runner starts the adapter outside the
enclosure, with its own environment, a minute to answer, and `${argument}` in its
arguments replaced by the argument the policy gives, which the definition's pattern
must match whole: one word of the command line, never a shell's. It prints one
document, `credential.schema.json`, and exits 0; anything else is no run, and the line it
wrote to standard error is the reason given.

```json
{"version": 1, "token": "…", "expires_at": "2026-09-19T14:00:00Z",
 "apply": [
   {"hosts": ["git.example.com"], "scheme": "basic", "username": "x-access-token",
    "paths": ["/acme/shop.git/*", "/acme/shop/*"]},
   {"hosts": ["api.git.example.com"], "scheme": "bearer",
    "paths": ["/repos/acme/shop", "/repos/acme/shop/*"]}],
 "placeholders": ["GIT_HOST_TOKEN"]}
```

The adapter says how its token is used, because hosts differ in it: which hosts, which
scheme, and which paths make up what the run asked for. The schemes are a closed set,
`bearer`, `basic` with a `username`, `header` with a header's name; an adapter chooses
among what the runner does and adds nothing to it. Of a host with `paths` the run
reaches those and no other, so one repository's credential does not open another
organization's on the same host; a path the adapter leaves out, the host's GraphQL
endpoint say, is not reached. A definition may name `hosts` and `paths` of its own for
an adapter: the most it may claim. For `env` and `file`, which have nobody to say it,
the definition's `hosts`, scheme and `paths` are the use itself.

Before the run starts every selected credential is resolved, and what cannot hold is
no run: a name the machine does not define, an argument it does not provide for, a host
two credentials claim, a claim above the definition's, and under `enforce` a host the
run's allow list does not cover. The runner asks an adapter again five minutes before
`expires_at`, and when a host answers 401 to a request it set the token on, not more
often than every thirty seconds. The new answer changes the token and nothing else: one
that names other hosts, schemes or paths is refused and reported, and the old token
stays, because what a run reaches is fixed when it starts.

**Placeholders.** A program often does not start without a credential set. A
definition, or an adapter's answer, names variables the enclosure gets with the value
`qory-sets-the-credential-outside-the-enclosure`, which is no credential anywhere; the
proxy replaces what the program sends. A run that passes a value of its own for such a
variable does not start.

**Termination.** For the hosts the credentials are for, and the hosts with path rules,
the proxy ends the session's TLS itself, answering as the host with a certificate of an
authority made for the run. The authority's key is in the runner's memory and nowhere
else, and gone with the run; its certificate is what the wall gives the enclosure to
trust (§The wall). The proxy verifies the real host against the machine's own roots.
Every other host stays a tunnel the proxy does not read, and a run with no credential
and no path rule has no authority at all.

**The record.** `ai.qory.run.policy_applied` carries each use, `name`, `hosts`, `scheme`
and `paths`, and the `terminated` hosts. On a terminated host `ai.qory.run.egress` is one
event per request, `method: HTTPS` with `request_method`, `path` without its query,
`path_rule`, and `credential`, the name of the one the proxy set. No event, no report
and no error carries a token.

## The events

Every event is a [CloudEvents 1.0](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/spec.md)
event in the [JSON format](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/formats/json-format.md).
`event.schema.json` is the envelope: the published CloudEvents schema, vendored as
`cloudevents.schema.json`, plus what this contract fixes.

| Attribute | Value |
|---|---|
| `specversion` | `1.0` |
| `id` | a UUID, unique per event. A receiver deduplicates on it |
| `source` | `urn:qory:run:<run id>`. One run is one source, so `sequence` orders within it |
| `subject` | the run id. A receiver creates the run on the first event with an unknown subject |
| `type` | one of the types below, and nothing else. A breaking change to a type's data is a new type |
| `time` | the runner's clock, RFC 3339, UTC |
| `sequence` | the [sequence extension](https://github.com/cloudevents/spec/blob/main/cloudevents/extensions/sequence.md): the runner-assigned order of the event within the run, a decimal zero-padded to ten digits, from `0000000001`, contiguous. A receiver orders by it, never by arrival |
| `dataschema` | `https://qory.dev/contracts/runner/v1/events/<type without ai.qory.>.schema.json`, the schema of `data` |
| `datacontenttype` | absent, which the JSON format reads as `application/json` |
| `data` | a JSON object validating against `dataschema` |

The types, one namespace. The runner's own:

| Type | When | Data |
|---|---|---|
| `ai.qory.ping` | before the runtime starts, to the webhook only, when one is configured | `runner_version`, `events` |
| `ai.qory.run.started` | the runtime is about to start; the first event in the file | `runtime`, `runtime_version`, `command`, `args`, `dir`, `interactive`, `runner_version`, `host`, behind a wall `wall`, `image`, and `labels` when the caller gave any |
| `ai.qory.run.policy_applied` | right after, once | `mode`, `allow`, `source`, `digest`, `declared`, and with them set `paths`, `credentials`, `terminated` |
| `ai.qory.run.log` | one per chunk of output: one line or 4096 bytes, whichever comes first | `stream`, `bytes` |
| `ai.qory.run.egress` | one per connection through the proxy, allowed or denied; on a terminated host one per request | `host`, `port`, `method`, `decision`, `mode`, `rule`, and per request `request_method`, `path`, `path_rule`, `credential` |
| `ai.qory.run.heartbeat` | every `interval_seconds` while the runtime runs | `elapsed_seconds`, `interval_seconds` |
| `ai.qory.run.exited` | the runtime exited; the result and the last event | `state`, `exit_code`, `signal`, `reason`, `duration_ms` |

The session's, produced by a descriptor from what the runtime reports:

| Type | When | Data |
|---|---|---|
| `ai.qory.session.started` | the runtime opened its session | `session_id`, `source`, `model`, `cwd` |
| `ai.qory.session.prompt_submitted` | a prompt reached the runtime | `session_id`, `prompt` |
| `ai.qory.session.tool_started` | the runtime is about to run a tool | `session_id`, `tool`, `tool_use_id`, `input` |
| `ai.qory.session.tool_finished` | a tool ran and returned | the same, `response`, `duration_ms` |
| `ai.qory.session.tool_failed` | a tool ran and failed | the same, `error`, `interrupted`, `duration_ms` |
| `ai.qory.session.turn_finished` | the runtime finished responding | `session_id`, `message` |
| `ai.qory.session.turn_failed` | a turn ended on an API error | `session_id`, `error`, `details`, `message` |
| `ai.qory.session.subagent_started` | a subagent was spawned | `session_id`, `agent_id`, `agent_type` |
| `ai.qory.session.subagent_finished` | a subagent finished | the same, `message` |
| `ai.qory.session.notification` | the runtime notified its user: waiting for a permission, idle | `session_id`, `kind`, `message`, `title` |
| `ai.qory.session.ended` | the runtime closed its session | `session_id`, `reason` |
| `ai.qory.session.result` | a non-interactive session printed its result | `session_id`, `outcome`, `is_error`, `turns`, `duration_ms`, `cost_usd`, `result` |

Every session event that comes from a hook may carry `agent_id` and `agent_type` when
it happened inside a subagent; `ai.qory.session.result`, read from the runtime's output,
carries neither. The schema of each type, under `events/`, says which fields are required and
what each holds. Values are copied from the runtime unchanged: `input` and `response`
have the shape the tool gave them, `error` is display text, and the enumerations in
`source`, `reason`, `kind`, `outcome` are the runtime's words.

The log is an event like the others. `bytes` is base64 of the chunk as the runtime
wrote it, terminal escapes included; a chunk is cut at a line break or at 4096 bytes and
never at a character boundary, so a multibyte character may straddle two chunks and the
concatenation, not a chunk, is text. On a pseudo-terminal `stream` is `terminal`; on
pipes the runtime's standard output and standard error are chunked apart.

## The record files

`.qory/runs/<id>/` in the checkout, kept out of git by the compose:

- `events.jsonl`: every event of the run, one per line, in sequence order, the ping
  included when one was sent. The record of truth; the webhook is a copy.
- `output.log`: the raw bytes of the session's output, the concatenation of the
  `ai.qory.run.log` chunks. Both files tail.
- `settings.json`: the runtime's settings with the runner's hooks added, when hooks
  were installed.
- `undelivered/`: the batches the webhook did not accept, when there were any.

`fixtures/run/<id>/` is one such directory, recorded. The control plane's CI replays it.

## The webhook

`webhook.schema.json`. The document the command hands the runner, from the machine's
own configuration: for `qory`, the `webhook` section of `~/.config/qory/runner.yaml`.
Configuring one makes the run fail closed on the ping.

```yaml
version: 1
url: https://receiver.example/qory/events
secret: ...                    # at least 16 characters; shared with the receiver alone
events:                        # absent: every type
  - ai.qory.run.started
  - ai.qory.run.egress
  - ai.qory.run.exited
```

`url` is `https`, or `http` to a loopback address for a receiver on the same machine.
The secret lives outside any repository, in the machine's configuration or its
environment; it is never a fixture, never in a checkout, never in an event. `events` filters by full type name,
`*` for all; the ping is always sent.

**Delivery**, after [GitHub's model](https://docs.github.com/en/webhooks/webhook-events-and-payloads#delivery-headers).
One `POST` per batch:

| Header | Value |
|---|---|
| `Content-Type` | `application/cloudevents-batch+json` |
| `User-Agent` | `qory-runner/<version>` |
| `X-Qory-Delivery` | a UUID per batch. A retry of the same batch carries the same id |
| `X-Qory-Signature-256` | `sha256=` followed by the hex HMAC SHA-256 of the raw request body, keyed with the secret |

The body is a `batch.schema.json` document: a JSON array of events of one run, in
sequence order, never empty. The runner cuts a batch at one hundred events, at one
mebibyte, or after one second since its first event, whichever comes first; the ping is
a batch of one, sent before anything else. A receiver verifies the signature over the
raw bytes with a constant-time comparison before parsing, then deduplicates on each
event's `id`, since delivery is at least once.

The receiver answers with a status; the body is ignored:

| Status | Meaning |
|---|---|
| 2xx | accepted; the runner forgets the batch |
| 410 | stop: the receiver wants nothing more for this run. The runner sends no further batch and the run continues on the file sink |
| anything else, or no answer within ten seconds | retried with exponential backoff, one second doubling to one minute, until the run ends |

What is still undelivered when the run ends is spooled to `.qory/runs/<id>/undelivered/`
as batch files with their delivery ids, and the runner reports the count on its standard
error. The file sink has every event regardless. The webhook never delays the session:
posting is asynchronous behind a bounded queue, and a queue that fills spools to the
same directory rather than blocking the runtime.

**After a runner that died.** The run directory says what the receiver is still owed
without the runner that wrote it. `events.jsonl` is written as events happen.
`delivered.log` beside it gets a line as each batch is accepted, the delivery id and the
sequence of every event in it, and the one word `stopped` for a 410. `lock` is held by
the runner for as long as it lives, by the kernel, so it is free once the runner is gone
however it went. Sending a run again is the job's last step, whatever happened before
it: refused while the lock is held; then what the run's wall left behind is removed, by
the run's label; a record with no `ai.qory.run.exited` gets one, numbered on from the
last event, with `state: failed`, `exit_code: -1` and `reason: runner_lost`; and every
event the webhook's filter wants that no accepted batch named is posted, in order, in
batches cut the same way, until accepted or given up on. What is still not accepted is
under `undelivered/` again. A receiver sees some events twice when the runner died
between an answer and its line, and discards them by `id` as ever. Nothing of this
recovers a machine that died: the record went with it, and a receiver learns of that
from heartbeats that stop.

No timestamp is signed and no replay window is checked: a replayed batch is a duplicate
the receiver already discards by event id, and the secret is the only credential.
Stripe's signed timestamp and the Standard Webhooks headers were considered and set
aside for that reason.

**A worked example** of these rules is `internal/receiver` in this module: a handler
that verifies the signature, deduplicates and appends to a file, which the module's tests
run the webhook sink against. It is not a public package and no command ships it; a
receiver written by anyone else follows this section, and may read that code.

## The runtime descriptor

`descriptor.schema.json`. One YAML file per runtime under `runtimes/<name>/`, embedded
in the binary as the default and overridden by `<name>.yaml` in a directory the command
names, `~/.config/qory/runtimes/` for `qory`. It has three parts.

**Sources**: how the runner attaches. The terminal bytes always, with nothing to match
in them and so no source. `output`: JSON lines on the runtime's standard output, when the
session runs on pipes; a line that is not a JSON object is not a record. `hooks`: the
runtime's hook events, for each of which the runner installs its forwarder as a command
hook, the way `install` names among the installers the binary has; `claude-settings`
adds a group per event under `hooks` in the JSON settings the launch passes with
`--settings`, leaving the groups already there. The forwarder writes the JSON it read on
its standard input to the local socket, as one record. A forwarder exits 0 and prints
nothing, which every hook interface reads as no decision, so an installed hook observes
and never changes what the runtime does.

**Rules**: for each source, a match and a target. `match` is a map of dotted paths into
the record to values: equality with a string, number or boolean, or `{present: true}`.
Every entry must hold. `data` is a map of the event's fields to dotted paths whose
values are copied unchanged; a path that does not exist leaves the field out, which is
how optional fields work. Rules run in order and the first that matches produces one
event; a record no rule matches produces nothing. No patterns, no expressions, no
defaults, no concatenation. If equality and presence prove too narrow, the next step is
a bounded expression language, and before that a runner change.

**Fixtures**: `fixtures/<case>/records.jsonl`, records as the runtime produced them, in
the shape of `record.schema.json`, beside `expected/events.jsonl`, one `{type, data}`
per event the rules produce from them, in order. A descriptor without fixtures is not
accepted. The tests validate every record and every expected event against the schemas;
the session package replays the records through the rules and compares.

`runtimes/claude/descriptor.yaml` is the Claude Code descriptor, written against version
2.1.273 as installed and its published hooks reference. Its hooks are the canonical
source in both modes; its standard output adds the result line, the one thing the hooks
do not report. A descriptor records the version it was written against; the runner
reports that version and does not check the installed one.

## The local socket

`QORY_RUN_SOCKET` is an address, not a path to open: a path, or `unix:` and a path, is
the local socket, and any other scheme names a transport, which a forwarder that does
not have it refuses by name. Only the local socket exists in this version. It is a Unix
domain socket the runner creates before the runtime starts, in a private directory of its own under the system's temporary directory, mode
`0700`, because a socket path has a short limit on some systems and a run directory in
a deep checkout can exceed it. A client connects, writes one record per line in the shape
of `record.schema.json`, and closes; the runner reads until end of file, one connection
at a time in the order they arrived, so two hook calls in a row keep their order. There
is no answer and no framing beyond the newline. The forwarder the runner installs as a
hook is one such client; a harness that wants to report something of its own writes the
same shape with `source: hooks`. Nothing on the socket reaches a receiver except through
the descriptor's rules. The socket is removed when the run ends.

## The wall

A wall is what makes a connection around the proxy fail. It is optional: with none, the
runtime is a process of the machine and enforcement is cooperative (§Limits). With one,
the runtime runs in an **enclosure** and the session runner stays outside it with the
proxy, the policy and the webhook's secret; the record is written from outside, and the
enclosure sees the run directory read-only. A wall is built by an adapter,
one per container interface; the contract names no tool in its rules and states one list
for all of them.

**What every wall guarantees:**

- no route out of the enclosure except to the session runner's proxy;
- a resolver that resolves nothing outside the enclosure;
- no cloud metadata address, which is a network path to credentials;
- no file of the host beyond the mounts the run lists, and no environment beyond what
  the run passes;
- a user that is not root, no added capabilities, no privileged mode, no host
  namespaces;
- no credential the run's policy selects: a placeholder where a program wants one set
  and the certificate of the run's authority, never a token and never the authority's
  key;
- never the container runtime's own socket: a process that can ask the daemon for a
  container on the host's network has left the wall. A mount that is a socket, or a
  directory holding a runtime's, is refused.

When the run has an authority of its own (§Credentials), a wall gives the enclosure
one bundle to trust, the image's own authorities with the run's certificate after them,
and points the variables programs read a bundle's path from at it: `SSL_CERT_FILE`,
`GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE`
unless the caller names others. The bundle is the image's and one more, never the run's
alone, because those variables replace a program's trust and do not add to it; an image
that keeps a bundle nowhere known gets the run's alone and reaches only the terminated
hosts over TLS, which is the image's to mend. The authority's key never crosses.

A run may name limits on what the agent uses, processors, memory, processes and the
size of `/dev/shm`; an adapter passes them to its engine and a run that names none gets
the engine's defaults. They are no guarantee of the wall's: they keep one run from
starving a machine, not an agent inside.

The proxy is part of the list, because it dials from outside on behalf of what is
inside: behind a wall it is **guarded**, and what is on the runner's machine is not
reached by default. Two rules, in either mode, observe included:

- The link-local range, where a cloud keeps its metadata service, is refused whatever
  the allow list says.
- The runner's own machine, loopback and every address it holds, is refused unless an
  `egress.allow` entry of the policy names the host itself. A `*.` suffix over it does
  not count, and neither does a name a harness declared under such a suffix: the
  machine's owner names what is opened on the machine, a repository cannot. A
  local MCP server or model endpoint is reached through the proxy like everything else,
  decided and recorded, when the policy names it, and the rule that names it applies
  under observe as well. The session names such a server by the machine's host name or
  an alias of it, not by `localhost`, which `NO_PROXY` keeps inside the enclosure.

A refusal the proxy can make without resolving, a literal address or `localhost`, is a
`denied` `ai.qory.run.egress` with the rule `wall:own-address` and a `403`; a name that
resolves to such an address passes the decision, is refused when dialled, and the
runtime gets a `502`. Without the guard the way around a wall is through the proxy.

**What crosses**, all three the session runner's, none carrying a credential of the
runner's: the proxy, as a network address; the pseudo-terminal or the pipes, through the
adapter's own command; the hook socket, as a mounted file where a file can cross.

**The relay.** The agent reaches the proxy by a name, through a relay: a process of the
runner's on the enclosure's network and on an ordinary one, listening on a fixed port and
copying every byte to one address fixed when it starts, the proxy's. It reads nothing,
decides nothing and takes no instruction from the agent; the policy stays in the session
runner. It exists because the host is not always where a container thinks it is: with
the engine in a virtual machine the network's gateway is the virtual machine's, not the
host's.

**One conformance suite**, the `wall/walltest` package, checks the list from inside the
enclosure with a real session behind the adapter, and an adapter ships when the suite
passes for it. The suite needs Linux and the tool, so it runs in the runner's CI on a
Linux machine; the ordinary tests compare the commands an adapter generates with golden
files and need neither.

**What ships:** `docker`, through the `docker` command and no library, serving whatever
engine that command reaches. It is supported where the suite passes. An engine in a
virtual machine on a Mac is where a wall is developed, not a target: the suite passes
there without the hook check (§Limits). The agent's image is the caller's; the wall
builds none.

## Fixtures

| Directory | Holds | Validated against |
|---|---|---|
| `fixtures/policy/` | policy documents that are accepted | `policy.schema.json` |
| `fixtures/webhook/` | webhook configurations that are accepted; the secrets are synthetic | `webhook.schema.json` |
| `fixtures/batch/` | delivery bodies: the ping, a first batch | `batch.schema.json` |
| `fixtures/run/<id>/` | one recorded run: `events.jsonl` and `output.log` | `event.schema.json` per line, plus the sequence, source and concatenation rules |
| `fixtures/invalid/` | documents each schema refuses, named `<schema>-<reason>` | the schema the name starts with, expecting a failure |
| `runtimes/<name>/fixtures/<case>/` | descriptor fixtures | `record.schema.json` and the data schema of each expected type |

Every fixture is synthetic. No host name of anyone's infrastructure, no real secret, no
recorded session of anyone's work.

## Sources

Public sources this contract was written from, and nothing else:

- CloudEvents 1.0.2: the [core specification](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/spec.md),
  the [JSON event format](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/formats/json-format.md)
  with its batch format, the [HTTP protocol binding](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/bindings/http-protocol-binding.md),
  the [sequence extension](https://github.com/cloudevents/spec/blob/main/cloudevents/extensions/sequence.md)
  in its current form, without the retired `sequencetype`, and the published
  [JSON schema](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/formats/cloudevents.json),
  vendored unchanged as `cloudevents.schema.json` under its Apache License, Version 2.0.
- HTTP: [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html) §9.3.6 for `CONNECT`,
  §15.5.4 for 403 and §15.5.8 for why not 407; [RFC 9112](https://www.rfc-editor.org/rfc/rfc9112.html)
  §3.2 for the absolute-form target a proxy receives. UUID version 7 from
  [RFC 9562](https://www.rfc-editor.org/rfc/rfc9562.html).
- The proxy variables: [curl's environment](https://curl.se/docs/manpage.html#ENVIRONMENT),
  which reads `http_proxy` in lower case only; [Go's httpproxy](https://pkg.go.dev/golang.org/x/net/http/httpproxy),
  which reads both cases and exempts loopback; [Node's built-in proxy support](https://nodejs.org/api/http.html#built-in-proxy-support);
  [Claude Code's proxy configuration](https://code.claude.com/docs/en/network-config);
  [git's http.proxy](https://git-scm.com/docs/git-config#Documentation/git-config.txt-httpproxy);
  the [npm](https://docs.npmjs.com/cli/v11/using-npm/config#proxy), [pip](https://pip.pypa.io/en/stable/user_guide/#using-a-proxy-server),
  [uv](https://docs.astral.sh/uv/reference/environment/) and [cargo](https://doc.rust-lang.org/cargo/reference/config.html#httpproxy)
  configuration pages. Setting both cases and listing loopback in `NO_PROXY` is what the
  union of them requires.
- The wall: Docker's [`network create --internal`](https://docs.docker.com/reference/cli/docker/network/create/),
  which gives a network no route out; the [`docker run` reference](https://docs.docker.com/reference/cli/docker/container/run/)
  for `--cap-drop`, `--security-opt no-new-privileges`, `--user`, `--env-file`, `--mount`
  and `--add-host` with `host-gateway`; Docker's [note on the daemon socket](https://docs.docker.com/engine/security/#docker-daemon-attack-surface),
  which is why the socket never crosses; the instance metadata service of
  [AWS](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-options.html),
  the address the list names; Kubernetes' [network policies](https://kubernetes.io/docs/concepts/services-networking/network-policies/),
  the picture the relay matches: one named peer and nothing else.
- Claude Code 2.1.273: the [hooks reference](https://code.claude.com/docs/en/hooks) for
  the event names, the input on standard input and the rule that exit 0 with no output
  is no decision; the [CLI reference](https://code.claude.com/docs/en/cli-reference) and
  [settings](https://code.claude.com/docs/en/settings) for `--settings` and
  `--setting-sources`; the [headless page](https://code.claude.com/docs/en/headless) and
  the [Agent SDK types](https://code.claude.com/docs/en/agent-sdk/typescript) for the
  JSON lines of `--output-format stream-json`; the [sessions page](https://code.claude.com/docs/en/sessions)
  for the statement that the transcript format is internal, which is why no descriptor
  reads it.
- Webhooks: GitHub's [delivery headers](https://docs.github.com/en/webhooks/webhook-events-and-payloads#delivery-headers),
  [signature validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
  and [best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks),
  the model for the headers, the HMAC and the ten-second answer; [Stripe's signed
  timestamp](https://docs.stripe.com/webhooks) and [Standard Webhooks](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md),
  the alternatives set aside.
