// Package execx is the one place the operator tooling (init, migrate, backup,
// restore, upgrade) starts external programs. Everything goes through the
// Runner interface so orchestration logic can be tested with a fake that
// records commands instead of running docker or PostgreSQL client tools.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd describes one program invocation.
type Cmd struct {
	Name string
	Args []string
	Dir  string
	// Env is appended to the parent environment. Secrets (PGPASSWORD,
	// DATABASE_URL) travel here, never in Args, so they stay out of `ps`.
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// String renders the command for logs without the environment.
func (c Cmd) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Runner runs a command to completion.
type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// OS runs commands as child processes.
type OS struct{}

// Run implements Runner.
func (OS) Run(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}

// Error is a failed command with the tail of its stderr, which is almost
// always what the operator needs to see.
type Error struct {
	Cmd    string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %v", e.Cmd, e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += "\n" + s
	}
	return msg
}

// Unwrap exposes the underlying error (for example *exec.ExitError).
func (e *Error) Unwrap() error { return e.Err }

// Output runs c and returns trimmed stdout. Stderr is captured into the
// returned error. Stdout and Stderr set on c are replaced.
func Output(ctx context.Context, r Runner, c Cmd) (string, error) {
	var out bytes.Buffer
	var errb tail
	c.Stdout = &out
	c.Stderr = &errb
	if err := r.Run(ctx, c); err != nil {
		return strings.TrimSpace(out.String()), &Error{Cmd: c.String(), Err: err, Stderr: errb.String()}
	}
	return strings.TrimSpace(out.String()), nil
}

// Do runs c and wraps a failure with its stderr tail. A caller-provided
// Stderr still receives the stream; Stdout is passed through untouched.
func Do(ctx context.Context, r Runner, c Cmd) error {
	var errb tail
	if c.Stderr != nil {
		c.Stderr = io.MultiWriter(c.Stderr, &errb)
	} else {
		c.Stderr = &errb
	}
	if err := r.Run(ctx, c); err != nil {
		return &Error{Cmd: c.String(), Err: err, Stderr: errb.String()}
	}
	return nil
}

// Missing reports whether err means the program is not installed.
func Missing(err error) bool {
	var ee *exec.Error
	return errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound)
}

// tail keeps roughly the last 8 KiB written to it.
type tail struct{ buf bytes.Buffer }

const tailMax = 8 << 10

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > tailMax*2 {
		b := t.buf.Bytes()
		t.buf = *bytes.NewBuffer(append([]byte(nil), b[len(b)-tailMax:]...))
	}
	return len(p), nil
}

func (t *tail) String() string {
	b := t.buf.Bytes()
	if len(b) > tailMax {
		b = b[len(b)-tailMax:]
	}
	return string(b)
}
