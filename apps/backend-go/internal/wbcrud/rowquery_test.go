//go:build integration

package wbcrud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type rqEnv struct {
	pool  *pgxpool.Pool
	owner *pgxpool.Pool
	ws    string
	wb    string
	other string // a second workbook with a view
	cols  []wbcrud.Column
}

func rqSetup(t *testing.T) *rqEnv {
	t.Helper()
	e := &rqEnv{}
	e.pool = testpg.NewAppPool(t, 4)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e.owner = owner
	e.ws = fmt.Sprintf("rq_ws_%d", os.Getpid())
	e.wb = fmt.Sprintf("rq_wb_%d", os.Getpid())
	e.other = fmt.Sprintf("rq_wb2_%d", os.Getpid())
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM workbooks WHERE workspace_id=$1`, e.ws)
	}
	clean()
	t.Cleanup(clean)
	e.cols = []wbcrud.Column{
		wbcrud.MakeColumn("id", "name", "name", "Name", "type", "lead_field"),
		wbcrud.MakeColumn("id", "mail", "name", "Mail", "type", "input", "lead_field", "email"),
		wbcrud.MakeColumn("id", "score", "name", "Score", "type", "enrichment"),
	}
	for _, id := range []string{e.wb, e.other} {
		if _, err := owner.Exec(ctx, `INSERT INTO workbooks (id,name,workspace_id,columns_config) VALUES ($1,$1,$2,'[]')`, id, e.ws); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *rqEnv) wbq() wbcrud.QueryWorkbook { return wbcrud.QueryWorkbook{ID: e.wb, Columns: e.cols} }

// seed inserts rows (position = index) with the given data/enrichments JSON.
func (e *rqEnv) seed(t *testing.T, rows [][2]string) []int64 {
	t.Helper()
	var ids []int64
	for i, r := range rows {
		var id int64
		var enr any
		if r[1] != "" {
			enr = r[1]
		}
		err := e.owner.QueryRow(context.Background(),
			`INSERT INTO workbook_rows (workbook_id, position, data, enrichments, workspace_id) VALUES ($1,$2,$3::json,$4::json,$5) RETURNING id`,
			e.wb, i, r[0], enr, e.ws).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *rqEnv) run(t *testing.T, fn func(tx pgx.Tx)) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error { fn(tx); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (e *rqEnv) ids(t *testing.T, q wbcrud.RowQuery) []int64 {
	t.Helper()
	var out []int64
	e.run(t, func(tx pgx.Tx) {
		rows, err := tx.Query(context.Background(), q.SQL, q.Args...)
		if err != nil {
			t.Fatalf("%v\n%s", err, q.SQL)
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, int64(vals[0].(int32)))
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
	})
	return out
}

func rqView(f ...wbcrud.ViewFilter) *wbcrud.QueryView {
	return &wbcrud.QueryView{ID: "v", Config: wbcrud.ViewConfig{Filters: f}}
}

func TestFilterOpsMatchPythonResults(t *testing.T) {
	e := rqSetup(t)
	names := []string{"Acme", "acme corp", "100% done", "a_b", "axb", "", "   ", "BETA", "50%"}
	var rows [][2]string
	for _, n := range names {
		b, _ := json.Marshal(map[string]any{"name": n})
		rows = append(rows, [2]string{string(b), ""})
	}
	rows = append(rows, [2]string{`{"other":"x"}`, ""}) // name missing -> NULL
	ids := e.seed(t, rows)
	all := append(append([]string{}, names...), "<null>")
	// Reference semantics of _workbook_rows_query: lower(coalesce(x,'')) with
	// '%' and '_' literal in contains; empty = trim(..) = ''.
	ref := func(op, needle string) []int64 {
		var out []int64
		for i, n := range all {
			if n == "<null>" {
				n = ""
			}
			l := strings.ToLower(n)
			nl := strings.ToLower(needle)
			var ok bool
			switch op {
			case "equals":
				ok = l == nl
			case "not_equals":
				ok = l != nl
			case "contains":
				ok = strings.Contains(l, nl)
			case "not_contains":
				ok = !strings.Contains(l, nl)
			case "empty":
				ok = strings.Trim(l, " ") == ""
			case "not_empty":
				ok = strings.Trim(l, " ") != ""
			}
			if ok {
				out = append(out, ids[i])
			}
		}
		return out
	}
	cases := []struct{ op, val string }{
		{"equals", "acme"}, {"equals", "ACME"}, {"not_equals", "acme"},
		{"contains", "ACME"}, {"contains", "%"}, {"contains", "_"}, {"contains", "a_b"}, {"contains", "0% d"},
		{"not_contains", "%"}, {"not_contains", "a"}, {"empty", ""}, {"not_empty", ""}, {"equals", ""},
	}
	for _, c := range cases {
		q, g := wbcrud.BuildRowQuery(e.wbq(), rqView(wbcrud.ViewFilter{Column: "name", Op: c.op, Value: c.val}), "")
		if g != nil {
			t.Fatal(g)
		}
		got := e.ids(t, q)
		if want := ref(c.op, c.val); !reflect.DeepEqual(got, want) {
			t.Errorf("%s %q: got %v want %v", c.op, c.val, got, want)
		}
	}
}

func TestSearchEscapesLikeWildcards(t *testing.T) {
	e := rqSetup(t)
	ids := e.seed(t, [][2]string{
		{`{"name":"100% real"}`, ""},
		{`{"name":"1000 real"}`, ""},
		{`{"name":"a_b"}`, `{"score":{"value":"Hot_Lead","status":"complete"}}`},
		{`{"name":"axb"}`, `{"score":{"value":"hotXlead","status":"complete"}}`},
	})
	for search, want := range map[string][]int64{
		"%":        {ids[0]},
		"0% r":     {ids[0]},
		"_":        {ids[2]},
		"HOT_lead": {ids[2]},
		"  A_B  ":  {ids[2]},
		"hotxlead": {ids[3]},
		"nomatch":  nil,
		"":         {ids[0], ids[1], ids[2], ids[3]},
	} {
		q, g := wbcrud.BuildRowQuery(e.wbq(), nil, search)
		if g != nil {
			t.Fatal(g)
		}
		if got := e.ids(t, q); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("search %q: got %v want %v", search, got, want)
		}
	}
}

// page walks the query with cursors the way the route does.
func (e *rqEnv) page(t *testing.T, base wbcrud.RowQuery, size int) []int64 {
	t.Helper()
	var all []int64
	cursor := ""
	for guard := 0; guard < 100; guard++ {
		q := base
		if cursor != "" {
			e.run(t, func(tx pgx.Tx) {
				vals, g, err := base.ResolveCursor(context.Background(), tx, cursor)
				if err != nil || g != nil {
					t.Fatalf("resolve: %v %v", err, g)
				}
				var g2 *wbcrud.GuardError
				q, g2 = base.After(vals)
				if g2 != nil {
					t.Fatal(g2)
				}
			})
		}
		q = q.WithLimit(size+1, 0)
		var batch []int64
		var positions []int
		e.run(t, func(tx pgx.Tx) {
			rows, err := tx.Query(context.Background(), q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%v\n%s", err, q.SQL)
			}
			defer rows.Close()
			for rows.Next() {
				vals, _ := rows.Values()
				batch = append(batch, int64(vals[0].(int32)))
				p := 0
				if v, ok := vals[2].(int32); ok {
					p = int(v)
				}
				positions = append(positions, p)
			}
		})
		hasMore := len(batch) > size
		if hasMore {
			batch, positions = batch[:size], positions[:size]
		}
		all = append(all, batch...)
		if !hasMore {
			return all
		}
		e.run(t, func(tx pgx.Tx) {
			var err error
			cursor, err = base.NextCursor(context.Background(), tx, batch[len(batch)-1], positions[len(positions)-1])
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func TestSortByColumnWithEmptiesIsStableAcrossPages(t *testing.T) {
	e := rqSetup(t)
	long := strings.Repeat("z", 700) // forces v2 (digest) cursors
	vals := []string{"b", "", "a", "b", "", "B", "a", "c", "", "b", "é", "e", "a", "", "c", "b", "A", "", "d", "b", long, long, "a", "", "c"}
	var rows [][2]string
	for i, v := range vals {
		if i%4 == 3 {
			rows = append(rows, [2]string{`{"other":1}`, ""}) // NULL value
			continue
		}
		b, _ := json.Marshal(map[string]any{"name": v})
		rows = append(rows, [2]string{string(b), ""})
	}
	e.seed(t, rows)
	if len(rows) != 25 {
		t.Fatalf("seed %d", len(rows))
	}
	for _, dir := range []string{"asc", "desc"} {
		view := &wbcrud.QueryView{ID: "v", Config: wbcrud.ViewConfig{Sort: []wbcrud.ViewSort{{Column: "name", Dir: dir}}}}
		base, g := wbcrud.BuildRowQuery(e.wbq(), view, "")
		if g != nil {
			t.Fatal(g)
		}
		want := e.ids(t, base)
		if len(want) != 25 {
			t.Fatalf("full set %d", len(want))
		}
		got := e.page(t, base, 3)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: paged != full\n got %v\nwant %v", dir, got, want)
		}
	}
	// no sort: position/id row cursors
	base, _ := wbcrud.BuildRowQuery(e.wbq(), nil, "")
	if got, want := e.page(t, base, 4), e.ids(t, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("row cursor paging: %v != %v", got, want)
	}
	// two sorts incl. enrichment column
	two := &wbcrud.QueryView{ID: "v", Config: wbcrud.ViewConfig{Sort: []wbcrud.ViewSort{{Column: "score", Dir: "desc"}, {Column: "mail", Dir: "asc"}}}}
	base, _ = wbcrud.BuildRowQuery(e.wbq(), two, "")
	if got, want := e.page(t, base, 7), e.ids(t, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("two-term paging: %v != %v", got, want)
	}
}

func TestCursorFromOtherQueryRejected(t *testing.T) {
	e := rqSetup(t)
	e.seed(t, [][2]string{{`{"name":"a"}`, ""}, {`{"name":"b"}`, ""}})
	long := strings.Repeat("q", 700)
	e.seed(t, [][2]string{{`{"name":"` + long + `"}`, ""}})
	asc, _ := wbcrud.BuildRowQuery(e.wbq(), &wbcrud.QueryView{ID: "v", Config: wbcrud.ViewConfig{Sort: []wbcrud.ViewSort{{Column: "name", Dir: "asc"}}}}, "")
	desc, _ := wbcrud.BuildRowQuery(e.wbq(), &wbcrud.QueryView{ID: "v", Config: wbcrud.ViewConfig{Sort: []wbcrud.ViewSort{{Column: "name", Dir: "desc"}}}}, "")
	var cur string
	e.run(t, func(tx pgx.Tx) {
		var id int64
		if err := tx.QueryRow(context.Background(), `SELECT id FROM workbook_rows WHERE workbook_id=$1 AND data->>'name' = $2`, e.wb, long).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var err error
		if cur, err = asc.NextCursor(context.Background(), tx, id, 2); err != nil {
			t.Fatal(err)
		}
		_, g, err := desc.ResolveCursor(context.Background(), tx, cur)
		if err != nil || g == nil || g.Status != 400 || g.Detail != "Invalid workbook row cursor; restart pagination" {
			t.Fatalf("cross-query cursor: %v %v", g, err)
		}
		if _, g, err := asc.ResolveCursor(context.Background(), tx, cur); err != nil || g != nil {
			t.Fatalf("own cursor: %v %v", g, err)
		}
	})
}

func TestViewOfOtherWorkbook404(t *testing.T) {
	e := rqSetup(t)
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO workbook_views (id,workspace_id,workbook_id,name,config) VALUES ($1,$2,$3,'v','{"sort":[{"column":"name","dir":"asc"}]}')`,
		"rq_view_"+e.other, e.ws, e.other); err != nil {
		t.Fatal(err)
	}
	e.run(t, func(tx pgx.Tx) {
		v, g, err := wbcrud.LoadView(context.Background(), tx, e.wb, "rq_view_"+e.other)
		if err != nil {
			t.Fatal(err)
		}
		if v != nil || g == nil || g.Status != 404 || g.Detail != "Workbook view not found" {
			t.Fatalf("other workbook's view: %v %v", v, g)
		}
		if v, g, err := wbcrud.LoadView(context.Background(), tx, e.wb, "nope"); err != nil || v != nil || g == nil || g.Status != 404 {
			t.Fatalf("missing view: %v %v", v, g)
		}
		v, g, err = wbcrud.LoadView(context.Background(), tx, e.other, "rq_view_"+e.other)
		if err != nil || g != nil || v == nil || len(v.Config.Sort) != 1 {
			t.Fatalf("own view: %v %v", v, g)
		}
	})
}

func TestLoadViewDBErrorIsNotA404(t *testing.T) {
	e := rqSetup(t)
	e.run(t, func(tx pgx.Tx) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		v, g, err := wbcrud.LoadView(ctx, tx, e.wb, "anything")
		if err == nil || g != nil || v != nil {
			t.Fatalf("cancelled ctx must surface as error, got view=%v guard=%v err=%v", v, g, err)
		}
	})
}

// Rows with NULL position (legacy data) sort as position 0 and are neither
// skipped nor duplicated across pages (Python drops them after page 1).
func TestNullPositionRowsPageWithoutSkips(t *testing.T) {
	e := rqSetup(t)
	for i := 0; i < 25; i++ {
		pos := any(i)
		if i%3 == 0 {
			pos = nil
		}
		if _, err := e.owner.Exec(context.Background(),
			`INSERT INTO workbook_rows (workbook_id, position, data, workspace_id) VALUES ($1,$2,$3::json,$4)`,
			e.wb, pos, fmt.Sprintf(`{"name":"n%d"}`, i%2), e.ws); err != nil {
			t.Fatal(err)
		}
	}
	plain, _ := wbcrud.BuildRowQuery(e.wbq(), nil, "")
	sorted, _ := wbcrud.BuildRowQuery(e.wbq(), &wbcrud.QueryView{ID: "v", Config: wbcrud.ViewConfig{Sort: []wbcrud.ViewSort{{Column: "name", Dir: "asc"}}}}, "")
	for name, base := range map[string]wbcrud.RowQuery{"row-cursor": plain, "query-cursor": sorted} {
		want := e.ids(t, base)
		got := e.page(t, base, 4)
		if len(want) != 25 || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: paged %v != full %v", name, got, want)
		}
	}
}
