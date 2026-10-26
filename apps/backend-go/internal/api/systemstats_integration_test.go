//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Plan 14 Task 17: GET /api/system-stats against the recorded Python fixtures (testdata/contract/jobs/system_stats_*).
// The proxy_pool and rate_limiter objects come from the pyworker RPC POST /internal/leadgen/system-stats; the fake
// worker here replays the recorded answer.

const statsToken = "svc-token-abc-0123456789abcdef0123456789"

type statsWorker struct {
	srv   *httptest.Server
	calls atomic.Int32
	delay time.Duration
	code  int
	body  string
}

func newStatsWorker(t *testing.T) *statsWorker {
	t.Helper()
	// the answer of the RPC: Python's ProxyPool().stats() and RateLimiter().stats() in source order
	w := &statsWorker{code: 200, body: `{"proxy_pool": {"total": 0, "http": 0, "socks5": 0, "socks4": 0, "blocked": 0, "domain_assignments": 0}, "rate_limiter": {"tracked_domains": 0, "tripped_domains": []}}`}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/internal/leadgen/system-stats" || r.Header.Get("Authorization") != "Bearer "+statsToken {
			rw.WriteHeader(401)
			return
		}
		time.Sleep(w.delay)
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(w.code)
		_, _ = rw.Write([]byte(w.body))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// newStatsEnv is newJobsEnv with a pyworker client (nil: no worker) and the RPC timeout in seconds.
func newStatsEnv(t *testing.T, py *pyworker.Client, timeoutS int) jobsEnv {
	t.Helper()
	return newJobsEnv(t, nil).withWorker(py, timeoutS)
}

// withWorker re-registers the jobs routes on the same pool with another worker client (one env per test: NewAppPool
// holds an advisory lock for the whole test).
func (e jobsEnv) withWorker(py *pyworker.Client, timeoutS int) jobsEnv {
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, PyWorker: py,
		Flags:                   config.Flags{PGLeadStore: true, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: true, SystemStatsRPCTimeoutSeconds: timeoutS}},
		CollectionLedgerBackend: "postgres"}
	RegisterJobs(r, d)
	e.h = r.Handler()
	return e
}

func (e jobsEnv) statsGet(who, ws string) (*httptest.ResponseRecorder, contract.Resp) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	return e.do("GET", "/api/system-stats", "", "", hdr)
}

func TestSystemStatsMatchesFixture(t *testing.T) {
	w := newStatsWorker(t)
	e := newStatsEnv(t, pyworker.New(w.srv.URL, statsToken, nil), 3)
	for _, c := range []struct{ fixture, who, ws string }{
		{"system_stats_alpha", "alice", ""},
		{"system_stats_beta", "bob", "ws-beta"},
		{"system_stats_viewer", "bob", "ws-alpha"}, // viewers read: current_workspace only
		{"system_stats_no_auth", "", ""},
	} {
		fx := contract.Load(t, "jobs/"+c.fixture)
		rec, got := e.statsGet(c.who, c.ws)
		contract.AssertEqual(t, got, fx.Response)
		if fx.Response.Status == 200 && rec.Header().Get("X-Stats-Source") != "" {
			t.Errorf("%s: degraded header on a healthy answer", c.fixture)
		}
	}
	// key order of Python's dict: proxy_pool, rate_limiter, jobs; jobs total, pending, running, done, failed
	rec, _ := e.statsGet("alice", "")
	want := `{"proxy_pool":{"total":0,"http":0,"socks5":0,"socks4":0,"blocked":0,"domain_assignments":0},"rate_limiter":{"tracked_domains":0,"tripped_domains":[]},"jobs":{"total":7,"pending":1,"running":2,"done":2,"failed":1}}`
	if rec.Body.String() != want {
		t.Errorf("body bytes:\n got %s\nwant %s", rec.Body.String(), want)
	}
}

// insertJobs writes n collection_jobs rows of the workspace; created_at is ISO TEXT, lexical = chronological.
func insertJobs(t *testing.T, e jobsEnv, ws string, rows []struct{ id, status, created string }) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM collection_jobs"); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO collection_jobs (id, query, status, created_at, workspace_id) VALUES ($1, 'q', $2, $3, $4)`, r.id, r.status, r.created, ws); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func jobCounts(t *testing.T, e jobsEnv, who, ws string) map[string]int {
	t.Helper()
	rec, _ := e.statsGet(who, ws)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var b struct {
		Jobs map[string]int `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b.Jobs
}

func TestJobCountsWindowOf100(t *testing.T) {
	w := newStatsWorker(t)
	e := newStatsEnv(t, pyworker.New(w.srv.URL, statsToken, nil), 3)
	var rows []struct{ id, status, created string }
	add := func(n int, status string) {
		for i := 0; i < n; i++ {
			k := len(rows)
			rows = append(rows, struct{ id, status, created string }{fmt.Sprintf("c%031d", k), status, fmt.Sprintf("2026-09-01T00:%02d:%02d+00:00", k/60, k%60)})
		}
	}
	add(20, "failed") // the 20 oldest rows fall outside get_jobs(limit=100)
	add(40, "pending")
	add(30, "running")
	add(20, "done")
	add(5, "failed")
	add(5, "pending")
	insertJobs(t, e, "ws-beta", rows)
	got := jobCounts(t, e, "bob", "ws-beta")
	want := map[string]int{"total": 100, "pending": 45, "running": 30, "done": 20, "failed": 5}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all %v)", k, got[k], v, got)
		}
	}
}

func TestCancelledOnlyInTotal(t *testing.T) {
	w := newStatsWorker(t)
	e := newStatsEnv(t, pyworker.New(w.srv.URL, statsToken, nil), 3)
	insertJobs(t, e, "ws-beta", []struct{ id, status, created string }{
		{fmt.Sprintf("d%031d", 1), "cancelled", "2026-09-01T00:00:01+00:00"},
		{fmt.Sprintf("d%031d", 2), "cancelled", "2026-09-01T00:00:02+00:00"},
		{fmt.Sprintf("d%031d", 3), "done", "2026-09-01T00:00:03+00:00"},
	})
	got := jobCounts(t, e, "bob", "ws-beta")
	if got["total"] != 3 || got["done"] != 1 || got["pending"]+got["running"]+got["failed"] != 0 {
		t.Errorf("counts %v", got)
	}
}

func TestSystemStatsTenantJobsOnly(t *testing.T) {
	w := newStatsWorker(t)
	e := newStatsEnv(t, pyworker.New(w.srv.URL, statsToken, nil), 3)
	insertJobs(t, e, "ws-alpha", []struct{ id, status, created string }{{fmt.Sprintf("e%031d", 1), "running", "2026-09-01T00:00:01+00:00"}})
	insertJobs(t, e, "ws-beta", nil)
	if got := jobCounts(t, e, "alice", ""); got["total"] != 1 || got["running"] != 1 {
		t.Errorf("alpha sees %v", got)
	}
	if got := jobCounts(t, e, "bob", "ws-beta"); got["total"] != 0 {
		t.Errorf("beta sees alpha's jobs: %v", got)
	}
}

const degradedBody = `{"proxy_pool":{"total":0,"http":0,"socks5":0,"socks4":0,"blocked":0,"domain_assignments":0},"rate_limiter":{"tracked_domains":0,"tripped_domains":[]},"jobs":{"total":7,"pending":1,"running":2,"done":2,"failed":1}}`

func assertDegraded(t *testing.T, e jobsEnv) {
	t.Helper()
	rec, _ := e.statsGet("alice", "")
	if rec.Code != 200 || rec.Header().Get("X-Stats-Source") != "degraded" || rec.Body.String() != degradedBody {
		t.Errorf("want degraded 200: %d %q %s", rec.Code, rec.Header().Get("X-Stats-Source"), rec.Body)
	}
}

func TestPyWorkerDownDegradedShape(t *testing.T) {
	// no worker configured, connection refused, 5xx, garbage and a wrong shape all degrade (jobs stay real)
	e := newStatsEnv(t, nil, 3)
	assertDegraded(t, e)
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	assertDegraded(t, e.withWorker(pyworker.New(url, statsToken, nil), 3))
	for _, c := range []struct {
		code int
		body string
	}{{500, `{}`}, {200, `not json`}, {200, `{"proxy_pool": 1, "rate_limiter": {}}`}, {200, `{"proxy_pool": {}}`}, {200, `[]`}} {
		w := newStatsWorker(t)
		w.code, w.body = c.code, c.body
		assertDegraded(t, e.withWorker(pyworker.New(w.srv.URL, statsToken, nil), 3))
	}
	// a worker that rejects the token (401) degrades
	bad := newStatsWorker(t)
	rec, _ := e.withWorker(pyworker.New(bad.srv.URL, "wrong-token", nil), 3).statsGet("alice", "")
	if rec.Header().Get("X-Stats-Source") != "degraded" || rec.Code != 200 {
		t.Errorf("401 from worker: %d", rec.Code)
	}
}

func TestPyWorkerSlowTimesOut(t *testing.T) {
	w := newStatsWorker(t)
	w.delay = 3 * time.Second
	e := newStatsEnv(t, pyworker.New(w.srv.URL, statsToken, nil), 1)
	start := time.Now()
	assertDegraded(t, e)
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Errorf("took %v, the RPC budget is 1 s", d)
	}
}

// TestSystemStatsHidesTrippedDomains: the worker's rate limiter is process-global, so tripped_domains names domains
// scraped for any tenant. Only a global admin sees them; everyone else gets [] (key kept, tracked_domains kept).
func TestSystemStatsHidesTrippedDomains(t *testing.T) {
	w := newStatsWorker(t)
	w.body = `{"proxy_pool": {"total": 0, "http": 0, "socks5": 0, "socks4": 0, "blocked": 0, "domain_assignments": 0}, "rate_limiter": {"tracked_domains": 2, "tripped_domains": ["tenant-b-target.example"]}}`
	e := newStatsEnv(t, pyworker.New(w.srv.URL, statsToken, nil), 3)
	erin := e.mustUserByName(t, "erin")
	e.member(t, "ws-beta", erin.ID, "admin") // workspace admin, not a global admin
	for _, c := range []struct {
		who, ws string
		want    string
	}{
		{"bob", "ws-alpha", `[]`},                    // viewer
		{"erin", "ws-alpha", `[]`},                   // editor
		{"erin", "ws-beta", `[]`},                    // workspace admin
		{"alice", "", `["tenant-b-target.example"]`}, // global admin
	} {
		rec, _ := e.statsGet(c.who, c.ws)
		var b struct {
			RL struct {
				Tracked int             `json:"tracked_domains"`
				Tripped json.RawMessage `json:"tripped_domains"`
			} `json:"rate_limiter"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", c.who, rec.Code, rec.Body)
		}
		if string(b.RL.Tripped) != c.want || b.RL.Tracked != 2 {
			t.Errorf("%s/%s: tripped %s tracked %d, want %s", c.who, c.ws, b.RL.Tripped, b.RL.Tracked, c.want)
		}
	}
}
