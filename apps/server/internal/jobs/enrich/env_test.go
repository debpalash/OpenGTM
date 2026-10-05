package enrich

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

const itKey = "it-secret-value"

// sim is a deterministic HTTPS provider. The domain a row supplies picks the
// behaviour: block.example waits until released (or the caller hangs up),
// slow.example takes delay, err500.example fails, everything else answers.
type sim struct {
	*httptest.Server
	mu                    sync.Mutex
	reqs                  []simReq
	inflight, maxInflight atomic.Int64
	delay                 time.Duration
	release               chan struct{}
	started               chan struct{} // one send per request that reached block.example
}

type simReq struct {
	Method, Path, Domain, Key, Body string
	Header                          http.Header
}

func newSim(t *testing.T) *sim {
	t.Helper()
	s := &sim{release: make(chan struct{}), started: make(chan struct{}, 1024)}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *sim) serve(w http.ResponseWriter, r *http.Request) {
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxInflight.Load()
		if n <= m || s.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	var body struct{ Domain string }
	raw := make([]byte, 4096)
	k, _ := r.Body.Read(raw)
	_ = json.Unmarshal(raw[:k], &body)
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		domain = body.Domain
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, simReq{r.Method, r.URL.Path, domain, r.URL.Query().Get("key"), string(raw[:k]), r.Header.Clone()})
	s.mu.Unlock()

	switch domain {
	case "block.example":
		s.started <- struct{}{}
		select {
		case <-s.release:
		case <-r.Context().Done():
			return
		}
	case "slow.example":
		time.Sleep(s.delay)
	case "err500.example":
		http.Error(w, `{"error":"boom"}`, 500)
		return
	}
	if s.delay > 0 && domain != "slow.example" && domain != "block.example" {
		time.Sleep(s.delay)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"data":{"email":"info@%s","phone":"+15550100"}}`, domain)
}

func (s *sim) count(domain string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if domain == "" || r.Domain == domain {
			n++
		}
	}
	return n
}

func (s *sim) requests() []simReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]simReq(nil), s.reqs...)
}

func (s *sim) client(t *testing.T) *egress.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	c, err := egress.New(egress.Options{
		Version: "test", DefaultRPS: 5000, AllowPrivateForTesting: true, TLSConfig: &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const manifestTmpl = `manifest_version: "1"
name: %s
capability: email
default_confidence: 0.8
cost_per_lookup: %s
%srequest:
  method: %s
  url: BASE%s
  timeout: 5
%sresponse:
  error_path: error
  mappings:
    email: "$.data.email"
    phone: "$.data.phone"
`

func writeConnectors(t *testing.T, base string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, cost, auth, method, path, extra string) {
		text := fmt.Sprintf(manifestTmpl, name, cost, auth, method, path, extra)
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(strings.ReplaceAll(text, "BASE", base)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	q := "  query:\n    domain: \"{{input.domain}}\"\n"
	write("it_free", "0.0", "", "GET", "/free", q)
	write("it_phone", "0.0", "", "GET", "/free", q)
	write("it_key", "0.0", "auth:\n  type: header\n  param: X-Api-Key\n  env_var: IT_KEY\n", "GET", "/free", q)
	write("it_query", "0.0", "auth:\n  type: query\n  param: key\n  env_var: IT_KEY\n", "GET", "/free", q)
	write("it_paid", "0.05", "", "POST", "/paid", "  body:\n    domain: \"{{input.domain}}\"\n")
	return dir
}

type env struct {
	t      *testing.T
	owner  *pgxpool.Pool
	app    *pgxpool.Pool
	sim    *sim
	dir    string
	w      *Worker
	spaces []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	t.Setenv("IT_KEY", itKey)
	s := newSim(t)
	e := &env{
		t: t, owner: dbtest.Pool(t, ownerURL, 4), app: dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 24),
		sim: s, dir: writeConnectors(t, s.URL),
	}
	e.w = NewWorker(e.app, loadConnectors(t, e.dir), s.client(t), nil, 5000)
	t.Cleanup(e.cleanup)
	return e
}

// ws returns a unique workspace id and schedules its cleanup.
func (e *env) ws(label string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	ws := "it-" + label + "-" + hex.EncodeToString(b[:])
	e.spaces = append(e.spaces, ws)
	return ws
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) scalar(sql string, args ...any) string {
	e.t.Helper()
	var v *string
	if err := e.owner.QueryRow(context.Background(), "SELECT ("+sql+")::text", args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	if v == nil {
		return "<nil>"
	}
	return *v
}

func (e *env) cleanup() {
	ctx := context.Background()
	for _, ws := range e.spaces {
		for _, t := range []string{"workbook_spend_attempts", "workbook_enrichments", "workbook_rows", "workbooks"} {
			_, _ = e.owner.Exec(ctx, "DELETE FROM "+t+" WHERE workspace_id = $1", ws)
		}
		_, _ = e.owner.Exec(ctx, `DELETE FROM jobs WHERE type = $1 AND (workspace_id = $2 OR payload->>'workspace_id' = $2)`, JobType, ws)
	}
	_, _ = e.owner.Exec(ctx, `DELETE FROM provider_stats WHERE provider LIKE 'it\_%'`)
}

type colCfg = map[string]any

func enrichCol(id, provider, target string) colCfg {
	return colCfg{"id": id, "name": strings.ToUpper(id[:1]) + id[1:], "type": "enrichment", "provider": provider,
		"target_field": target, "verify": false}
}

func chainCol(id string, chain []string, target string) colCfg {
	return colCfg{"id": id, "name": id, "type": "waterfall", "waterfall": chain, "target_field": target, "verify": false}
}

// workbook seeds a workbook and rows (each a website) and returns the row ids.
func (e *env) workbook(ws, id string, budget float64, cols []colCfg, sites ...string) []int64 {
	e.t.Helper()
	raw, _ := json.Marshal(cols)
	e.exec(`INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
		filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, refresh_policy)
		VALUES ($1, $1, '', 'draft', $2, 'csv', '{}', '{}', $3::json, 0, 0, true, $4, 0, '{}')`, id, ws, string(raw), budget)
	var ids []int64
	for i, site := range sites {
		var rid int64
		data, _ := json.Marshal(map[string]any{"company": fmt.Sprintf("Company %d", i), "website": site})
		if err := e.owner.QueryRow(context.Background(), `INSERT INTO workbook_rows
			(workbook_id, workspace_id, position, data, enrichments, corroboration_count)
			VALUES ($1, $2, $3, $4::json, '{}'::json, 1) RETURNING id`, id, ws, i, string(data)).Scan(&rid); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, rid)
	}
	return ids
}

func colIDs(cols []colCfg) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i], _ = c["id"].(string)
	}
	return out
}

func (e *env) payload(ws, wb string, cols []colCfg, rows []int64, extra map[string]any) map[string]any {
	p := map[string]any{
		"workbook_id": wb, "workspace_id": ws, "run_id": "run-" + wb, "column_ids": colIDs(cols), "row_ids": rows,
		"concurrency": 3, "retry_passes": 0, "provider_timeout": 5, "provider_workers": 4, "max_providers": 0,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// job inserts a processing job holding a lease, as the queue would hand it over.
func (e *env) job(ws string, payload map[string]any) queue.Job {
	e.t.Helper()
	raw, _ := json.Marshal(payload)
	j := queue.Job{Type: JobType, Payload: raw, WorkspaceID: ws, Lease: queue.Lease{WorkerID: "it-worker"}}
	err := e.owner.QueryRow(context.Background(), `INSERT INTO jobs
		(type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count, worker_id, locked_at)
		VALUES ($1, $2::json, $3, 1, 'processing', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0, 'it-worker', LOCALTIMESTAMP)
		RETURNING id, locked_at`, JobType, string(raw), ws).Scan(&j.ID, &j.Lease.LockedAt)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

// cell reads one stored cell: value, status, provider, error.
func (e *env) cell(wb string, row int64, col string) (value, status, provider, errText string) {
	e.t.Helper()
	ctx := context.Background()
	var v, s, p, er *string
	err := e.owner.QueryRow(ctx, `SELECT value, status, provider, error FROM workbook_enrichments
		WHERE workbook_id = $1 AND lead_id = $2 AND column_id = $3`, wb, row, col).Scan(&v, &s, &p, &er)
	if err != nil {
		e.t.Fatalf("cell %s/%d/%s: %v", wb, row, col, err)
	}
	str := func(x *string) string {
		if x == nil {
			return "<nil>"
		}
		return *x
	}
	return str(v), str(s), str(p), str(er)
}
