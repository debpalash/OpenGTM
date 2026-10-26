//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Plan 14 Task 16: the jobs write routes (cancel, delete, retry) against the recorded Python fixtures
// (testdata/contract/jobs) and the atomicity / tenancy properties the brief requires. The jobsEnv seed
// (jobsread_integration_test.go) carries the ledger rows, stages and leads; this file adds the durable
// `collect` queue rows of the recorder's seed.

type jwEnv struct{ jobsEnv }

// newJWEnv seeds the ledger (newJobsEnv) plus the collect queue rows of the recorder's seed.
func newJWEnv(t *testing.T) jwEnv {
	t.Helper()
	e := jwEnv{newJobsEnv(t, nil)}
	purgeQueue := func() {
		ownerExec(t, `DELETE FROM jobs WHERE type = 'collect' AND (workspace_id IN ('ws-alpha','ws-beta') OR fire_key LIKE 'collect:ws-%')`)
	}
	purgeQueue()
	t.Cleanup(func() {
		purgeQueue()
		jobsCancelQueue, jobsRetryEnqueue = nil, nil
	})
	ownerExec(t, `UPDATE workspaces SET slug = 'alpha' WHERE id = 'ws-alpha'`)
	ownerExec(t, `UPDATE workspaces SET slug = 'beta' WHERE id = 'ws-beta'`)
	// queue_seeded: pending a1, processing a2, completed a3, failed a5, cancelled a6, processing a7, processing b2
	for _, q := range []struct{ ws, slug, id, status string }{
		{"ws-alpha", "alpha", jA1, "pending"}, {"ws-alpha", "alpha", jA2, "processing"}, {"ws-alpha", "alpha", jA3, "completed"},
		{"ws-alpha", "alpha", jA5, "failed"}, {"ws-alpha", "alpha", "a1000000000000000000000000000006", "cancelled"},
		{"ws-alpha", "alpha", jA7, "processing"}, {"ws-beta", "beta", jB2, "processing"},
	} {
		e.queueRow(t, q.ws, q.slug, q.id, q.status, "collect:"+q.ws+":"+q.id)
	}
	return e
}

func (e jwEnv) queueRow(t *testing.T, ws, slug, id, status, fireKey string) {
	t.Helper()
	payload := fmt.Sprintf(`{"job_id": %q, "query": "seeded", "slug": %q, "workspace_id": %q}`, id, slug, ws)
	ownerExec(t, `INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status, created_at, next_run_at, max_retries, retry_count)
		VALUES ('python', 'collect', $1::json, $2, 1, $3, $4, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC', 3, 0)`, payload, ws, fireKey, status)
}

type qRow struct {
	ID                        int
	FireKey, Status, Executor string
	Error                     *string
	HasWorker, HasCompleted   bool
	Payload                   map[string]any
	PayloadText               string
}

// queueRows reads every collect queue row of the two test workspaces in id order.
func queueRows(t *testing.T) []qRow {
	t.Helper()
	ctx := context.Background()
	var out []qRow
	conn := ownerConn(t)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT id, fire_key, status, executor, error, worker_id IS NOT NULL, completed_at IS NOT NULL, payload::text
		FROM jobs WHERE type = 'collect' AND fire_key LIKE 'collect:ws-%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r qRow
		if err := rows.Scan(&r.ID, &r.FireKey, &r.Status, &r.Executor, &r.Error, &r.HasWorker, &r.HasCompleted, &r.PayloadText); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(r.PayloadText), &r.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func ownerConn(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func queueByKey(t *testing.T, fireKey string) []qRow {
	t.Helper()
	var out []qRow
	for _, r := range queueRows(t) {
		if r.FireKey == fireKey {
			out = append(out, r)
		}
	}
	return out
}

func (e jwEnv) req(who, ws, method, path string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do(method, path, "", "", hdr)
	return rec
}

func (e jwEnv) ledger(t *testing.T, ws, id string) (status, errText, completed string, attempts int, found bool) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT status, error, completed_at, attempts FROM collection_jobs WHERE id = $1`, id).Scan(&status, &errText, &completed, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (e jwEnv) leadExists(t *testing.T, ws string, id int) bool {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM leads WHERE id = $1`, id).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func jwActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth_401"):
		return "", ""
	case strings.HasSuffix(name, "_viewer_403"):
		return "bob", "ws-alpha"
	case strings.HasSuffix(name, "_editor"), name == "delete_keep_leads_a4":
		return "erin", ""
	}
	return "alice", ""
}

func jwResp(rec *httptest.ResponseRecorder) contract.Resp {
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	return got
}

// fixtureQueue returns the recorded queue rows of the seeded and retried jobs (a1xx/b1xx fire keys; the collect
// route's rows are not seeded here).
func fixtureQueue(t *testing.T, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range fixtureRows(t, name) {
		if fk, _ := r["fire_key"].(string); strings.Contains(fk, ":a1000") || strings.Contains(fk, ":b1000") {
			out = append(out, r)
		}
	}
	return out
}

// assertQueue compares the queue to a recorded /__contract/queue snapshot. Ids differ (serial values), and a
// retry row's payload also carries `fire_key` last (EnqueueOnce stores {**payload, "fire_key"}, Python add_job
// does not): both are deviations, everything else must match.
func assertQueue(t *testing.T, name string) {
	t.Helper()
	want, got := fixtureQueue(t, name), queueRows(t)
	if len(want) != len(got) {
		t.Fatalf("%s: %d queue rows, want %d", name, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		var gErr any
		if g.Error != nil {
			gErr = *g.Error
		}
		payload := map[string]any{}
		for k, v := range g.Payload {
			if k != "fire_key" {
				payload[k] = v
			}
		}
		if w["fire_key"] != g.FireKey || w["status"] != g.Status || w["executor"] != g.Executor || w["error"] != gErr ||
			w["has_completed_at"] != g.HasCompleted || (w["worker_id"] != nil) != g.HasWorker || !reflect.DeepEqual(w["payload"], payload) {
			t.Errorf("%s row %d: got %+v, want %v", name, i, g, w)
		}
	}
}

// assertLedger checks a job's stored state against a recorded detail fixture (status, attempts, error, completed_at).
// A completed_at stamped by the route is a fresh timestamp: it only has to be set.
func (e jwEnv) assertLedger(t *testing.T, ws, fixture string) {
	t.Helper()
	fx := contract.Load(t, "jobs/"+fixture)
	var want struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Error       string `json:"error"`
		CompletedAt string `json:"completed_at"`
		Attempts    int    `json:"attempts"`
	}
	if err := json.Unmarshal(fx.Response.Body, &want); err != nil {
		t.Fatal(err)
	}
	status, errText, completed, attempts, found := e.ledger(t, ws, want.ID)
	if !found || status != want.Status || errText != want.Error || attempts != want.Attempts {
		t.Errorf("%s: ledger %q %q attempts %d (found %v), want %+v", fixture, status, errText, attempts, found, want)
	}
	if want.Status == "cancelled" && want.Error == "Cancelled by user" {
		if _, err := time.Parse(time.RFC3339Nano, completed); err != nil {
			t.Errorf("%s: completed_at %q is not an isoformat stamp", fixture, completed)
		}
	} else if completed != want.CompletedAt {
		t.Errorf("%s: completed_at %q, want %q", fixture, completed, want.CompletedAt)
	}
}

func TestJobsWriteMatchesFixtures(t *testing.T) {
	e := newJWEnv(t)
	assertQueue(t, "queue_seeded")
	replay := func(names ...string) {
		for _, name := range names {
			fx := contract.Load(t, "jobs/"+name)
			who, ws := jwActor(name)
			var rec *httptest.ResponseRecorder
			if who == "" {
				rec, _ = e.do(fx.Request.Method, fx.Request.Path, "", "", nil)
			} else {
				rec = e.req(who, ws, fx.Request.Method, fx.Request.Path)
			}
			got := jwResp(rec)
			if rec.Code == 200 {
				var m map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if qid, ok := m["queue_job_id"].(float64); ok { // the recorder's serial value; ours is another id
					job := strings.TrimPrefix(fx.Request.Path[strings.LastIndex(fx.Request.Path, "/jobs/"):], "/jobs/")
					job = job[:strings.Index(job, "/")]
					rows := queueByKey(t, "collect:ws-alpha:"+job)
					if last := rows[len(rows)-1]; last.ID != int(qid) || last.Status != "pending" {
						t.Errorf("%s: queue_job_id %v is not the new pending row: %+v", name, qid, last)
					}
					var want map[string]any
					_ = json.Unmarshal(fx.Response.Body, &want)
					m["queue_job_id"] = want["queue_job_id"]
					got.Body, _ = json.Marshal(m)
				}
			}
			t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
		}
	}
	replay("cancel_running_a2", "cancel_pending_a1_editor", "cancel_done_a3_noop", "cancel_already_cancelled_a6", "cancel_failed_a5_noop",
		"cancel_foreign_b2_404", "cancel_unknown_404", "cancel_viewer_403", "cancel_no_auth_401")
	e.assertLedger(t, "ws-alpha", "job_detail_a2_after_cancel")
	e.assertLedger(t, "ws-alpha", "job_detail_a1_after_cancel")
	e.assertLedger(t, "ws-alpha", "job_detail_a3_after_cancel_noop")
	assertQueue(t, "queue_after_cancel")

	replay("retry_failed_a5", "retry_cancelled_a6_editor", "retry_cancelled_a2_after_cancel", "retry_done_a3_409", "retry_running_a7_409",
		"retry_pending_after_retry_409", "retry_foreign_b1_404", "retry_unknown_404", "retry_viewer_403", "retry_no_auth_401")
	e.assertLedger(t, "ws-alpha", "job_detail_a5_after_retry")
	e.assertLedger(t, "ws-alpha", "job_detail_a6_after_retry")
	assertQueue(t, "queue_after_retry")

	replay("delete_viewer_403", "delete_no_auth_401", "delete_keep_leads_bad_422", "delete_foreign_b1_404", "delete_unknown_404",
		"delete_keep_leads_a4")
	if _, _, _, _, found := e.ledger(t, "ws-alpha", jA4); found {
		t.Error("a4 still in the ledger")
	}
	if !e.leadExists(t, "ws-alpha", 104) {
		t.Error("lead 104 must be kept (keep_leads=true)")
	}
	replay("delete_a3_with_leads")
	if e.leadExists(t, "ws-alpha", 101) {
		t.Error("lead 101 still exists after delete")
	}
	if !e.leadExists(t, "ws-beta", 106) {
		t.Error("beta lead 106 was touched")
	}
	replay("delete_a3_twice_404", "delete_running_a7")
	assertQueue(t, "queue_final")
}

func TestCancelSetsLedgerAndQueueAtomically(t *testing.T) {
	e := newJWEnv(t)
	rec := e.req("alice", "", "POST", "/api/jobs/"+jA2+"/cancel")
	if rec.Code != 200 || rec.Body.String() != `{"ok":true,"message":"Job `+jA2+` cancelled"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if st, msg, done, _, _ := e.ledger(t, "ws-alpha", jA2); st != "cancelled" || msg != "Cancelled by user" || done == "" {
		t.Errorf("ledger %q %q %q", st, msg, done)
	}
	if q := queueByKey(t, "collect:ws-alpha:"+jA2); len(q) != 1 || q[0].Status != "cancelled" || !q[0].HasCompleted || q[0].HasWorker {
		t.Errorf("queue %+v", q)
	}

	// A failing queue half rolls the ledger half back (one transaction; Python commits them separately).
	jobsCancelQueue = func(context.Context, pgx.Tx, string, string, time.Time) (int, error) { return 0, errors.New("boom") }
	rec = e.req("alice", "", "POST", "/api/jobs/"+jA7+"/cancel")
	if rec.Code != 500 {
		t.Fatalf("queue failure: %d %s", rec.Code, rec.Body)
	}
	if st, _, _, _, _ := e.ledger(t, "ws-alpha", jA7); st != "running" {
		t.Errorf("ledger %q after failed queue cancel, want running", st)
	}
	if q := queueByKey(t, "collect:ws-alpha:"+jA7); q[0].Status != "processing" {
		t.Errorf("queue %+v", q)
	}
}

func TestCancelQueueMatchesFireKeyExactly(t *testing.T) {
	e := newJWEnv(t)
	// Rows of another workspace with the same job id, a longer id sharing the prefix and a LIKE wildcard id must not move.
	e.queueRow(t, "ws-beta", "beta", jA1, "pending", "collect:ws-beta:"+jA1)
	e.queueRow(t, "ws-alpha", "alpha", jA1+"extra", "pending", "collect:ws-alpha:"+jA1+"extra")
	e.queueRow(t, "ws-alpha", "alpha", "%", "pending", "collect:ws-alpha:%")
	if rec := e.req("alice", "", "POST", "/api/jobs/"+jA1+"/cancel"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for key, want := range map[string]string{
		"collect:ws-alpha:" + jA1: "cancelled", "collect:ws-beta:" + jA1: "pending",
		"collect:ws-alpha:" + jA1 + "extra": "pending", "collect:ws-alpha:%": "pending",
	} {
		if q := queueByKey(t, key); len(q) != 1 || q[0].Status != want {
			t.Errorf("%s: %+v, want %s", key, q, want)
		}
	}
}

func TestCancelTerminalJobStillOk(t *testing.T) {
	e := newJWEnv(t)
	before, _, bdone, _, _ := e.ledger(t, "ws-alpha", jA3)
	for _, id := range []string{jA3, jA5, "a1000000000000000000000000000006"} {
		rec := e.req("alice", "", "POST", "/api/jobs/"+id+"/cancel")
		if rec.Code != 200 || rec.Body.String() != `{"ok":true,"message":"Job `+id+` cancelled"}` {
			t.Errorf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if st, _, done, _, _ := e.ledger(t, "ws-alpha", jA3); st != before || done != bdone {
		t.Errorf("done job changed: %q %q", st, done)
	}
}

func TestCancelForeignJob404(t *testing.T) {
	e := newJWEnv(t)
	for _, c := range []struct{ who, ws, id, fk string }{
		{"alice", "", jB2, "collect:ws-beta:" + jB2}, {"bob", "ws-beta", jA2, "collect:ws-alpha:" + jA2},
	} {
		rec := e.req(c.who, c.ws, "POST", "/api/jobs/"+c.id+"/cancel")
		if rec.Code != 404 || rec.Body.String() != `{"detail":"Job not found"}` {
			t.Errorf("%s: %d %s", c.id, rec.Code, rec.Body)
		}
		if q := queueByKey(t, c.fk); q[0].Status != "processing" {
			t.Errorf("foreign queue row cancelled: %+v", q)
		}
	}
	if st, _, _, _, _ := e.ledger(t, "ws-beta", jB2); st != "running" {
		t.Errorf("beta ledger %q", st)
	}
}

func (e jwEnv) betaLeadFor(t *testing.T) {
	t.Helper()
	// a beta lead that references the alpha job id in both attachment forms: a delete in alpha must not touch it
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, "ws-beta", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO leads (id, workspace_id, collection_job_id, company, source, created_at, updated_at)
			VALUES (107, 'ws-beta', $1, 'Beta Lookalike', 'job:'||$1, '2026-08-28T07:05:00', '2026-08-28T07:05:00')`, jA3)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteKeepLeadsFalseRemovesLeads(t *testing.T) {
	e := newJWEnv(t)
	e.betaLeadFor(t)
	rec := e.req("alice", "", "DELETE", "/api/jobs/"+jA4+"?keep_leads=false")
	if rec.Code != 200 || rec.Body.String() != `{"ok":true,"message":"Job `+jA4+` deleted","leads_kept":false}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// 104 is attached through source "job:<id>" only, 105 through both forms
	if e.leadExists(t, "ws-alpha", 104) || e.leadExists(t, "ws-alpha", 105) {
		t.Error("a4 leads survived")
	}
	rec = e.req("alice", "", "DELETE", "/api/jobs/"+jA3) // default keep_leads=false
	if rec.Code != 200 || !strings.HasSuffix(rec.Body.String(), `"leads_kept":false}`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, id := range []int{101, 102, 103} {
		if e.leadExists(t, "ws-alpha", id) {
			t.Errorf("lead %d survived", id)
		}
	}
	if !e.leadExists(t, "ws-beta", 106) || !e.leadExists(t, "ws-beta", 107) {
		t.Error("another workspace's leads were deleted")
	}
	if _, _, _, _, found := e.ledger(t, "ws-alpha", jA3); found {
		t.Error("ledger row survived")
	}
	ctx := context.Background()
	var stages int
	_ = tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM collection_job_stages WHERE job_id IN ($1, $2)`, jA3, jA4).Scan(&stages)
	})
	if stages != 0 {
		t.Errorf("%d stages left", stages)
	}
}

func TestDeleteKeepLeadsTrueKeepsLeads(t *testing.T) {
	e := newJWEnv(t)
	rec := e.req("alice", "", "DELETE", "/api/jobs/"+jA3+"?keep_leads=true")
	if rec.Code != 200 || rec.Body.String() != `{"ok":true,"message":"Job `+jA3+` deleted","leads_kept":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, id := range []int{101, 102, 103} {
		if !e.leadExists(t, "ws-alpha", id) {
			t.Errorf("lead %d deleted with keep_leads=true", id)
		}
	}
	// the active queue row is cancelled before the ledger row goes
	e.req("alice", "", "DELETE", "/api/jobs/"+jA7)
	if q := queueByKey(t, "collect:ws-alpha:"+jA7); q[0].Status != "cancelled" {
		t.Errorf("queue %+v", q)
	}
}

func TestDeleteForeignJob404NoLeadDeleted(t *testing.T) {
	e := newJWEnv(t)
	e.betaLeadFor(t)
	rec := e.req("bob", "ws-beta", "DELETE", "/api/jobs/"+jA3)
	if rec.Code != 404 || rec.Body.String() != `{"detail":"Job not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = e.req("alice", "", "DELETE", "/api/jobs/"+jB1)
	if rec.Code != 404 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, id := range []int{101, 102, 103} {
		if !e.leadExists(t, "ws-alpha", id) {
			t.Errorf("alpha lead %d deleted by a foreign request", id)
		}
	}
	if !e.leadExists(t, "ws-beta", 106) || !e.leadExists(t, "ws-beta", 107) {
		t.Error("beta lead deleted by a foreign request")
	}
	if _, _, _, _, found := e.ledger(t, "ws-alpha", jA3); !found {
		t.Error("foreign delete removed the ledger row")
	}
	if q := queueByKey(t, "collect:ws-alpha:"+jA2); q[0].Status != "processing" {
		t.Errorf("queue %+v", q)
	}
}

func TestRetryOnlyFailedOrCancelled409(t *testing.T) {
	e := newJWEnv(t)
	for _, id := range []string{jA1, jA2, jA3, jA7} { // pending, running, done, running
		rec := e.req("alice", "", "POST", "/api/jobs/"+id+"/retry")
		if rec.Code != 409 || rec.Body.String() != `{"detail":"Only failed or cancelled jobs can be retried"}` {
			t.Errorf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if n := len(queueRows(t)); n != 7 {
		t.Errorf("%d queue rows, retry of a non-terminal job enqueued", n)
	}
	rec := e.req("alice", "", "POST", "/api/jobs/"+jA5+"/retry")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rows := queueByKey(t, "collect:ws-alpha:"+jA5)
	if len(rows) != 2 {
		t.Fatalf("queue rows %+v", rows)
	}
	n := rows[1]
	want := `{"job_id":"` + jA5 + `","query":"payroll outsourcing Delhi","workspace_id":"ws-alpha","slug":"alpha","fire_key":"collect:ws-alpha:` + jA5 + `"}`
	if n.Status != "pending" || n.Executor != "python" || n.PayloadText != want {
		t.Errorf("new row %+v\nwant payload %s", n, want)
	}
	if rec.Body.String() != fmt.Sprintf(`{"ok":true,"queue_job_id":%d,"message":"Job %s queued for retry"}`, n.ID, jA5) {
		t.Errorf("body %s", rec.Body)
	}
}

func TestRetryEnqueueFailureRollsBack(t *testing.T) {
	e := newJWEnv(t)
	jobsRetryEnqueue = func(context.Context, pgx.Tx, string, any, string) (int64, bool, error) {
		return 0, false, errors.New("queue down")
	}
	rec := e.req("alice", "", "POST", "/api/jobs/"+jA5+"/retry")
	if rec.Code != 503 || rec.Body.String() != `{"detail":"Collection queue unavailable"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	st, errText, _, attempts, _ := e.ledger(t, "ws-alpha", jA5)
	if st != "failed" || errText == "" || attempts != 3 {
		t.Errorf("ledger %q %q %d: the retry reset must roll back with the enqueue", st, errText, attempts)
	}
	if n := len(queueByKey(t, "collect:ws-alpha:"+jA5)); n != 1 {
		t.Errorf("%d queue rows for a5", n)
	}
	// the job is still retryable once the queue is back
	jobsRetryEnqueue = nil
	if rec := e.req("alice", "", "POST", "/api/jobs/"+jA5+"/retry"); rec.Code != 200 {
		t.Errorf("retry after recovery: %d %s", rec.Code, rec.Body)
	}
}

func TestRetryTwiceConcurrentSingleQueueRow(t *testing.T) {
	e := newJWEnv(t)
	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = e.req("alice", "", "POST", "/api/jobs/"+jA5+"/retry").Code
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
		default:
			t.Errorf("codes %v", codes)
		}
	}
	if ok != 1 {
		t.Errorf("%d requests succeeded, want exactly 1: %v", ok, codes)
	}
	active := 0
	for _, r := range queueByKey(t, "collect:ws-alpha:"+jA5) {
		if r.Status == "pending" || r.Status == "processing" {
			active++
		}
	}
	if active != 1 {
		t.Errorf("%d active queue rows", active)
	}
}

func TestWriteRoutesViewer403(t *testing.T) {
	e := newJWEnv(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/jobs/" + jA2 + "/cancel"}, {"DELETE", "/api/jobs/" + jA3}, {"DELETE", "/api/jobs/" + jA3 + "?keep_leads=maybe"},
		{"POST", "/api/jobs/" + jA5 + "/retry"},
	} {
		rec := e.req("bob", "ws-alpha", c.method, c.path)
		if rec.Code != 403 {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if st, _, _, _, _ := e.ledger(t, "ws-alpha", jA2); st != "running" {
		t.Errorf("viewer cancelled a job: %q", st)
	}
	if !e.leadExists(t, "ws-alpha", 101) || len(queueRows(t)) != 7 {
		t.Error("viewer changed data")
	}
}

// TestRunnerSeesCancelledStatus: the Python runner's _is_cancelled reads PgJobLedger.get_status
// (SELECT status FROM collection_jobs WHERE id = :id) under the workspace tenant; Task 22 runs the real Python.
func TestRunnerSeesCancelledStatus(t *testing.T) {
	e := newJWEnv(t)
	if rec := e.req("alice", "", "POST", "/api/jobs/"+jA2+"/cancel"); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	ctx := context.Background()
	var status string
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM collection_jobs WHERE id = $1`, jA2).Scan(&status)
	}); err != nil || status != "cancelled" {
		t.Fatalf("status %q err %v", status, err)
	}
}
