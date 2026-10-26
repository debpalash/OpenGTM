package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// capLeads records the ListParams the tool built.
type capLeads struct {
	fakeLeads
	got   []ListParams
	stats StatsResult
}

func (f *capLeads) List(ctx context.Context, tx pgx.Tx, p ListParams) ([]Lead, error) {
	f.got = append(f.got, p)
	return f.fakeLeads.List(ctx, tx, p)
}

func readHandler(t *testing.T, d Deps, name string) Tool {
	t.Helper()
	for _, tl := range readTools(d) {
		if tl.Name == name {
			if tl.Capability != "leads:read" {
				t.Fatalf("%s capability %q", name, tl.Capability)
			}
			return tl
		}
	}
	t.Fatalf("read tool %s not registered", name)
	return Tool{}
}

func readDeps(l LeadPort) Deps { return Deps{InWorkspace: nilTxRunner, Leads: l} }

func callRead(t *testing.T, d Deps, name string, a Args) (string, error) {
	t.Helper()
	return readHandler(t, d, name).Handler(context.Background(), Ctx{WorkspaceID: "ws-x", Capabilities: map[string]bool{"leads:read": true}}, a)
}

func TestReadToolsRegistered(t *testing.T) {
	d := readDeps(&capLeads{})
	var names []string
	for _, tl := range readTools(d) {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "find_leads,get_lead_detail,get_pipeline_stats" {
		t.Fatalf("names %v", names)
	}
}

func TestFindLeadsLimitDefaultAndCap(t *testing.T) {
	f := &capLeads{}
	d := readDeps(f)
	for _, tc := range []struct {
		args Args
		want int
	}{
		{nil, 10}, {Args{"limit": nil}, 10}, {Args{"limit": float64(3)}, 3}, {Args{"limit": float64(200)}, 200},
		{Args{"limit": float64(201)}, 200}, {Args{"limit": float64(1 << 40)}, 200},
	} {
		f.got = nil
		if _, err := callRead(t, d, "find_leads", tc.args); err != nil {
			t.Fatal(err)
		}
		if len(f.got) != 1 || f.got[0].Limit != tc.want {
			t.Fatalf("args %v: limit %+v want %d", tc.args, f.got, tc.want)
		}
	}
	for _, bad := range []any{float64(0), float64(-1), "x", 1.5, true, []any{}} {
		f.got = nil
		out, err := callRead(t, d, "find_leads", Args{"limit": bad})
		if err != nil || out != `{"error": "limit must be a positive integer"}` || len(f.got) != 0 {
			t.Fatalf("limit %v: %q %v calls=%d", bad, out, err, len(f.got))
		}
	}
}

func TestFindLeadsTierCityFilters(t *testing.T) {
	f := &capLeads{}
	if _, err := callRead(t, readDeps(f), "find_leads", Args{"query": "acme", "city": "Austin", "tier": "warm", "limit": float64(2)}); err != nil {
		t.Fatal(err)
	}
	want := ListParams{Search: "acme", City: "Austin", ScoreTier: "warm", Limit: 2}
	if len(f.got) != 1 || f.got[0] != want {
		t.Fatalf("%+v want %+v", f.got, want)
	}
	// a mistyped filter is an argument error, never a panic
	if _, err := callRead(t, readDeps(f), "find_leads", Args{"city": 5.0}); err == nil {
		t.Fatal("city: 5 accepted")
	}
}

func strp(s string) *string { return &s }

func TestFindLeadsUnicodeCompany(t *testing.T) {
	sc := int64(55)
	f := &capLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{6: {ID: 6, Company: "東京テック", City: strp("東京"), Score: &sc}}}}
	out, err := callRead(t, readDeps(f), "find_leads", Args{"query": "東京"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"company": "\u6771\u4eac\u30c6\u30c3\u30af"`) || !strings.Contains(out, `"email": null`) ||
		!strings.HasSuffix(out, "\n  ],\n  \"count\": 1\n}") {
		t.Fatalf("%s", out)
	}
}

func TestGetLeadDetailDecisionMakersParsedOrKept(t *testing.T) {
	for _, tc := range []struct {
		dm   *string
		want string
	}{
		{strp(`[{"name": "Sam Lee", "title": "CEO"}]`), "\"decision_makers\": [\n    {\n      \"name\": \"Sam Lee\",\n      \"title\": \"CEO\"\n    }\n  ],"},
		{strp(`{"b": 1, "a": 2}`), "\"decision_makers\": {\n    \"b\": 1,\n    \"a\": 2\n  },"},
		{strp(`not json`), `"decision_makers": "not json",`},
		{strp(``), `"decision_makers": "",`},
		{nil, `"decision_makers": null,`},
	} {
		f := &capLeads{fakeLeads: fakeLeads{Rows: map[int64]*Lead{1: {ID: 1, Company: "A", DecisionMakers: tc.dm}}}}
		out, err := callRead(t, readDeps(f), "get_lead_detail", Args{"lead_id": float64(1)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, tc.want) || !strings.HasPrefix(out, "{\n  \"id\": 1,\n  \"company\": \"A\",") ||
			!strings.HasSuffix(out, "\n  \"last_enriched_at\": null\n}") {
			t.Fatalf("dm %v: %s", tc.dm, out)
		}
	}
}

func TestGetLeadDetailNotFoundAndBadArgs(t *testing.T) {
	d := readDeps(&capLeads{})
	for _, tc := range []struct {
		args Args
		want string
	}{
		{Args{"lead_id": float64(999)}, `{"error": "Lead not found"}`},
		{Args{"lead_id": float64(1 << 40)}, `{"error": "Lead not found"}`},
		{Args{}, `{"error": "lead_id is required"}`},
		{nil, `{"error": "lead_id is required"}`},
		{Args{"lead_id": nil}, `{"error": "lead_id is required"}`},
	} {
		out, err := callRead(t, d, "get_lead_detail", tc.args)
		if err != nil || out != tc.want {
			t.Fatalf("%v: %q %v", tc.args, out, err)
		}
	}
	for _, bad := range []any{"1", 1.5, true, []any{}, json.Number("x")} {
		out, err := callRead(t, d, "get_lead_detail", Args{"lead_id": bad})
		if err != nil || out != `{"error": "lead_id must be an integer"}` {
			t.Fatalf("%v: %q %v", bad, out, err)
		}
	}
}

func TestReadToolsUnavailableWithoutAdapter(t *testing.T) {
	_, err := callRead(t, Deps{InWorkspace: nilTxRunner}, "get_pipeline_stats", nil)
	if !IsUnavailable(err) {
		t.Fatalf("err %v", err)
	}
	_, err = callRead(t, Deps{Leads: &capLeads{}}, "find_leads", nil)
	if !IsUnavailable(err) {
		t.Fatalf("no runner: %v", err)
	}
}
