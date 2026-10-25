//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// sim is a programmable provider: every manifest posts to
// http://sim.provider.example/<name>, which the test client dials to the
// httptest server (the public-looking host passes the egress URL guard).
type sim struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   map[string]int
	handler func(name string, n int, w http.ResponseWriter, r *http.Request)
}

func newSim(t *testing.T, h func(name string, n int, w http.ResponseWriter, r *http.Request)) *sim {
	t.Helper()
	s := &sim{calls: map[string]int{}, handler: h}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice a client abort
		name := strings.TrimPrefix(r.URL.Path, "/")
		s.mu.Lock()
		s.calls[name]++
		n := s.calls[name]
		s.mu.Unlock()
		if s.handler != nil {
			s.handler(name, n, w, r)
			return
		}
		fmt.Fprint(w, `{"v":"1999"}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sim) client() *http.Client {
	addr := s.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func (s *sim) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

func (s *sim) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

func manifestJSON(name string) string {
	return fmt.Sprintf(`{"name":%q,"capability":"company_size","default_confidence":0.8,"auth":{"type":"none"},
	  "request":{"method":"POST","url":"http://sim.provider.example/%s","timeout":5,"body_template":{"company":"{{input.company}}"}},
	  "response":{"mappings":{"founded_year":"$.v"}}}`, name, name)
}

const target = "founded_year"

func colJSON(id string, chain ...string) string {
	b, _ := json.Marshal(chain)
	return fmt.Sprintf(`{"id":%q,"type":"waterfall","name":%q,"target_field":%q,"waterfall":%s}`, id, "Col "+id, target, b)
}

// payloadFor builds a v1 run_workbook payload.
func payloadFor(ws, wb string, costs map[string]float64, extra map[string]any) []byte {
	manifests, sha := map[string]json.RawMessage{}, map[string]string{}
	for name := range costs {
		manifests[name] = json.RawMessage(manifestJSON(name))
		sha[name] = "deadbeef"
	}
	p := map[string]any{"workspace_id": ws, "workbook_id": wb, "retry_passes": 0, "concurrency": 1,
		"go": map[string]any{"version": 1, "manifests": manifests, "manifest_sha256": sha, "costs": costs, "default_concurrency": 4}}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return b
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	ctx   context.Context
	redis string
}

func setup(t *testing.T, workspaces ...string) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 16)
	e := &env{t: t, pool: pool, ctx: context.Background(), redis: os.Getenv("TEST_REDIS_URL")}
	if _, err := pool.Exec(e.ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(e.ctx, `DELETE FROM provider_stats`); err != nil {
		t.Fatal(err)
	}
	for _, ws := range workspaces {
		e.inWS(ws, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM workbook_spend_attempts`, `DELETE FROM workbook_enrichments`,
				`DELETE FROM workbook_rows`, `DELETE FROM workbooks`} {
				if _, err := tx.Exec(e.ctx, q); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return e
}

func (e *env) inWS(ws string, fn func(pgx.Tx) error) {
	e.t.Helper()
	if err := tenancy.InWorkspace(e.ctx, e.pool, ws, fn); err != nil {
		e.t.Fatal(err)
	}
}

// seed creates a workbook with n rows and returns the row ids.
func (e *env) seed(ws, wb, columns string, n int, budgetMax float64) []int32 {
	e.t.Helper()
	var ids []int32
	e.inWS(ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO workbooks (id, name, status, workspace_id, source_type, columns_config, budget_max_usd, budget_spent_usd, total_rows, completed_rows)
		  VALUES ($1, 'wb', 'idle', $2, 'manual', $3::json, $4, 0, 0, 0)`, wb, ws, columns, budgetMax); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			var id int32
			if err := tx.QueryRow(e.ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments)
			  VALUES ($1, $2, $3, $4::json, '{}'::json) RETURNING id`, wb, ws, i,
				fmt.Sprintf(`{"company":"Acme %d","website":"acme%d.example.com"}`, i, i)).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids
}

func (e *env) claim(ws string, payload []byte) *queue.Claim {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
	  VALUES ('run_workbook', $1::json, 'pending', 0, now() AT TIME ZONE 'UTC', 0, 3, $2, 'go')`, string(payload), ws); err != nil {
		e.t.Fatal(err)
	}
	c, err := queue.ClaimNext(e.ctx, e.pool, "go-test-1", 0)
	if err != nil || c == nil {
		e.t.Fatalf("claim: %v %v", c, err)
	}
	return c
}

// reclaim claims the requeued job as a second worker.
func (e *env) reclaim() *queue.Claim {
	e.t.Helper()
	c, err := queue.ClaimNext(e.ctx, e.pool, "go-test-2", 0)
	if err != nil || c == nil {
		e.t.Fatalf("reclaim: %v %v", c, err)
	}
	return c
}

// manifestSrv serves manifestJSON(name) with the harness hash "deadbeef" (what
// payloadFor claims), unless a test overrides a name's body or hash.
type manifestSrv struct {
	body    map[string]string // per-name body override
	sha     map[string]string // per-name hash override
	missing map[string]bool   // per-name 404
	fail    bool              // transient API failure
}

func (m manifestSrv) Manifest(_ context.Context, name string) (json.RawMessage, string, error) {
	if m.fail {
		return nil, "", errors.New("api down")
	}
	if m.missing[name] {
		return nil, "", credentials.ErrManifestNotFound
	}
	body, ok := m.body[name]
	if !ok {
		body = manifestJSON(name)
	}
	return json.RawMessage(body), orStr(m.sha[name], "deadbeef"), nil
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (e *env) deps(s *sim, pub progress.Publisher) workbook.Deps {
	if pub == nil {
		pub = progress.Nop{}
	}
	return workbook.Deps{Pool: e.pool, Client: s.client(), Progress: pub, Manifests: manifestSrv{}, Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StopCheckInterval: 20 * time.Millisecond}
}

func (e *env) one(ws, q string, args ...any) (v int64) {
	e.t.Helper()
	e.inWS(ws, func(tx pgx.Tx) error { return tx.QueryRow(e.ctx, q, args...).Scan(&v) })
	return v
}

func (e *env) str(ws, q string, args ...any) (v string) {
	e.t.Helper()
	e.inWS(ws, func(tx pgx.Tx) error {
		var s *string
		err := tx.QueryRow(e.ctx, q, args...).Scan(&s)
		if s != nil {
			v = *s
		}
		return err
	})
	return v
}

func (e *env) spentUSD(ws, wb string) (v float64) {
	e.inWS(ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT budget_spent_usd FROM workbooks WHERE id=$1`, wb).Scan(&v)
	})
	return v
}

// watch emulates the worker's heartbeat/cancel watcher: ctx is cancelled when
// the lease is lost or the job is cancelled.
func (e *env) watch(c *queue.Claim) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(15 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				active, err := queue.Heartbeat(ctx, e.pool, c)
				cancelled, _ := queue.IsCancelled(ctx, e.pool, c)
				if (err == nil && !active) || cancelled {
					cancel(fmt.Errorf("lease lost or job cancelled"))
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); <-done }
}

// collector records messages published for a workbook.
type collector struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (e *env) subscribe(wb string) (*collector, progress.Publisher) {
	e.t.Helper()
	if e.redis == "" {
		return &collector{}, progress.Nop{}
	}
	opt, err := redis.ParseURL(e.redis)
	if err != nil {
		e.t.Fatal(err)
	}
	cl := redis.NewClient(opt)
	ps := cl.Subscribe(e.ctx, progress.Channel(wb))
	if _, err := ps.Receive(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	col := &collector{}
	go func() {
		for m := range ps.Channel() {
			var v map[string]any
			if json.Unmarshal([]byte(m.Payload), &v) == nil {
				col.mu.Lock()
				col.msgs = append(col.msgs, v)
				col.mu.Unlock()
			}
		}
	}()
	e.t.Cleanup(func() { _ = ps.Close(); _ = cl.Close() })
	pub, err := progress.NewRedis(e.redis, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = pub.Close() })
	return col, pub
}

func (c *collector) wait(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.msgs) >= n {
			out := append([]map[string]any(nil), c.msgs...)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("expected %d redis messages, got %d: %v", n, len(c.msgs), c.msgs)
	return nil
}
