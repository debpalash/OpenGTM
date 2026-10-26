//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type nativeEnv struct {
	pool, owner *pgxpool.Pool
	ws, other   string
	acme, beta  int64
	foreign     int64
}

func i32(n int32) *int32 { return &n }

func nativeSetup(t *testing.T) *nativeEnv {
	t.Helper()
	e := &nativeEnv{pool: testpg.NewAppPool(t, 4)}
	ctx := context.Background()
	var err error
	e.owner, err = pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.ws = fmt.Sprintf("t12n_a_%d", os.Getpid())
	e.other = fmt.Sprintf("t12n_b_%d", os.Getpid())
	clean := func() {
		c := context.Background()
		_, _ = e.owner.Exec(c, `DELETE FROM leads WHERE workspace_id = ANY($1)`, []string{e.ws, e.other})
		_, _ = e.owner.Exec(c, `DELETE FROM workspace_members WHERE workspace_id = ANY($1)`, []string{e.ws, e.other})
		_, _ = e.owner.Exec(c, `DELETE FROM workspace_member_permissions WHERE workspace_id = ANY($1)`, []string{e.ws, e.other})
		_, _ = e.owner.Exec(c, `DELETE FROM workspaces WHERE id = ANY($1)`, []string{e.ws, e.other})
	}
	clean()
	t.Cleanup(clean)
	for _, w := range []string{e.ws, e.other} {
		if _, err := e.owner.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text)`, w); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(ws, co, city, spec, status, tier string, score int, dm any) int64 {
		var id int64
		if err := e.owner.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, specialization, status, score_tier, score, email, decision_makers)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, ws, co, city, spec, status, tier, score, co+"@x.io", dm).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	e.acme = ins(e.ws, "Acme", "Pune", "plumbing", "new", "hot", 80, `[{"name": "Ann", "title": "CEO"}]`)
	e.beta = ins(e.ws, "Beta", "Goa", "roofing", "contacted", "warm", 40, "not json {")
	ins(e.ws, "Gamma", "Pune", "plumbing", "dead", "cold", 5, nil)
	e.foreign = ins(e.other, "Foreign Co", "Pune", "plumbing", "new", "hot", 99, nil)
	for uid, role := range map[int]string{1: "admin", 2: "editor", 3: "viewer", 4: "owner", 5: "member"} {
		if _, err := e.owner.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,$3)`, e.ws, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *nativeEnv) exec(t *testing.T, ws, name string, args map[string]any, uid *int32) (string, bool) {
	t.Helper()
	var out string
	var ok bool
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		var err error
		out, ok, err = ExecNative(context.Background(), tx, name, args, ws, uid)
		return err
	})
	if err != nil {
		t.Fatalf("ExecNative(%s): %v", name, err)
	}
	return out, ok
}

func TestSearchLeadsMatchesVector(t *testing.T) {
	e := nativeSetup(t)
	out, ok := e.exec(t, e.ws, "search_leads", map[string]any{"query": "Acme"}, nil)
	if !ok {
		t.Fatal("search_leads must be native")
	}
	want := fmt.Sprintf(`{"leads": [{"id": %d, "company": "Acme", "city": "Pune", "email": "Acme@x.io", "phone": null, "score": 80, "tier": "hot", "status": "new", "website": null, "specialization": "plumbing", "company_size": null, "contact_person": null, "linkedin": null, "industry_tags": null, "founded_year": null}], "count": 1}`, e.acme)
	if out != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	// filters, ordering (score DESC) and limit; numbers may arrive as json.Number or float64
	out, _ = e.exec(t, e.ws, "search_leads", map[string]any{"query": "", "city": "Pune", "limit": json.Number("1")}, nil)
	var r struct {
		Leads []struct{ Company string } `json:"leads"`
		Count int                        `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Count != 1 || r.Leads[0].Company != "Acme" {
		t.Fatalf("city+limit: %s (%v)", out, err)
	}
	out, _ = e.exec(t, e.ws, "search_leads", map[string]any{"query": "nomatchxyz"}, nil)
	if out != `{"leads": [], "count": 0}` {
		t.Fatalf("empty: %s", out)
	}
	// tenant isolation
	out, _ = e.exec(t, e.ws, "search_leads", map[string]any{"query": "Foreign"}, nil)
	if out != `{"leads": [], "count": 0}` {
		t.Fatalf("leaked foreign lead: %s", out)
	}
	// defaults: limit 10 when absent
	out, _ = e.exec(t, e.ws, "search_leads", map[string]any{}, nil)
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Count != 3 {
		t.Fatalf("no-filter count: %s", out)
	}
}

func TestSearchLeadsUnusableArgsFallThrough(t *testing.T) {
	e := nativeSetup(t)
	for _, args := range []map[string]any{
		{"query": 5}, {"query": "a", "limit": "ten"}, {"query": "a", "limit": nil}, {"query": "a", "city": []any{"x"}},
	} {
		if out, ok := e.exec(t, e.ws, "search_leads", args, nil); ok {
			t.Errorf("args %v answered natively: %s (must fall through to Python)", args, out)
		}
	}
}

func TestGetLeadDetailParsesDecisionMakers(t *testing.T) {
	e := nativeSetup(t)
	out, ok := e.exec(t, e.ws, "get_lead_detail", map[string]any{"lead_id": json.Number(fmt.Sprint(e.acme))}, nil)
	if !ok {
		t.Fatal("native")
	}
	var r struct {
		Lead map[string]json.RawMessage `json:"lead"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if got := string(r.Lead["decision_makers"]); got != `[{"name": "Ann", "title": "CEO"}]` {
		t.Fatalf("decision_makers = %s (want parsed JSON in python format)", got)
	}
	if string(r.Lead["company"]) != `"Acme"` || string(r.Lead["workspace_id"]) != `"`+e.ws+`"` {
		t.Fatalf("lead = %s", out)
	}
	// key order is Lead.to_dict order: id, company, website, email ...
	if want := fmt.Sprintf(`{"lead": {"id": %d, "company": "Acme", "website": null, "email": "Acme@x.io",`, e.acme); out[:len(want)] != want {
		t.Fatalf("prefix = %.120s", out)
	}
	// invalid JSON in decision_makers stays the raw string
	out, _ = e.exec(t, e.ws, "get_lead_detail", map[string]any{"lead_id": float64(e.beta)}, nil)
	json.Unmarshal([]byte(out), &r)
	if string(r.Lead["decision_makers"]) != `"not json {"` {
		t.Fatalf("raw decision_makers = %s", r.Lead["decision_makers"])
	}
	// null decision_makers stays null
	var gamma int64
	if err := e.owner.QueryRow(context.Background(), `SELECT id FROM leads WHERE workspace_id=$1 AND company='Gamma'`, e.ws).Scan(&gamma); err != nil {
		t.Fatal(err)
	}
	out, _ = e.exec(t, e.ws, "get_lead_detail", map[string]any{"lead_id": gamma}, nil)
	json.Unmarshal([]byte(out), &r)
	if string(r.Lead["decision_makers"]) != "null" {
		t.Fatalf("null decision_makers = %s", r.Lead["decision_makers"])
	}
	// not found and foreign ids
	for _, id := range []int64{e.foreign, 999999999} {
		out, ok = e.exec(t, e.ws, "get_lead_detail", map[string]any{"lead_id": id}, nil)
		if !ok || out != `{"error": "Lead not found"}` {
			t.Fatalf("id %d: %s", id, out)
		}
	}
	// missing key is a controlled error, not a panic
	out, ok = e.exec(t, e.ws, "get_lead_detail", map[string]any{}, nil)
	if !ok || out != `{"error": "Missing required argument: lead_id"}` {
		t.Fatalf("missing: %s", out)
	}
}

func TestGetLeadStatsDefaultStr(t *testing.T) {
	e := nativeSetup(t)
	out, ok := e.exec(t, e.ws, "get_lead_stats", map[string]any{}, nil)
	if !ok {
		t.Fatal("native")
	}
	// python: json.dumps(stats, default=str): default separators, avg_score a float with one decimal.
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["total"]) != "3" {
		t.Fatalf("total = %s", got["total"])
	}
	want := `{"total": 3, "by_status": {`
	if out[:len(want)] != want {
		t.Fatalf("prefix = %.80s", out)
	}
	if !contains(out, `"enrichment": {"total": 3, "with_email": 3, "with_phone": 0, "with_website": 0, "with_linkedin": 0, "with_contact": 0, "avg_score": 41.7}}`) {
		t.Fatalf("enrichment = %s", out)
	}
	// other workspace must not see these rows
	out, _ = e.exec(t, e.other, "get_lead_stats", map[string]any{}, nil)
	if !contains(out, `"total": 1,`) {
		t.Fatalf("other ws stats = %s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func (e *nativeEnv) status(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := e.owner.QueryRow(context.Background(), `SELECT status FROM leads WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpdateLeadStatusRoleCheck(t *testing.T) {
	e := nativeSetup(t)
	deny := `{"error": "Insufficient workspace role"}`
	for _, uid := range []int32{3, 5, 99} { // viewer, member, non-member
		out, ok := e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.acme, "status": "qualified"}, i32(uid))
		if !ok || out != deny {
			t.Fatalf("uid %d: %s", uid, out)
		}
		if s := e.status(t, e.acme); s != "new" {
			t.Fatalf("uid %d mutated status to %s", uid, s)
		}
	}
	for _, uid := range []int32{1, 2, 4} { // admin, editor, owner
		out, ok := e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.acme, "status": "qualified", "note": "n"}, i32(uid))
		want := fmt.Sprintf(`{"ok": true, "lead_id": %d, "new_status": "qualified"}`, e.acme)
		if !ok || out != want {
			t.Fatalf("uid %d: %s", uid, out)
		}
		if s := e.status(t, e.acme); s != "qualified" {
			t.Fatalf("status = %s", s)
		}
		e.owner.Exec(context.Background(), `UPDATE leads SET status='new' WHERE id=$1`, e.acme)
	}
	// uid nil = self-host: python skips the role check
	out, _ := e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.beta, "status": "dead"}, nil)
	if !contains(out, `"ok": true`) || e.status(t, e.beta) != "dead" {
		t.Fatalf("self-host: %s", out)
	}
	// an explicit per-member deny beats the editor role; an allow lifts a viewer
	ctx := context.Background()
	e.owner.Exec(ctx, `INSERT INTO workspace_member_permissions (workspace_id,user_id,permission,effect,updated_at) VALUES ($1,2,'tables.write','deny',0),($1,3,'tables.write','allow',0)`, e.ws)
	out, _ = e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.acme, "status": "x"}, i32(2))
	if out != deny {
		t.Fatalf("explicit deny ignored: %s", out)
	}
	out, _ = e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.acme, "status": "x"}, i32(3))
	if !contains(out, `"ok": true`) {
		t.Fatalf("explicit allow ignored: %s", out)
	}
}

func TestUpdateLeadStatusForeignAndBadArgs(t *testing.T) {
	e := nativeSetup(t)
	// foreign lead: python answers ok (UPDATE matches 0 rows) and must not touch the other tenant
	out, ok := e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.foreign, "status": "dead"}, i32(1))
	if !ok || !contains(out, `"ok": true`) {
		t.Fatalf("foreign: %s", out)
	}
	if s := e.status(t, e.foreign); s != "new" {
		t.Fatalf("cross-tenant write: %s", s)
	}
	out, ok = e.exec(t, e.ws, "update_lead_status", map[string]any{"status": "dead"}, i32(1))
	if !ok || out != `{"error": "Missing required argument: lead_id"}` {
		t.Fatalf("missing lead_id: %s", out)
	}
	out, ok = e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.acme}, i32(1))
	if !ok || out != `{"error": "Missing required argument: status"}` {
		t.Fatalf("missing status: %s", out)
	}
	// the role check runs before argument validation (viewer learns nothing)
	out, _ = e.exec(t, e.ws, "update_lead_status", map[string]any{}, i32(3))
	if out != `{"error": "Insufficient workspace role"}` {
		t.Fatalf("viewer: %s", out)
	}
	// non-string status / odd lead_id fall through to Python
	if _, ok = e.exec(t, e.ws, "update_lead_status", map[string]any{"lead_id": e.acme, "status": 5}, i32(1)); ok {
		t.Fatal("non-string status must fall through")
	}
}

func TestExecNativeNotNative(t *testing.T) {
	e := nativeSetup(t)
	for _, n := range []string{"start_collection", "enrich_lead", "nope", ""} {
		if out, ok := e.exec(t, e.ws, n, map[string]any{}, i32(1)); ok {
			t.Errorf("%s answered natively: %s", n, out)
		}
	}
}
