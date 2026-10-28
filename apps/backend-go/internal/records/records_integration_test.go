//go:build integration

package records

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func recordEnv(t *testing.T) (Store, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	name := fmt.Sprintf("records_%d", time.Now().UnixNano())
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE template0 ENCODING 'UTF8'"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)") })
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + name
	db, e := sql.Open("pgx", u.String())
	if e != nil {
		t.Fatal(e)
	}
	_, e = migrate.Run(ctx, db, migrate.Options{AppRole: "opengtm_records_test_app"})
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	p, e := pgxpool.New(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.Close)
	if _, e = p.Exec(ctx, `INSERT INTO workspaces(id,name,slug) VALUES('records-test','Records test','records-test'),('other-tenant','Other','other-tenant')`); e != nil {
		t.Fatal(e)
	}
	return Store{Pool: p}, "records-test"
}
func mustRecord(t *testing.T, s Store, ws string, k Kind, w Write, src, key string) Record {
	t.Helper()
	r, e := s.Upsert(context.Background(), ws, k, w, src, key)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestSharedRecordsImportEmploymentAndWorkbook(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Acme", "domain": "https://www.acme.test/path", "industry": "Software"}}, "manual", "")
	ana := mustRecord(t, s, ws, Person, Write{CompanyID: c.ID, Person: map[string]any{"full_name": "Ana", "email": "ana@acme.test", "linkedin_url": "linkedin.com/in/ana"}, Employment: map[string]any{"title": "Engineer"}}, "import:clay:people", "ana")
	bo := mustRecord(t, s, ws, Person, Write{CompanyID: c.ID, Person: map[string]any{"full_name": "Bo", "email": "bo@acme.test"}}, "import:clay:people", "bo")
	if ana.ID == bo.ID || ana.CompanyID == nil || *ana.CompanyID != c.ID {
		t.Fatal("people were not independently linked")
	}
	wb, e := s.Workbook(ctx, ws, Person, []string{ana.ID, bo.ID}, "Contacts")
	if e != nil {
		t.Fatal(e)
	}
	c, e = s.Patch(ctx, ws, Company, c.ID, c.Version, Write{Company: map[string]any{"industry": "Robotics"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Patch(ctx, ws, Company, c.ID, c.Version-1, Write{Company: map[string]any{"industry": "stale"}}); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale edit: %v", e)
	}
	mustRecord(t, s, ws, Company, Write{CompanyID: c.ID, Company: map[string]any{"industry": "Old value"}}, "import:clay:companies", "acme")
	var industries []string
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT data->>'industry' FROM record_workbook_rows WHERE workbook_id=$1 ORDER BY id`, wb)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if e = rows.Scan(&v); e != nil {
				return e
			}
			industries = append(industries, v)
		}
		return rows.Err()
	})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Join(industries, ",") != "Robotics,Robotics" {
		t.Fatalf("shared company facts %v", industries)
	}
	newCompany := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Next", "domain": "next.test"}}, "manual", "")
	changed, e := s.Patch(ctx, ws, Person, ana.ID, ana.Version, Write{CompanyID: newCompany.ID, Employment: map[string]any{"title": "Director"}})
	if e != nil {
		t.Fatal(e)
	}
	if changed.ID != ana.ID || changed.CompanyID == nil || *changed.CompanyID != newCompany.ID || len(changed.Employments) != 2 {
		t.Fatalf("employment continuity: %+v", changed)
	}
	protected := mustRecord(t, s, ws, Person, Write{CompanyID: c.ID, Person: map[string]any{"email": "ana@acme.test"}}, "import:clay:old-table", "ana-old")
	if *protected.CompanyID != newCompany.ID {
		t.Fatal("stale imported relationship replaced manual employer")
	}
	if _, e = s.Get(ctx, "other-tenant", Person, ana.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-workspace read: %v", e)
	}
	if _, e = s.Upsert(ctx, "other-tenant", Person, Write{CompanyID: c.ID, Person: map[string]any{"email": "x@other.test"}}, "manual", ""); e == nil {
		t.Fatal("cross-workspace link accepted")
	}
	if _, e = s.Upsert(ctx, ws, Person, Write{Person: map[string]any{"email": "ana@acme.test", "linkedin_url": "linkedin.com/in/someone-else"}}, "manual", ""); e == nil {
		t.Fatal("conflicting profile merged")
	}

	req := ImportRequest{Kind: Person, Source: "clay:incoming", IdempotencyKey: "batch1", CSV: "Row ID,Full Name,Work Email,Company Domain,Role,Custom JSON\nrow-1,Cal,cal@acme.test,acme.test,Founder,\"{\"\"tags\"\":[\"\"a\"\",\"\"b\"\"]}\"\nrow-2,Broken,bad-email,acme.test,Role,\n", Mapping: map[string]string{"Role": "employment.title"}}
	preview, e := s.Import(ctx, ws, req, true)
	if e != nil {
		t.Fatal(e)
	}
	if preview.Imported != 1 || preview.Rejected != 1 {
		t.Fatalf("preview: %+v", preview)
	}
	before, e := s.List(ctx, ws, Person, "", 100, 0)
	if e != nil {
		t.Fatal(e)
	}
	if before.Total != 2 {
		t.Fatal("preview mutated database")
	}
	imported, e := s.Import(ctx, ws, req, false)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Import(ctx, ws, req, false)
	if e != nil {
		t.Fatal(e)
	}
	if !again.Replayed || imported.Rows[0].ID != again.Rows[0].ID || imported.Imported != 1 {
		t.Fatal("import replay not stable")
	}
	req.CSV += "\n"
	req.Mapping["Role"] = "person.Role"
	if _, e = s.Import(ctx, ws, req, false); !errors.Is(e, ErrConflict) {
		t.Fatalf("reused key: %v", e)
	}
	var exported strings.Builder
	if e = s.ExportCSV(ctx, ws, Person, &exported); e != nil {
		t.Fatal(e)
	}
	csvRows, e := csv.NewReader(strings.NewReader(exported.String())).ReadAll()
	if e != nil || len(csvRows) != 4 {
		t.Fatalf("CSV: %v, %v", csvRows, e)
	}
}

func TestConcurrentImportIdentityAndLegacyAdapters(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Upsert(ctx, ws, Company, Write{Company: map[string]any{"name": "Shared", "domain": "shared.test"}}, "import:concurrent", "")
			if e != nil {
				errs <- e
			} else {
				ids <- r.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if first != id {
			t.Fatal("concurrent import duplicated company")
		}
	}
	e := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		for _, name := range []string{"Ana", "Bo"} {
			_, e := tx.Exec(ctx, `INSERT INTO leads(workspace_id,company,city,website,contact_person,email) VALUES($1,'Shared','Austin','https://shared.test',$2,$3)`, ws, name, strings.ToLower(name)+"@shared.test")
			if e != nil {
				return e
			}
		}
		var companies, people int
		if e := tx.QueryRow(ctx, `SELECT count(DISTINCT company_entity_id),count(DISTINCT person_entity_id) FROM leads WHERE workspace_id=$1`, ws).Scan(&companies, &people); e != nil {
			return e
		}
		if companies != 1 || people != 2 {
			return fmt.Errorf("legacy ownership: %d companies, %d people", companies, people)
		}
		_, e := tx.Exec(ctx, `UPDATE leads SET description='Shared fact' WHERE workspace_id=$1 AND contact_person='Ana'`, ws)
		if e != nil {
			return e
		}
		var n int
		e = tx.QueryRow(ctx, `SELECT count(*) FROM record_leads WHERE workspace_id=$1 AND description='Shared fact'`, ws).Scan(&n)
		if e == nil && n != 2 {
			return fmt.Errorf("legacy shared edit did not propagate: %d", n)
		}
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
}
