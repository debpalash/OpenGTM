//go:build integration

package providerrun_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type enrichFn func(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error)

type fakeProv struct {
	name string
	fn   enrichFn
}

func (f *fakeProv) Name() string { return f.name }
func (f *fakeProv) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	return f.fn(ctx, c, key, in)
}

type keyedProv struct {
	fakeProv
	envs []string
}

func (k *keyedProv) KeyEnv() []string { return k.envs }

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), seq.Add(1)) }

func okOut(field, value string, conf float64) providers.Output {
	x := providers.NewObj()
	x.Set(field, value)
	return providers.Output{Value: value, Confidence: conf, Extra: x}
}

type fakeSecrets struct {
	mu     sync.Mutex
	vals   map[string]string // "ws|key"
	err    error
	calls  []string
	global map[string]string
}

func (f *fakeSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ws+"|"+key)
	if f.err != nil {
		return "", "", f.err
	}
	if v, ok := f.vals[ws+"|"+key]; ok && ws != "" {
		return v, "workspace", nil
	}
	if v, ok := f.global[key]; ok {
		return v, "global", nil
	}
	return "", "default", nil
}

type harness struct {
	pool *pgxpool.Pool
	r    *providerrun.Runner
	sec  *fakeSecrets
}

func newHarness(t *testing.T, mut func(*providerrun.Deps)) *harness {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_stats WHERE provider LIKE 't18\_%' OR provider = 'reacher'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_stats WHERE provider = 'numverify'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	sec := &fakeSecrets{vals: map[string]string{}, global: map[string]string{}}
	d := providerrun.Deps{Pool: pool, Secrets: sec, Workers: 4, Cost: func(string) float64 { return 0.05 }}
	if mut != nil {
		mut(&d)
	}
	return &harness{pool: pool, r: providerrun.New(d), sec: sec}
}

// with builds another Runner over the same pool (NewAppPool holds a
// cross-package advisory lock until the test ends, so a test gets one pool).
func (h *harness) with(mut func(*providerrun.Deps)) *harness {
	sec := &fakeSecrets{vals: map[string]string{}, global: map[string]string{}}
	d := providerrun.Deps{Pool: h.pool, Secrets: sec, Workers: 4, Cost: func(string) float64 { return 0.05 }}
	if mut != nil {
		mut(&d)
	}
	return &harness{pool: h.pool, r: providerrun.New(d), sec: sec}
}

type stat struct {
	attempts, hits       int
	conf, latency, cost  float64
	cooldownSecondsAhead float64 // -1 when NULL
}

func (h *harness) stat(t *testing.T, provider, field string) (stat, bool) {
	t.Helper()
	var s stat
	err := h.pool.QueryRow(context.Background(), `SELECT attempts, hits, total_confidence, total_latency_ms, total_cost_usd,
	  COALESCE(EXTRACT(EPOCH FROM (cooldown_until - (now() AT TIME ZONE 'UTC'))), -1)::float8
	  FROM provider_stats WHERE provider=$1 AND field=$2`, provider, field).
		Scan(&s.attempts, &s.hits, &s.conf, &s.latency, &s.cost, &s.cooldownSecondsAhead)
	if err != nil {
		return stat{}, false
	}
	return s, true
}

func TestRunGoProvider(t *testing.T) {
	name := uniq("t18_go")
	var gotKey string
	var gotSettings map[string]any
	providers.Register(&keyedProv{fakeProv{name, func(_ context.Context, _ *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
		gotKey, _ = key()
		gotSettings = in.Settings
		return okOut("email", "a@acme.co", 0.8), nil
	}}, []string{"T18_KEY"}})
	h := newHarness(t, nil)
	h.sec.vals["ws1|T18_KEY"] = "ws1-secret"
	h.sec.vals["ws2|T18_KEY"] = "ws2-secret"

	in := providers.Input{Lead: map[string]any{"company": "Acme"}, Field: "email", Settings: map[string]any{"REACHER_URL": "http://evil.internal"}}
	out, err := h.r.Run(context.Background(), "ws1", name, in, 5*time.Second)
	if err != nil || out.Error != "" || out.Value != "a@acme.co" || out.Confidence != 0.8 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if gotKey != "ws1-secret" {
		t.Fatalf("key resolved for the wrong workspace: %q", gotKey)
	}
	if len(gotSettings) != 0 {
		t.Fatalf("caller Settings must be discarded, got %v", gotSettings)
	}
	s, ok := h.stat(t, name, "email")
	if !ok || s.attempts != 1 || s.hits != 1 || s.conf != 0.8 || s.cost != 0.05 || s.cooldownSecondsAhead != -1 {
		t.Fatalf("ledger %+v ok=%v", s, ok)
	}
}

func TestKeylessProviderAndKeyLookupFailsClosed(t *testing.T) {
	keyless := uniq("t18_keyless")
	providers.Register(&fakeProv{keyless, func(_ context.Context, _ *http.Client, key func() (string, error), _ providers.Input) (providers.Output, error) {
		if k, err := key(); k != "" || err != nil {
			t.Errorf("keyless provider got key %q err %v", k, err)
		}
		return okOut("description", "d", 0.5), nil
	}})
	keyed := uniq("t18_failclosed")
	called := false
	providers.Register(&keyedProv{fakeProv{keyed, func(_ context.Context, _ *http.Client, key func() (string, error), _ providers.Input) (providers.Output, error) {
		called = true
		k, err := key()
		return providers.Output{}, errors.Join(err, errors.New("key="+k))
	}}, []string{"T18_KEY"}})
	h := newHarness(t, nil)
	if out, err := h.r.Run(context.Background(), "ws1", keyless, providers.Input{Field: "description"}, time.Second); err != nil || out.Error != "" {
		t.Fatalf("keyless: %+v %v", out, err)
	}
	h.sec.err = errors.New("db down")
	h.sec.global["T18_KEY"] = "env-fallback"
	_, err := h.r.Run(context.Background(), "ws1", keyed, providers.Input{Field: "email"}, time.Second)
	if err == nil || !called || strings.Contains(err.Error(), "env-fallback") {
		t.Fatalf("lookup error must surface without fallback: err=%v called=%v", err, called)
	}
	if _, ok := h.stat(t, keyed, "email"); ok {
		t.Fatal("an infrastructure error must not write a ledger row")
	}
}

func rpcServer(t *testing.T, h http.HandlerFunc) *pyworker.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return pyworker.New(srv.URL, "tok-tok-tok", nil)
}

func TestRunPythonProviderViaRPC(t *testing.T) {
	name := uniq("t18_py")
	var mu sync.Mutex
	var calls []map[string]any
	py := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		calls = append(calls, m)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok","output":{"provider":"x","success":true,"fields":{"zeta":"z","email":"p@acme.co"},"confidence":0.7,"error":""},"error":null,"cost_usd":0.05}`))
	})
	h := newHarness(t, func(d *providerrun.Deps) { d.Py = py })
	in := providers.Input{Lead: map[string]any{"company": "Acme"}, Field: "email"}
	out, err := h.r.Run(context.Background(), "ws1", name, in, 3*time.Second)
	if err != nil || out.Error != "" || out.Value != "p@acme.co" || out.Confidence != 0.7 || out.CostUSD != 0.05 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if keys := out.Extra.Keys(); len(keys) != 2 || keys[0] != "zeta" {
		t.Fatalf("field order lost: %v", keys)
	}
	if _, err := h.r.Run(context.Background(), "ws1", name, in, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0]["provider"] != name || calls[0]["workspace_id"] != "ws1" {
		t.Fatalf("calls %v", calls)
	}
	if calls[0]["call_id"] == "" || calls[0]["call_id"] == calls[1]["call_id"] {
		t.Fatalf("call_id must be fresh per attempt: %v %v", calls[0]["call_id"], calls[1]["call_id"])
	}
	if lead, _ := calls[0]["input"].(map[string]any); lead["company"] != "Acme" {
		t.Fatalf("input %v", calls[0]["input"])
	}
	if s, ok := h.stat(t, name, "email"); !ok || s.attempts != 2 || s.hits != 2 {
		t.Fatalf("ledger %+v", s)
	}
}

func TestRunTimeoutDoesNotBlockSlot(t *testing.T) {
	slow, fast := uniq("t18_slow"), uniq("t18_fast")
	providers.Register(&fakeProv{slow, func(ctx context.Context, _ *http.Client, _ func() (string, error), _ providers.Input) (providers.Output, error) {
		<-ctx.Done()
		return providers.Output{}, ctx.Err()
	}})
	providers.Register(&fakeProv{fast, func(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
		return okOut("description", "d", 0.5), nil
	}})
	h := newHarness(t, func(d *providerrun.Deps) { d.Workers = 1 })
	t0 := time.Now()
	out, err := h.r.Run(context.Background(), "ws1", slow, providers.Input{Field: "description"}, 50*time.Millisecond)
	if err != nil || out.Error != "timeout" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("timeout did not fire")
	}
	if out, err := h.r.Run(context.Background(), "ws1", fast, providers.Input{Field: "description"}, time.Second); err != nil || out.Error != "" {
		t.Fatalf("slot not released after a ctx-honouring timeout: %+v %v", out, err)
	}
	s, ok := h.stat(t, slow, "description")
	if !ok || s.hits != 0 || s.cooldownSecondsAhead < 150 || s.cooldownSecondsAhead > 181 {
		t.Fatalf("timeout ledger %+v ok=%v (want 180s cooldown)", s, ok)
	}
}

func TestAbandonedCallsBounded(t *testing.T) {
	name := uniq("t18_stuck")
	release := make(chan struct{})
	var started atomic.Int32
	providers.Register(&fakeProv{name, func(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
		started.Add(1)
		<-release // ignores ctx on purpose
		return okOut("description", "late", 0.5), nil
	}})
	h := newHarness(t, func(d *providerrun.Deps) { d.Workers = 2 })
	base := telemetry.AbandonedProviderCalls()
	for i := 0; i < 2; i++ {
		out, err := h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "description"}, 30*time.Millisecond)
		if err != nil || out.Error != "timeout" {
			t.Fatalf("run %d: %+v %v", i, out, err)
		}
	}
	if got := telemetry.AbandonedProviderCalls() - base; got != 2 {
		t.Fatalf("abandoned gauge %d, want 2", got)
	}
	out, err := h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "description"}, 30*time.Millisecond)
	if err != nil || out.Error != "provider_busy" {
		t.Fatalf("third call must be refused while both slots are abandoned: %+v %v", out, err)
	}
	if started.Load() != 2 {
		t.Fatalf("provider started %d times, want 2 (bounded)", started.Load())
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for telemetry.AbandonedProviderCalls() != base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if telemetry.AbandonedProviderCalls() != base {
		t.Fatal("gauge must fall back when the abandoned calls return")
	}
	out, err = h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "description"}, time.Second)
	if err != nil || out.Error != "" {
		t.Fatalf("slots must be reusable: %+v %v", out, err)
	}
}

func TestRunCacheHitSkipsProvider(t *testing.T) {
	name := uniq("t18_cache")
	var n atomic.Int32
	providers.Register(&fakeProv{name, func(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
		n.Add(1)
		return okOut("description", "We sell anvils", 0.9), nil
	}})
	h := newHarness(t, nil)
	wsA, wsB := uniq("t18wsA"), uniq("t18wsB")
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM enrichment_cache WHERE workspace_id = ANY($1)`, []string{wsA, wsB})
	})
	lead := map[string]any{"website": "https://www.acme.co/about", "company": "Acme"}
	in := providers.Input{Lead: lead, Field: "description"}
	for i := 0; i < 2; i++ {
		out, err := h.r.Run(context.Background(), wsA, name, in, time.Second)
		if err != nil || out.Value != "We sell anvils" || out.Error != "" {
			t.Fatalf("run %d: %+v %v", i, out, err)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("provider called %d times, want 1 (second served from cache)", n.Load())
	}
	if s, _ := h.stat(t, name, "description"); s.attempts != 1 {
		t.Fatalf("a cache hit must not write the ledger: %+v", s)
	}
	if _, err := h.r.Run(context.Background(), wsB, name, in, time.Second); err != nil || n.Load() != 2 {
		t.Fatalf("workspace B must not be served workspace A's cache (calls=%d err=%v)", n.Load(), err)
	}
}

func TestRunCancelPropagatesToRPC(t *testing.T) {
	name := uniq("t18_pycancel")
	arrived, cancelled := make(chan struct{}), make(chan struct{})
	py := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a dropped client only after the body is read
		close(arrived)
		<-r.Context().Done()
		close(cancelled)
	})
	h := newHarness(t, func(d *providerrun.Deps) { d.Py = py })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.r.Run(ctx, "ws1", name, providers.Input{Field: "email"}, 30*time.Second)
		done <- err
	}()
	<-arrived
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("RPC request was not cancelled")
	}
	if _, ok := h.stat(t, name, "email"); ok {
		t.Fatal("a cancelled call must not write the ledger")
	}
}

func TestStatsLedgerRowsMatchPython(t *testing.T) {
	okP, badP, rlP := uniq("t18_ok"), uniq("t18_bad"), uniq("t18_rl")
	providers.Register(&fakeProv{okP, func(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
		return okOut("phone", "+1", 0.6), nil
	}})
	providers.Register(&fakeProv{badP, func(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
		return providers.Output{Error: "HTTP 500"}, nil
	}})
	py := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","output":null,"error":"RuntimeError: 429 Too Many Requests","cost_usd":0}`))
	})
	h := newHarness(t, func(d *providerrun.Deps) { d.Py = py })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, _ = h.r.Run(ctx, "ws1", okP, providers.Input{Field: "phone"}, time.Second)
	}
	_, _ = h.r.Run(ctx, "ws1", badP, providers.Input{Field: "phone"}, time.Second)
	out, err := h.r.Run(ctx, "ws1", rlP, providers.Input{Field: "phone"}, time.Second)
	if err != nil || out.Error == "" {
		t.Fatalf("rl: %+v %v", out, err)
	}
	if s, _ := h.stat(t, okP, "phone"); s.attempts != 2 || s.hits != 2 || s.conf != 1.2 || s.cost != 0.1 || s.cooldownSecondsAhead != -1 || s.latency < 0 {
		t.Errorf("ok ledger %+v", s)
	}
	if s, _ := h.stat(t, badP, "phone"); s.attempts != 1 || s.hits != 0 || s.conf != 0 || s.cost != 0 || s.cooldownSecondsAhead != -1 {
		t.Errorf("failure ledger %+v (a plain failed result sets no cooldown)", s)
	}
	if s, _ := h.stat(t, rlP, "phone"); s.attempts != 1 || s.hits != 0 || s.cooldownSecondsAhead < 270 || s.cooldownSecondsAhead > 301 {
		t.Errorf("rate-limited exception ledger %+v (want 300s cooldown)", s)
	}
}

func TestUnavailableIsDistinctError(t *testing.T) {
	name := uniq("t18_nopy")
	in := providers.Input{Field: "email"}
	h := newHarness(t, nil)
	_, err := h.r.Run(context.Background(), "ws1", name, in, time.Second)
	if !errors.Is(err, providerrun.ErrUnavailable) || !errors.Is(err, pyworker.ErrUnavailable) {
		t.Fatalf("nil Py: %v", err)
	}
	h = h.with(func(d *providerrun.Deps) { d.Py = pyworker.New("", "", nil) })
	if _, err := h.r.Run(context.Background(), "ws1", name, in, time.Second); !errors.Is(err, providerrun.ErrUnavailable) {
		t.Fatalf("unconfigured client: %v", err)
	}
	for status, want := range map[int]error{404: providerrun.ErrUnknownProvider, 401: providerrun.ErrUnauthorized} {
		st := status
		py := rpcServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(st) })
		h = h.with(func(d *providerrun.Deps) { d.Py = py })
		_, err := h.r.Run(context.Background(), "ws1", name, in, time.Second)
		if !errors.Is(err, want) {
			t.Errorf("status %d: err=%v want %v", status, err, want)
		}
		if _, ok := h.stat(t, name, "email"); ok {
			t.Errorf("status %d: rejected call must not write the ledger", status)
		}
	}
}

func TestPythonTimeoutNoRetry(t *testing.T) {
	name := uniq("t18_pyto")
	var n atomic.Int32
	py := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`{"status":"timeout","output":null,"error":"provider timed out","cost_usd":0}`))
	})
	h := newHarness(t, func(d *providerrun.Deps) { d.Py = py })
	out, err := h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "email"}, time.Second)
	if err != nil || out.Error != "timeout" || n.Load() != 1 {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, n.Load())
	}
	if s, _ := h.stat(t, name, "email"); s.attempts != 1 || s.cooldownSecondsAhead < 150 {
		t.Fatalf("ledger %+v", s)
	}
}

// Reacher returns Error=="" for unconfigured / breaker_open; the runner must
// read Extra.detail so those are not counted as a success.
func TestReacherUnconfiguredIsNotSuccess(t *testing.T) {
	h := newHarness(t, nil)
	in := providers.Input{Lead: map[string]any{"email": "a@acme.co"}, Field: "email"}
	out, err := h.r.Run(context.Background(), "ws1", "reacher", in, time.Second)
	if err != nil || out.Error == "" {
		t.Fatalf("unconfigured reacher must be an error output: %+v %v", out, err)
	}
	s, ok := h.stat(t, "reacher", "email")
	if !ok || s.attempts != 1 || s.hits != 0 {
		t.Fatalf("ledger %+v ok=%v", s, ok)
	}
}

func reacherServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`{"is_reachable":"safe","smtp":{"is_deliverable":true,"is_catch_all":false}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestReacherSettingsGlobalOnlyAndOperatorAllowList(t *testing.T) {
	srv, hits := reacherServer(t)
	hostport := strings.TrimPrefix(srv.URL, "http://")
	_, _, _ = net.SplitHostPort(hostport)
	in := providers.Input{Lead: map[string]any{"email": "a@acme.co"}, Field: "email"}
	setup := func(d *providerrun.Deps) {}
	h := newHarness(t, setup)
	h.sec.global["REACHER_URL"] = srv.URL
	h.sec.global["REACHER_ENABLED"] = "1"
	h.sec.vals["ws1|REACHER_URL"] = "http://169.254.169.254/" // a workspace value must never be read
	// Not on the operator allow-list: the loopback server is refused.
	out, err := h.r.Run(context.Background(), "ws1", "reacher", in, 3*time.Second)
	if err != nil || out.Error == "" || hits.Load() != 0 {
		t.Fatalf("loopback must be refused without an allow-list: %+v %v hits=%d", out, err, hits.Load())
	}
	for _, c := range h.sec.calls {
		if c == "ws1|REACHER_URL" || c == "ws1|REACHER_ENABLED" || c == "ws1|REACHER_TIMEOUT" {
			t.Fatalf("settings were resolved per workspace: %v", h.sec.calls)
		}
	}
	// Listed exactly: works.
	h2 := h.with(func(d *providerrun.Deps) { d.OperatorAllow = []string{hostport} })
	h2.sec.global["REACHER_URL"] = srv.URL
	h2.sec.global["REACHER_ENABLED"] = "1"
	h2.sec.vals["ws1|REACHER_URL"] = "http://169.254.169.254/"
	out, err = h2.r.Run(context.Background(), "ws1", "reacher", in, 3*time.Second)
	if err != nil || out.Error != "" || out.Value != "valid" || hits.Load() != 1 {
		t.Fatalf("allow-listed private reacher: %+v %v hits=%d", out, err, hits.Load())
	}
}

func TestSettingsLookupErrorFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	h.sec.err = errors.New("db down")
	in := providers.Input{Lead: map[string]any{"email": "a@acme.co"}, Field: "email"}
	if _, err := h.r.Run(context.Background(), "ws1", "reacher", in, time.Second); err == nil {
		t.Fatal("a settings lookup failure must be returned, never treated as unconfigured")
	}
}
