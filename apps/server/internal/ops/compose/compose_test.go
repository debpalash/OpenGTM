package compose

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/execx"
)

type scripted struct{ outputs []string }

func (s *scripted) Run(_ context.Context, c execx.Cmd) error {
	out := s.outputs[0]
	if len(s.outputs) > 1 {
		s.outputs = s.outputs[1:]
	}
	_, err := io.WriteString(c.Stdout, out)
	return err
}

func entry(service, state, health string, code int) string {
	return `{"Service":"` + service + `","State":"` + state + `","Health":"` + health + `","ExitCode":` + string(rune('0'+code)) + `}` + "\n"
}

func TestParsePSAcceptsLinesAndArray(t *testing.T) {
	lines, err := parsePS(entry("a", "running", "healthy", 0) + entry("b", "exited", "", 0))
	if err != nil || len(lines) != 2 || lines[1].State != "exited" {
		t.Fatalf("ndjson: %+v %v", lines, err)
	}
	arr, err := parsePS(`[{"Service":"a","State":"running","Health":"","ExitCode":0}]`)
	if err != nil || len(arr) != 1 {
		t.Fatalf("array: %+v %v", arr, err)
	}
}

func TestWaitHealthyTreatsFinishedOneShotsAsDone(t *testing.T) {
	p := Project{Dir: ".", Run: &scripted{outputs: []string{
		entry("api", "running", "starting", 0) + entry("migrate", "exited", "", 0),
		entry("api", "running", "healthy", 0) + entry("migrate", "exited", "", 0) + entry("seed", "exited", "", 0),
	}}}
	if err := p.WaitHealthy(context.Background(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHealthyFailsFastOnCrashAndUnhealthy(t *testing.T) {
	for name, out := range map[string]string{
		"crashed":   entry("migrate", "exited", "", 1),
		"unhealthy": entry("api", "running", "unhealthy", 0),
	} {
		p := Project{Dir: ".", Run: &scripted{outputs: []string{out}}}
		start := time.Now()
		err := p.WaitHealthy(context.Background(), 30*time.Second)
		if err == nil || time.Since(start) > 5*time.Second {
			t.Errorf("%s: err = %v after %s, want an immediate failure", name, err, time.Since(start))
		}
	}
}

func TestWaitHealthyTimesOutNamingTheLaggard(t *testing.T) {
	p := Project{Dir: ".", Run: &scripted{outputs: []string{entry("api", "running", "starting", 0)}}}
	err := p.WaitHealthy(context.Background(), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "api") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitHealthyRequiresNamedServicesToExist(t *testing.T) {
	p := Project{Dir: ".", Run: &scripted{outputs: []string{entry("redis", "running", "healthy", 0)}}}
	err := p.WaitHealthy(context.Background(), 2*time.Second, "postgres")
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("err = %v", err)
	}
}
