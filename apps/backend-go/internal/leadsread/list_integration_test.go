//go:build integration

package leadsread_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type listEnv struct {
	pool     *pgxpool.Pool
	wsA, wsB string
	id       map[string]int64
}

type seedRow struct {
	co, city, spec, status, email, phone, source, job, tier, size, notes string
	score                                                                *int
}

func ip(n int) *int { return &n }

func listSetup(t *testing.T) *listEnv {
	t.Helper()
	e := &listEnv{pool: testpg.NewAppPool(t, 4), id: map[string]int64{}}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e.wsA = fmt.Sprintf("t4la_%d", os.Getpid())
	e.wsB = fmt.Sprintf("t4lb_%d", os.Getpid())
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM leads WHERE workspace_id = ANY($1)`, []string{e.wsA, e.wsB})
	}
	clean()
	t.Cleanup(clean)
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	rows := []seedRow{
		{co: "Alpha", city: "Pune", spec: "plumbing pipes", status: "new", email: "a@x.io", phone: "1", source: "csv", tier: "hot", size: "11-50", score: ip(50), notes: "vip"},
		{co: "Bravo", city: "Pune", spec: "plumbing pipes", status: "dead", email: "", phone: "2", source: "job:j9", tier: "warm", size: "1-10", score: ip(50)},
		{co: "Charlie", city: "Pune", spec: "roofing", status: "new", email: "c@x.io", source: "csv", job: "j9", tier: "hot", score: ip(50)},
		{co: "Delta", city: "Goa", spec: "plumbing pipes", status: "contacted", source: "csv", tier: "cold", score: ip(10)},
		{co: "Echo", city: "Goa", spec: "", status: "new", email: "e@x.io", source: "web", tier: "hot", score: ip(50)},
		{co: "Foxtrot", city: "Pune", spec: "", status: "new", source: "web", score: ip(50)},
		{co: "Golf", city: "Delhi", spec: "", status: "", source: "web", score: nil},
	}
	for _, r := range rows {
		var id int64
		err := owner.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, specialization, status, email, phone, source, collection_job_id, score_tier, company_size, notes, score)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
			e.wsA, r.co, nul(r.city), nul(r.spec), nul(r.status), nul(r.email), nul(r.phone), nul(r.source), nul(r.job), nul(r.tier), nul(r.size), nul(r.notes), r.score).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		e.id[r.co] = id
	}
	if _, err := owner.Exec(ctx, `INSERT INTO leads (workspace_id, company, city, specialization, score) VALUES ($1,'Zulu','Pune','plumbing pipes',99)`, e.wsB); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *listEnv) list(t *testing.T, ws string, p leadsread.ListParams) []string {
	t.Helper()
	var out []string
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		rows, err := leadsread.ListLeads(context.Background(), tx, p)
		out = companies(rows)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListLeadsFilters(t *testing.T) {
	e := listSetup(t)
	tr, fl := true, false
	cases := []struct {
		name string
		p    leadsread.ListParams
		want []string
	}{
		{"status", leadsread.ListParams{Status: "new", OrderBy: "company ASC"}, []string{"Alpha", "Charlie", "Echo", "Foxtrot"}},
		{"city", leadsread.ListParams{City: "Goa", OrderBy: "company ASC"}, []string{"Delta", "Echo"}},
		{"source", leadsread.ListParams{Source: "web", OrderBy: "company ASC"}, []string{"Echo", "Foxtrot", "Golf"}},
		{"job matches column and source", leadsread.ListParams{CollectionJobID: "j9", OrderBy: "company ASC"}, []string{"Bravo", "Charlie"}},
		{"score min", leadsread.ListParams{ScoreMin: ip(50), OrderBy: "company ASC"}, []string{"Alpha", "Bravo", "Charlie", "Echo", "Foxtrot"}},
		{"score max", leadsread.ListParams{ScoreMax: ip(10), OrderBy: "company ASC"}, []string{"Delta"}},
		{"tier", leadsread.ListParams{ScoreTier: "hot", OrderBy: "company ASC"}, []string{"Alpha", "Charlie", "Echo"}},
		{"size", leadsread.ListParams{CompanySize: "1-10"}, []string{"Bravo"}},
		{"search ilike company", leadsread.ListParams{Search: "alph"}, []string{"Alpha"}},
		{"search notes", leadsread.ListParams{Search: "vip"}, []string{"Alpha"}},
		{"search tsquery", leadsread.ListParams{Search: "roofing"}, []string{"Charlie"}},
		{"has_email true", leadsread.ListParams{HasEmail: &tr, OrderBy: "company ASC"}, []string{"Alpha", "Charlie", "Echo"}},
		{"has_phone false", leadsread.ListParams{HasPhone: &fl, OrderBy: "company ASC"}, []string{"Charlie", "Echo", "Foxtrot", "Golf"}},
		{"combined", leadsread.ListParams{City: "Pune", Status: "new", ScoreTier: "hot", OrderBy: "company ASC"}, []string{"Alpha", "Charlie"}},
		{"limit offset", leadsread.ListParams{OrderBy: "company ASC", Limit: 2, Offset: 1}, []string{"Bravo", "Charlie"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.p.Limit == 0 {
				c.p.Limit = 500
			}
			if got := e.list(t, e.wsA, c.p); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestListLeadsOrderAllowList(t *testing.T) {
	e := listSetup(t)
	def := e.list(t, e.wsA, leadsread.ListParams{Limit: 500})
	// score DESC, id DESC: NULL score first (Postgres DESC), then 50s by id desc, then 10.
	want := []string{"Golf", "Foxtrot", "Echo", "Charlie", "Bravo", "Alpha", "Delta"}
	if !reflect.DeepEqual(def, want) {
		t.Fatalf("default order %v want %v", def, want)
	}
	for _, bad := range []string{"bogus", "score; DROP TABLE leads", "company DESC, id ASC", "id ASC", ""} {
		if got := e.list(t, e.wsA, leadsread.ListParams{Limit: 500, OrderBy: bad}); !reflect.DeepEqual(got, want) {
			t.Fatalf("order %q fell back to %v", bad, got)
		}
	}
	if got := e.list(t, e.wsA, leadsread.ListParams{Limit: 3, OrderBy: "company DESC"}); !reflect.DeepEqual(got, []string{"Golf", "Foxtrot", "Echo"}) {
		t.Fatalf("company DESC: %v", got)
	}
	if got := e.list(t, e.wsA, leadsread.ListParams{Limit: 3, OrderBy: "score ASC"}); !reflect.DeepEqual(got, []string{"Delta", "Alpha", "Bravo"}) {
		t.Fatalf("score ASC (id ASC tiebreak): %v", got)
	}
	var tableOK bool
	_ = tenancy.InWorkspace(context.Background(), e.pool, e.wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) > 0 FROM leads`).Scan(&tableOK)
	})
	if !tableOK {
		t.Fatal("leads table damaged")
	}
}

func TestListLeadsTieOrderStable(t *testing.T) {
	e := listSetup(t)
	seen := map[string]bool{}
	var all []string
	for off := 0; off < 9; off += 3 {
		for _, c := range e.list(t, e.wsA, leadsread.ListParams{Limit: 3, Offset: off, OrderBy: "score DESC"}) {
			if seen[c] {
				t.Fatalf("%s repeated across pages", c)
			}
			seen[c] = true
			all = append(all, c)
		}
	}
	if len(all) != 7 {
		t.Fatalf("pages covered %v", all)
	}
}

func TestExcludeDeadDefault(t *testing.T) {
	e := listSetup(t)
	if got := e.list(t, e.wsA, leadsread.ListParams{Limit: 500, OrderBy: "company ASC"}); !contains(got, "Bravo") {
		t.Fatalf("dead lead must be listed by default: %v", got)
	}
	got := e.list(t, e.wsA, leadsread.ListParams{Limit: 500, OrderBy: "company ASC", ExcludeDead: true})
	if contains(got, "Bravo") || !contains(got, "Golf") || len(got) != 6 {
		t.Fatalf("exclude_dead: %v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestHasEmailEmptyStringCountsAsMissing(t *testing.T) {
	e := listSetup(t)
	fl := false
	got := e.list(t, e.wsA, leadsread.ListParams{Limit: 500, HasEmail: &fl, OrderBy: "company ASC"})
	if !contains(got, "Bravo") || !contains(got, "Delta") || contains(got, "Alpha") {
		t.Fatalf("has_email=false: %v", got)
	}
}

func (e *listEnv) similar(t *testing.T, ws, company string, limit int) []string {
	t.Helper()
	var out []string
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		ref, err := leadsread.LeadsByIDs(context.Background(), tx, []int64{e.id[company]})
		if err != nil || len(ref) != 1 {
			return fmt.Errorf("ref %v %v", ref, err)
		}
		rows, err := leadsread.SimilarLeads(context.Background(), tx, ref[0], limit)
		out = companies(rows)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSimilarExcludesSelf(t *testing.T) {
	e := listSetup(t)
	got := e.similar(t, e.wsA, "Alpha", 10)
	if contains(got, "Alpha") || !reflect.DeepEqual(got, []string{"Delta", "Bravo"}) {
		t.Fatalf("got %v", got)
	}
	// limit+1 fetched then reference dropped then cut to limit.
	if got := e.similar(t, e.wsA, "Alpha", 1); !reflect.DeepEqual(got, []string{"Delta"}) {
		t.Fatalf("limit 1: %v", got)
	}
}

func TestSimilarFallsBackToCity(t *testing.T) {
	e := listSetup(t)
	got := e.similar(t, e.wsA, "Echo", 10) // no specialization, city Goa
	if !reflect.DeepEqual(got, []string{"Delta"}) {
		t.Fatalf("got %v", got)
	}
	// Specialization search that only finds the reference falls back to city.
	got = e.similar(t, e.wsA, "Charlie", 10)
	if !reflect.DeepEqual(got, []string{"Foxtrot", "Bravo", "Alpha"}) {
		t.Fatalf("roofing only self -> city Pune: %v", got)
	}
}

func TestListTenantIsolation(t *testing.T) {
	e := listSetup(t)
	if got := e.list(t, e.wsA, leadsread.ListParams{Limit: 500, Search: "plumbing"}); contains(got, "Zulu") {
		t.Fatalf("leak: %v", got)
	}
	if got := e.list(t, e.wsB, leadsread.ListParams{Limit: 500}); !reflect.DeepEqual(got, []string{"Zulu"}) {
		t.Fatalf("ws B: %v", got)
	}
	if got := e.similar(t, e.wsA, "Alpha", 50); contains(got, "Zulu") {
		t.Fatalf("similar leak: %v", got)
	}
}
