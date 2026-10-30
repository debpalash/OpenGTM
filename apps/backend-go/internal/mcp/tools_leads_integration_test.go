//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// pgLeadWrites is the test-local write half of the LeadPort (the production
// adapter is Task 17): Python upsert_lead semantics over the leads table.
type pgLeadWrites struct{ pgLeads }

var leadWriteCols = []string{"company", "website", "email", "phone", "contact_person", "contact_title", "city",
	"state", "address", "specialization", "company_size", "description", "industry_tags", "linkedin_url",
	"twitter_url", "facebook_url", "notes", "status", "source"}

func (*pgLeadWrites) Upsert(ctx context.Context, tx pgx.Tx, f LeadFields) (int64, error) {
	return (PGLeadPort{}).Upsert(ctx, tx, f)
}

func (*pgLeadWrites) UpdateFields(ctx context.Context, tx pgx.Tx, id int64, f LeadFields) error {
	for k, v := range f {
		ok := false
		for _, c := range leadWriteCols {
			ok = ok || c == k
		}
		if !ok {
			return fmt.Errorf("column %q not writable", k)
		}
		if _, err := tx.Exec(ctx, `UPDATE leads SET `+k+`=$1 WHERE id=$2`, v, id); err != nil {
			return err
		}
	}
	return nil
}

func (*pgLeadWrites) SetStatus(ctx context.Context, tx pgx.Tx, id int64, s string) error {
	_, err := tx.Exec(ctx, `UPDATE leads SET status=$1 WHERE id=$2`, s, id)
	return err
}

func (*pgLeadWrites) Delete(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM leads WHERE id=$1`, id)
	return err
}

type leadEnv struct {
	*wEnv
	other   string // a second workspace holding a lead the token must never reach
	ids     []int64
	otherID int64
}

func newLeadEnv(t *testing.T, role string) *leadEnv {
	t.Helper()
	w := newWEnv(t, role)
	w.d.Leads = &pgLeadWrites{}
	e := &leadEnv{wEnv: w}
	e.other = w.workspace(t, "w2", &w.uid, "")
	ctx := context.Background()
	seed := func(ws, company, city, email string) int64 {
		var id int64
		err := tenancy.InWorkspace(ctx, w.pool, ws, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, email, status, score, score_tier, created_at, updated_at)
				VALUES ($1,$2,$3,$4,'new',10,'cold','2026-01-01T00:00:00','2026-01-01T00:00:00') RETURNING id`, ws, company, city, email).Scan(&id)
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, c := range []string{"Acme Robotics", "Globex Corp", "Eco", "Suppressed", "Delete Me Ltd"} {
		e.ids = append(e.ids, seed(w.ws, c, "City"+fmt.Sprint(i), c+"@x.example.com"))
	}
	e.otherID = seed(e.other, "Initech", "Pune", "team@initech.example.com")
	t.Cleanup(func() {
		for _, ws := range []string{w.ws, e.other} {
			_ = tenancy.InWorkspace(ctx, w.pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM leads`)
				return err
			})
		}
	})
	return e
}

func (e *leadEnv) leadRow(t *testing.T, id int64) (company, city, email, phone, status string, found bool) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, e.ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT company, coalesce(city,''), coalesce(email,''), coalesce(phone,''), coalesce(status,'') FROM leads WHERE id=$1`, id).
			Scan(&company, &city, &email, &phone, &status)
		if err == pgx.ErrNoRows {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (e *leadEnv) leadCount(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM leads WHERE workspace_id=$1`, e.ws)
}

type leadFx struct {
	Name string
	Args map[string]any
	Text string
}

func loadLeadFx(t *testing.T, file string) leadFx {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, "tools", file))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Request struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		} `json:"request"`
		Response struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return leadFx{fx.Request.Params.Name, fx.Request.Params.Arguments, fx.Response.Result.Content[0].Text}
}

var leadIDRe = regexp.MustCompile(`"lead_id": \d+`)

// replay runs a recorded write; fixture lead ids 1-5 map to the seeded leads and
// created ids (not stable across runs) are compared by shape.
func (e *leadEnv) replay(t *testing.T, s *Server, file string) string {
	t.Helper()
	fx := loadLeadFx(t, file)
	args := Args{}
	for k, v := range fx.Args {
		args[k] = v
	}
	want := fx.Text
	if v, ok := args["lead_id"].(float64); ok && v >= 1 && int(v) <= len(e.ids) {
		args["lead_id"] = float64(e.ids[int(v)-1])
		want = leadIDRe.ReplaceAllString(want, fmt.Sprintf(`"lead_id": %d`, e.ids[int(v)-1]))
	}
	got := e.call(s, fx.Name, args)
	if fx.Name == "create_lead" && strings.Contains(want, `"created"`) {
		got = leadIDRe.ReplaceAllString(got, `"lead_id": N`)
		want = leadIDRe.ReplaceAllString(want, `"lead_id": N`)
	}
	if got != want {
		t.Errorf("%s:\n got %s\nwant %s", file, got, want)
	}
	return got
}

func TestCreateLeadMatchesFixture(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	for _, f := range []string{"writes__create_lead.json", "writes__create_lead_company_missing.json",
		"writes__create_lead_company_required.json", "writes__create_lead_non_allowlisted_fields_dropped.json",
		"writes__create_lead_differing_case_is_new.json", "writes__create_lead_long_unicode.json"} {
		e.replay(t, s, f)
	}
}

func TestCreateLeadDedupSemanticsMatchUpsert(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	before := e.leadCount(t)
	first := e.call(s, "create_lead", Args{"company": "Newco", "city": "Paris", "email": "n@newco.example.com"})
	second := e.call(s, "create_lead", Args{"company": "Newco", "city": "Paris", "phone": "+33 1 555 0000"})
	diffCase := e.call(s, "create_lead", Args{"company": "newco", "city": "paris"})
	var a, b, c struct {
		LeadID int64 `json:"lead_id"`
	}
	for i, p := range []any{&a, &b, &c} {
		raw := []string{first, second, diffCase}[i]
		if err := json.Unmarshal([]byte(raw), p); err != nil {
			t.Fatal(raw, err)
		}
	}
	if a.LeadID == 0 || a.LeadID != b.LeadID || c.LeadID == a.LeadID {
		t.Fatalf("ids %d %d %d", a.LeadID, b.LeadID, c.LeadID)
	}
	if n := e.leadCount(t); n != before+2 {
		t.Fatalf("count %d want %d (same company+city overwrites, different case is new)", n, before+2)
	}
	// Python overwrites every field: the second call omitted email.
	_, _, email, phone, status, _ := e.leadRow(t, a.LeadID)
	if email != "" || phone != "+33 1 555 0000" || status != "new" {
		t.Fatalf("email %q phone %q status %q", email, phone, status)
	}
}

func TestCreateLeadAllowlistDropsUnknownAndWorkspace(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	got := e.call(s, "create_lead", Args{"company": "Filtered Co", "id": 999, "score": 99, "workspace_id": e.other,
		"created_at": "1999-01-01", "email": nil, "notes": "n", "unknown_field": 1})
	var r struct {
		LeadID int64 `json:"lead_id"`
	}
	if err := json.Unmarshal([]byte(got), &r); err != nil || r.LeadID == 0 || r.LeadID == 999 {
		t.Fatalf("%s %v", got, err)
	}
	if _, _, _, _, _, found := e.leadRow(t, r.LeadID); !found {
		t.Fatal("lead not in the token's workspace")
	}
	n := 0
	_ = tenancy.InWorkspace(context.Background(), e.pool, e.other, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM leads WHERE company='Filtered Co'`).Scan(&n)
	})
	if n != 0 {
		t.Fatal("smuggled workspace_id was honoured")
	}
}

func TestCreateLeadBlankCompany(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	before := e.leadCount(t)
	for _, a := range []Args{{"city": "Paris"}, {"company": "   ", "city": "Paris"}} {
		if got := e.call(s, "create_lead", a); got != `{"error": "company is required"}` {
			t.Fatal(got)
		}
	}
	if e.leadCount(t) != before || e.held(t) != 0 {
		t.Fatal("a refused create must not write or hold a cap slot")
	}
	e.wantAudit(t, 2, "error", "company is required")
}

func TestCreateLeadNonStringField(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	before := e.leadCount(t)
	if got := e.call(s, "create_lead", Args{"company": "X", "city": float64(12)}); got != `{"error": "city must be a string"}` {
		t.Fatal(got)
	}
	if e.leadCount(t) != before {
		t.Fatal("wrote on a refused call")
	}
}

func TestUpdateLeadFieldsAndStatus(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	for _, f := range []string{"writes__update_lead_fields_and_status_company_is_allowlisted.json",
		"writes__update_lead_no_fields.json", "writes__update_lead_null_values_dropped.json",
		"writes__update_lead_status_only.json", "writes__update_lead_missing_lead_id.json", "writes__update_lead_not_found.json"} {
		e.replay(t, s, f)
	}
	company, city, _, phone, status, _ := e.leadRow(t, e.ids[0])
	if company != "Ignored Rename" || city != "Munich" || phone != "+49 1" || status != "qualified" {
		t.Fatalf("lead 1: %q %q %q %q", company, city, phone, status)
	}
}

func TestUpdateLeadCrossTenantNotFound(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	got := e.call(s, "update_lead", Args{"lead_id": float64(e.otherID), "notes": "pwned", "status": "won"})
	if got != `{"error": "Lead not found"}` {
		t.Fatal(got)
	}
	var notes, status string
	_ = tenancy.InWorkspace(context.Background(), e.pool, e.other, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT coalesce(notes,''), status FROM leads WHERE id=$1`, e.otherID).Scan(&notes, &status)
	})
	if notes != "" || status != "new" {
		t.Fatalf("other tenant lead modified: %q %q", notes, status)
	}
	if e.held(t) != 0 {
		t.Fatal("cap slot held")
	}
}

func TestUpdateLeadNoFieldsStillOK(t *testing.T) {
	e := newLeadEnv(t, "editor")
	s := e.server(leadWriteTools(e.d)...)
	want := fmt.Sprintf(`{"status": "updated", "lead_id": %d, "fields": []}`, e.ids[1])
	if got := e.call(s, "update_lead", Args{"lead_id": float64(e.ids[1])}); got != want {
		t.Fatal(got)
	}
	e.wantAudit(t, 1, "ok", "")
}

func TestDeleteLeadIdempotent(t *testing.T) {
	e := newLeadEnv(t, "admin")
	s := e.server(leadWriteTools(e.d)...)
	for _, f := range []string{"writes__delete_lead.json", "writes__delete_lead_again_not_found.json", "writes__delete_lead_missing_id.json"} {
		e.replay(t, s, f)
	}
	if _, _, _, _, _, found := e.leadRow(t, e.ids[4]); found {
		t.Fatal("lead still present")
	}
	if e.held(t) != 0 {
		t.Fatal("held reservation")
	}
}

func TestDeleteLeadCrossTenantNoop(t *testing.T) {
	e := newLeadEnv(t, "admin")
	s := e.server(leadWriteTools(e.d)...)
	want := fmt.Sprintf(`{"status": "not_found", "lead_id": %d}`, e.otherID)
	if got := e.call(s, "delete_lead", Args{"lead_id": float64(e.otherID)}); got != want {
		t.Fatal(got)
	}
	n := 0
	_ = tenancy.InWorkspace(context.Background(), e.pool, e.other, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM leads WHERE id=$1`, e.otherID).Scan(&n)
	})
	if n != 1 {
		t.Fatal("other tenant lead deleted")
	}
}

// Each scenario owns its own env (the test DB lock is per testing.T).
func TestLeadWritesRequireCapabilityRoleAndFlag(t *testing.T) {
	t.Run("capability missing", func(t *testing.T) {
		e := newLeadEnv(t, "editor")
		s := e.server(leadWriteTools(e.d)...)
		e.ctx.Capabilities = ReadCaps
		if got := e.call(s, "create_lead", Args{"company": "X"}); !strings.Contains(got, "capability not granted") {
			t.Fatal(got)
		}
		e.wantAudit(t, 1, "denied", "capability not granted")
	})
	t.Run("flag off", func(t *testing.T) {
		e := newLeadEnv(t, "admin")
		e.cfg.WriteEnabled = false
		s := e.server(leadWriteTools(e.d)...)
		for _, n := range []string{"create_lead", "update_lead", "delete_lead"} {
			if got := e.call(s, n, Args{"company": "X", "lead_id": float64(e.ids[0])}); got != `{"error": "mcp writes disabled"}` {
				t.Fatalf("%s: %s", n, got)
			}
		}
		if _, _, _, _, _, found := e.leadRow(t, e.ids[0]); !found {
			t.Fatal("deleted with writes disabled")
		}
	})
	t.Run("editor may not delete", func(t *testing.T) {
		e := newLeadEnv(t, "editor")
		s := e.server(leadWriteTools(e.d)...)
		if got := e.call(s, "delete_lead", Args{"lead_id": float64(e.ids[0])}); got != `{"error": "insufficient workspace role"}` {
			t.Fatal(got)
		}
		if _, _, _, _, _, found := e.leadRow(t, e.ids[0]); !found {
			t.Fatal("editor deleted a lead")
		}
		if got := e.call(s, "create_lead", Args{"company": "Okay"}); !strings.Contains(got, `"status": "created"`) {
			t.Fatal(got)
		}
	})
	t.Run("viewer downgraded", func(t *testing.T) {
		e := newLeadEnv(t, "viewer")
		s := e.server(leadWriteTools(e.d)...)
		for _, n := range []string{"create_lead", "update_lead"} {
			if got := e.call(s, n, Args{"company": "X", "lead_id": float64(e.ids[0])}); got != `{"error": "insufficient workspace role"}` {
				t.Fatalf("%s: %s", n, got)
			}
		}
		e.wantAudit(t, 2, "denied", "insufficient workspace role")
	})
}
