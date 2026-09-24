package walltest_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/qoryai/runner/wall"
	"github.com/qoryai/runner/wall/walltest"
)

func TestMain(m *testing.M) {
	walltest.Main()
	os.Exit(m.Run())
}

// TestDockerConforms runs the suite against the Docker adapter and whatever engine the
// docker command reaches. QORY_WALL_COMMAND names another command, podman say, and
// QORY_WALL_IMAGE another image with a shell.
func TestDockerConforms(t *testing.T) {
	command := os.Getenv("QORY_WALL_COMMAND")
	if command == "" {
		command = "docker"
	}
	if out, err := exec.Command(command, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		walltest.Skip(t, command+" reaches no engine: "+strings.TrimSpace(string(out)))
	}
	image := os.Getenv("QORY_WALL_IMAGE")
	if image == "" {
		image = "busybox:stable"
	}
	// The origin an allowed request reaches is a container of its own, because the proxy
	// of a walled run never dials this machine.
	helper := walltest.Helper(t)
	name := fmt.Sprintf("qory-walltest-origin-%d", os.Getpid())
	if out, err := exec.Command(command, "run", "--detach", "--rm", "--name", name, image, "sh", "-c", "mkdir /w && echo ok > /w/index.html && httpd -f -p 8080 -h /w").CombinedOutput(); err != nil {
		t.Fatalf("the origin: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command(command, "rm", "--force", name).Run() })
	ip, err := exec.Command(command, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	walltest.Run(t, walltest.Options{
		Origin:    "http://" + strings.TrimSpace(string(ip)) + ":8080/",
		Wall:      &wall.Docker{Command: command, Helper: helper, RelayArgs: walltest.RelayArgs},
		Image:     image,
		Forwarder: []string{wall.HelperPath, "forward"},
		Probe:     wall.HelperPath,
		// A socket crosses a bind mount on a Linux host and not a virtual machine's file
		// share.
		Hooks: runtime.GOOS == "linux",
		Leftovers: func(runID string) ([]string, error) {
			var left []string
			for _, list := range [][]string{{"ps", "--all", "--quiet"}, {"network", "ls", "--quiet"}} {
				out, err := exec.Command(command, append(list, "--filter", "label=dev.qory.run="+runID)...).CombinedOutput()
				if err != nil {
					return nil, err
				}
				left = append(left, strings.Fields(string(out))...)
			}
			return left, nil
		},
	})
}
