// Package wall builds the enclosure an agent runs in: a place with no route out except
// to the session runner's proxy.
//
// The session runner stays outside. It owns the proxy, the policy decision and the
// record; a [Wall] owns only what makes a connection around the proxy fail rather than
// succeed unseen. An adapter wraps the launch and nothing else: the session runner asks
// the [Enclosure] where its proxy must listen, starts the wrapped command on the
// pseudo-terminal or the pipes it already owns, and closes the enclosure at exit.
//
// What every wall guarantees is one list, contracts/runner/v1/README.md §The wall, the
// same for every adapter, and the conformance suite in the walltest package checks it
// from inside the enclosure. An adapter ships when the suite passes for it. [Docker] is
// the first.
package wall

import "context"

// Wall is one way of building an enclosure: one implementation per container
// interface.
type Wall interface {
	// Name names the adapter in the run's record: docker.
	Name() string
	// Prepare builds what must exist before the proxy listens. The caller closes the
	// enclosure it returns, whatever happens after.
	Prepare(ctx context.Context, req Request) (Enclosure, error)
}

// Request is what a wall is told about the run it encloses.
type Request struct {
	// RunID names what the wall creates, so two runs never share anything.
	RunID string
	// Image is the agent's image: the runtime and the project's toolchain, built and
	// pinned by the caller. The wall builds nothing.
	Image string
}

// Enclosure is one run's wall, built.
type Enclosure interface {
	// ProxyAddr is where the session runner's proxy must listen, host:port, for the
	// enclosure to reach it; port 0 leaves the port to the system.
	ProxyAddr() string
	// Wrap returns the launch that starts l inside the enclosure, on the terminal or
	// the pipes of whoever starts it. It is called once, after the proxy listens.
	Wrap(ctx context.Context, l Launch) (Launch, error)
	// Close removes everything the wall created, within the context's deadline. It is
	// safe to call after a failed Wrap and more than once.
	Close(ctx context.Context) error
}

// Launch is a command to start. Given to [Enclosure.Wrap] it is the agent's, with what
// must cross the wall beside it; returned from Wrap it is the adapter's own command,
// and only Command, Args, Env and Dir are set.
type Launch struct {
	Command string
	Args    []string
	// Env is the whole environment, NAME=value. Given to Wrap, it is everything the
	// enclosure gets besides the proxy and socket variables, which the enclosure sets
	// itself because it knows the addresses inside. Returned from Wrap, nil means the
	// starting process's own.
	Env []string
	// Dir is the working directory, the run's workspace. The enclosure shows it at the
	// same path, writable.
	Dir string
	// Interactive says the command runs on a pseudo-terminal.
	Interactive bool
	// Proxy is the address the proxy listens on, host:port with the port it got.
	Proxy string
	// Socket is the path of the hook socket on the host, empty when there is none.
	Socket string
	// Mounts are the files and directories of the host the run lists beside Dir: the
	// checkout around Dir, a composed home, the run directory read-only. The
	// enclosure shows each at the same path, and nothing of the host besides them.
	Mounts []Mount
}

// Mount is one file or directory of the host an enclosure shows, at the same path.
type Mount struct {
	Path string
	// ReadOnly says the enclosure cannot change it.
	ReadOnly bool
}
