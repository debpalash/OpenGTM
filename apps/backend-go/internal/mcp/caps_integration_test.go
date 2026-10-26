//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type capEnv struct {
	pool *pgxpool.Pool
	d    Deps
	sfx  string
}

func newCapEnv(t *testing.T) *capEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	e := &capEnv{pool: pool, sfx: fmt.Sprintf("%d", time.Now().UnixNano())}
	e.d = Deps{InWorkspace: func(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
		return tenancy.InWorkspace(ctx, pool, ws, fn)
	}}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM trigger_cap_reservations WHERE workspace_id LIKE $1`, "t6-%-"+e.sfx)
		_, _ = pool.Exec(ctx, `DELETE FROM mcp_audit_log WHERE workspace_id LIKE $1`, "t6-%-"+e.sfx)
	})
	return e
}

func (e *capEnv) ws(name string) string { return "t6-" + name + "-" + e.sfx }

// reserve runs Reserve in its own committed transaction.
func (e *capEnv) reserve(t testing.TB, ws, rule string, cap int, key string) Reservation {
	t.Helper()
	var r Reservation
	err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		var err error
		r, err = Reserve(context.Background(), tx, ws, rule, cap, key)
		return err
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}

func (e *capEnv) finish(t testing.TB, ws string, r Reservation, release bool) {
	t.Helper()
	err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		if release {
			return Release(context.Background(), tx, r)
		}
		return Settle(context.Background(), tx, r)
	})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func (e *capEnv) state(t testing.TB, ws, key string) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT set_config('app.workspace_id',$1,false)`, ws).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *capEnv) rowState(t testing.TB, ws, key string) (st, day string, n int) {
	t.Helper()
	_ = e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT state, day_utc FROM trigger_cap_reservations WHERE workspace_id=$1 AND idempotency_key=$2`, ws, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			n++
			if err := rows.Scan(&st, &day); err != nil {
				t.Fatal(err)
			}
		}
		return rows.Err()
	})
	return
}

func TestReserveReplayOnHeldAndSettled(t *testing.T) {
	e := newCapEnv(t)
	ws := e.ws("replay")
	a := e.reserve(t, ws, mcpWriteRule, 0, "k1")
	if !a.OK || a.Replay || a.Key != "k1" {
		t.Fatalf("first = %+v", a)
	}
	if b := e.reserve(t, ws, mcpWriteRule, 0, "k1"); !b.OK || !b.Replay {
		t.Fatalf("held replay = %+v", b)
	}
	e.finish(t, ws, a, false)
	if st, day, _ := e.rowState(t, ws, "k1"); st != "settled" || day != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("state=%s day=%s", st, day)
	}
	if c := e.reserve(t, ws, mcpWriteRule, 0, "k1"); !c.OK || !c.Replay {
		t.Fatalf("settled replay = %+v", c)
	}
	if e.reserve(t, ws, mcpWriteRule, 0, "").Key == "" {
		t.Fatal("empty key must be generated")
	}
	if k := e.reserve(t, ws, mcpWriteRule, 0, "").Key; !strings.HasPrefix(k, "mcpw_") || len(k) != 5+32 {
		t.Fatalf("generated key %q", k)
	}
}

func TestReserveAfterReleaseAllowed(t *testing.T) {
	e := newCapEnv(t)
	ws := e.ws("rel")
	a := e.reserve(t, ws, mcpWriteRule, 1, "k")
	e.finish(t, ws, a, true)
	if st, _, _ := e.rowState(t, ws, "k"); st != "released" {
		t.Fatalf("state %s", st)
	}
	b := e.reserve(t, ws, mcpWriteRule, 1, "k")
	if !b.OK || b.Replay {
		t.Fatalf("retry after release = %+v", b)
	}
	if st, _, n := e.rowState(t, ws, "k"); st != "held" || n != 1 {
		t.Fatalf("state=%s rows=%d", st, n)
	}
	// Transitions only from held: settling twice or releasing a settled one is a no-op.
	e.finish(t, ws, b, false)
	e.finish(t, ws, b, true)
	if st, _, _ := e.rowState(t, ws, "k"); st != "settled" {
		t.Fatalf("settled row changed to %s", st)
	}
}

func TestReserveCapExceeded(t *testing.T) {
	e := newCapEnv(t)
	ws := e.ws("cap")
	for i := 0; i < 2; i++ {
		r := e.reserve(t, ws, mcpWriteRule, 2, fmt.Sprintf("c%d", i))
		if !r.OK {
			t.Fatalf("reservation %d refused", i)
		}
		if i == 0 {
			e.finish(t, ws, r, false)
		}
	}
	over := e.reserve(t, ws, mcpWriteRule, 2, "c2")
	if over.OK || over.Reason != "cap" || over.Replay {
		t.Fatalf("over = %+v", over)
	}
	if _, _, n := e.rowState(t, ws, "c2"); n != 0 {
		t.Fatal("refused reservation left a row")
	}
	// A released slot is free again; the enrichment rule has its own bucket.
	r1 := e.reserve(t, ws, mcpWriteRule, 2, "c1")
	if !r1.Replay {
		t.Fatalf("c1 should replay: %+v", r1)
	}
	e.finish(t, ws, Reservation{OK: true, id: idOf(t, e, ws, "c1")}, true)
	if r := e.reserve(t, ws, mcpWriteRule, 2, "c3"); !r.OK {
		t.Fatalf("released slot not freed: %+v", r)
	}
	if r := e.reserve(t, ws, mcpEnrichRule, 1, "en1"); !r.OK {
		t.Fatalf("enrich bucket: %+v", r)
	}
	if r := e.reserve(t, ws, mcpEnrichRule, 1, "en2"); r.OK || r.Reason != "cap" {
		t.Fatalf("enrich cap: %+v", r)
	}
	// cap 0 is unlimited.
	for i := 0; i < 5; i++ {
		if r := e.reserve(t, ws, mcpWriteRule, 0, fmt.Sprintf("u%d", i)); !r.OK {
			t.Fatal("cap 0 refused")
		}
	}
}

func idOf(t testing.TB, e *capEnv, ws, key string) int64 {
	t.Helper()
	var id int64
	_ = e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT id FROM trigger_cap_reservations WHERE workspace_id=$1 AND idempotency_key=$2`, ws, key).Scan(&id)
	})
	return id
}

func TestReserveConcurrentSameKeyOnce(t *testing.T) {
	e := newCapEnv(t)
	ws := e.ws("conc")
	var fresh, replay, refused int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r Reservation
			err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
				var err error
				r, err = Reserve(context.Background(), tx, ws, mcpWriteRule, 5, "same")
				return err
			})
			switch {
			case err != nil:
				t.Errorf("reserve: %v", err)
			case r.OK && r.Replay:
				atomic.AddInt32(&replay, 1)
			case r.OK:
				atomic.AddInt32(&fresh, 1)
			default:
				atomic.AddInt32(&refused, 1)
			}
		}()
	}
	wg.Wait()
	if fresh != 1 || replay != 15 || refused != 0 {
		t.Fatalf("fresh=%d replay=%d refused=%d", fresh, replay, refused)
	}
	if _, _, n := e.rowState(t, ws, "same"); n != 1 {
		t.Fatalf("rows=%d", n)
	}
}

func TestReserveConcurrentDistinctKeysRespectCap(t *testing.T) {
	e := newCapEnv(t)
	ws := e.ws("concap")
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.reserve(t, ws, mcpWriteRule, 3, fmt.Sprintf("d%d", i))
			if r.OK {
				atomic.AddInt32(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	if ok != 3 {
		t.Fatalf("granted %d, cap 3", ok)
	}
}

func TestReserveDailyBucketRollover(t *testing.T) {
	e := newCapEnv(t)
	ws := e.ws("day")
	old := nowUTC
	defer func() { nowUTC = old }()
	nowUTC = func() time.Time { return time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC) }
	if r := e.reserve(t, ws, mcpWriteRule, 1, "d1"); !r.OK {
		t.Fatal("day 1")
	}
	if r := e.reserve(t, ws, mcpWriteRule, 1, "d2"); r.OK {
		t.Fatal("cap must hold within the day")
	}
	nowUTC = func() time.Time { return time.Date(2026, 3, 2, 0, 0, 1, 0, time.UTC) }
	if r := e.reserve(t, ws, mcpWriteRule, 1, "d2"); !r.OK {
		t.Fatal("new UTC day must reset the cap")
	}
	if _, day, _ := e.rowState(t, ws, "d2"); day != "2026-03-02" {
		t.Fatalf("day_utc %q", day)
	}
}

func TestReserveTenantIsolation(t *testing.T) {
	e := newCapEnv(t)
	a, b := e.ws("isoa"), e.ws("isob")
	if r := e.reserve(t, a, mcpWriteRule, 1, "shared"); !r.OK || r.Replay {
		t.Fatalf("a = %+v", r)
	}
	// Same key and a full cap in A are invisible to B.
	if r := e.reserve(t, b, mcpWriteRule, 1, "shared"); !r.OK || r.Replay {
		t.Fatalf("b = %+v", r)
	}
	if r := e.reserve(t, a, mcpWriteRule, 1, "other"); r.OK {
		t.Fatal("a cap must be full")
	}
	// A tx bound to A cannot reserve for B (FORCE RLS WITH CHECK).
	err := e.d.RunInWorkspace(context.Background(), a, func(tx pgx.Tx) error {
		_, err := Reserve(context.Background(), tx, b, mcpWriteRule, 0, "forge")
		return err
	})
	if err == nil {
		t.Fatal("cross-workspace reservation succeeded")
	}
}

func TestRecordWritesRowInTenantScope(t *testing.T) {
	e := newCapEnv(t)
	a, b := e.ws("auda"), e.ws("audb")
	uid := int32(7)
	long := strings.Repeat("y", 600)
	Record(context.Background(), e.d, Ctx{WorkspaceID: a, TokenID: "tok-1", UserID: &uid}, "create_automation",
		Args{"email": "x@y.example.com", "long": long, "actions": []any{map[string]any{"config": map[string]any{"api_key": "NESTEDSECRET"}}}},
		AuditOutcome{Status: "capped", Error: "daily MCP write cap exceeded", CostUSD: 0.25, IdempotencyKey: "ik-1"})
	Record(context.Background(), e.d, Ctx{WorkspaceID: b}, "find_leads", nil, AuditOutcome{Status: "ok"})

	type row struct {
		Tool, Status, Err, Args, Key string
		Token                        *string
		User                         *int32
		Cost                         float64
	}
	read := func(ws string) []row {
		var out []row
		err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT tool_name, result_status, COALESCE(error,''), arguments_redacted::text,
				COALESCE(idempotency_key,''), token_id, user_id, estimated_cost_usd FROM mcp_audit_log WHERE workspace_id LIKE $1`, "t6-%-"+e.sfx)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.Tool, &r.Status, &r.Err, &r.Args, &r.Key, &r.Token, &r.User, &r.Cost); err != nil {
					return err
				}
				out = append(out, r)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ra, rb := read(a), read(b)
	if len(ra) != 1 || len(rb) != 1 {
		t.Fatalf("rows a=%d b=%d (RLS scoping)", len(ra), len(rb))
	}
	r := ra[0]
	if r.Tool != "create_automation" || r.Status != "capped" || r.Err != "daily MCP write cap exceeded" || r.Key != "ik-1" ||
		r.Token == nil || *r.Token != "tok-1" || r.User == nil || *r.User != 7 || r.Cost != 0.25 {
		t.Fatalf("row = %+v", r)
	}
	if strings.Contains(r.Args, "NESTEDSECRET") || strings.Contains(r.Args, "x@y.example.com") || strings.Contains(r.Args, long) {
		t.Fatalf("args not redacted: %s", r.Args)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Args), &m); err != nil || m["email"] != "***redacted***" {
		t.Fatalf("args %s", r.Args)
	}
	if rb[0].Tool != "find_leads" || rb[0].Token != nil || rb[0].User != nil {
		t.Fatalf("b row = %+v", rb[0])
	}
}

func TestRecordClosedPoolDoesNotPanic(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	d := Deps{InWorkspace: func(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
		return tenancy.InWorkspace(ctx, pool, ws, fn)
	}}
	pool.Close()
	Record(context.Background(), d, Ctx{WorkspaceID: "t6-closed"}, "find_leads", Args{"a": 1.0}, AuditOutcome{Status: "ok"})
}
