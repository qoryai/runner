// Package session runs one coding agent session inside the runner's boundary.
//
// [Run] takes a [Spec], the program to start and how, and returns a [Result], the exit
// status and where the run's record is. Between the two it does what the contract,
// contracts/runner/v1/README.md, lists as the runner's duties, in that order:
//
//   - reads the policy once and pins it; an unreadable policy is a [*policy.Error] and
//     no run, no policy is observe everything
//   - starts a proxy in the policy's mode, on loopback or where the wall says, and
//     points the session at it
//   - with a [wall.Wall] in the spec, starts the runtime inside an enclosure whose only
//     route out leads to that proxy, and removes the enclosure at exit
//   - keeps every credential of the runner's out of the session: the session's
//     environment is the caller's plus the proxy and socket variables, nothing else
//   - heartbeats while the runtime runs and reports the exit as the result
//   - emits every event to the file sink in .qory/runs/<id>/ and, when a webhook is
//     configured and the spec is not local, to the webhook too, after a ping the
//     receiver must accept
//   - takes the harness's reports over a local socket and maps them, with the
//     runtime's structured output, to session events through the runtime's descriptor
//
// The package knows nothing of stacks, modules, homes or reports. The caller, the qory
// command, turns those into the spec; a node runner hands the same spec down through
// the environment.
package session
