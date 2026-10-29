//go:build integration

package leadsread_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// vectors mirrors testdata/leads_vectors.json (legacy/python/scripts/gen_leads_vectors.py).
type vectors struct {
	KeyOrder []string          `json:"key_order"`
	RowJSON  map[string]string `json:"row_json"`
	Seed     []map[string]any  `json:"seed"`
	Cases    []vectorCase      `json:"cases"`
	Stats    map[string]any    `json:"stats"`
	Options  map[string]any    `json:"filter_options"`
	Other    string            `json:"other_tenant_company"`
}

type vectorCase struct {
	Filter    json.RawMessage `json:"filter"`
	Page      int             `json:"page"`
	PageSize  int             `json:"page_size"`
	Total     int             `json:"total"`
	Companies []string        `json:"companies"`
}

type env struct {
	pool     *pgxpool.Pool
	v        vectors
	wsA, wsB string
	idByCo   map[string]int64
	seedByCo map[string]map[string]any
	otherCo  string
}

func setup(t *testing.T) *env {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/leads_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{idByCo: map[string]int64{}, seedByCo: map[string]map[string]any{}}
	if err := json.Unmarshal(raw, &e.v); err != nil {
		t.Fatal(err)
	}
	e.pool = testpg.NewAppPool(t, 4)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e.wsA = fmt.Sprintf("t4a_%d", os.Getpid())
	e.wsB = fmt.Sprintf("t4b_%d", os.Getpid())
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM leads WHERE workspace_id = ANY($1)`, []string{e.wsA, e.wsB})
	}
	clean()
	t.Cleanup(clean)

	insert := func(ws string, row map[string]any) int64 {
		cols := []string{"workspace_id"}
		args := []any{ws}
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cols = append(cols, k)
			val := row[k]
			if f, ok := val.(float64); ok {
				val = int64(f)
			}
			args = append(args, val)
		}
		ph := make([]string, len(cols))
		for i := range cols {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		var id int64
		err := owner.QueryRow(ctx, fmt.Sprintf(`INSERT INTO leads (%s) VALUES (%s) RETURNING id`,
			strings.Join(cols, ","), strings.Join(ph, ",")), args...).Scan(&id)
		if err != nil {
			t.Fatalf("seed %v: %v", row["company"], err)
		}
		return id
	}
	for _, row := range e.v.Seed {
		co := row["company"].(string)
		e.idByCo[co] = insert(e.wsA, row)
		e.seedByCo[co] = row
	}
	e.otherCo = e.v.Other
	insert(e.wsB, map[string]any{"company": e.otherCo, "city": "Pune", "score": 99, "source": "csv_import", "collection_job_id": "job1"})
	return e
}

func (e *env) in(t *testing.T, ws string, fn func(tx pgx.Tx)) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error { fn(tx); return nil }); err != nil {
		t.Fatal(err)
	}
}

func companies(rows []leadsread.Lead) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Company
	}
	return out
}

func (e *env) filterFor(t *testing.T, raw json.RawMessage) leadsread.Filter {
	t.Helper()
	var withNames struct {
		Names []string `json:"lead_ids_companies"`
	}
	_ = json.Unmarshal(raw, &withNames)
	var f leadsread.Filter
	if len(withNames.Names) > 0 {
		for _, n := range withNames.Names {
			f.LeadIDs = append(f.LeadIDs, e.idByCo[n])
		}
		return f
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestQueryLeadsMatchesPythonVectors(t *testing.T) {
	e := setup(t)
	if len(e.v.Cases) < 100 {
		t.Fatalf("too few vector cases: %d", len(e.v.Cases))
	}
	e.in(t, e.wsA, func(tx pgx.Tx) {
		for i, c := range e.v.Cases {
			f := e.filterFor(t, c.Filter)
			rows, total, err := leadsread.QueryLeads(context.Background(), tx, f, c.Page, c.PageSize)
			if err != nil {
				t.Fatalf("case %d %s: %v", i, c.Filter, err)
			}
			if total != c.Total {
				t.Errorf("case %d %s p%d/%d: total %d want %d", i, c.Filter, c.Page, c.PageSize, total, c.Total)
			}
			if got := companies(rows); !reflect.DeepEqual(got, c.Companies) && !(len(got) == 0 && len(c.Companies) == 0) {
				t.Errorf("case %d %s p%d/%d:\n got %v\nwant %v", i, c.Filter, c.Page, c.PageSize, got, c.Companies)
			}
			for _, r := range rows {
				co := r.Company
				if r.ID != e.idByCo[co] {
					t.Fatalf("id mismatch for %s: %v", co, r.ID)
				}
				if r.WorkspaceID != e.wsA {
					t.Fatalf("workspace_id %v", r.WorkspaceID)
				}
				if len(r.Values()) != len(e.v.KeyOrder) {
					t.Fatalf("row has %d keys want %d", len(r.Values()), len(e.v.KeyOrder))
				}
				for k, want := range e.seedByCo[co] {
					got, ok := r.Get(k)
					if !ok {
						t.Fatalf("Get(%q) unknown", k)
					}
					if w, ok := want.(float64); ok {
						if fmt.Sprint(got) != fmt.Sprint(int64(w)) {
							t.Fatalf("%s.%s = %v want %v", co, k, got, want)
						}
						continue
					}
					if got != want {
						t.Fatalf("%s.%s = %#v want %#v", co, k, got, want)
					}
				}
				// Byte-for-byte JSON parity with Python json.dumps (key order included).
				r.ID, r.WorkspaceID = 0, "WS"
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				if err := enc.Encode(r); err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSuffix(buf.String(), "\n"); got != e.v.RowJSON[co] {
					t.Fatalf("json for %s\n got %s\nwant %s", co, got, e.v.RowJSON[co])
				}
			}
		}
	})
}

func TestQueryLeadsOrderingStable(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsA, func(tx pgx.Tx) {
		var seen []string
		for p := 1; p <= 20; p++ {
			rows, total, err := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{}, p, 1)
			if err != nil {
				t.Fatal(err)
			}
			if total != len(e.v.Seed) {
				t.Fatalf("total %d", total)
			}
			seen = append(seen, companies(rows)...)
		}
		if len(seen) != len(e.v.Seed) {
			t.Fatalf("paged %d rows want %d: %v", len(seen), len(e.v.Seed), seen)
		}
		uniq := map[string]bool{}
		for _, s := range seen {
			uniq[s] = true
		}
		if len(uniq) != len(seen) {
			t.Fatalf("duplicate rows across pages: %v", seen)
		}
		// Tied scores: id DESC.
		rows, _, _ := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{City: "Tie"}, 1, 10)
		want := []string{"Theta Tie 4", "Theta Tie 3", "Theta Tie 2", "Theta Tie 1"}
		if !reflect.DeepEqual(companies(rows), want) {
			t.Fatalf("tie order %v", companies(rows))
		}
	})
}

func TestJobIDsFilter(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsA, func(tx pgx.Tx) {
		rows, total, err := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{JobIDs: []string{"job2"}}, 1, 50)
		if err != nil {
			t.Fatal(err)
		}
		got := companies(rows)
		sort.Strings(got)
		// Delta HR via collection_job_id, Gamma Talent via source "job:job2".
		if total != 2 || !reflect.DeepEqual(got, []string{"Delta HR", "Gamma Talent"}) {
			t.Fatalf("total=%d rows=%v", total, got)
		}
		_, total, _ = leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{JobIDs: []string{}}, 1, 50)
		if total != len(e.v.Seed) {
			t.Fatalf("empty job_ids must not filter, total=%d", total)
		}
	})
}

func TestLeadsByIDs(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsA, func(tx pgx.Tx) {
		rows, err := leadsread.LeadsByIDs(context.Background(), tx, []int64{e.idByCo["Zeta Works"], e.idByCo["Acme Staffing"], 1 << 40})
		if err != nil {
			t.Fatal(err)
		}
		if got := companies(rows); len(got) != 2 {
			t.Fatalf("rows %v", got)
		}
		if rows[0].ID == 0 || len(rows[0].Values()) != len(e.v.KeyOrder) {
			t.Fatalf("row shape %v", rows[0])
		}
	})
}

func TestStatsMatchPython(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsA, func(tx pgx.Tx) {
		st, err := leadsread.Stats(context.Background(), tx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(st)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if !reflect.DeepEqual(got, e.v.Stats) {
			t.Fatalf("stats\n got %v\nwant %v", got, e.v.Stats)
		}
		if !strings.HasPrefix(string(b), `{"total":`) || strings.Index(string(b), `"by_status"`) > strings.Index(string(b), `"by_tier"`) {
			t.Fatalf("key order: %s", b)
		}
		for i := 1; i < len(st.ByCity); i++ {
			if st.ByCity[i].N > st.ByCity[i-1].N {
				t.Fatalf("by_city not ordered by count desc: %v", st.ByCity)
			}
		}
	})
}

func TestFilterOptionsMatchPython(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsA, func(tx pgx.Tx) {
		opts, err := leadsread.FilterOptions(context.Background(), tx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(opts)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if !reflect.DeepEqual(got, e.v.Options) {
			t.Fatalf("options\n got %v\nwant %v", got, e.v.Options)
		}
	})
}

func TestLeadsTenantIsolation(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsB, func(tx pgx.Tx) {
		rows, total, err := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{}, 1, 100)
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(rows) != 1 || rows[0].Company != e.otherCo {
			t.Fatalf("ws B sees %d rows: %v", total, companies(rows))
		}
		byID, err := leadsread.LeadsByIDs(context.Background(), tx, []int64{e.idByCo["Acme Staffing"]})
		if err != nil || len(byID) != 0 {
			t.Fatalf("ws B read ws A lead by id: %v %v", byID, err)
		}
		st, _ := leadsread.Stats(context.Background(), tx)
		if st.Total != 1 {
			t.Fatalf("stats total %d", st.Total)
		}
		opts, _ := leadsread.FilterOptions(context.Background(), tx)
		if opts["total_leads"] != int64(1) {
			t.Fatalf("options %v", opts)
		}
	})
	e.in(t, "t4-empty-"+e.wsA, func(tx pgx.Tx) {
		_, total, _ := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{}, 1, 100)
		if total != 0 {
			t.Fatalf("empty workspace sees %d", total)
		}
	})
}

func TestPageSizeBounds(t *testing.T) {
	e := setup(t)
	e.in(t, e.wsA, func(tx pgx.Tx) {
		for _, c := range []struct{ page, size, want int }{{0, 0, 1}, {-3, -1, 1}, {1, 1000, len(e.v.Seed)}, {2, 5, 5}} {
			rows, total, err := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{}, c.page, c.size)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != c.want || total != len(e.v.Seed) {
				t.Errorf("page=%d size=%d: rows=%d total=%d want rows=%d", c.page, c.size, len(rows), total, c.want)
			}
		}
		rows, _, err := leadsread.QueryLeads(context.Background(), tx, leadsread.Filter{}, 1<<62, 1<<62)
		if err != nil || len(rows) != 0 {
			t.Errorf("huge page: %v rows=%d", err, len(rows))
		}
	})
}
