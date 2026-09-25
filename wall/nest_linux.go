package wall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// nest starts the daemon and becomes the agent: see [Nest].
func nest(user string, argv []string) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("nest: the enclosure starts as uid %d, not as its root; the wall starts it as 0:0 under a runtime that maps it to a user of the machine's", os.Getuid())
	}
	uid, gid, err := nestIDs(user, passwd)
	if err != nil {
		return err
	}
	dockerd, err := exec.LookPath("dockerd")
	if err != nil {
		return errors.New("nest: the image holds no dockerd; an image with a Docker of the agent's own carries the daemon")
	}
	command, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("nest: the launch: %w", err)
	}
	log, err := os.OpenFile(NestLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("nest: the daemon's log: %w", err)
	}
	daemon := exec.Command(dockerd, "--host=unix://"+NestSocket, "--group", strconv.Itoa(gid))
	daemon.Dir, daemon.Stdout, daemon.Stderr = "/", log, log
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := daemon.Start(); err != nil {
		return fmt.Errorf("nest: dockerd: %w", err)
	}
	log.Close()
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	if err := nestAnswers(exited); err != nil {
		tail, _ := os.ReadFile(NestLog)
		if len(tail) > 2048 {
			tail = tail[len(tail)-2048:]
		}
		return fmt.Errorf("%w; the daemon said:\n%s", err, bytes.TrimSpace(tail))
	}

	env := os.Environ()
	if os.Getenv("DOCKER_CONFIG") == "" {
		config, err := nestProxies(os.Getenv, net.LookupHost)
		if err != nil {
			return err
		}
		if config != nil {
			dir, err := writeNestConfig(config, uid, gid)
			if err != nil {
				return fmt.Errorf("nest: the agent's docker configuration: %w", err)
			}
			env = append(env, "DOCKER_CONFIG="+dir)
		}
	}

	// Capabilities belong to a thread: the bounding set is dropped on the thread that
	// executes the launch. Changing the user drops the rest, on every thread.
	runtime.LockOSThread()
	last := 63
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = n
		}
	}
	for c := 0; c <= last; c++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("nest: dropping capability %d: %w", c, err)
		}
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("nest: setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("nest: setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("nest: setuid: %w", err)
	}
	return fmt.Errorf("nest: exec %s: %w", command, syscall.Exec(command, argv, env))
}

// nestAnswers waits until the daemon answers on its socket, or exits, or the time runs
// out.
func nestAnswers(exited <-chan error) error {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", NestSocket)
		},
	}}
	deadline := time.Now().Add(nestWait * time.Second)
	for {
		if resp, err := client.Get("http://docker/_ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case err := <-exited:
			return fmt.Errorf("nest: dockerd exited: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nest: dockerd did not answer on %s within %d seconds", NestSocket, nestWait)
		}
	}
}
