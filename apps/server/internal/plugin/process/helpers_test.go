//go:build unix

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", ".."))
}

func sdkPath() string { return filepath.Join(repoRoot(), "packages", "sdk-python", "src") }

func testdataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata")
}

func requirePython(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// pspec describes a test plugin manifest.
type pspec struct {
	name     string
	kind     string
	script   string
	network  []string
	secrets  []string
	timeout  float64
	memory   int
	maxPages int
	outputs  string // YAML for `outputs:` (default "company")
	dir      string
	command  string
	extra    string // appended to the manifest (cost_per_lookup, ...)
}

func (s pspec) build(t testing.TB) *manifest.Plugin {
	t.Helper()
	if s.name == "" {
		s.name = "raw_plugin"
	}
	if s.kind == "" {
		s.kind = "provider"
	}
	if s.script == "" {
		s.script = "rawplugin.py"
	}
	if s.timeout == 0 {
		s.timeout = 20
	}
	if s.memory == 0 {
		s.memory = 128
	}
	if s.maxPages == 0 {
		s.maxPages = 10
	}
	if s.outputs == "" {
		s.outputs = "company"
	}
	if s.dir == "" {
		s.dir = testdataDir()
	}
	if s.command == "" {
		s.command = "[python3, " + s.script + "]"
	}
	list := func(xs []string) string {
		b, _ := json.Marshal(append([]string{}, xs...))
		return string(b)
	}
	text := fmt.Sprintf(`manifest_version: "2"
name: %s
kind: %s
runtime: process
version: 0.1.0
capabilities:
  network: %s
  secrets: %s
limits:
  timeout_seconds: %g
  memory_mb: %d
  max_pages: %d
inputs: { mode: "string?" }
outputs: %s
process:
  command: %s
%s
`, s.name, s.kind, list(s.network), list(s.secrets), s.timeout, s.memory, s.maxPages, s.outputs, s.command, s.extra)
	p, err := manifest.LoadBytes("plugin.yaml", []byte(text))
	if err != nil {
		t.Fatalf("manifest: %v\n%s", err, text)
	}
	p.Dir = s.dir
	p.Path = filepath.Join(s.dir, "plugin.yaml")
	return p
}

// roundTripFunc is an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// testClient is an egress client replaying canned responses.
func testClient(t testing.TB, rt http.RoundTripper) *egress.Client {
	t.Helper()
	c, err := egress.New(egress.Options{Transport: rt, ReplayWithoutRateLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func okTransport() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/robots.txt" {
			return respond(404, "", nil), nil
		}
		return respond(200, `{"hello":"`+r.URL.Path+`"}`, map[string]string{"Content-Type": "application/json"}), nil
	})
}

type captureLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLog) add(level, msg string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, level+" "+msg+" "+fmt.Sprint(args...))
}
func (c *captureLog) Info(msg string, a ...any)  { c.add("info", msg, a...) }
func (c *captureLog) Warn(msg string, a ...any)  { c.add("warn", msg, a...) }
func (c *captureLog) Error(msg string, a ...any) { c.add("error", msg, a...) }
func (c *captureLog) Debug(msg string, a ...any) { c.add("debug", msg, a...) }
func (c *captureLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// newSup builds a supervisor with a private state directory and the SDK on
// PYTHONPATH. Callers may adjust opts first.
func newSup(t testing.TB, opts Options) *Supervisor {
	t.Helper()
	requirePython(t)
	if opts.StateDir == "" {
		opts.StateDir = t.TempDir()
	}
	if opts.PythonPath == nil {
		opts.PythonPath = []string{sdkPath()}
	}
	if opts.Logger == nil {
		opts.Logger = &captureLog{}
	}
	s, err := NewSupervisor(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func run(t testing.TB, s *Supervisor, p *manifest.Plugin, inputs map[string]any, secrets map[string]string) (*Outcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return s.Run(ctx, p, Request{RunID: "t", Inputs: inputs, Secrets: secrets})
}

func mustRun(t testing.TB, s *Supervisor, p *manifest.Plugin, inputs map[string]any, secrets map[string]string) *Outcome {
	t.Helper()
	out, err := run(t, s, p, inputs, secrets)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return out
}

func wantError(t testing.TB, err error, code string, retryable bool) *Error {
	t.Helper()
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *Error %s, got %T %v", code, err, err)
	}
	if pe.Code != code || pe.Retryable != retryable {
		t.Fatalf("want code=%s retryable=%v, got code=%s retryable=%v (%s)", code, retryable, pe.Code, pe.Retryable, pe.Message)
	}
	return pe
}

// procGone reports whether pid no longer runs (a zombie awaiting a reaper
// that is not us counts as gone).
func procGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}

func waitGone(t testing.TB, what string, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !procGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running after %s", what, pid, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFile(t testing.TB, path string, within time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 && json.Valid(b) {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not written within %s", path, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type treePIDs struct {
	Self, SameGroup, Setsid, Orphan int
}

func readTree(t testing.TB, path string) treePIDs {
	t.Helper()
	var raw struct {
		Self      int `json:"self"`
		SameGroup int `json:"same_group"`
		Setsid    int `json:"setsid"`
		Orphan    int `json:"orphan"`
	}
	if err := json.Unmarshal(waitFile(t, path, 20*time.Second), &raw); err != nil {
		t.Fatal(err)
	}
	return treePIDs{raw.Self, raw.SameGroup, raw.Setsid, raw.Orphan}
}

func (p treePIDs) all() map[string]int {
	return map[string]int{"plugin": p.Self, "same-group child": p.SameGroup, "setsid child": p.Setsid, "orphaned double-fork": p.Orphan}
}

func (p treePIDs) assertAlive(t testing.TB) {
	t.Helper()
	for name, pid := range p.all() {
		if procGone(pid) {
			t.Fatalf("%s (pid %d) should be running", name, pid)
		}
	}
}

func (p treePIDs) assertGone(t testing.TB, within time.Duration) {
	t.Helper()
	for name, pid := range p.all() {
		waitGone(t, name, pid, within)
	}
}

// fieldsOf returns the first record's fields.
func fieldsOf(t testing.TB, out *Outcome) map[string]any {
	t.Helper()
	if len(out.Records) == 0 {
		t.Fatal("no records")
	}
	return out.Records[0].Fields
}

func asMap(t testing.TB, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

