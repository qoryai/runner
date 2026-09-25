//go:build !linux

package wall

import "errors"

// nest runs only in a Linux enclosure.
func nest(string, []string) error {
	return errors.New("nest: a Docker of the agent's own starts only inside a Linux enclosure")
}
