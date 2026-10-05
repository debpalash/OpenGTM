//go:build linux

package pluginrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/process"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// These tests run process plugins through the real queue, the real Python SDK
// and PostgreSQL with row-level security.

const (
	helperEnv = "OPENGTM_PLUGINRUN_TEST_HELPER"
	pluginAPI = "https://api.example.com"
)

func repoRoot(t testing.TB) string { return dbtest.RepoRoot(t) }

func processTestdata(t testing.TB) string {
	return filepath.Join(repoRoot(t), "apps", "server", "internal", "plugin", "process", "testdata")
}

func sdkSrc(t testing.TB) string { return filepath.Join(repoRoot(t), "packages", "sdk-python", "src") }

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okTransport() http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"hello":%q}`, r.URL.Path)
		if r.URL.Path == "/robots.txt" {
			body = ""
		}
		status := 200
		if r.URL.Path == "/robots.txt" {
			status = 404
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: httptestBody(body), Request: r}, nil
	})
}

func httptestBody(s string) *bodyReader { return &bodyReader{strings.NewReader(s)} }

type bodyReader struct{ *strings.Reader }

func (bodyReader) Close() error { return nil }

// writePlugins creates the plugin directory used by these tests.
func writePlugins(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(processTestdata(t), "rawplugin.py"))
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := os.ReadFile(filepath.Join(processTestdata(t), "sdk_all.py"))
	if err != nil {
		t.Fatal(err)
	}
	manifestFor := func(name, script string, timeout float64) string {
		return fmt.Sprintf(`manifest_version: "2"
name: %s
kind: provider
runtime: process
version: 0.1.0
capabilities:
  network: ["%s"]
  secrets: [API_KEY]
limits: {timeout_seconds: %g, memory_mb: 128, max_pages: 5}
inputs: {type: object}
outputs: company
process: {command: [python3, %s]}
`, name, pluginAPI, timeout, script)
	}
	for _, p := range []struct {
		name, script, src string
		timeout           float64
	}{
		{"raw_proc", "rawplugin.py", string(raw), 20},
		{"raw_short", "rawplugin.py", string(raw), 1.5},
		{"sdk_prov", "sdk_all.py", string(sdk), 20},
	} {
		dir := filepath.Join(root, p.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		must(t, os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifestFor(p.name, p.script, p.timeout)), 0o644))
		must(t, os.WriteFile(filepath.Join(dir, p.script), []byte(p.src), 0o644))
	}
	return root
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type quietLog struct{}

func (quietLog) Info(string, ...any)  {}
func (quietLog) Warn(string, ...any)  {}
func (quietLog) Error(string, ...any) {}
func (quietLog) Debug(string, ...any) {}

type procHarness struct {
	*harness
	pool     *pgxpool.Pool
	dbURL    string
	plugins  string
	state    string
	sup      *process.Supervisor
	scratch  string
	runner   *Runner
	catalogE []LoadError
}

type procOpts struct {
	queue        queue.Options
	maxProcesses int
	maxPerPlugin int
	noWorker     bool
}

func newProcessHarness(t *testing.T, o procOpts) *procHarness {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	appURL := dbtest.AppURL(t, owner)
	pool := dbtest.Pool(t, appURL, 12)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs WHERE type = $1`, JobType); err != nil {
		t.Fatal(err)
	}

	plugins := writePlugins(t)
	catalog := LoadCatalog(config.Plugins{Dirs: []string{plugins}, SignaturePolicy: "optional"})
	for _, n := range []string{"raw_proc", "raw_short", "sdk_prov"} {
		if _, ok := catalog.Get(n); !ok {
			t.Fatalf("plugin %s not loaded: %+v", n, catalog.Errors)
		}
	}
	client, err := egress.New(egress.Options{Transport: okTransport(), ReplayWithoutRateLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	return buildHarness(t, pool, appURL, catalog, client, plugins, state, o)
}

func buildHarness(t *testing.T, pool *pgxpool.Pool, appURL string, catalog *Catalog, client *egress.Client,
	plugins, state string, o procOpts) *procHarness {
	t.Helper()
	maxProcs := o.maxProcesses
	if maxProcs == 0 {
		maxProcs = 4
	}
	sup, err := process.NewSupervisor(process.Options{
		Client: client, StateDir: state, PythonPath: []string{sdkSrc(t)}, Logger: quietLog{},
		MaxProcesses: maxProcs, MaxPerPlugin: o.maxPerPlugin, CancelGrace: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(client, nil, nil)
	runner.SetProcessSupervisor(sup)
	t.Cleanup(func() { runner.Close(context.Background()) })

	reg := queue.NewRegistry()
	NewWorker(pool, catalog, runner, nil).Register(reg)
	qo := o.queue
	if qo.Concurrency == 0 {
		qo.Concurrency = 2
	}
	if qo.IdlePoll == 0 {
		qo.IdlePoll = 20 * time.Millisecond
	}
	if qo.ClaimCheckInterval == 0 {
		qo.ClaimCheckInterval = 50 * time.Millisecond
	}
	q := queue.New(pool, reg, qo)

	az, err := authz.New(fakeLegacy(t).URL, authz.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewAPI(pool, catalog, az).Mount(mux)
	return &procHarness{harness: &harness{t: t, mux: mux, queue: q}, pool: pool, dbURL: appURL, plugins: plugins,
		state: state, sup: sup, scratch: t.TempDir(), runner: runner}
}

func (h *procHarness) start(t *testing.T, plugin string, inputs map[string]any) string {
	t.Helper()
	code, run := h.do("editor-a", "POST", "/api/v2/plugin-runs", map[string]any{"plugin": plugin, "inputs": inputs})
	if code != http.StatusAccepted {
		t.Fatalf("create run: %d %v", code, run)
	}
	return run["id"].(string)
}

// speedUp makes pending plugin_run jobs claimable now, skipping retry backoff.
func (h *procHarness) speedUp(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE jobs SET next_run_at = LOCALTIMESTAMP - interval '1 second' WHERE type = $1 AND status = 'pending'`, JobType); err != nil {
		t.Fatal(err)
	}
}

// waitFinal polls until the run is in a terminal state, skipping backoff.
func (h *procHarness) waitFinal(t *testing.T, id string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		_, run := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id, nil)
		switch run["status"] {
		case "completed", "failed", "cancelled":
			return run
		}
		h.speedUp(t)
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish: %v", id, run)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func (h *procHarness) job(t *testing.T, runID string) (status string, retries int, errMsg string) {
	t.Helper()
	var e *string
	err := h.pool.QueryRow(context.Background(),
		`SELECT status, retry_count, error FROM jobs WHERE type = $1 AND fire_key = $2`, JobType, "plugin_run:"+runID).Scan(&status, &retries, &e)
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		errMsg = *e
	}
	return
}

// waitJob waits for the queue to finalize the job (the handler commits the
// run first, the queue marks the job afterwards).
func (h *procHarness) waitJob(t *testing.T, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, _, _ := h.job(t, runID); st == want {
			return
		}
		if time.Now().After(deadline) {
			st, retries, e := h.job(t, runID)
			t.Fatalf("job is %s (retries %d, %q), want %s", st, retries, e, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *procHarness) results(t *testing.T, id string) []map[string]any {
	t.Helper()
	_, res := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id+"/results", nil)
	var out []map[string]any
	for _, r := range res["results"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func TestProcessPluginRunsEndToEnd(t *testing.T) {
	h := newProcessHarness(t, procOpts{})
	const secret = "sk-e2e-0123456789"
	t.Setenv("API_KEY", secret)
	t.Setenv("DATABASE_URL", "postgres://never-reaches-plugins")

	code, body := h.do("editor-a", "GET", "/api/v2/plugins", nil)
	if code != http.StatusOK || !strings.Contains(mustJSON(body), `"sdk_prov"`) {
		t.Fatalf("catalog: %d %v", code, body)
	}
	for _, p := range body["plugins"].([]any) {
		if pm := p.(map[string]any); pm["runtime"] == "process" && pm["runnable"] != true {
			t.Fatalf("process plugin not runnable: %v", pm)
		}
	}

	h.startWorker()
	id := h.start(t, "sdk_prov", map[string]any{"domain": "acme.example"})
	run := h.waitFinal(t, id, 30*time.Second)
	if run["status"] != "completed" {
		t.Fatalf("run: %v", run)
	}
	stats := run["stats"].(map[string]any)
	if stats["records"] != float64(1) || stats["pages"] != float64(1) || stats["cost_usd"] != 0.01 {
		t.Fatalf("stats: %v", stats)
	}
	res := h.results(t, id)
	if len(res) != 1 {
		t.Fatalf("results: %v", res)
	}
	data := res[0]["data"].(map[string]any)
	if data["domain"] != "acme.example" || data["status"] != float64(200) {
		t.Fatalf("data: %v", data)
	}
	ev := res[0]["evidence"].(map[string]any)
	fetches := ev["fetches"].([]any)
	if ev["runtime"] != "process" || len(fetches) != 1 || fetches[0].(map[string]any)["sha256"] == "" {
		t.Fatalf("evidence: %v", ev)
	}

	// The secret reached the plugin (the fake API answered) but is nowhere in
	// what was stored, including through the evidence the plugin controls.
	var stored string
	if err := h.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(data::text || evidence::text, ''), '') FROM plugin_results`).Scan(&stored); err == nil && strings.Contains(stored, secret) {
		t.Fatal("secret stored in plugin_results")
	}

	// Another tenant cannot see it (RLS), through the same code path.
	if code, _ := h.do("editor-b", "GET", "/api/v2/plugin-runs/"+id+"/results", nil); code != http.StatusNotFound {
		t.Fatalf("cross-tenant results: %d", code)
	}
}

func TestProcessPermanentFailuresAreNotRetried(t *testing.T) {
	h := newProcessHarness(t, procOpts{})
	h.startWorker()

	cases := []struct {
		name, plugin string
		inputs       map[string]any
		wantErr      string
		env          bool
	}{
		{"unhandled exception", "sdk_prov", map[string]any{"domain": "x", "mode": "boom"}, "plugin_exception: ValueError: kaput", true},
		{"missing secret", "sdk_prov", map[string]any{"domain": "x"}, "auth_failed", false},
		{"output the manifest cannot hold", "raw_proc", map[string]any{"mode": "records_and_result"}, "invalid_output", false},
		{"protocol violation", "raw_proc", map[string]any{"mode": "garbage"}, "protocol_error", false},
		{"capability denied", "sdk_prov", map[string]any{"domain": "x", "mode": "denied"}, "capability_denied", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env {
				t.Setenv("API_KEY", "sk-perm-0001")
			}
			id := h.start(t, c.plugin, c.inputs)
			run := h.waitFinal(t, id, 30*time.Second)
			if run["status"] != "failed" || !strings.Contains(fmt.Sprint(run["error"]), c.wantErr) {
				t.Fatalf("run: %v", run)
			}
			h.waitJob(t, id, "completed")
			if _, retries, _ := h.job(t, id); retries != 0 {
				t.Fatalf("a permanent failure was retried %d times", retries)
			}
			if n := len(h.results(t, id)); n != 0 {
				t.Fatalf("%d results committed for a failed run", n)
			}
		})
	}
}

func TestProcessCrashIsRetriedAndThenSucceeds(t *testing.T) {
	h := newProcessHarness(t, procOpts{})
	h.startWorker()
	marker := filepath.Join(h.scratch, "marker")
	id := h.start(t, "raw_proc", map[string]any{"mode": "flaky", "marker": marker})

	// Attempt 1 crashes (SIGSEGV): the run goes back to pending with the reason.
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, run := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id, nil)
		if run["status"] == "pending" && strings.Contains(fmt.Sprint(run["error"]), "Retrying: plugin_crashed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("crash was not reported as a retry: %v", run)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if n := len(h.results(t, id)); n != 0 {
		t.Fatalf("results from a crashed attempt: %d", n)
	}

	run := h.waitFinal(t, id, 30*time.Second) // waitFinal skips the backoff
	if run["status"] != "completed" || run["error"] != nil {
		t.Fatalf("run: %v", run)
	}
	if _, retries, _ := h.job(t, id); retries != 1 {
		t.Fatalf("retries: %d", retries)
	}
	res := h.results(t, id)
	if len(res) != 1 || res[0]["data"].(map[string]any)["recovered"] != true {
		t.Fatalf("results: %v", res)
	}
}

func TestProcessCrashLoopEndsInPermanentFailure(t *testing.T) {
	h := newProcessHarness(t, procOpts{})
	h.startWorker()
	attempts := filepath.Join(h.scratch, "attempts")
	id := h.start(t, "raw_proc", map[string]any{"mode": "count_crash", "attempts": attempts})

	run := h.waitFinal(t, id, 60*time.Second)
	if run["status"] != "failed" || !strings.Contains(fmt.Sprint(run["error"]), "plugin_crashed") {
		t.Fatalf("run: %v", run)
	}
	b, _ := os.ReadFile(attempts)
	h.waitJob(t, id, "failed")
	status, retries, _ := h.job(t, id)
	// The queue's default is 3 retries: four attempts in total, then it stops.
	if n := strings.Count(string(b), "x"); n != 4 || retries != 3 || status != "failed" {
		t.Fatalf("attempts=%d retries=%d job=%s", n, retries, status)
	}
	if n := len(h.results(t, id)); n != 0 {
		t.Fatalf("results: %d", n)
	}
}

func TestProcessTimeoutKillsTheTreeAndIsRetried(t *testing.T) {
	h := newProcessHarness(t, procOpts{})
	h.startWorker()
	pidfile := filepath.Join(h.scratch, "pids.json")
	id := h.start(t, "raw_short", map[string]any{"mode": "tree", "pidfile": pidfile})

	pids := readPIDs(t, pidfile)
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, run := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id, nil)
		if run["status"] == "pending" && strings.Contains(fmt.Sprint(run["error"]), "Retrying: timeout") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout not reported as a retry: %v", run)
		}
		time.Sleep(30 * time.Millisecond)
	}
	for name, pid := range pids {
		waitGone(t, name, pid)
	}
}

func TestCancellingARunningProcessKillsItsTreeAndKeepsTheCancellation(t *testing.T) {
	h := newProcessHarness(t, procOpts{})
	h.startWorker()
	pidfile := filepath.Join(h.scratch, "pids.json")
	id := h.start(t, "raw_proc", map[string]any{"mode": "tree", "pidfile": pidfile})
	pids := readPIDs(t, pidfile)
	h.waitStatus("editor-a", id, "running")

	if code, _ := h.do("editor-a", "POST", "/api/v2/plugin-runs/"+id+"/cancel", nil); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
	for name, pid := range pids {
		waitGone(t, name, pid)
	}
	time.Sleep(500 * time.Millisecond)
	final := h.waitStatus("editor-a", id, "cancelled", "completed", "failed")
	if final["status"] != "cancelled" {
		t.Fatalf("cancellation overwritten: %v", final)
	}
	if n := len(h.results(t, id)); n != 0 {
		t.Fatalf("results after cancel: %d", n)
	}
	if st := h.sup.Stats(); st.Running != 0 {
		t.Fatalf("supervisor still running something: %+v", st)
	}
}

func TestSaturatedProcessPoolDoesNotLoseRuns(t *testing.T) {
	// 6 runs, 2 workers' worth of queue concurrency, a pool of 2 processes:
	// everything completes, nothing is dropped, and the pool never exceeds 2.
	h := newProcessHarness(t, procOpts{maxProcesses: 2, maxPerPlugin: 2, queue: queue.Options{Concurrency: 6, MaxActivePerWorkspace: 6}})
	h.startWorker()
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, h.start(t, "raw_proc", map[string]any{"mode": "hold_slot", "seconds": 0.3}))
	}
	for _, id := range ids {
		if run := h.waitFinal(t, id, 60*time.Second); run["status"] != "completed" {
			t.Fatalf("run %s: %v", id, run)
		}
	}
	if st := h.sup.Stats(); st.PeakRunning != 2 || st.Succeeded != 6 {
		t.Fatalf("stats: %+v", st)
	}
}

// ---- worker crash --------------------------------------------------------------

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "worker" {
		helperWorker()
		return
	}
	os.Exit(m.Run())
}

// helperWorker is a complete worker in its own process: it claims plugin_run
// jobs and supervises Python plugins until it is killed.
func helperWorker() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("HELPER_DB"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	catalog := LoadCatalog(config.Plugins{Dirs: []string{os.Getenv("HELPER_PLUGINS")}, SignaturePolicy: "optional"})
	client, _ := egress.New(egress.Options{Transport: okTransport(), ReplayWithoutRateLimit: true})
	sup, err := process.NewSupervisor(process.Options{Client: client, StateDir: os.Getenv("HELPER_STATE"),
		PythonPath: []string{os.Getenv("HELPER_SDK")}, Logger: quietLog{}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	runner := NewRunner(client, nil, nil)
	runner.SetProcessSupervisor(sup)
	reg := queue.NewRegistry()
	NewWorker(pool, catalog, runner, nil).Register(reg)
	q := queue.New(pool, reg, queue.Options{Concurrency: 1, IdlePoll: 20 * time.Millisecond, ClaimCheckInterval: 50 * time.Millisecond,
		HeartbeatInterval: 200 * time.Millisecond, WorkerID: "helper-worker-A"})
	fmt.Println("READY", os.Getpid())
	_ = q.Run(ctx)
}

func TestWorkerCrashMidRunIsReconciledByAnotherWorker(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		t.Skip()
	}
	h := newProcessHarness(t, procOpts{noWorker: true,
		queue: queue.Options{StaleAfter: 2 * time.Second, ReapInterval: 200 * time.Millisecond, HeartbeatInterval: 200 * time.Millisecond}})
	// The supervisor built by the harness belongs to "worker B", created
	// before worker A dies. Start A as a real process sharing the state dir.
	marker := filepath.Join(h.scratch, "marker")
	pidfile := filepath.Join(h.scratch, "pids.json")
	id := h.start(t, "raw_proc", map[string]any{"mode": "slow_first", "marker": marker, "pidfile": pidfile})

	a := exec.Command(os.Args[0])
	a.Env = append(os.Environ(), helperEnv+"=worker", "HELPER_DB="+h.dbURL, "HELPER_PLUGINS="+h.plugins,
		"HELPER_STATE="+h.state, "HELPER_SDK="+sdkSrc(t), "API_KEY=sk-crash-0001")
	out, err := a.StdoutPipe()
	must(t, err)
	a.Stderr = os.Stderr
	must(t, a.Start())
	t.Cleanup(func() { _ = a.Process.Kill(); _ = a.Wait() })
	buf := make([]byte, 64)
	if n, _ := out.Read(buf); !strings.HasPrefix(string(buf[:n]), "READY") {
		t.Fatalf("worker A did not start: %q", buf[:n])
	}

	// Worker A claims the run and its plugin starts hanging with a process tree.
	pids := readPIDs(t, pidfile)
	h.waitStatus("editor-a", id, "running")

	// Crash worker A. No handler, deferred function or reconciler runs.
	must(t, a.Process.Signal(syscall.SIGKILL))
	_ = a.Wait()
	// The kernel takes the plugin down with its supervisor; background
	// children linger until the next supervisor starts.
	waitGone(t, "plugin", pids["self"])
	_, retries, _ := h.job(t, id)
	if status, _, _ := h.job(t, id); status != "processing" || retries != 0 {
		t.Fatalf("job should still be leased to the dead worker: %s retries=%d", status, retries)
	}
	_, run := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id, nil)
	if run["status"] != "running" {
		t.Fatalf("run: %v", run)
	}

	// Worker B (a restarted worker): its supervisor sweeps A's leftovers at
	// start-up, and its queue reaps the stale lease, requeues, and re-runs.
	sup2, err := process.NewSupervisor(process.Options{Client: h.runner.client, StateDir: h.state,
		PythonPath: []string{sdkSrc(t)}, Logger: quietLog{}})
	must(t, err)
	h.sup.Close()
	h.runner.SetProcessSupervisor(sup2)
	t.Cleanup(sup2.Close)
	for name, pid := range pids {
		waitGone(t, name, pid)
	}
	h.startWorker()

	final := h.waitFinal(t, id, 60*time.Second)
	if final["status"] != "completed" {
		t.Fatalf("run: %v", final)
	}
	res := h.results(t, id)
	if len(res) != 1 || res[0]["data"].(map[string]any)["attempt"] != "second" {
		t.Fatalf("results: %v", res)
	}
	h.waitJob(t, id, "completed")
	if _, retries, _ := h.job(t, id); retries != 1 {
		t.Fatalf("retries=%d, want the one recovery", retries)
	}
}

// ---- process helpers -------------------------------------------------------------

func readPIDs(t *testing.T, path string) map[string]int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && json.Valid(b) {
			var raw map[string]int
			must(t, json.Unmarshal(b, &raw))
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func procGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !procGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
