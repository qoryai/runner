package session_test

import (
	"context"
	"fmt"
	"os"

	"github.com/qoryai/runner/session"
)

// Example runs one headless Claude Code turn inside the boundary, with the policy and
// the webhook read from the caller's configuration directory, and exits with the
// runtime's status. It compiles with the module's tests and is not run, since it
// starts a real program.
func Example() {
	exe, _ := os.Executable()
	res, err := session.Run(context.Background(), session.Spec{
		Runtime:     "claude",
		Command:     "claude",
		Args:        []string{"--settings", "/path/to/settings.json", "-p", "Reply with the single word pong."},
		PolicyPath:  os.ExpandEnv("$HOME/.config/qory/policy.yaml"),
		WebhookPath: os.ExpandEnv("$HOME/.config/qory/webhook.yaml"),
		Forwarder:   []string{exe, "forward"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "the run did not start:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "recorded in", res.Dir)
	os.Exit(res.ExitCode)
}

// Example_forward is the hook command the runner installs, `<exe> forward` for the
// spec above: it reads the hook's input from stdin and hands it to the run that
// installed it, and exits 0 whatever happened.
func Example_forward() {
	if err := session.Forward(context.Background(), os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
