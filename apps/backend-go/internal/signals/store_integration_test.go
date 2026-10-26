//go:build integration

package signals_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signals"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool, owner *pgxpool.Pool
	wsA, wsB    string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 4)}
	var err error
	e.owner, err = pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.wsA = fmt.Sprintf("t6a_%d", os.Getpid())
	e.wsB = fmt.Sprintf("t6b_%d", os.Getpid())
	clean := func() {
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM signals WHERE workspace_id = ANY($1)`, []string{e.wsA, e.wsB})
	}
	clean()
	t.Cleanup(clean)
	return e
}

type sig struct {
	id, ws            string
	lead              any
	company, typ, src any
	weight            any
	at                float64
	read              bool
}

func (e *env) seed(t *testing.T, rows ...sig) {
	t.Helper()
	for _, r := range rows {
		_, err := e.owner.Exec(context.Background(),
			`INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, description, source, source_url, weight, created_at, read)
			 VALUES ($1,$2,$3,$4,$5,'t','d',$6,'u',$7,$8,$9)`,
			r.id, r.ws, r.lead, r.company, r.typ, r.src, r.weight, r.at, r.read)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) in(t *testing.T, ws string, fn func(pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

func ids(ss []signals.Signal) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func i64(n int64) *int64 { return &n }

func TestListOrderTieBreak(t *testing.T) {
	e := setup(t)
	e.seed(t,
		sig{id: "a", ws: e.wsA, typ: "hiring", at: 100},
		sig{id: "c", ws: e.wsA, typ: "hiring", at: 100},
		sig{id: "b", ws: e.wsA, typ: "hiring", at: 100},
		sig{id: "z", ws: e.wsA, typ: "news", at: 50},
		sig{id: "n", ws: e.wsA, typ: "hiring", at: 200},
	)
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		got, err := signals.List(context.Background(), tx, "", nil, 10, 0)
		if err != nil {
			return err
		}
		if want := []string{"n", "c", "b", "a", "z"}; !reflect.DeepEqual(ids(got), want) {
			t.Errorf("got %v want %v", ids(got), want)
		}
		got, _ = signals.List(context.Background(), tx, "", nil, 2, 1)
		if want := []string{"c", "b"}; !reflect.DeepEqual(ids(got), want) {
			t.Errorf("limit/offset got %v want %v", ids(got), want)
		}
		got, _ = signals.List(context.Background(), tx, "news", nil, 10, 0)
		if want := []string{"z"}; !reflect.DeepEqual(ids(got), want) {
			t.Errorf("type got %v", ids(got))
		}
		return nil
	})
}

func TestPageKeysetAcrossTies(t *testing.T) {
	e := setup(t)
	var rows []sig
	for i := 0; i < 25; i++ {
		rows = append(rows, sig{id: fmt.Sprintf("s%02d", i), ws: e.wsA, typ: "hiring", at: 1788000000.25})
	}
	e.seed(t, rows...)
	var seen []string
	cursor := ""
	var sizes []int
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		for guard := 0; guard < 10; guard++ {
			p, err := signals.Page(context.Background(), tx, nil, nil, 7, cursor)
			if err != nil {
				return err
			}
			sizes = append(sizes, len(p.Signals))
			seen = append(seen, ids(p.Signals)...)
			if !p.HasMore {
				if p.NextCursor != nil {
					t.Errorf("last page has cursor")
				}
				return nil
			}
			cursor = *p.NextCursor
			last := p.Signals[len(p.Signals)-1]
			if cursor != signals.EncodeCursor(last.CreatedAt, last.ID) {
				t.Errorf("cursor is not the last row's")
			}
		}
		return nil
	})
	if !reflect.DeepEqual(sizes, []int{7, 7, 7, 4}) {
		t.Fatalf("sizes %v", sizes)
	}
	if len(seen) != 25 {
		t.Fatalf("seen %d", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] <= seen[i] {
			t.Fatalf("not strictly descending at %d: %v", i, seen)
		}
	}
}

func TestPageFilterByTypeAndLead(t *testing.T) {
	e := setup(t)
	e.seed(t,
		sig{id: "a", ws: e.wsA, lead: 1, typ: "hiring", at: 5},
		sig{id: "b", ws: e.wsA, lead: 2, typ: "hiring", at: 4},
		sig{id: "c", ws: e.wsA, lead: 1, typ: "news", at: 3},
		sig{id: "d", ws: e.wsA, lead: 1, typ: "funding", at: 2},
	)
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		ctx := context.Background()
		p, err := signals.Page(ctx, tx, []string{"hiring"}, nil, 10, "")
		if err != nil || !reflect.DeepEqual(ids(p.Signals), []string{"a", "b"}) {
			t.Errorf("type: %v %v", ids(p.Signals), err)
		}
		p, _ = signals.Page(ctx, tx, nil, i64(1), 10, "")
		if !reflect.DeepEqual(ids(p.Signals), []string{"a", "c", "d"}) {
			t.Errorf("lead: %v", ids(p.Signals))
		}
		p, _ = signals.Page(ctx, tx, []string{"hiring", "news"}, i64(1), 10, "")
		if !reflect.DeepEqual(ids(p.Signals), []string{"a", "c"}) {
			t.Errorf("both: %v", ids(p.Signals))
		}
		if _, err := signals.Page(ctx, tx, nil, nil, 10, "!!!"); err != signals.ErrBadCursor {
			t.Errorf("bad cursor err = %v", err)
		}
		return nil
	})
}

func TestPageNaNInfCursor(t *testing.T) {
	e := setup(t)
	e.seed(t, sig{id: "a", ws: e.wsA, typ: "x", at: 5}, sig{id: "b", ws: e.wsA, typ: "x", at: 6})
	// Python sends NaN/Infinity straight to Postgres: NaN sorts above every number
	// and +Infinity above every finite value, so all rows qualify; -Infinity none.
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		for _, c := range []struct {
			cur  string
			want int
		}{
			{"W05hTiwiYSJd", 2},         // [NaN,"a"]
			{"W0luZmluaXR5LCJhIl0", 2},  // [Infinity,"a"]
			{"Wy1JbmZpbml0eSwiYSJd", 0}, // [-Infinity,"a"]
		} {
			p, err := signals.Page(context.Background(), tx, nil, nil, 10, c.cur)
			if err != nil || len(p.Signals) != c.want {
				t.Errorf("%s: %d %v", c.cur, len(p.Signals), err)
			}
		}
		return nil
	})
}

func TestCountsKeys(t *testing.T) {
	e := setup(t)
	e.seed(t,
		sig{id: "1", ws: e.wsA, typ: "news", at: 1, read: true},
		sig{id: "2", ws: e.wsA, typ: "hiring", at: 2},
		sig{id: "3", ws: e.wsA, typ: "funding", at: 3},
		sig{id: "4", ws: e.wsA, typ: "hiring", at: 4},
		sig{id: "5", ws: e.wsB, typ: "zzz", at: 5},
	)
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		c, err := signals.Counts(context.Background(), tx)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(c)
		if want := `{"funding":1,"hiring":2,"news":1,"total":4,"unread":3}`; string(b) != want {
			t.Errorf("got %s want %s", b, want)
		}
		return nil
	})
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		if err := signals.MarkRead(context.Background(), tx, []string{"2", "3"}); err != nil {
			return err
		}
		c, _ := signals.Counts(context.Background(), tx)
		b, _ := json.Marshal(c)
		if want := `{"funding":1,"hiring":2,"news":1,"total":4,"unread":1}`; string(b) != want {
			t.Errorf("after mark got %s", b)
		}
		return nil
	})
	e.in(t, e.wsA, func(tx pgx.Tx) error { // empty workspace shape
		return nil
	})
	empty := e.wsA + "_empty"
	e.in(t, empty, func(tx pgx.Tx) error {
		c, _ := signals.Counts(context.Background(), tx)
		b, _ := json.Marshal(c)
		if string(b) != `{"total":0,"unread":0}` {
			t.Errorf("empty got %s", b)
		}
		return nil
	})
}

func TestMarkReadOnlyOwnWorkspace(t *testing.T) {
	e := setup(t)
	e.seed(t, sig{id: "a1", ws: e.wsA, typ: "x", at: 1}, sig{id: "b1", ws: e.wsB, typ: "x", at: 1})
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		if err := signals.MarkRead(context.Background(), tx, nil); err != nil {
			t.Error(err)
		}
		return signals.MarkRead(context.Background(), tx, []string{"a1", "b1", "nope"})
	})
	var a, b bool
	_ = e.owner.QueryRow(context.Background(), `SELECT read FROM signals WHERE id='a1'`).Scan(&a)
	_ = e.owner.QueryRow(context.Background(), `SELECT read FROM signals WHERE id='b1'`).Scan(&b)
	if !a || b {
		t.Fatalf("a1 read=%v (want true), b1 read=%v (want false)", a, b)
	}
}

func TestSignalsTenantIsolation(t *testing.T) {
	e := setup(t)
	e.seed(t, sig{id: "a1", ws: e.wsA, typ: "x", at: 1}, sig{id: "b1", ws: e.wsB, typ: "x", at: 2})
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		l, _ := signals.List(context.Background(), tx, "", nil, 10, 0)
		p, _ := signals.Page(context.Background(), tx, nil, nil, 10, "")
		c, _ := signals.Counts(context.Background(), tx)
		a, _ := signals.Analytics(context.Background(), tx, 30, time.Unix(86400, 0))
		if !reflect.DeepEqual(ids(l), []string{"a1"}) || !reflect.DeepEqual(ids(p.Signals), []string{"a1"}) || c.Total != 1 || a.Summary.Total != 1 {
			t.Errorf("leak: %v %v %d %d", ids(l), ids(p.Signals), c.Total, a.Summary.Total)
		}
		return nil
	})
}

func TestSignalJSONKeyOrderAndFloats(t *testing.T) {
	s := signals.Signal{ID: "i", WorkspaceID: "w", LeadID: i64(3), Company: "é<c>", SignalType: "hiring", Title: "t", Description: "d",
		Source: "s", SourceURL: "u", Weight: nil, CreatedAt: 1788000000, Read: 1}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // what httpx.WriteJSON does
	_ = enc.Encode(s)
	b := bytes.TrimSpace(buf.Bytes())
	want := `{"id":"i","workspace_id":"w","lead_id":3,"company":"é<c>","signal_type":"hiring","title":"t","description":"d","source":"s","source_url":"u","weight":null,"created_at":1788000000.0,"read":1}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
}

// Analytics seed, now = 2026-08-29 06:00 UTC (D = that UTC day, 30-day window).
func TestAnalyticsMatchesPython(t *testing.T) {
	e := setup(t)
	now := time.Date(2026, 8, 29, 6, 0, 0, 0, time.UTC)
	day0 := float64(now.Unix() / 86400 * 86400)
	cutoff30 := float64(now.Unix()) - 30*86400
	e.seed(t,
		sig{id: "s1", ws: e.wsA, lead: 1, company: "Acme", typ: "hiring", src: "jobspy", weight: 5, at: day0 + 3600},
		// 13:53 UTC: CAST(x/86400 AS INTEGER) rounds up into tomorrow's bucket (outside the trend)
		sig{id: "s2", ws: e.wsA, lead: 1, company: "Acme", typ: "hiring", src: "jobspy", weight: 3, at: day0 + 50000},
		// previous day 16:40: rounds up into today's bucket
		sig{id: "s3", ws: e.wsA, lead: 1, company: "Acme", typ: "hiring", src: "newsapi", weight: 2, at: day0 - 26400},
		sig{id: "s4", ws: e.wsA, lead: 2, company: nil, typ: "funding", src: "newsapi", weight: 10, at: day0 - 5*86400 + 100},
		sig{id: "s8", ws: e.wsA, lead: nil, company: nil, typ: nil, src: nil, weight: nil, at: day0 - 2*86400},
		// previous window (2 rows) and older than both windows (excluded everywhere)
		sig{id: "p1", ws: e.wsA, lead: 3, company: "P", typ: "news", src: "x", weight: 1, at: cutoff30 - 86400},
		sig{id: "p2", ws: e.wsA, lead: 3, company: "P", typ: "news", src: "x", weight: 1, at: cutoff30 - 2*86400},
		sig{id: "old", ws: e.wsA, lead: 4, company: "O", typ: "news", src: "x", weight: 1, at: cutoff30 - 61*86400},
		sig{id: "other", ws: e.wsB, lead: 9, company: "B", typ: "news", src: "x", weight: 99, at: day0},
	)
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		r, err := signals.Analytics(context.Background(), tx, 30, now)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(r)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if got["period_days"].(float64) != 30 {
			t.Errorf("period_days")
		}
		sum := got["summary"].(map[string]any)
		if sum["total"].(float64) != 5 || sum["weighted_score"].(float64) != 20 || sum["active_accounts"].(float64) != 2 || sum["momentum_pct"].(float64) != 150 {
			t.Errorf("summary %v", sum)
		}
		wantTypes := `[{"signal_type":"hiring","count":3,"weight":10},{"signal_type":"funding","count":1,"weight":10},{"signal_type":"unknown","count":1,"weight":0}]`
		wantSrc := `[{"source":"jobspy","count":2},{"source":"newsapi","count":2},{"source":"unknown","count":1}]`
		// ORDER BY sum(weight) DESC puts the all-NULL-weight group first in Postgres (Python parity)
		wantTop := `[{"lead_id":null,"company":"Lead None","count":1,"weight":0},{"lead_id":1,"company":"Acme","count":3,"weight":10},{"lead_id":2,"company":"Lead 2","count":1,"weight":10}]`
		for k, w := range map[string]string{"by_type": wantTypes, "by_source": wantSrc, "top_accounts": wantTop} {
			if g, _ := json.Marshal(got[k]); string(g) != w && !jsonEqual(g, w) {
				t.Errorf("%s = %s want %s", k, g, w)
			}
		}
		trend := got["trend"].([]any)
		if len(trend) != 30 {
			t.Fatalf("trend len %d", len(trend))
		}
		if first := trend[0].(map[string]any)["date"]; first != "2026-07-31" {
			t.Errorf("first date %v", first)
		}
		last := trend[29].(map[string]any)
		if last["date"] != "2026-08-29" || last["count"].(float64) != 2 || last["weight"].(float64) != 7 {
			t.Errorf("today bucket %v", last)
		}
		counts := map[string]float64{}
		total := 0.0
		for _, x := range trend {
			m := x.(map[string]any)
			counts[m["date"].(string)] = m["count"].(float64)
			total += m["count"].(float64)
		}
		if counts["2026-08-24"] != 1 || counts["2026-08-27"] != 1 || total != 4 {
			t.Errorf("trend counts %v total %v (s2 is in tomorrow's bucket, outside the trend)", counts, total)
		}
		// key order
		if want := `{"period_days":30,"summary":{"total":5,"weighted_score":20,"active_accounts":2,"momentum_pct":150.0},"trend":`; string(b[:len(want)]) != want {
			t.Errorf("key order/float: %s", b[:140])
		}
		return nil
	})
	// 90-day window also sees p1/p2 (total 7, weight 22); "old" is the only previous-window row
	e.in(t, e.wsA, func(tx pgx.Tx) error {
		r, _ := signals.Analytics(context.Background(), tx, 90, now)
		b, _ := json.Marshal(r)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		sum := got["summary"].(map[string]any)
		if sum["total"].(float64) != 7 || sum["weighted_score"].(float64) != 22 || sum["momentum_pct"].(float64) != 600 || len(got["trend"].([]any)) != 90 {
			t.Errorf("90d: %s", b[:200])
		}
		return nil
	})
	// previous window empty -> null momentum
	e.seed(t, sig{id: "solo", ws: e.wsB, lead: 1, typ: "x", at: float64(now.Unix()) - 3600})
	e.in(t, e.wsB, func(tx pgx.Tx) error {
		r, _ := signals.Analytics(context.Background(), tx, 7, now)
		if r.Summary.MomentumPct != nil || r.Summary.Total < 1 {
			t.Errorf("null momentum expected: %+v", r.Summary)
		}
		return nil
	})
	empty := e.wsA + "_empty"
	e.in(t, empty, func(tx pgx.Tx) error {
		r, _ := signals.Analytics(context.Background(), tx, 7, now)
		b, _ := json.Marshal(r)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if len(got["trend"].([]any)) != 7 || got["summary"].(map[string]any)["momentum_pct"] != nil ||
			string(mustJSON(got["by_type"])) != "[]" || string(mustJSON(got["top_accounts"])) != "[]" {
			t.Errorf("empty: %s", b)
		}
		return nil
	})
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func jsonEqual(a []byte, b string) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal([]byte(b), &y)
	return reflect.DeepEqual(x, y)
}
