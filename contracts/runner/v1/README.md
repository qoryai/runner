# Runner contract, v1

What the runner reads, and what it emits. The runner is the security boundary around one
coding agent session: the only thing between the agent and the world. This directory is
its contract: the documents, one JSON schema per document, and the fixtures a reader or
a receiver is tested against.

## The boundary

The runner's duties, in the order that matters when they conflict:

1. **Policy.** The runner reads one policy document, pinned for the run, that can only
   narrow what the binary allows: the egress mode and the allow list now, mounts and
   credentials later. A policy that cannot be read means no run. No policy means observe
   everything and deny nothing. Nothing in a policy grants; a stale or failed policy
   degrades toward more restrictive, never toward more permissive.
2. **Egress.** The runner owns a loopback HTTP proxy and starts the session behind it.
   Every connection the session opens through the proxy is observed and recorded as one
   event: the host, the port, whether it was a `CONNECT` tunnel or a plain request, the
   decision, and the rule that made it. In enforce mode a connection to a host outside
   the allow list is denied. A denied attempt is recorded and the session continues; a
   denial never ends a run.
3. **Credentials.** The session holds none of the runner's. On a developer machine the
   session runs with the developer's own environment. Under a node runner, the runner
   holds the run's git and model credentials in memory and the session sees a workspace
   and a local model endpoint; that mode is the node layer's and is not in this version.
4. **Liveness.** A heartbeat while the session runs; the exit as the result.
5. **Reporting.** The session's terminal bytes as log chunks, the runner's observations
   as events, the runtime's own output mapped to session events by a descriptor. Every
   event goes to files. When a webhook is configured, every event goes there too.
6. **The harness reports over a local socket**, never over a network. A hook the
   runtime calls forwards what it received to the runner's socket; the runner is the
   only thing that speaks to a receiver.

## Limits

Stated so a receiver reads the record for what it is.

- The proxy sees host names and ports, never the content of a TLS connection. A
  `CONNECT` tunnel is a blind relay once established.
- Only proxy-aware programs are seen. The agent CLIs, git over HTTPS, curl, the package
  managers and the language runtimes honour the proxy variables; SSH does not, and a
  program that ignores the variables is not seen. Without a sandbox, enforce mode is
  advisory against such a program. Enforcement against bypass belongs to the container
  layer: under a node runner the session runner runs on the node and the agent in a
  container whose only route out leads to the proxy, so a connection around the proxy
  fails there rather than succeeding unseen. The runner records what came through it
  and nothing else, on either machine.
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
   directory, whether the session is interactive, the path of the policy file, the path
   of the webhook configuration, the egress the harness declared, and the runtime name.
   The spec comes from the `qory` command; the runner knows nothing of what composed it.
2. The runner makes a run id, a UUID version 7, and the run directory
   `.qory/runs/<id>/` in the checkout.
3. It reads the policy file once. Unreadable: the run does not start, the error names
   the file. Absent: mode `observe`, everything allowed and recorded. Present: pinned.
   The effective allow list is the policy's entries, or, when the harness declared
   egress, the declared entries the policy covers (§The policy).
4. It reads the webhook configuration once, when one is given. It posts one
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

Under a node runner, step 1 is the node runner handing the same spec down through the
environment, with the run id it already holds; everything after is one code path.

## The policy

`policy.schema.json`. A YAML or JSON document in the user's configuration directory or at
a path given on the command line; never inside the checkout, where the agent it
constrains could write it. The same document a run start answer will carry once a
control plane exists, so nothing is designed twice.

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
| `ai.qory.run.started` | the runtime is about to start; the first event in the file | `runtime`, `runtime_version`, `command`, `args`, `dir`, `interactive`, `runner_version`, `host` |
| `ai.qory.run.policy_applied` | right after, once | `mode`, `allow`, `source`, `path`, `digest`, `declared` |
| `ai.qory.run.log` | one per chunk of output: one line or 4096 bytes, whichever comes first | `stream`, `bytes` |
| `ai.qory.run.egress` | one per connection through the proxy, allowed or denied | `host`, `port`, `method`, `decision`, `mode`, `rule` |
| `ai.qory.run.heartbeat` | every `interval_seconds` while the runtime runs | `elapsed_seconds`, `interval_seconds` |
| `ai.qory.run.exited` | the runtime exited; the result and the last event | `state`, `exit_code`, `signal`, `duration_ms` |

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

Every session event may carry `agent_id` and `agent_type` when it happened inside a
subagent. The schema of each type, under `events/`, says which fields are required and
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

`webhook.schema.json`. In the user's configuration directory beside the policy, or at a
path given on the command line. Configuring one makes the run fail closed on the ping.

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
The secret is a literal here because the file lives outside any repository; it is never
a fixture, never in a checkout, never in an event. `events` filters by full type name,
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

No timestamp is signed and no replay window is checked: a replayed batch is a duplicate
the receiver already discards by event id, and the secret is the only credential.
Stripe's signed timestamp and the Standard Webhooks headers were considered and set
aside for that reason.

**The reference receiver** is the `receiver` package of this module: a handler that
verifies the signature, deduplicates and appends to a file. No command ships it; it is
the test of the webhook sink, run against it in this module's tests, and the model for a
receiver written by anyone else.

## The runtime descriptor

`descriptor.schema.json`. One YAML file per runtime under `runtimes/<name>/`, embedded
in the binary as the default and overridden by `<name>.yaml` in a directory the command
names, beside the policy. It has three parts.

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

`QORY_RUN_SOCKET` names a Unix domain socket the runner creates before the runtime
starts, in a private directory of its own under the system's temporary directory, mode
`0700`, because a socket path has a short limit on some systems and a run directory in
a deep checkout can exceed it. A client connects, writes one record per line in the shape
of `record.schema.json`, and closes; the runner reads until end of file, one connection
at a time in the order they arrived, so two hook calls in a row keep their order. There
is no answer and no framing beyond the newline. The forwarder the runner installs as a
hook is one such client; a harness that wants to report something of its own writes the
same shape with `source: hooks`. Nothing on the socket reaches a receiver except through
the descriptor's rules. The socket is removed when the run ends.

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
