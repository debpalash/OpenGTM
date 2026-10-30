//go:build integration

package records

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestWorkbenchSummaryFiltersAndRecovery(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Alpha", "domain": "alpha.test", "score": 10, "score_tier": "cold"}}, "manual", "")
	mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Beta", "score": 90, "score_tier": "hot"}}, "manual", "")
	mustRecord(t, s, "other-tenant", Company, Write{Company: map[string]any{"name": "Secret", "domain": "secret.test"}}, "manual", "")
	mustRecord(t, s, ws, Person, Write{Person: map[string]any{"full_name": "Ana", "email": "ana@alpha.test", "email_verify_status": "valid"}}, "manual", "")
	mustRecord(t, s, ws, Person, Write{Person: map[string]any{"full_name": "Bo", "linkedin_url": "https://linkedin.com/in/bo"}}, "manual", "")
	raw, err := s.Overview(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Companies, People int
		Email             int `json:"people_with_email"`
		Verified          int `json:"verified_emails"`
		MissingDomain     int `json:"companies_needing_domain"`
	}
	if err = json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Companies != 2 || summary.People != 2 || summary.Email != 1 || summary.Verified != 1 || summary.MissingDomain != 1 {
		t.Fatalf("incorrect summary %s", raw)
	}
	page, err := s.ListFiltered(ctx, ws, Company, ListOptions{Sort: "score", Direction: "desc", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Records) != 1 || page.Records[0].Fields["name"] != "Beta" || page.Records[0].UpdatedAt == "" {
		t.Fatalf("bad score page: %#v", page)
	}
	page, err = s.ListFiltered(ctx, ws, Company, ListOptions{Filter: "qualified", Limit: 100})
	if err != nil || page.Total != 1 {
		t.Fatalf("qualified: %#v %v", page, err)
	}
	page, err = s.ListFiltered(ctx, ws, Company, ListOptions{Filter: "needs_domain", Limit: 100})
	if err != nil || page.Total != 1 {
		t.Fatalf("missing domain: %#v %v", page, err)
	}
	page, err = s.ListFiltered(ctx, ws, Person, ListOptions{Filter: "needs_email", Limit: 100})
	if err != nil || page.Total != 1 {
		t.Fatalf("missing email: %#v %v", page, err)
	}
	page, err = s.ListFiltered(ctx, ws, Company, ListOptions{Search: "Secret", Limit: 100})
	if err != nil || page.Total != 0 {
		t.Fatalf("tenant leaked: %#v %v", page, err)
	}
	_, err = s.ListFiltered(ctx, ws, Company, ListOptions{Sort: "name; DROP TABLE workspaces", Limit: 100})
	if err == nil {
		t.Fatal("invalid sort accepted")
	}
	req := ImportRequest{Kind: Company, Source: "clay:receipt", IdempotencyKey: "receipt-test", CSV: "Company Name,Domain\nGamma,gamma.test\n"}
	imported, err := s.Import(ctx, ws, req, false)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.ImportReceipt(ctx, ws, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	var recovered ImportResult
	if err = json.Unmarshal(receipt, &recovered); err != nil || recovered.Imported != imported.Imported {
		t.Fatalf("bad receipt %s: %v", receipt, err)
	}
	if _, err = s.ImportReceipt(ctx, "other-tenant", req.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("receipt crossed tenant: %v", err)
	}
}
