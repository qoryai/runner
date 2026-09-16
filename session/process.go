package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/qoryai/runner/internal/chunk"
)

// process is the runtime's process and where its streams go.
type process struct {
	command string
	args    []string
	env     []string
	dir     string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	// logs returns the chunk consumer for a stream.
	logs func(stream string) func([]byte)
	// output takes each JSON object the runtime prints as a line; nil when the
	// runtime has no output source.
	output func(map[string]any)
}

// exitStatus is how the runtime ended.
type exitStatus struct {
	code   int
	signal string
}

// termGrace is how long the runtime gets after SIGTERM before SIGKILL, when the
// context ends.
const termGrace = 10 * time.Second

// newCmd builds the command with the context ending it: SIGTERM, then SIGKILL after
// termGrace.
func (p *process) newCmd(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, p.command, p.args...)
	cmd.Env = p.env
	cmd.Dir = p.dir
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = termGrace
	return cmd
}

// runPipes runs the process on pipes: its input is stdin, its standard output goes to
// the terminal-less log as stdout, to the output source when there is one, and to
// stdout; its standard error to the log as stderr and to stderr.
func (p *process) runPipes(ctx context.Context) (exitStatus, error) {
	cmd := p.newCmd(ctx)
	cmd.Stdin = p.stdin
	out := chunk.New(p.logs("stdout"))
	errs := chunk.New(p.logs("stderr"))
	writers := []io.Writer{out, p.stdout}
	var lines *lineParser
	if p.output != nil {
		lines = &lineParser{on: p.output}
		writers = append(writers, lines)
	}
	cmd.Stdout = io.MultiWriter(writers...)
	cmd.Stderr = io.MultiWriter(errs, p.stderr)
	if err := cmd.Start(); err != nil {
		return exitStatus{}, fmt.Errorf("%w: %v", ErrNotStarted, err)
	}
	err := cmd.Wait()
	out.Flush()
	errs.Flush()
	if lines != nil {
		lines.flush()
	}
	return status(cmd, err)
}

// runPTY runs the process on a pseudo-terminal: what it writes goes to the log as
// terminal and to stdout, what stdin holds goes to it, and when stdin is a terminal
// it is put in raw mode and its size follows.
func (p *process) runPTY(ctx context.Context) (exitStatus, error) {
	cmd := p.newCmd(ctx)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return exitStatus{}, fmt.Errorf("%w: %v", ErrNotStarted, err)
	}
	defer ptmx.Close()
	if f, ok := p.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		restore := attach(f, ptmx)
		defer restore()
	}
	go func() {
		io.Copy(ptmx, p.stdin)
	}()
	log := chunk.New(p.logs("terminal"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(io.MultiWriter(log, p.stdout), ptmx)
	}()
	err = cmd.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	log.Flush()
	return status(cmd, err)
}

// attach puts a terminal in raw mode, sizes the pseudo-terminal like it and follows
// its resizes until the returned function restores it.
func attach(tty *os.File, ptmx *os.File) func() {
	fd := int(tty.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		state = nil
	}
	resize := func() {
		if size, err := pty.GetsizeFull(tty); err == nil {
			pty.Setsize(ptmx, size)
		}
	}
	resize()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			resize()
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
		if state != nil {
			term.Restore(fd, state)
		}
	}
}

// status turns Wait's error into the exit status.
func status(cmd *exec.Cmd, err error) (exitStatus, error) {
	if err == nil {
		return exitStatus{code: 0}, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return exitStatus{}, err
	}
	st := exitStatus{code: exit.ExitCode()}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		st.code = -1
		st.signal = "SIG" + signalName(ws.Signal())
	}
	return st, nil
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGHUP:
		return "HUP"
	}
	return fmt.Sprintf("%d", int(s))
}

// lineParser hands every line that is a JSON object to on, and ignores the rest.
type lineParser struct {
	on  func(map[string]any)
	buf bytes.Buffer
}

func (l *lineParser) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		i := bytes.IndexByte(l.buf.Bytes(), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := make([]byte, i)
		copy(line, l.buf.Bytes()[:i])
		l.buf.Next(i + 1)
		l.line(line)
	}
}

func (l *lineParser) flush() {
	if l.buf.Len() > 0 {
		l.line(l.buf.Bytes())
		l.buf.Reset()
	}
}

func (l *lineParser) line(b []byte) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err == nil && m != nil {
		l.on(m)
	}
}

func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
