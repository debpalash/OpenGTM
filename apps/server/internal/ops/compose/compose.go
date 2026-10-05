// Package compose drives `docker compose` for an OpenGTM install directory.
//
// The install directory is the one `opengtm init` writes: compose.yml and
// .env side by side. Compose reads .env itself (interpolation and
// COMPOSE_PROFILES), so this package only adds the project name and file.
package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
)

// File is the compose file name `opengtm init` writes.
const File = "compose.yml"

// Project is one Compose project rooted at Dir.
type Project struct {
	Dir  string
	Name string // optional; empty lets Compose derive it from COMPOSE_PROJECT_NAME or the directory
	Run  execx.Runner
	// Log receives the output of long-running commands (pull, up). Nil discards.
	Log io.Writer
}

func (p Project) base(args ...string) execx.Cmd {
	a := []string{"compose", "--project-directory", p.Dir, "-f", File}
	if p.Name != "" {
		a = append(a, "-p", p.Name)
	}
	return execx.Cmd{Name: "docker", Args: append(a, args...), Dir: p.Dir}
}

func (p Project) runner() execx.Runner {
	if p.Run == nil {
		return execx.OS{}
	}
	return p.Run
}

// Cmd builds a docker compose command (exposed for exec-style callers that
// attach stdin or stdout, such as pg_dump streaming).
func (p Project) Cmd(args ...string) execx.Cmd { return p.base(args...) }

func (p Project) stream(ctx context.Context, args ...string) error {
	c := p.base(args...)
	c.Stdout = p.Log
	c.Stderr = p.Log
	return execx.Do(ctx, p.runner(), c)
}

// Config validates the compose file and its interpolation.
func (p Project) Config(ctx context.Context) error {
	_, err := execx.Output(ctx, p.runner(), p.base("config", "--quiet"))
	return err
}

// Services lists the services enabled by the active profiles.
func (p Project) Services(ctx context.Context) ([]string, error) {
	out, err := execx.Output(ctx, p.runner(), p.base("config", "--services"))
	if err != nil {
		return nil, err
	}
	var s []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s = append(s, line)
		}
	}
	return s, nil
}

// Pull fetches the images for the active profiles.
func (p Project) Pull(ctx context.Context) error { return p.stream(ctx, "pull", "--quiet") }

// Up starts services (all, or the named ones and their dependencies) and then
// waits until each is running and healthy. `docker compose up --wait` is not
// used because it fails on one-shot services that finished successfully (the
// seed job), which are a normal part of this stack.
func (p Project) Up(ctx context.Context, wait time.Duration, services ...string) error {
	args := append([]string{"up", "-d", "--remove-orphans"}, services...)
	if err := p.stream(ctx, args...); err != nil {
		return err
	}
	return p.WaitHealthy(ctx, wait, services...)
}

// UpNoDeps starts only the named services, ignoring their dependencies.
func (p Project) UpNoDeps(ctx context.Context, wait time.Duration, services ...string) error {
	if err := p.stream(ctx, append([]string{"up", "-d", "--no-deps"}, services...)...); err != nil {
		return err
	}
	return p.WaitHealthy(ctx, wait, services...)
}

// ServiceStatus is one container as reported by `docker compose ps`.
type ServiceStatus struct {
	Service  string
	State    string
	Health   string
	ExitCode int
}

// parsePS reads `docker compose ps --format json`, which prints one JSON
// object per line (Compose 2.21+) or a single array (older releases).
func parsePS(out string) ([]ServiceStatus, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	if strings.HasPrefix(out, "[") {
		var all []ServiceStatus
		return all, json.Unmarshal([]byte(out), &all)
	}
	var all []ServiceStatus
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var e ServiceStatus
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		all = append(all, e)
	}
	return all, nil
}

// Status lists every container of the project, including finished one-shots.
func (p Project) Status(ctx context.Context) ([]ServiceStatus, error) {
	out, err := execx.Output(ctx, p.runner(), p.base("ps", "-a", "--format", "json"))
	if err != nil {
		return nil, err
	}
	return parsePS(out)
}

// WaitHealthy polls until every container (of the named services, or all) is
// running and, where a healthcheck exists, healthy. A container that exited
// with status 0 counts as done (a finished one-shot job); a non-zero exit or an
// unhealthy verdict fails immediately.
func (p Project) WaitHealthy(ctx context.Context, wait time.Duration, services ...string) error {
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	want := map[string]bool{}
	for _, s := range services {
		want[s] = true
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var pending string
	for {
		entries, err := p.Status(ctx)
		if err != nil {
			return err
		}
		pending = ""
		for _, e := range entries {
			if len(want) > 0 && !want[e.Service] {
				continue
			}
			switch {
			case e.State == "exited" && e.ExitCode == 0:
			case e.State == "exited" || e.State == "dead":
				return fmt.Errorf("service %s exited with status %d (see `docker compose logs %s`)", e.Service, e.ExitCode, e.Service)
			case e.Health == "unhealthy":
				return fmt.Errorf("service %s is unhealthy (see `docker compose logs %s`)", e.Service, e.Service)
			case e.State == "running" && (e.Health == "" || e.Health == "healthy"):
			default:
				pending = e.Service + " (" + e.State + " " + e.Health + ")"
			}
			if pending != "" {
				break
			}
		}
		if pending == "" {
			seen := map[string]bool{}
			for _, e := range entries {
				seen[e.Service] = true
			}
			for s := range want {
				if !seen[s] {
					pending = s + " (no container)"
				}
			}
		}
		if pending == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("compose: %s is not ready after %s", pending, wait)
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// Stop stops the named services without removing their containers.
func (p Project) Stop(ctx context.Context, services ...string) error {
	if len(services) == 0 {
		return nil
	}
	return p.stream(ctx, append([]string{"stop"}, services...)...)
}

// RunOnce runs a one-shot service container with an optional command
// override and removes it afterwards. Dependencies are not started.
func (p Project) RunOnce(ctx context.Context, service string, command ...string) error {
	return p.stream(ctx, append([]string{"run", "--rm", "--no-deps", "-T", service}, command...)...)
}

// RunOnceOutput is RunOnce returning stdout.
func (p Project) RunOnceOutput(ctx context.Context, service string, command ...string) (string, error) {
	return execx.Output(ctx, p.runner(), p.base(append([]string{"run", "--rm", "--no-deps", "-T", service}, command...)...))
}

// Running lists the services that currently have a running container.
func (p Project) Running(ctx context.Context) ([]string, error) {
	out, err := execx.Output(ctx, p.runner(), p.base("ps", "--services", "--status", "running"))
	if err != nil {
		return nil, err
	}
	var s []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s = append(s, line)
		}
	}
	return s, nil
}
