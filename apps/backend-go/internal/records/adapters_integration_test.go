//go:build integration

package records

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcewb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkbookExplicitDeltaAndEmploymentContinuity(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Old", "domain": "old.test"}}, "import:clay", "company")
	p := mustRecord(t, s, ws, Person, Write{CompanyID: c.ID, Person: map[string]any{"email": "person@old.test", "full_name": "Person"}}, "import:clay", "person")
	wb, err := s.Workbook(ctx, ws, Person, []string{p.ID}, "Edit shared facts")
	if err != nil {
		t.Fatal(err)
	}
	var row int64
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE workbook_rows SET data='{"company":"Old","website":"https://old.test"}' WHERE workbook_id=$1 RETURNING id`, wb).Scan(&row)
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Get(ctx, ws, Company, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Patch(ctx, ws, Company, c.ID, c.Version, Write{Company: map[string]any{"name": "New"}})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(key string, value any) {
		t.Helper()
		err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
			svc := wbcrud.Service{}
			w, e := svc.Owned(ctx, tx, ws, wb, false)
			if e != nil {
				return e
			}
			body := wbcrud.NewObj()
			body.Set(key, value)
			_, g, e := svc.PatchRow(ctx, tx, ws, w, row, body, false)
			if g != nil {
				return g
			}
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	patch("company", "Old")
	c, err = s.Get(ctx, ws, Company, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Fields["name"] != "Old" {
		t.Fatalf("explicit revert was lost: %+v", c.Fields)
	}
	next := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Next", "domain": "next.test"}}, "manual", "")
	p, err = s.Patch(ctx, ws, Person, p.ID, p.Version, Write{CompanyID: next.ID, Employment: map[string]any{"title": "Director"}})
	if err != nil {
		t.Fatal(err)
	}
	patch("notes", "Edited after changing jobs")
	p, err = s.Get(ctx, ws, Person, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.CompanyID == nil || *p.CompanyID != next.ID {
		t.Fatal("workbook edit restored the old employer")
	}
	// Legacy alias writes must follow the current person too.
	var lead int
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO leads(workspace_id,company,city,website,contact_person,email) VALUES($1,'Old','','https://old.test','Person','person@old.test') RETURNING id`, ws).Scan(&lead)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE leads SET notes='Legacy note' WHERE id=$1`, lead)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Get(ctx, ws, Person, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.CompanyID == nil || *p.CompanyID != next.ID {
		t.Fatal("legacy edit restored the old employer")
	}
}

type discoveredPeople struct{}

func (discoveredPeople) FindPeopleByTitles(context.Context, sourcewb.PeopleQuery) (sourcewb.PeopleResult, error) {
	return sourcewb.PeopleResult{People: []sourcewb.Person{{Name: "Discovered", Title: "Director", LinkedIn: "https://linkedin.com/in/records-discovered"}}, SearchesUsed: 1}, nil
}
func TestCompanyDiscoveryCreatesSharedPeopleAndReplays(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Acme", "domain": "acme.test"}}, "manual", "")
	wb, e := s.Discover(ctx, ws, Discovery{IDs: []string{c.ID}, Titles: []string{"Director"}, MaxPerCompany: 2, MaxSearches: 2})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		var result *wbcrud.Obj
		result, e = sourcewb.Materialize(ctx, sourcewb.Deps{Pool: s.Pool, People: discoveredPeople{}, PeopleSearchEnabled: true}, ws, wb, "people_source")
		if e != nil {
			t.Fatal(e)
		}
		if v, ok := result.Get("error"); ok {
			t.Fatalf("discovery failed: %v", v)
		}
	}
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM person_entities WHERE workspace_id=$1 AND company_entity_id=$2`, ws, c.ID).Scan(&n)
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("expected one canonical person, got %d", n)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}

func TestTenThousandRowImportPreviewCommitAndReplay(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	cfg := s.Pool.Config()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, e := c.Exec(ctx, `SET ROLE opengtm_records_test_app`)
		return e
	}
	runtime, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(runtime.Close)
	s.Pool = runtime
	var b strings.Builder
	b.WriteString("Row ID,Company Name,Company Domain,Custom JSON\n")
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&b, "row-%d,Company %d,company-%d.test,\"{\"\"number\"\":9007199254740993}\"\n", i, i, i)
	}
	req := ImportRequest{Kind: Company, Source: "clay:bulk", IdempotencyKey: "10k", CSV: b.String()}
	for _, preview := range []bool{true, false, false} {
		start := time.Now()
		r, e := s.Import(ctx, ws, req, preview)
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("preview=%v replay=%v: %v", preview, r.Replayed, time.Since(start))
		if r.Imported != 10000 || r.Rejected != 0 {
			t.Fatalf("import %+v", r)
		}
	}
	var count int
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM company_entities WHERE workspace_id=$1`, ws).Scan(&count)
	})
	if e != nil || count != 10000 {
		t.Fatalf("count %d: %v", count, e)
	}
}

func TestRuntimeRoleCannotReadOrWriteAnotherWorkspace(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Private", "domain": "private.test"}}, "manual", "")
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SET LOCAL ROLE opengtm_records_test_app`); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('app.workspace_id','other-tenant',true)`); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM company_entities WHERE id=$1`, c.ID).Scan(&n); e != nil || n != 0 {
		t.Fatalf("RLS read count %d: %v", n, e)
	}
	if _, e = tx.Exec(ctx, `SELECT opengtm_record_accept($1,'company',$2,'{"name":"Attack"}','manual',true)`, ws, c.ID); e == nil {
		t.Fatal("cross-workspace write accepted by runtime role")
	}
}

func TestCompanyCSVExportReimportsWithoutMappingChanges(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Roundtrip", "domain": "roundtrip.test", "custom": map[string]any{"values": []any{"a", "b"}}}}, "import:clay", "company")
	var csv strings.Builder
	if e := s.ExportCSV(ctx, ws, Company, &csv); e != nil {
		t.Fatal(e)
	}
	out, e := s.Import(ctx, ws, ImportRequest{Kind: Company, Source: "clay:roundtrip", IdempotencyKey: "csv", CSV: csv.String()}, false)
	if e != nil || out.Imported != 1 || out.Rows[0].ID != c.ID {
		t.Fatalf("roundtrip %+v: %v", out, e)
	}
}

func TestPersonRoleWithoutCompanyAndRoleOnlyPatch(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	p := mustRecord(t, s, ws, Person, Write{Person: map[string]any{"email": "independent@person.test", "full_name": "Independent"}, Employment: map[string]any{"title": "Engineer"}}, "import:clay", "independent")
	if p.CompanyID != nil || len(p.Employments) != 1 || p.Employments[0].Fields["title"] != "Engineer" {
		t.Fatalf("unlinked role lost: %+v", p)
	}
	var err error
	p, err = s.Patch(ctx, ws, Person, p.ID, p.Version, Write{Employment: map[string]any{"title": "Director"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Employments[0].Fields["title"] != "Director" {
		t.Fatal("role-only edit ignored")
	}
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Employer", "domain": "employer.test"}}, "manual", "")
	p, err = s.Patch(ctx, ws, Person, p.ID, p.Version, Write{CompanyID: c.ID, Employment: map[string]any{"title": "VP"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Patch(ctx, ws, Person, p.ID, p.Version, Write{Employment: map[string]any{"title": "Chief"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.CompanyID == nil || *p.CompanyID != c.ID || len(p.Employments) != 2 || p.Employments[0].Fields["title"] != "Chief" {
		t.Fatalf("role-only edit changed history: %+v", p)
	}
}

func TestEnrichmentWritebackUsesCurrentCompanyAndEmployment(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	old := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Old", "domain": "before.test"}}, "manual", "")
	next := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Next", "domain": "after.test"}}, "manual", "")
	p := mustRecord(t, s, ws, Person, Write{CompanyID: old.ID, Person: map[string]any{"email": "moves@person.test"}}, "manual", "")
	e := s.SaveWorkflow(ctx, ws, Person, Workflow{Name: "Current facts", Columns: []map[string]any{{"id": "description", "name": "Description", "type": "enrichment", "target_field": "description", "provider": "test"}, {"id": "department", "name": "Department", "type": "enrichment", "target_field": "department", "provider": "test"}}})
	if e != nil {
		t.Fatal(e)
	}
	wb, e := s.Workbook(ctx, ws, Person, []string{p.ID}, "Current facts")
	if e != nil {
		t.Fatal(e)
	}
	p, e = s.Patch(ctx, ws, Person, p.ID, p.Version, Write{CompanyID: next.ID})
	if e != nil {
		t.Fatal(e)
	}
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbook_enrichments(workspace_id,workbook_id,lead_id,column_id,value,status,provider) SELECT $1::text,$2::text,id,c.col,c.value,'complete','test' FROM workbook_rows CROSS JOIN (VALUES('description','Current employer description'),('department','Engineering')) c(col,value) WHERE workbook_id=$2::text`, ws, wb)
		return err
	})
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.Get(ctx, ws, Company, next.ID)
	if e != nil || c.Fields["description"] != "Current employer description" {
		t.Fatalf("current company writeback: %+v %v", c, e)
	}
	c, e = s.Get(ctx, ws, Company, old.ID)
	if e != nil || c.Fields["description"] != nil {
		t.Fatalf("former employer was modified: %+v %v", c, e)
	}
	p, e = s.Get(ctx, ws, Person, p.ID)
	if e != nil || p.Employments[0].Fields["department"] != "Engineering" {
		t.Fatalf("current employment writeback: %+v %v", p, e)
	}
}
