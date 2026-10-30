//go:build integration

package records

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

func TestLegacySnapshotsAndCanonicalFactsKeepTheirOwnContracts(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Original", "domain": "original.test"}}, "manual", "")
	var lead int64
	original := `{"z":1,"company":"Original","a":2}`
	const overlay = `{"result":{"value":"Observed","status":"done"}}`
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO leads(workspace_id,company,city,company_entity_id) VALUES($1,'Original','',$2) RETURNING id`, ws, c.ID).Scan(&lead); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workbooks(id,workspace_id,name) VALUES('legacy-view',$1,'Legacy snapshot')`, ws); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workbook_rows(workbook_id,workspace_id,data,enrichments,company_entity_id) VALUES('legacy-view',$1,$2::json,$3::json,$4)`, ws, original, overlay, c.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Get(ctx, ws, Company, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Patch(ctx, ws, Company, c.ID, c.Version, Write{Company: map[string]any{"name": "Accepted name"}})
	if err != nil {
		t.Fatal(err)
	}
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		leads, err := leadsread.LeadsByIDs(ctx, tx, []int64{lead})
		if err != nil {
			return err
		}
		if len(leads) != 1 || leads[0].Company != "Original" {
			t.Fatalf("legacy observation changed: %+v", leads)
		}
		var data, cells, accepted string
		if err := tx.QueryRow(ctx, `SELECT data::text,enrichments::text FROM record_workbook_rows WHERE workbook_id='legacy-view'`).Scan(&data, &cells); err != nil {
			return err
		}
		if data != original || cells != overlay {
			t.Fatalf("legacy snapshot changed: %s / %s", data, cells)
		}
		if err := tx.QueryRow(ctx, `SELECT company FROM record_leads WHERE id=$1`, lead).Scan(&accepted); err != nil {
			return err
		}
		if accepted != "Accepted name" {
			t.Fatalf("canonical facts are stale: %s", accepted)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRoleMailboxDoesNotMergeDifferentPeople(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	var ids []string
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		for _, name := range []string{"Alice", "Bob"} {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO leads(workspace_id,company,city,contact_person,email,linkedin_url) VALUES($1,'Shared company','',$2::text,'sales@shared.test','https://linkedin.com/in/'||lower($2::text)) RETURNING person_entity_id`, ws, name).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] == ids[1] {
		t.Fatal("a company role mailbox merged independent people")
	}
	for _, id := range ids {
		person, err := s.Get(ctx, ws, Person, id)
		if err != nil {
			t.Fatal(err)
		}
		if person.Fields["email"] != nil {
			t.Fatalf("company mailbox became a personal email: %+v", person.Fields)
		}
	}
}

func TestLegacyInvalidObservationsDoNotBecomeAcceptedFacts(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	var personID string
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO leads(workspace_id,company,city,contact_person,email,score,employee_count_exact) VALUES($1,'Legacy observations','','Person','not an email',150,-7) RETURNING person_entity_id`, ws).Scan(&personID)
	})
	if err != nil {
		t.Fatal(err)
	}
	person, err := s.Get(ctx, ws, Person, personID)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"email", "score", "employee_count_exact"} {
		if person.Fields[field] != nil {
			t.Fatalf("invalid legacy %s became accepted: %+v", field, person.Fields)
		}
	}
}

func TestSocialWebsiteDoesNotCollapseIndependentCompanies(t *testing.T) {
	s, ws := recordEnv(t)
	for _, host := range []string{"facebook.com", "m.facebook.com", "yelp.com"} {
		t.Run(host, func(t *testing.T) {
			a := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": host + " A", "website": "https://" + host + "/company-a"}}, "import:directory", host+"-a")
			b := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": host + " B", "website": "https://" + host + "/company-b"}}, "import:directory", host+"-b")
			if a.ID == b.ID {
				t.Fatal("a shared profile website collapsed independent companies")
			}
			if a.Fields["domain"] != nil || b.Fields["domain"] != nil {
				t.Fatal("a profile became an inferred company domain")
			}
			// Legacy writers enter through SQL triggers rather than Go normalization.
			var first, second string
			err := tenancy.InWorkspace(context.Background(), s.Pool, ws, func(tx pgx.Tx) error {
				for i, dest := range []*string{&first, &second} {
					name := host + []string{" legacy A", " legacy B"}[i]
					if err := tx.QueryRow(context.Background(), `INSERT INTO leads(workspace_id,company,city,website) VALUES($1,$2,'',$3) RETURNING company_entity_id`, ws, name, "https://"+host+"/"+name).Scan(dest); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if first == second {
				t.Fatal("a legacy website collapsed independent companies")
			}
		})
	}
}
