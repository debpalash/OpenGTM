//go:build integration

package autocaps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool *pgxpool.Pool
	sfx  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 48)
	e := &env{pool: pool, sfx: fmt.Sprintf("%d", time.Now().UnixNano())}
	prev := nowUTC
	t.Cleanup(func() {
		nowUTC = prev
		_, _ = pool.Exec(context.Background(), `DELETE FROM trigger_cap_reservations WHERE workspace_id LIKE $1`, "p08t5-%-"+e.sfx)
	})
	return e
}

func (e *env) ws(n string) string { return "p08t5-" + n + "-" + e.sfx }

func (e *env) tx(t testing.TB, ws string, fn func(pgx.Tx) error) error {
	t.Helper()
	return tenancy.InWorkspace(context.Background(), e.pool, ws, fn)
}

func (e *env) reserve(t testing.TB, ws string, tc TriggerCaps, cost float64, key string, global float64) Reservation {
	t.Helper()
	var r Reservation
	if err := e.tx(t, ws, func(tx pgx.Tx) error {
		var err error
		r, err = TryReserve(context.Background(), tx, tc, ws, cost, key, global)
		return err
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}

func (e *env) finish(t testing.TB, ws string, r Reservation, release bool) {
	t.Helper()
	if err := e.tx(t, ws, func(tx pgx.Tx) error {
		if release {
			return Release(context.Background(), tx, r)
		}
		return Settle(context.Background(), tx, r)
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func (e *env) state(t testing.TB, ws, key string) (state string, n int) {
	t.Helper()
	_ = e.tx(t, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT state FROM trigger_cap_reservations WHERE workspace_id=$1 AND idempotency_key=$2`, ws, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			n++
			_ = rows.Scan(&state)
		}
		return rows.Err()
	})
	return
}

func i32(v int32) *int32     { return &v }
func f64(v float64) *float64 { return &v }
func clock(s string) func() time.Time {
	tm, _ := time.Parse("2006-01-02", s)
	return func() time.Time { return tm.Add(12 * time.Hour) }
}

type vstep struct {
	Op      string  `json:"op"`
	Trigger string  `json:"trigger"`
	Key     string  `json:"key"`
	Cost    any     `json:"cost"`
	WS      string  `json:"ws"`
	Day     string  `json:"day"`
	OK      bool    `json:"ok"`
	Reason  *string `json:"reason"`
	HasID   bool    `json:"has_id"`
	IdemKey *string `json:"idempotency_key"`
	Actions int64   `json:"actions"`
}

type vrow struct {
	WorkspaceID    string  `json:"workspace_id"`
	TriggerID      string  `json:"trigger_id"`
	DayUTC         string  `json:"day_utc"`
	ReservedUSD    float64 `json:"reserved_usd"`
	ReservedAction int     `json:"reserved_actions"`
	IdempotencyKey string  `json:"idempotency_key"`
	State          string  `json:"state"`
}

type vscenario struct {
	Name      string  `json:"name"`
	GlobalCap float64 `json:"global_cap_usd"`
	Triggers  map[string]struct {
		MaxActions *int32   `json:"max_actions"`
		MaxSpend   *float64 `json:"max_spend"`
	} `json:"triggers"`
	Steps []vstep `json:"steps"`
	Rows  []vrow  `json:"rows"`
}

func vcost(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case string:
		var f float64
		_, _ = fmt.Sscanf(x, "%g", &f)
		return f
	}
	return 0
}

func TestCapsMatchVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/caps_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Scenarios []vscenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Scenarios) < 19 {
		t.Fatalf("expected 19 scenarios, got %d", len(doc.Scenarios))
	}
	e := newEnv(t)
	for si, sc := range doc.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			wsOf := func(w string) string {
				if w == "" {
					w = "ws-1"
				}
				return e.ws(fmt.Sprintf("v%d-%s", si, w))
			}
			nowUTC = clock("2026-08-29")
			held := map[string]Reservation{}
			tcs := func(id string) TriggerCaps {
				c := sc.Triggers[id]
				return TriggerCaps{ID: id, MaxActionsPerDay: c.MaxActions, MaxSpendUSDPerDay: c.MaxSpend}
			}
			for i, s := range sc.Steps {
				switch s.Op {
				case "day":
					nowUTC = clock(s.Day)
				case "reserve":
					r := e.reserve(t, wsOf(s.WS), tcs(s.Trigger), vcost(s.Cost), s.Key, sc.GlobalCap)
					if r.OK != s.OK || (s.Reason != nil && r.Reason != *s.Reason) || (s.Reason == nil && r.Reason != "") ||
						(r.ID != 0) != s.HasID || (s.IdemKey != nil && r.IdempotencyKey != *s.IdemKey) || (s.IdemKey == nil && r.IdempotencyKey != "") {
						t.Fatalf("step %d reserve %+v: got %+v", i, s, r)
					}
					if r.OK {
						held[s.Key] = r
					}
				case "settle":
					e.finish(t, wsOf(s.WS), held[s.Key], false)
				case "release":
					e.finish(t, wsOf(s.WS), held[s.Key], true)
				case "settle_none":
					e.finish(t, wsOf(""), Reservation{}, false)
					e.finish(t, wsOf(""), Reservation{OK: false, Reason: "cap"}, true)
				case "actions_today":
					day := s.Day
					if day == "" {
						day = DayUTC(nowUTC())
					}
					var n int64
					if err := e.tx(t, wsOf(s.WS), func(tx pgx.Tx) error {
						var err error
						n, err = ActionsToday(context.Background(), tx, s.Trigger, wsOf(s.WS), day)
						return err
					}); err != nil || n != s.Actions {
						t.Fatalf("step %d actions_today = %d, %v; want %d", i, n, err, s.Actions)
					}
				default:
					t.Fatalf("unknown op %q", s.Op)
				}
			}
			// final rows, per workspace, in id order
			var want []vrow
			want = append(want, sc.Rows...)
			var got []vrow
			wss := map[string]bool{}
			for _, r := range want {
				wss[r.WorkspaceID] = true
			}
			for _, s := range sc.Steps {
				if s.Op == "reserve" {
					if s.WS == "" {
						wss["ws-1"] = true
					} else {
						wss[s.WS] = true
					}
				}
			}
			for w := range wss {
				_ = e.tx(t, wsOf(w), func(tx pgx.Tx) error {
					rows, err := tx.Query(context.Background(), `SELECT trigger_id, day_utc, reserved_usd, reserved_actions, idempotency_key, state FROM trigger_cap_reservations WHERE workspace_id=$1 ORDER BY id`, wsOf(w))
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					for rows.Next() {
						r := vrow{WorkspaceID: w}
						if err := rows.Scan(&r.TriggerID, &r.DayUTC, &r.ReservedUSD, &r.ReservedAction, &r.IdempotencyKey, &r.State); err != nil {
							t.Fatal(err)
						}
						got = append(got, r)
					}
					return rows.Err()
				})
			}
			if len(got) != len(want) {
				t.Fatalf("rows: got %d want %d (%+v)", len(got), len(want), got)
			}
			// match as sets keyed by (ws,key)
			idx := map[string]vrow{}
			for _, r := range got {
				idx[r.WorkspaceID+"|"+r.IdempotencyKey] = r
			}
			for _, w := range want {
				g, ok := idx[w.WorkspaceID+"|"+w.IdempotencyKey]
				if !ok || g != w {
					t.Fatalf("row %+v: got %+v", w, g)
				}
			}
		})
	}
}

func TestConcurrentReserversNearCapAdmitExactlyCap(t *testing.T) {
	e := newEnv(t)
	nowUTC = func() time.Time { return time.Now().UTC() }
	run := func(name string, tc TriggerCaps, cost float64, global float64, want int64) {
		ws := e.ws(name)
		var ok atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				err := e.tx(t, ws, func(tx pgx.Tx) error {
					r, err := TryReserve(context.Background(), tx, tc, ws, cost, fmt.Sprintf("k%d", i), global)
					if err != nil {
						return err
					}
					if r.OK {
						ok.Add(1)
					}
					return nil
				})
				if err != nil {
					t.Errorf("worker %d: %v", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if ok.Load() != want {
			t.Fatalf("%s: admitted %d, want %d", name, ok.Load(), want)
		}
		var n int64
		_ = e.tx(t, ws, func(tx pgx.Tx) error {
			var err error
			n, err = ActionsToday(context.Background(), tx, tc.ID, ws, DayUTC(nowUTC()))
			return err
		})
		if n != want {
			t.Fatalf("%s: rows %d want %d", name, n, want)
		}
	}
	run("actions", TriggerCaps{ID: "t", MaxActionsPerDay: i32(5)}, 0, 0, 5)
	run("spend", TriggerCaps{ID: "t", MaxSpendUSDPerDay: f64(1.00)}, 0.25, 0, 4)
	run("global", TriggerCaps{ID: "t"}, 0.25, 1.00, 4)
}

func TestGlobalCapAcrossRules(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	ws := e.ws("glob")
	if r := e.reserve(t, ws, TriggerCaps{ID: "a"}, 0.6, "1", 1.0); !r.OK {
		t.Fatal("first must pass")
	}
	if r := e.reserve(t, ws, TriggerCaps{ID: "b"}, 0.5, "2", 1.0); r.OK || r.Reason != "cap" {
		t.Fatalf("other rule must hit the workspace cap: %+v", r)
	}
	if r := e.reserve(t, ws, TriggerCaps{ID: "b"}, 0.4, "3", 1.0); !r.OK {
		t.Fatalf("exactly at cap must pass: %+v", r)
	}
	if r := e.reserve(t, ws, TriggerCaps{ID: "b"}, 0, "4", 1.0); !r.OK {
		t.Fatalf("free action ignores spend caps: %+v", r)
	}
}

func TestIdempotentReserveReturnsSame(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	ws := e.ws("idem")
	tc := TriggerCaps{ID: "t", MaxActionsPerDay: i32(1)}
	a := e.reserve(t, ws, tc, 0.1, "k", 0)
	b := e.reserve(t, ws, tc, 0.1, "k", 0)
	if !a.OK || !b.OK || a.ID != b.ID || a.ID == 0 {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if _, n := e.state(t, ws, "k"); n != 1 {
		t.Fatalf("rows = %d", n)
	}
	// same key is not counted twice even at the cap
	if c := e.reserve(t, ws, tc, 0.1, "k", 0); !c.OK {
		t.Fatalf("replay at cap: %+v", c)
	}
}

func TestReleasedKeyIsRefused(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	ws := e.ws("rk")
	tc := TriggerCaps{ID: "t", MaxActionsPerDay: i32(5)}
	a := e.reserve(t, ws, tc, 0, "k", 0)
	e.finish(t, ws, a, true)
	if r := e.reserve(t, ws, tc, 0, "k", 0); r.OK || r.Reason != "cap" {
		t.Fatalf("released key must be refused: %+v", r)
	}
	if st, n := e.state(t, ws, "k"); st != "released" || n != 1 {
		t.Fatalf("state=%s n=%d", st, n)
	}
}

func TestSettleReleaseAreOneWay(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	ws := e.ws("ow")
	tc := TriggerCaps{ID: "t"}
	a := e.reserve(t, ws, tc, 0.1, "a", 0)
	e.finish(t, ws, a, false)
	e.finish(t, ws, a, false) // idempotent
	e.finish(t, ws, a, true)  // settled stays settled
	if st, _ := e.state(t, ws, "a"); st != "settled" {
		t.Fatalf("a = %s", st)
	}
	b := e.reserve(t, ws, tc, 0.1, "b", 0)
	e.finish(t, ws, b, true)
	e.finish(t, ws, b, true)
	e.finish(t, ws, b, false) // released stays released
	if st, _ := e.state(t, ws, "b"); st != "released" {
		t.Fatalf("b = %s", st)
	}
}

func TestDayRolloverUTC(t *testing.T) {
	// 23:59:59 at UTC-5 is already the next UTC day.
	loc := time.FixedZone("x", -5*3600)
	if d := DayUTC(time.Date(2026, 8, 29, 23, 59, 59, 0, loc)); d != "2026-08-30" {
		t.Fatalf("DayUTC = %s", d)
	}
	e := newEnv(t)
	ws := e.ws("day")
	tc := TriggerCaps{ID: "t", MaxActionsPerDay: i32(1)}
	nowUTC = clock("2026-08-29")
	if !e.reserve(t, ws, tc, 0, "a", 0).OK || e.reserve(t, ws, tc, 0, "b", 0).OK {
		t.Fatal("cap of one on day 1")
	}
	nowUTC = clock("2026-08-30")
	if !e.reserve(t, ws, tc, 0, "b", 0).OK {
		t.Fatal("a new UTC day has a fresh budget")
	}
}

func TestCostRounding(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	ws := e.ws("round")
	tc := TriggerCaps{ID: "t", MaxSpendUSDPerDay: f64(0.3)}
	// 0.1 + 0.2 = 0.30000000000000004 in binary float; rounded to 6 places it fits.
	if !e.reserve(t, ws, tc, 0.1, "a", 0).OK || !e.reserve(t, ws, tc, 0.2, "b", 0).OK {
		t.Fatal("0.1 + 0.2 must fit a 0.3 cap")
	}
	if e.reserve(t, ws, tc, 0.000001, "c", 0).OK {
		t.Fatal("one micro-dollar over must be refused")
	}
	// Non-finite costs fail closed (never over-admit), negative costs are 0.
	for i, c := range []float64{math.NaN(), math.Inf(1)} {
		if r := e.reserve(t, ws, TriggerCaps{ID: "t2"}, c, fmt.Sprintf("n%d", i), 0); r.OK {
			t.Fatalf("non-finite cost %v admitted", c)
		}
	}
	if r := e.reserve(t, ws, TriggerCaps{ID: "t3", MaxSpendUSDPerDay: f64(math.NaN())}, 0.1, "nanCap", 0); r.OK {
		t.Fatal("NaN spend cap must refuse paid actions")
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	w1, w2 := e.ws("iso1"), e.ws("iso2")
	tc := TriggerCaps{ID: "t", MaxActionsPerDay: i32(1)}
	a := e.reserve(t, w1, tc, 0.5, "same", 1.0)
	b := e.reserve(t, w2, tc, 0.5, "same", 1.0)
	if !a.OK || !b.OK || a.ID == b.ID {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	// ws2 cannot see or settle ws1's reservation (RLS): its Settle is a no-op.
	if err := e.tx(t, w2, func(tx pgx.Tx) error { return Settle(context.Background(), tx, a) }); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.state(t, w1, "same"); st != "held" {
		t.Fatalf("ws1 reservation changed by ws2: %s", st)
	}
	var n int64
	_ = e.tx(t, w2, func(tx pgx.Tx) error {
		var err error
		n, err = ActionsToday(context.Background(), tx, "t", w1, "2026-08-29")
		return err
	})
	if n != 0 {
		t.Fatalf("ws2 sees ws1 actions: %d", n)
	}
}

func TestRollsBackWithCallerTx(t *testing.T) {
	e := newEnv(t)
	nowUTC = clock("2026-08-29")
	ws := e.ws("rb")
	tc := TriggerCaps{ID: "t", MaxActionsPerDay: i32(1)}
	boom := errors.New("boom")
	err := e.tx(t, ws, func(tx pgx.Tx) error {
		r, err := TryReserve(context.Background(), tx, tc, ws, 0.1, "k", 0)
		if err != nil || !r.OK {
			t.Fatalf("%+v %v", r, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, n := e.state(t, ws, "k"); n != 0 {
		t.Fatal("reservation survived caller rollback")
	}
	if !e.reserve(t, ws, tc, 0.1, "k2", 0).OK {
		t.Fatal("slot must be free after rollback")
	}
}
