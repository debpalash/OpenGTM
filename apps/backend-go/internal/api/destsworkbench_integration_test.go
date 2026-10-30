//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/records"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

func TestActivationAccessUsesEffectivePermissionAndWorkspace(t *testing.T) {
	e := newDrEnv(t, nil)
	rec, _ := e.call("", "ws-alpha", "GET", drBase+"/access", "")
	if rec.Code != 401 {
		t.Fatalf("unauthenticated activation access: %d", rec.Code)
	}
	for _, c := range []struct {
		user, ws string
		want     bool
	}{
		{"alice", "ws-alpha", true}, {"bob", "ws-alpha", false}, {"erin", "ws-alpha", true}, {"bob", "ws-beta", true},
	} {
		code, body, raw := e.drJSON(t, c.user, c.ws, "GET", drBase+"/access")
		if code != 200 || body["can_write"] != c.want {
			t.Fatalf("%s %s: %d %s", c.user, c.ws, code, raw)
		}
	}
	for _, c := range []struct {
		user, effect string
		want         bool
	}{{"bob", "allow", true}, {"erin", "deny", false}} {
		if _, err := audOwner(t).Exec(context.Background(), `INSERT INTO workspace_member_permissions(workspace_id,user_id,permission,effect,updated_at) SELECT 'ws-alpha',id,'activation.write',$2,1 FROM users WHERE username=$1`, c.user, c.effect); err != nil {
			t.Fatal(err)
		}
		code, body, raw := e.drJSON(t, c.user, "ws-alpha", "GET", drBase+"/access")
		if code != 200 || body["can_write"] != c.want {
			t.Fatalf("override %s: %d %s", c.user, code, raw)
		}
	}
	code, _, _ := e.drJSON(t, "erin", "ws-beta", "GET", drBase+"/access")
	if code != 403 {
		t.Fatalf("cross-workspace access: %d", code)
	}
}

func TestCanonicalDeliveriesExposeRecordIdentityWithoutLegacyZero(t *testing.T) {
	e := newDrEnv(t, nil)
	ctx := context.Background()
	s := records.Store{Pool: e.pool}
	company, err := s.Upsert(ctx, "ws-alpha", records.Company, records.Write{Company: map[string]any{"name": "Delivery company", "domain": "delivery.test"}}, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	person, err := s.Upsert(ctx, "ws-alpha", records.Person, records.Write{Person: map[string]any{"full_name": "Delivery person", "email": "person@delivery.test"}, CompanyID: company.ID}, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ key, company, person string }{{"company", company.ID, ""}, {"person", company.ID, person.ID}} {
		err = tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
			_, _, err := deststore.UpsertDelivery(ctx, tx, deststore.UpsertDeliveryInput{RunID: "run-act-w6", DestinationID: "dst-act-webhook", CompanyID: item.company, PersonID: item.person, SubjectKey: item.key + ":" + company.ID, Operation: "upsert", IdempotencyKey: "workbench:" + item.key, Fingerprint: "fixture", Payload: []byte(`{}`), Status: "success", Attempts: 1})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rec, _ := e.call("alice", "ws-alpha", "GET", drBase+"/runs/run-act-w6/deliveries?limit=100", "")
	if rec.Code != 200 {
		t.Fatalf("deliveries %d: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	foundCompany, foundPerson := false, false
	for _, row := range rows {
		if row["company_id"] == company.ID {
			if row["lead_id"] != nil {
				t.Fatalf("canonical legacy ID must be null: %v", row)
			}
			if row["person_id"] == person.ID {
				foundPerson = true
			} else {
				foundCompany = true
			}
		}
	}
	if !foundCompany || !foundPerson {
		t.Fatalf("canonical identities missing: %v", rows)
	}
	code, _, _ := e.drJSON(t, "bob", "ws-beta", "GET", drBase+"/runs/run-act-w6/deliveries")
	if code != 404 {
		t.Fatalf("cross-tenant receipt: %d", code)
	}
}
