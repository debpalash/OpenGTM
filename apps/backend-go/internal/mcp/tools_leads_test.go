package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// recLeads records the LeadPort calls the lead write tools make.
type recLeads struct {
	fakeLeads
	rmu      sync.Mutex
	upserted []LeadFields
	updated  []LeadFields
	statuses []string
}

func (r *recLeads) Upsert(ctx context.Context, tx pgx.Tx, f LeadFields) (int64, error) {
	r.rmu.Lock()
	r.upserted = append(r.upserted, f)
	r.rmu.Unlock()
	return r.fakeLeads.Upsert(ctx, tx, f)
}

func (r *recLeads) UpdateFields(_ context.Context, _ pgx.Tx, _ int64, f LeadFields) error {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.updated = append(r.updated, f)
	return r.Err
}

func (r *recLeads) SetStatus(_ context.Context, _ pgx.Tx, _ int64, s string) error {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.statuses = append(r.statuses, s)
	return r.Err
}

func leadTools(p LeadPort) map[string]WriteTool {
	m := map[string]WriteTool{}
	for _, t := range leadWriteTools(Deps{Leads: p}) {
		m[t.Name] = t
	}
	return m
}

func applyLead(t *testing.T, p LeadPort, name string, args Args) string {
	t.Helper()
	wt, ok := leadTools(p)[name]
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	out, err := wt.Apply(context.Background(), nil, Ctx{WorkspaceID: "ws-a"}, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func TestLeadWriteToolMetadata(t *testing.T) {
	m := leadTools(&recLeads{})
	if len(m) != 3 {
		t.Fatalf("tools = %d", len(m))
	}
	for _, n := range []string{"create_lead", "update_lead"} {
		if m[n].Capability != "leads:write" || !reflect.DeepEqual(m[n].Roles, WriteRoles) {
			t.Errorf("%s: %+v", n, m[n])
		}
	}
	d := m["delete_lead"]
	if d.Capability != "leads:delete" || !reflect.DeepEqual(d.Roles, AdminRoles) {
		t.Errorf("delete_lead: %+v", d)
	}
}

func TestCreateLeadAllowlistDropsUnknownAndWorkspacePort(t *testing.T) {
	r := &recLeads{}
	out := applyLead(t, r, "create_lead", Args{
		"company": "Filtered Co", "id": 999, "score": 99, "workspace_id": "ws-beta",
		"created_at": "1999-01-01", "email": nil, "notes": "n", "industry_tags": "a,b",
		"unknown_field": 1, "status": "qualified",
	})
	if out != `{"status": "created", "lead_id": 1, "company": "Filtered Co"}` {
		t.Fatal(out)
	}
	want := LeadFields{"company": "Filtered Co", "notes": "n", "industry_tags": "a,b", "status": "qualified"}
	if !reflect.DeepEqual(r.upserted[0], want) {
		t.Fatalf("upserted %v, want %v", r.upserted[0], want)
	}
}

func TestCreateLeadAllNineteenFieldsAllowed(t *testing.T) {
	r := &recLeads{}
	args := Args{}
	for _, k := range []string{"company", "website", "email", "phone", "contact_person", "contact_title", "city",
		"state", "address", "specialization", "company_size", "description", "industry_tags", "linkedin_url",
		"twitter_url", "facebook_url", "notes", "status", "source"} {
		args[k] = "v-" + k
	}
	applyLead(t, r, "create_lead", args)
	if len(r.upserted[0]) != 19 {
		t.Fatalf("fields = %d: %v", len(r.upserted[0]), r.upserted[0])
	}
}

func TestCreateLeadBlankCompanyPort(t *testing.T) {
	for _, a := range []Args{{"city": "Paris"}, {"company": "   ", "city": "Paris"}, {"company": nil}, {"company": ""}} {
		r := &recLeads{}
		if got := applyLead(t, r, "create_lead", a); got != `{"error": "company is required"}` {
			t.Errorf("%v: %s", a, got)
		}
		if len(r.upserted) != 0 {
			t.Errorf("%v reached the port", a)
		}
	}
}

func TestCreateLeadNonStringFieldPort(t *testing.T) {
	cases := map[string]Args{
		"company":       {"company": 5},
		"city":          {"company": "X", "city": 12},
		"industry_tags": {"company": "X", "industry_tags": []any{"a"}},
		"notes":         {"company": "X", "notes": true},
	}
	for field, a := range cases {
		r := &recLeads{}
		want := `{"error": "` + field + ` must be a string"}`
		if got := applyLead(t, r, "create_lead", a); got != want {
			t.Errorf("%s: %s", field, got)
		}
		if len(r.upserted) != 0 {
			t.Errorf("%s reached the port", field)
		}
	}
}

func TestUpdateLeadFieldsAndStatusPort(t *testing.T) {
	r := &recLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{1: {ID: 1}}}}
	out := applyLead(t, r, "update_lead", Args{"lead_id": float64(1), "city": "Munich", "phone": "+49 1",
		"status": "qualified", "company": "Ignored Rename", "id": 5, "workspace_id": "ws-b", "email": nil})
	if out != `{"status": "updated", "lead_id": 1, "fields": ["city", "company", "phone", "status"]}` {
		t.Fatal(out)
	}
	want := LeadFields{"city": "Munich", "phone": "+49 1", "company": "Ignored Rename"}
	if len(r.updated) != 1 || !reflect.DeepEqual(r.updated[0], want) {
		t.Fatalf("updated %v", r.updated)
	}
	if !reflect.DeepEqual(r.statuses, []string{"qualified"}) {
		t.Fatalf("statuses %v", r.statuses)
	}
}

func TestUpdateLeadStatusOnlySkipsUpdateFields(t *testing.T) {
	r := &recLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{4: {ID: 4}}}}
	out := applyLead(t, r, "update_lead", Args{"lead_id": int64(4), "status": "contacted"})
	if out != `{"status": "updated", "lead_id": 4, "fields": ["status"]}` || len(r.updated) != 0 {
		t.Fatalf("%s %v", out, r.updated)
	}
}

func TestUpdateLeadNoFieldsStillOKPort(t *testing.T) {
	r := &recLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{2: {ID: 2}}}}
	out := applyLead(t, r, "update_lead", Args{"lead_id": float64(2)})
	if out != `{"status": "updated", "lead_id": 2, "fields": []}` {
		t.Fatal(out)
	}
	if len(r.updated) != 0 || len(r.statuses) != 0 {
		t.Fatal("port written")
	}
}

func TestUpdateLeadMissOrBadID(t *testing.T) {
	r := &recLeads{}
	cases := []struct {
		a    Args
		want string
	}{
		{Args{"lead_id": float64(999), "notes": "x"}, `{"error": "Lead not found"}`},
		{Args{"notes": "x"}, `{"error": "lead_id is required"}`},
		{Args{"lead_id": nil}, `{"error": "lead_id is required"}`},
		{Args{"lead_id": "7"}, `{"error": "lead_id must be an integer"}`},
		{Args{"lead_id": 1.5}, `{"error": "lead_id must be an integer"}`},
		{Args{"lead_id": float64(1), "notes": 3}, `{"error": "notes must be a string"}`},
	}
	r.Rows = map[int64]*Lead{1: {ID: 1}}
	for _, c := range cases {
		if got := applyLead(t, r, "update_lead", c.a); got != c.want {
			t.Errorf("%v: %s", c.a, got)
		}
	}
	if len(r.updated)+len(r.statuses) != 0 {
		t.Fatal("port written on a miss")
	}
}

func TestDeleteLeadHitMissAndBadID(t *testing.T) {
	r := &recLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{5: {ID: 5}}}}
	cases := []struct {
		a    Args
		want string
	}{
		{Args{"lead_id": float64(5)}, `{"status": "deleted", "lead_id": 5}`},
		{Args{"lead_id": float64(5)}, `{"status": "not_found", "lead_id": 5}`},
		{Args{}, `{"error": "lead_id is required"}`},
		{Args{"lead_id": "x"}, `{"error": "lead_id must be an integer"}`},
	}
	for _, c := range cases {
		if got := applyLead(t, r, "delete_lead", c.a); got != c.want {
			t.Errorf("%v: %s", c.a, got)
		}
	}
	if !reflect.DeepEqual(r.Deleted, []int64{5}) {
		t.Fatalf("deleted %v (a miss must not reach Delete)", r.Deleted)
	}
}

func TestLeadWriteResultsAreValidJSON(t *testing.T) {
	r := &recLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{1: {ID: 1}}}}
	out := applyLead(t, r, "update_lead", Args{"lead_id": float64(1), "state": "é\u2028", "city": "x"})
	var v struct{ Fields []string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || !sort.StringsAreSorted(v.Fields) {
		t.Fatalf("%s %v", out, err)
	}
	if strings.Contains(out, "\u2028") {
		t.Fatal("not ascii-escaped")
	}
}
