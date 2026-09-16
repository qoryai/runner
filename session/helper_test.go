package session_test

import "os/exec"

// shell runs a hook command line the way a runtime does, under sh -c.
func shell(line string) *exec.Cmd {
	return exec.Command("sh", "-c", line)
}
