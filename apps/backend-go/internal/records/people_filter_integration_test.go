//go:build integration

package records

import (
	"context"
	"testing"
)

func TestCompanyPeopleFilterIsScopedPagedAndCurrent(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	company := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Atlas", "domain": "atlas.test"}}, "manual", "")
	other := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Other", "domain": "other.test"}}, "manual", "")
	a := mustRecord(t, s, ws, Person, Write{Person: map[string]any{"full_name": "Ana", "email": "ana@atlas.test"}, CompanyID: company.ID, Employment: map[string]any{"title": "Founder"}}, "manual", "")
	mustRecord(t, s, ws, Person, Write{Person: map[string]any{"full_name": "Bo", "email": "bo@atlas.test"}, CompanyID: company.ID}, "manual", "")
	mustRecord(t, s, ws, Person, Write{Person: map[string]any{"full_name": "Cal", "email": "cal@other.test"}, CompanyID: other.ID}, "manual", "")
	options := ListOptions{CompanyID: company.ID, Limit: 1, Sort: "name"}
	first, err := s.ListFiltered(ctx, ws, Person, options)
	if err != nil || first.Total != 2 || len(first.Records) != 1 || first.Records[0].ID != a.ID {
		t.Fatalf("first page: %+v %v", first, err)
	}
	options.Offset = 1
	second, err := s.ListFiltered(ctx, ws, Person, options)
	if err != nil || second.Total != 2 || len(second.Records) != 1 || second.Records[0].ID == a.ID {
		t.Fatalf("second page: %+v %v", second, err)
	}
	options.Offset = 0
	options.Search = "Ana"
	found, err := s.ListFiltered(ctx, ws, Person, options)
	if err != nil || found.Total != 1 {
		t.Fatalf("search: %+v %v", found, err)
	}
	options.Search = ""
	foreign, err := s.ListFiltered(ctx, "other-tenant", Person, options)
	if err != nil || foreign.Total != 0 {
		t.Fatalf("tenant filter: %+v %v", foreign, err)
	}
	if _, err = s.Patch(ctx, ws, Person, a.ID, a.Version, Write{CompanyID: other.ID}); err != nil {
		t.Fatal(err)
	}
	moved, err := s.ListFiltered(ctx, ws, Person, options)
	if err != nil || moved.Total != 1 || moved.Records[0].ID == a.ID {
		t.Fatalf("former employment leaked: %+v %v", moved, err)
	}
	if _, err = s.ListFiltered(ctx, ws, Company, options); err == nil {
		t.Fatal("company scope accepted a people relationship filter")
	}
}
