package queue

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWorkerIDFormat(t *testing.T) {
	id := NewWorkerID()
	if !regexp.MustCompile(`^[^:]+:\d+:[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("worker id %q does not match hostname:pid:8hex", id)
	}
	if NewWorkerID() == id {
		t.Fatal("worker ids must be unique per queue")
	}
}

func TestBackoffMatchesPython(t *testing.T) {
	for exp, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
		if got := backoff(exp); got != want {
			t.Errorf("backoff(%d) = %v, want %v", exp, got, want)
		}
	}
	if backoff(500) <= 0 {
		t.Error("large exponents must not overflow")
	}
}

func TestTimeouts(t *testing.T) {
	q := New(nil, nil, Options{Timeouts: map[string]time.Duration{"x": time.Second}})
	cases := map[string]time.Duration{
		"x":                     time.Second,
		"run_workbook":          1800 * time.Second,
		"research_playbook_run": 3600 * time.Second,
		"plugin_run":            900 * time.Second,
		"not_a_registered_type": DefaultJobTimeout,
	}
	for jobType, want := range cases {
		if got := q.timeoutFor(jobType); got != want {
			t.Errorf("timeoutFor(%q) = %v, want %v", jobType, got, want)
		}
	}
	msg := (&timeoutError{id: 7, jobType: "run_workbook", limit: 1800 * time.Second}).Error()
	if msg != "job 7 (run_workbook) exceeded 1800s" {
		t.Errorf("timeout message %q differs from JobProcessTimeout", msg)
	}
	msg = (&timeoutError{id: 7, jobType: "x", limit: 200 * time.Millisecond}).Error()
	if msg != "job 7 (x) exceeded 0.2s" {
		t.Errorf("fractional timeout message %q", msg)
	}
}

func TestOptionsClampLikePython(t *testing.T) {
	q := New(nil, nil, Options{Concurrency: 0, MaxActivePerWorkspace: 99, ShutdownGrace: time.Hour})
	if q.opts.Concurrency != 1 || q.opts.MaxActivePerWorkspace != 64 || q.opts.ShutdownGrace != 300*time.Second {
		t.Fatalf("unexpected clamping: %+v", q.opts)
	}
}

func TestPythonStr(t *testing.T) {
	var payloads = map[string]string{
		`{"workspace_id": "ws-1"}`: "ws-1",
		`{"workspace_id": 42}`:     "42",
		`{"workspace_id": ""}`:     "<nil>",
		`{"workspace_id": null}`:   "<nil>",
		`{"workspace_id": 0}`:      "<nil>",
		`{}`:                       "<nil>",
	}
	for raw, want := range payloads {
		var probe struct {
			WorkspaceID any `json:"workspace_id"`
		}
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			t.Fatal(err)
		}
		got := "<nil>"
		if s := pythonStr(probe.WorkspaceID); s != nil {
			got = *s
		}
		if got != want {
			t.Errorf("%s: workspace = %q, want %q", raw, got, want)
		}
	}
}

func TestRegistryTypesSorted(t *testing.T) {
	r := NewRegistry()
	r.Register("b", nil)
	r.Register("a", nil)
	r.RegisterFailure("c", nil) // failure handlers alone do not make a type claimable
	if got := strings.Join(r.Types(), ","); got != "a,b" {
		t.Fatalf("Types() = %s", got)
	}
}
