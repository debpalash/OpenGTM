//go:build integration

package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool, owner *pgxpool.Pool
	prefix      string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 8), prefix: fmt.Sprintf("t5_%d_", os.Getpid())}
	var err error
	e.owner, err = pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.clean()
	t.Cleanup(e.clean)
	return e
}

var seededTables = []string{"governance_audit_events", "llm_usage_daily", "signals", "destination_deliveries",
	"destination_inbound_receipts", "audience_membership_events", "playbook_results", "outreach_sends",
	"retention_policies"}

func (e *env) clean() {
	ctx := context.Background()
	c, err := e.owner.Acquire(ctx)
	if err != nil {
		return
	}
	defer c.Release()
	_, _ = c.Exec(ctx, `SET session_replication_role = replica`)
	for _, tb := range seededTables {
		_, _ = c.Exec(ctx, `DELETE FROM `+tb+` WHERE workspace_id LIKE $1`, e.prefix+"%")
	}
	_, _ = c.Exec(ctx, `DELETE FROM retention_schedules WHERE workspace_id LIKE $1`, e.prefix+"%")
	_, _ = c.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE $1`, "retention:"+e.prefix+"%")
	_, _ = c.Exec(ctx, `RESET session_replication_role`)
}

// seed inserts the Python-seeded rows (same spec as gen_vectors.py.txt) with
// foreign keys skipped (replica role), so every table can be filled alone.
func (e *env) seed(t *testing.T, v vectors, now time.Time) {
	t.Helper()
	ctx := context.Background()
	c, err := e.owner.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(ctx, `RESET session_replication_role`)
	for i, r := range v.Rows {
		at := now.Add(-time.Duration(r.AgeDays * 24 * float64(time.Hour)))
		naive := at.UTC().Format("2006-01-02 15:04:05.000000")
		var sql string
		var args []any
		switch r.Table {
		case "governance_audit_events":
			sql = `INSERT INTO governance_audit_events (id,workspace_id,actor_role,method,route,resource_path,response_status,outcome,request_id,metadata_json,created_at) VALUES ('g'||$1::int,$2,'admin','POST','/r','/p',200,'success','rid','{}',$3::timestamp)`
			args = []any{i, r.WS, naive}
		case "llm_usage_daily":
			sql = `INSERT INTO llm_usage_daily (workspace_id,provider,date,updated_at) VALUES ($1,'p'||$2::int,$3,'x')`
			args = []any{r.WS, i, at.UTC().Format("2006-01-02")}
		case "signals":
			sql = `INSERT INTO signals (id,workspace_id,created_at) VALUES ('s'||$1::int,$2,$3)`
			args = []any{i, r.WS, float64(at.UnixMicro()) / 1e6}
		case "destination_deliveries":
			sql = `INSERT INTO destination_deliveries (workspace_id,run_id,destination_id,lead_id,operation,idempotency_key,payload_fingerprint,status,attempts,created_at) VALUES ($1,'r','d',$2::int,'upsert','k'||$2::int,'f','sent',0,$3::timestamp)`
			args = []any{r.WS, i, naive}
		case "destination_inbound_receipts":
			sql = `INSERT INTO destination_inbound_receipts (id,workspace_id,destination_id,provider,external_event_id,status,conflict_policy,applied_fields,ignored_fields,payload_fingerprint,created_at) VALUES ('i'||$1::int,$2,'d','p','e'||$1::int,'ok','x','{}','{}','f',$3::timestamp)`
			args = []any{i, r.WS, naive}
		case "audience_membership_events":
			sql = `INSERT INTO audience_membership_events (workspace_id,audience_id,lead_id,event_type,snapshot,created_at) VALUES ($1,'a',$2::int,'enter','{}',$3::timestamp)`
			args = []any{r.WS, i, naive}
		case "playbook_results":
			sql = `INSERT INTO playbook_results (workspace_id,run_id,lead_id,status,value,result_metadata,attempts,created_at) VALUES ($1,'r',$2::int,'ok','v','{}',0,$3::timestamp)`
			args = []any{r.WS, i, naive}
		case "outreach_sends":
			sql = `INSERT INTO outreach_sends (workspace_id,to_email,idempotency_key,created_at) VALUES ($1,'a@b.c','k'||$2::int,$3::timestamptz)`
			args = []any{r.WS, i, at.UTC().Format(time.RFC3339Nano)}
		default:
			t.Fatal(r.Table)
		}
		if _, err := c.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", r.Table, err)
		}
	}
}

func (e *env) wsMap() map[string]string {
	return map[string]string{"wsSched2": e.prefix + "wsSched2", "wsSched": e.prefix + "wsSched", "wsA": e.prefix + "wsA", "wsB": e.prefix + "wsB"}
}

func (e *env) preview(t *testing.T, ws string, tz string, days map[string]int, now time.Time) Counts {
	t.Helper()
	var got Counts
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		if tz != "" {
			if _, err := tx.Exec(context.Background(), `SELECT set_config('TimeZone', $1, true)`, tz); err != nil {
				return err
			}
		}
		var err error
		got, err = Preview(context.Background(), tx, ws, days, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPreviewCountsEachCategoryOldVsNew(t *testing.T) {
	e := setup(t)
	v := loadVectors(t, e.wsMap())
	now, _ := time.Parse(time.RFC3339, v.Now)
	e.seed(t, v, now)
	for _, c := range v.Cases {
		for tz, byWS := range c.Expected {
			for _, w := range []string{e.prefix + "wsA", e.prefix + "wsB"} {
				days := map[string]int{}
				for k, val := range c.Normalized {
					days[k] = val
				}
				got := e.preview(t, w, tz, days, now)
				want := byWS[w]
				if len(got) != 7 || len(want) != 7 {
					t.Fatalf("%s/%s: %v", c.Name, tz, got)
				}
				for cat, n := range want {
					if got[cat] != n {
						t.Errorf("%s tz=%s %s %s: got %d want %d (Python)", c.Name, tz, w, cat, got[cat], n)
					}
				}
				b, _ := json.Marshal(got)
				if !strings.HasPrefix(string(b), `{"audit":`) || !strings.HasSuffix(string(b), `}`) || strings.Index(string(b), "activation") > strings.Index(string(b), "agent_results") {
					t.Errorf("order %s", b)
				}
			}
		}
	}
}

func TestPreviewDoesNotDelete(t *testing.T) {
	e := setup(t)
	v := loadVectors(t, e.wsMap())
	now, _ := time.Parse(time.RFC3339, v.Now)
	e.seed(t, v, now)
	count := func() (n int) {
		for _, tb := range seededTables[:8] {
			var k int
			if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM `+tb+` WHERE workspace_id LIKE $1`, e.prefix+"%").Scan(&k); err != nil {
				t.Fatal(err)
			}
			n += k
		}
		return
	}
	before := count()
	e.preview(t, e.prefix+"wsA", "", map[string]int{"audit": 90, "signals": 30}, now)
	if before == 0 || count() != before {
		t.Fatalf("rows %d -> %d", before, count())
	}
}

func TestPreviewSignalsEpochAndLLMDateString(t *testing.T) {
	e := setup(t)
	ws := e.prefix + "wsE"
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour) // 2026-05-16T12:00:00Z
	ep := float64(cutoff.UnixMicro()) / 1e6
	ctx := context.Background()
	for i, d := range []float64{ep - 1, ep, ep + 1} {
		if _, err := e.owner.Exec(ctx, `INSERT INTO signals (id,workspace_id,created_at) VALUES ($1,$2,$3)`, fmt.Sprintf("%sx%d", e.prefix, i), ws, d); err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range []string{"2026-05-15", "2026-05-16", "2026-05-17"} {
		if _, err := e.owner.Exec(ctx, `INSERT INTO llm_usage_daily (workspace_id,provider,date,updated_at) VALUES ($1,$2,$3,'x')`, ws, fmt.Sprint("p", i), d); err != nil {
			t.Fatal(err)
		}
	}
	got := e.preview(t, ws, "", map[string]int{}, now) // defaults: 365d
	if got["signals"] != 0 || got["llm_usage"] != 0 {
		t.Fatalf("defaults %v", got)
	}
	got = e.preview(t, ws, "", map[string]int{"signals": 30, "llm_usage": 30}, now)
	// strictly older than the cutoff: epoch ep-1 only; date '2026-05-15' only ('2026-05-16' is not < '2026-05-16').
	if got["signals"] != 1 || got["llm_usage"] != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestPreviewTenantIsolation(t *testing.T) {
	e := setup(t)
	v := loadVectors(t, e.wsMap())
	now, _ := time.Parse(time.RFC3339, v.Now)
	e.seed(t, v, now)
	days := map[string]int{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}
	var cross Counts
	err := tenancy.InWorkspace(context.Background(), e.pool, e.prefix+"wsB", func(tx pgx.Tx) error {
		var err error
		cross, err = Preview(context.Background(), tx, e.prefix+"wsA", days, now) // RLS hides A's tenant rows under B
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for cat, n := range cross {
		if cat != "llm_usage" && cat != "signals" && n != 0 { // non-RLS tables are covered by the explicit workspace filter below
			t.Errorf("cross-tenant %s = %d", cat, n)
		}
	}
	// With RLS out of the picture (owner connection) the workspace filter still isolates.
	oc, _ := e.owner.Begin(context.Background())
	defer oc.Rollback(context.Background())
	a, err := Preview(context.Background(), oc, e.prefix+"wsA", days, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Preview(context.Background(), oc, e.prefix+"wsB", days, now)
	if !reflect.DeepEqual(map[string]int64(a), map[string]int64(b)) {
		t.Fatalf("A %v B %v (identical seeds must give identical counts)", a, b)
	}
	only, _ := Preview(context.Background(), oc, e.prefix+"nobody", days, now)
	for cat, n := range only {
		if n != 0 {
			t.Errorf("unknown workspace %s = %d", cat, n)
		}
	}
}

func TestPreviewRejectsInvalidDays(t *testing.T) {
	e := setup(t)
	err := tenancy.InWorkspace(context.Background(), e.pool, e.prefix+"wsA", func(tx pgx.Tx) error {
		_, err := Preview(context.Background(), tx, e.prefix+"wsA", map[string]int{"audit": 0}, time.Now())
		return err
	})
	if err == nil || err.Error() != "audit retention must be 90..3650 days" {
		t.Fatal(err)
	}
}

func (e *env) upsert(t *testing.T, ws string, enabled, hold bool, days map[string]int, by int32) Policy {
	t.Helper()
	var p Policy
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		var err error
		p, err = UpsertPolicy(context.Background(), tx, ws, enabled, hold, days, by)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpsertPolicyShapeMatchesFixture(t *testing.T) {
	e := setup(t)
	ws := e.prefix + "wsP"
	p := e.upsert(t, ws, true, false, map[string]int{"outreach_history": 30, "audit": 90}, 7)
	b, _ := json.Marshal(p)
	var ordered []string
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.Token()
	for dec.More() {
		k, _ := dec.Token()
		ordered = append(ordered, k.(string))
		var skip json.RawMessage
		dec.Decode(&skip)
	}
	if !reflect.DeepEqual(ordered, []string{"workspace_id", "enabled", "legal_hold", "retention_days", "next_run_at", "updated_by", "created_at", "updated_at"}) {
		t.Fatal(ordered)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["workspace_id"] != ws || m["enabled"] != true || m["legal_hold"] != false || m["updated_by"] != float64(7) || m["next_run_at"] != nil {
		t.Fatal(string(b))
	}
	// retention_days stored as the full normalized dict in DefaultDays order, not request order.
	if !strings.Contains(string(b), `"retention_days":{"audit":90,"llm_usage":365,"signals":365,"activation":180,"audience_history":365,"agent_results":180,"outreach_history":30}`) {
		t.Fatal(string(b))
	}
	for _, k := range []string{"created_at", "updated_at"} {
		s, _ := m[k].(string)
		if len(s) < 19 || strings.HasSuffix(s, "Z") || strings.Contains(s, "+") {
			t.Errorf("%s not naive isoformat: %q", k, s)
		}
	}
	// second upsert updates in place (one row), keeps created_at.
	p2 := e.upsert(t, ws, false, true, map[string]int{}, 9)
	if p2.Enabled || !p2.LegalHold || *p2.UpdatedBy != 9 || !p2.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("%+v", p2)
	}
	var n int
	_ = e.owner.QueryRow(context.Background(), `SELECT count(*) FROM retention_policies WHERE workspace_id=$1`, ws).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	got := e.get(t, ws)
	if got == nil || got.WorkspaceID != ws || !got.LegalHold {
		t.Fatalf("%+v", got)
	}
	if e.get(t, e.prefix+"none") != nil {
		t.Fatal("expected nil policy")
	}
}

func (e *env) get(t *testing.T, ws string) *Policy {
	t.Helper()
	var p *Policy
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		var err error
		p, err = GetPolicy(context.Background(), tx, ws)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpsertPolicyInvalidDaysRejected(t *testing.T) {
	e := setup(t)
	err := tenancy.InWorkspace(context.Background(), e.pool, e.prefix+"wsP", func(tx pgx.Tx) error {
		_, err := UpsertPolicy(context.Background(), tx, e.prefix+"wsP", true, false, map[string]int{"audit": 1}, 1)
		return err
	})
	if err == nil {
		t.Fatal("accepted audit=1")
	}
}

// schedule runs the Python scenario (testdata) under a session TimeZone and compares the rows.
func (e *env) runSchedule(t *testing.T, tz, scenario string, goExec bool) {
	t.Helper()
	v := loadVectors(t, e.wsMap())
	now, _ := time.Parse(time.RFC3339, v.Now)
	ws := e.prefix + "wsSched"
	enabled, hold := scenario != "disabled", scenario == "hold"
	ctx := context.Background()
	for _, j := range [][3]string{
		{"retention_enforce", "pending", ws + ":2020-01-01"}, {"retention_enforce", "processing", ws + ":2020-01-02"},
		{"retention_enforce", "pending", ws + "2:2020-01-01"}, {"other", "pending", ws + ":2020-01-03"},
	} {
		if _, err := e.owner.Exec(ctx, `INSERT INTO jobs (type,status,fire_key,payload,executor) VALUES ($1,$2,'retention:'||$3,'{}','python')`, j[0], j[1], j[2]); err != nil {
			t.Fatal(err)
		}
	}
	var next *time.Time
	var p Policy
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, tz); err != nil {
			return err
		}
		var err error
		p, err = UpsertPolicy(ctx, tx, ws, enabled, hold, map[string]int{}, 7)
		if err != nil {
			return err
		}
		exec := func(string) string { return "python" }
		if goExec {
			exec = func(string) string { return "go" }
		}
		next, err = SchedulePolicy(ctx, tx, &p, now, exec)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range v.Schedule {
		if s.TZ != tz || s.Scenario != scenario {
			continue
		}
		if (next == nil) != (s.Returned == nil) || (next != nil && next.Format(time.RFC3339) != "2026-06-16T12:00:00Z") {
			t.Errorf("returned %v want %v", next, s.Returned)
		}
		if got := p.NextRunAt; (got == nil) != (s.PolicyNextRunAt == nil) || (got != nil && isoformat(*got) != *s.PolicyNextRunAt) {
			t.Errorf("policy next_run_at %v want %v", got, s.PolicyNextRunAt)
		}
		var mEnabled bool
		var mNext *time.Time
		if err := e.owner.QueryRow(ctx, `SELECT enabled, next_run_at FROM retention_schedules WHERE workspace_id=$1`, ws).Scan(&mEnabled, &mNext); err != nil {
			t.Fatal(err)
		}
		if mEnabled != s.Mirror["enabled"] || (mNext == nil) != (s.Mirror["next_run_at"] == nil) || (mNext != nil && isoformat(*mNext) != s.Mirror["next_run_at"]) {
			t.Errorf("mirror %v %v want %v", mEnabled, mNext, s.Mirror)
		}
		rows, err := e.owner.Query(ctx, `SELECT type,status,fire_key,payload::text,executor,priority,max_retries,next_run_at,workspace_id FROM jobs WHERE fire_key LIKE $1 ORDER BY fire_key`, "retention:"+ws+"%")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []map[string]any
		for rows.Next() {
			var typ, status, fk, payload, executor string
			var prio, retries *int
			var nra *time.Time
			var wsid *string
			if err := rows.Scan(&typ, &status, &fk, &payload, &executor, &prio, &retries, &nra, &wsid); err != nil {
				t.Fatal(err)
			}
			var pl any
			_ = json.Unmarshal([]byte(payload), &pl)
			m := map[string]any{"type": typ, "status": status, "fire_key": fk, "payload": pl, "executor": executor, "priority": nil, "max_retries": nil, "next_run_at": nil, "workspace_id": nil}
			if prio != nil {
				m["priority"] = float64(*prio)
			}
			if retries != nil {
				m["max_retries"] = float64(*retries)
			}
			if nra != nil {
				m["next_run_at"] = isoformat(*nra)
			}
			if wsid != nil {
				m["workspace_id"] = *wsid
			}
			got = append(got, m)
		}
		want := s.Jobs
		if goExec {
			for _, j := range want {
				if j["fire_key"] == "retention:"+ws+":2026-06-16" {
					j["executor"] = "go"
				}
			}
		}
		// Python ordered by fire_key too (same collation-independent ASCII keys).
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		var gv, wv any
		_ = json.Unmarshal(gb, &gv)
		_ = json.Unmarshal(wb, &wv)
		if !reflect.DeepEqual(gv, wv) {
			t.Errorf("jobs\n got %s\nwant %s", gb, wb)
		}
		return
	}
	t.Fatalf("no vector for %s/%s", tz, scenario)
}

func TestSchedulePolicyCancelsPendingAndEnqueuesNextDay(t *testing.T) {
	for _, tz := range []string{"UTC", "Asia/Kolkata"} {
		t.Run(tz, func(t *testing.T) {
			e := setup(t)
			e.runSchedule(t, tz, "enabled", false)
		})
	}
}

func TestSchedulePolicyDisabledOrHoldClearsMirror(t *testing.T) {
	for _, tz := range []string{"UTC", "Asia/Kolkata"} {
		for _, sc := range []string{"disabled", "hold"} {
			t.Run(tz+"/"+sc, func(t *testing.T) {
				e := setup(t)
				e.runSchedule(t, tz, sc, false)
			})
		}
	}
}

func TestSchedulePolicyUsesGoExecutorOnlyInCohort(t *testing.T) {
	e := setup(t)
	e.runSchedule(t, "UTC", "enabled", true)
}

func TestSchedulePolicyIsIdempotentPerFireKey(t *testing.T) {
	e := setup(t)
	ws := e.prefix + "wsSched"
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
			p, err := UpsertPolicy(context.Background(), tx, ws, true, false, map[string]int{}, 7)
			if err != nil {
				return err
			}
			_, err = SchedulePolicy(context.Background(), tx, &p, now, nil)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = e.owner.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE fire_key LIKE $1 AND status='pending'`, "retention:"+ws+":%").Scan(&n)
	if n != 1 {
		t.Fatalf("%d pending jobs", n)
	}
}

func TestSchedulePolicyCancelDoesNotTouchOtherWorkspaceWithWildcards(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ws := e.prefix + "w_1" // '_' is a LIKE wildcard
	other := e.prefix + "wX1"
	if _, err := e.owner.Exec(ctx, `INSERT INTO jobs (type,status,fire_key,payload,executor) VALUES ('retention_enforce','pending','retention:'||$1||':2020-01-01','{}','python')`, other); err != nil {
		t.Fatal(err)
	}
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		p, err := UpsertPolicy(ctx, tx, ws, false, false, map[string]int{}, 1)
		if err != nil {
			return err
		}
		_, err = SchedulePolicy(ctx, tx, &p, time.Now(), nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var st string
	_ = e.owner.QueryRow(ctx, `SELECT status FROM jobs WHERE fire_key=$1`, "retention:"+other+":2020-01-01").Scan(&st)
	if st != "pending" {
		t.Fatalf("other workspace's job is %s", st)
	}
}
