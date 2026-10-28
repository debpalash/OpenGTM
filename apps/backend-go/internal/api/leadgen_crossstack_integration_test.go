//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 14 Task 22: cross-stack checks between the Go routes and the Python worker/ledger, over one
// Postgres. The Python half (legacy/python/tests/test_go_enqueued_collect.py) runs the real
// handle_collect and _is_cancelled against a Go-shaped row; this half proves the Go side of the same
// contract: the rows Go writes are what Python expects, and rows written with Python's SQL are served
// by the Go routes byte for byte in Python's key order.

var (
	crossJobKeys   = []string{"id", "query", "intent", "intent_details", "status", "tier", "attempts", "max_attempts", "leads_found", "proxy_used", "error", "created_at", "started_at", "completed_at", "workspace_id"}
	crossStageKeys = []string{"id", "job_id", "stage", "status", "input_count", "output_count", "rejected_count", "details", "started_at", "completed_at"}
	pyISOStamp     = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{6})?\+00:00$`)
)

type crossEnv struct{ collectEnv }

// newCrossEnv serves POST /api/collect and the jobs routes from one router (no Redis: events are empty).
func newCrossEnv(t *testing.T) crossEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Limiter: ratelimit.New(nil),
		Flags: config.Flags{PGLeadStore: true, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: true, CollectQueryMaxChars: 1000}}}
	RegisterCollect(r, d)
	RegisterJobs(r, d)
	e := collectEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge(t)
	ownerExec(t, `DELETE FROM collection_job_stages WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	advanceSeq(t, "collection_job_stages") // sequence-drawn ids stay above other tests' explicit low ids
	t.Cleanup(func() {
		e.purge(t)
		collectNewID, collectEnqueue, collectBus = nil, nil, nil
	})
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	alice, bob := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	ownerExec(t, `UPDATE workspaces SET slug = 'alpha' WHERE id = 'ws-alpha'`)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-beta", bob.ID, "owner")
	return crossEnv{e}
}

func (e crossEnv) get(who, path string) *httptest.ResponseRecorder {
	rec, _ := e.do("GET", path, "", "", bearer(who))
	return rec
}

func ownerRow(t *testing.T, sql string, args ...any) []any {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %s", sql)
	}
	vals, err := rows.Values()
	if err != nil {
		t.Fatal(err)
	}
	return vals
}

// TestGoEnqueuedCollectIsClaimableByGoWorker: the queue row and ledger row of POST /api/collect satisfy the
// worker's claim predicate (executor go, the only one since the plan 15 cutover) and the collect payload contract.
func TestGoEnqueuedCollectIsClaimableByGoWorker(t *testing.T) {
	e := newCrossEnv(t)
	rec := e.post("alice", "", `{"query":"IT staffing Pune"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp struct {
		JobID      string `json:"job_id"`
		QueueJobID int64  `json:"queue_job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.JobID == "" {
		t.Fatalf("%v %s", err, rec.Body)
	}

	// the claim predicate with executor='go', tenant cap off.
	if n := ownerCount(t, `SELECT count(*) FROM jobs j WHERE j.id = $1 AND j.type = 'collect' AND j.status = 'pending' AND j.executor = 'go'
		AND (j.next_run_at IS NULL OR j.next_run_at <= now() AT TIME ZONE 'utc')`, resp.QueueJobID); n != 1 {
		t.Fatalf("Go's collect queue row is not claimable by the Go worker (eligible rows %d)", n)
	}
	row := ownerRow(t, `SELECT payload::text, workspace_id, fire_key, retry_count, max_retries, priority FROM jobs WHERE id = $1`, resp.QueueJobID)
	if row[1] != "ws-alpha" || row[2] != "collect:ws-alpha:"+resp.JobID {
		t.Errorf("queue row %v", row)
	}
	// handle_collect reads job_id, query, workspace_id and slug (and intent); the Go payload order is Python's.
	if got := keyOrder(t, []byte(row[0].(string))); !reflect.DeepEqual(got[:5], []string{"job_id", "query", "intent", "workspace_id", "slug"}) {
		t.Errorf("payload key order %v", got)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(row[0].(string)), &payload)
	if payload["job_id"] != resp.JobID || payload["workspace_id"] != "ws-alpha" || payload["slug"] != "alpha" || payload["query"] != "IT staffing Pune" {
		t.Errorf("payload %v", payload)
	}

	// The ledger row is what PgJobLedger.create_job would have written for the same call.
	l := ownerRow(t, `SELECT status, tier, attempts, max_attempts, leads_found, proxy_used, error, intent_details, started_at, completed_at, created_at, intent
		FROM collection_jobs WHERE id = $1 AND workspace_id = 'ws-alpha'`, resp.JobID)
	if l[0] != "pending" || l[1] != int32(1) || l[2] != int32(0) || l[3] != int32(3) || l[4] != int32(0) || l[5] != "" || l[6] != "" || l[8] != "" || l[9] != "" {
		t.Errorf("ledger defaults %v", l)
	}
	if ts, _ := l[10].(string); !pyISOStamp.MatchString(ts) {
		t.Errorf("created_at %q is not a Python isoformat() stamp", l[10])
	}
	if l[7] == "" {
		t.Errorf("intent_details empty: %v", l[7])
	}

	// Go reads its own row back in Python's key order, detail with stages last.
	d := e.get("alice", "/api/jobs/"+resp.JobID)
	if d.Code != 200 {
		t.Fatalf("%d %s", d.Code, d.Body)
	}
	if got, want := keyOrder(t, d.Body.Bytes()), append(append([]string{}, crossJobKeys...), "stages"); !reflect.DeepEqual(got, want) {
		t.Errorf("detail key order\n got %v\nwant %v", got, want)
	}
}

// TestLegacyShapedJobRowIsServedInRecordedKeyOrder: a job created, claimed, staged and completed with the SQL of
// PgJobLedger (text isoformat stamps, ” defaults) is listed, read and staged by Go as Python's own routes
// serialise it: same keys in the same order, same compact bytes.
func TestLegacyShapedJobRowIsServedInRecordedKeyOrder(t *testing.T) {
	e := newCrossEnv(t)
	const id = "d0de0000000040008000000000000001"
	const created, started, stageStart, stageEnd, done = "2026-09-01T12:00:00.123456+00:00", "2026-09-01T12:00:01.000001+00:00",
		"2026-09-01T12:00:02.500000+00:00", "2026-09-01T12:00:03.250000+00:00", "2026-09-01T12:00:09+00:00"
	// create_job, claim_job, create_stage + complete_stage, complete_job: the statements of job_ledger.PgJobLedger.
	ownerExec(t, `INSERT INTO collection_jobs (id, workspace_id, query, intent, intent_details, created_at)
		VALUES ($1, 'ws-alpha', $2, 'market_search', '{}', $3) ON CONFLICT (id) DO NOTHING`, id, "Café staffing 東京 <b>", created)
	ownerExec(t, `UPDATE collection_jobs SET status = 'running', started_at = $2, attempts = attempts + 1 WHERE id = $1 AND workspace_id = 'ws-alpha'`, id, started)
	ownerExec(t, `INSERT INTO collection_job_stages (workspace_id, job_id, stage, status, started_at) VALUES ('ws-alpha', $1, 'discovery', 'running', $2)`, id, stageStart)
	ownerExec(t, `UPDATE collection_job_stages SET status = 'done', input_count = 4, output_count = 3, rejected_count = 1, details = '{"reasons": {"no_email": 1}}', completed_at = $2 WHERE job_id = $1`, id, stageEnd)
	ownerExec(t, `UPDATE collection_jobs SET status = 'done', completed_at = $2, leads_found = 3 WHERE id = $1 AND workspace_id = 'ws-alpha'`, id, done)

	job := `"id":"` + id + `","query":"Café staffing 東京 <b>","intent":"market_search","intent_details":{},"status":"done","tier":1,"attempts":1,"max_attempts":3,` +
		`"leads_found":3,"proxy_used":"","error":"","created_at":"` + created + `","started_at":"` + started + `","completed_at":"` + done + `","workspace_id":"ws-alpha"`
	stage := `[{"id":%ID%,"job_id":"` + id + `","stage":"discovery","status":"done","input_count":4,"output_count":3,"rejected_count":1,` +
		`"details":{"reasons":{"no_email":1}},"started_at":"` + stageStart + `","completed_at":"` + stageEnd + `"}]`
	stageID := ownerRow(t, `SELECT id FROM collection_job_stages WHERE job_id = $1`, id)[0]
	stageJSON := strings.Replace(stage, "%ID%", crossJSON(stageID), 1)

	check := func(path, want string) {
		t.Helper()
		rec := e.get("alice", path)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("%s\n got %s\nwant %s", path, got, want)
		}
	}
	// the list returns the stored intent_details text, the detail route parses it (Python get_jobs vs get_job_detail)
	check("/api/jobs", "[{"+strings.Replace(job, `"intent_details":{}`, `"intent_details":"{}"`, 1)+"}]")
	check("/api/jobs/"+id, "{"+job+`,"stages":`+stageJSON+"}")
	check("/api/jobs/"+id+"/stages", stageJSON)
	// the stage rows keep Python's order too
	rec := e.get("alice", "/api/jobs/"+id+"/stages")
	var stages []json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &stages)
	if got := keyOrder(t, stages[0]); !reflect.DeepEqual(got, crossStageKeys) {
		t.Errorf("stage key order %v", got)
	}

	// Another workspace never sees it (RLS), whichever stack wrote it.
	if rec := e.get("bob", "/api/jobs/"+id); rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Job not found"}` {
		t.Errorf("foreign read: %d %s", rec.Code, rec.Body)
	}
}

// TestGoCancelWritesTheRowShapeTheLedgerReads: the cancel route writes the row the Python runner's
// _is_cancelled polls (PgJobLedger.get_status), with Python's cancel text and the queue row
// cancelled in the same transaction.
func TestGoCancelWritesTheRowShapeTheLedgerReads(t *testing.T) {
	e := newCrossEnv(t)
	rec := e.post("alice", "", `{"query":"IT staffing Pune"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp struct {
		JobID      string `json:"job_id"`
		QueueJobID int64  `json:"queue_job_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	ownerExec(t, `UPDATE collection_jobs SET status = 'running' WHERE id = $1`, resp.JobID)
	c, _ := e.do("POST", "/api/jobs/"+resp.JobID+"/cancel", "", "", bearer("alice"))
	if c.Code != 200 {
		t.Fatalf("%d %s", c.Code, c.Body)
	}
	// PgJobLedger.get_status
	got := ownerRow(t, `SELECT status, error, completed_at FROM collection_jobs WHERE id = $1 AND workspace_id = 'ws-alpha'`, resp.JobID)
	if got[0] != "cancelled" || got[1] != "Cancelled by user" {
		t.Errorf("ledger after cancel %v", got)
	}
	if ts, _ := got[2].(string); !pyISOStamp.MatchString(ts) {
		t.Errorf("completed_at %q", got[2])
	}
	if q := ownerRow(t, `SELECT status FROM jobs WHERE id = $1`, resp.QueueJobID)[0]; q == "pending" || q == "processing" {
		t.Errorf("queue row still %v after cancel", q)
	}
}

func crossJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
