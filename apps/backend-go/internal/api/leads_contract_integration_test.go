//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type leadsEnv struct{ mcpEnv }

func newLeadsEnv(t *testing.T) leadsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	RegisterLeads(r, d)
	e := leadsEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedLeads(t)
	return e
}

func (e leadsEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "DELETE FROM leads")
			return err
		})
	}
}

// seedLeads loads the users, workspaces and 18 leads (ids 1-18) that contract_seed.py created
// before the lead fixtures were recorded (testdata/leads/seed.json is generated from its constants).
func (e leadsEnv) seedLeads(t *testing.T) {
	t.Helper()
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	alice, bob := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-beta", bob.ID, "owner")
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "leads", "seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	byWS := map[string][]map[string]any{}
	for _, r := range rows {
		ws := r["workspace_id"].(string)
		byWS[ws] = append(byWS[ws], r)
	}
	ctx := context.Background()
	for ws, rs := range byWS {
		b, _ := json.Marshal(rs)
		cols := []string{"id", "workspace_id", "company", "city", "website", "email", "phone", "contact_person", "specialization",
			"company_size", "description", "linkedin_url", "notes", "source", "score", "score_tier", "status", "created_at",
			"updated_at", "last_enriched_at"}
		list := strings.Join(cols, ", ")
		err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO leads (%[1]s) SELECT %[1]s FROM json_populate_recordset(null::leads, $1::json)`, list), string(b))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// leadActor says who sent a recorded request (the recorder redacts Authorization and X-Workspace-Id).
func leadActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case name == "list_bad_token":
		return "", ""
	case strings.HasSuffix(name, "_bob"), name == "list_beta_bob", name == "stats_beta", name == "filters_beta":
		return "bob", "ws-beta"
	case name == "list_viewer_bob_alpha", name == "get_lead_viewer_ok":
		return "bob", "ws-alpha"
	case name == "list_nonmember_alice_beta":
		return "alice", "ws-beta"
	}
	return "alice", ""
}

func (e leadsEnv) get(who, ws, path string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do("GET", path, "", "", hdr)
	return rec
}

func TestLeadReadFixturesMatchPython(t *testing.T) {
	e := newLeadsEnv(t)
	replayed := 0
	for _, full := range contractFixtureNames(t, "leads") {
		name := strings.TrimPrefix(full, "leads/")
		if strings.HasPrefix(name, "token_") || strings.HasPrefix(name, "bulk_enrich_") {
			continue // auth route / Task 14
		}
		fx := contract.Load(t, full)
		who, ws := leadActor(name)
		var rec *httptest.ResponseRecorder
		if name == "list_bad_token" {
			rec, _ = e.do("GET", fx.Request.Path, "", "", map[string]string{"Authorization": "Bearer not.a.token"})
		} else {
			rec = e.get(who, ws, fx.Request.Path)
		}
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		if rec.Code == 307 {
			got.Body = json.RawMessage("null")
		}
		replayed++
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if replayed != 52 {
		t.Fatalf("only %d lead read fixtures replayed", replayed)
	}
}

func TestReadsRequireWorkspace(t *testing.T) {
	e := newLeadsEnv(t)
	for _, p := range []string{"/api/leads", "/api/stats", "/api/filters", "/api/lead/1", "/api/lead/1/similar"} {
		if rec := e.get("", "", p); rec.Code != 401 {
			t.Errorf("%s without token: %d", p, rec.Code)
		}
		if rec := e.get("alice", "ws-beta", p); rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace access denied") {
			t.Errorf("%s foreign workspace: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestReadsTenantIsolation(t *testing.T) {
	e := newLeadsEnv(t)
	for _, p := range []string{"/api/lead/17", "/api/lead/17/similar"} {
		if rec := e.get("alice", "ws-alpha", p); rec.Code != 404 || !strings.Contains(rec.Body.String(), "Lead not found") {
			t.Errorf("%s as alpha: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	rec := e.get("bob", "ws-beta", "/api/leads?limit=1000")
	var list []struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) == 0 {
		t.Fatalf("beta list: %v %s", err, rec.Body.String())
	}
	for _, l := range list {
		if l.WorkspaceID != "ws-beta" {
			t.Fatalf("lead of %s leaked into ws-beta", l.WorkspaceID)
		}
	}
}

func TestListLeadsLimitCappedAt10000(t *testing.T) {
	e := newLeadsEnv(t)
	rec := e.get("alice", "ws-alpha", "/api/leads?limit=99999999")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var list []json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 14 { // 14 non-dead alpha leads
		t.Errorf("%d leads", len(list))
	}
	if rec := e.get("alice", "ws-alpha", "/api/leads?limit=-1"); rec.Code != 422 {
		t.Errorf("negative limit: %d", rec.Code)
	}
}

func TestStatsMatchesPythonEmptyWorkspace(t *testing.T) {
	e := newLeadsEnv(t)
	e.purge()
	rec := e.get("alice", "ws-alpha", "/api/stats")
	want := `{"total":0,"by_status":{},"by_tier":{},"by_city":{},"by_source":{},"enrichment":{"total":0,"with_email":0,"with_phone":0,"with_website":0,"with_linkedin":0,"with_contact":0,"avg_score":0.0}}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

// order_by is allow-listed in leadsread.ListLeads: anything else silently falls back to "score DESC"
// (Python parity), never reaching SQL.
func TestListLeadsOrderByInjectionFallsBackToDefault(t *testing.T) {
	e := newLeadsEnv(t)
	want := e.get("alice", "ws-alpha", "/api/leads?limit=50")
	if want.Code != 200 {
		t.Fatalf("default: %d", want.Code)
	}
	for _, ob := range []string{
		"score DESC; DROP TABLE leads--",
		"(SELECT 1)",
		"company ASC, (select pg_sleep(1))",
		"score DESC, id",
		"SCORE DESC",
	} {
		start := time.Now()
		rec := e.get("alice", "ws-alpha", "/api/leads?limit=50&order_by="+url.QueryEscape(ob))
		if rec.Code != 200 || rec.Body.String() != want.Body.String() {
			t.Errorf("order_by %q: status %d, body differs from default order", ob, rec.Code)
		}
		if time.Since(start) > 900*time.Millisecond {
			t.Errorf("order_by %q: slow, a statement may have run", ob)
		}
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_class WHERE relname='leads'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("leads table missing after injection attempts: %v %d", err, n)
	}
	if rec := e.get("alice", "ws-alpha", "/api/leads?limit=50"); rec.Body.String() != want.Body.String() {
		t.Error("data changed by injection attempts")
	}
}

// ownerExec runs SQL as the database owner (RLS bypass), for seeding foreign leftovers and sequence state.
func ownerExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// leftoverLead simulates a row another test file left behind with an explicit id.
func leftoverLead(t *testing.T, id int) {
	t.Helper()
	ownerExec(t, `INSERT INTO leads (id, workspace_id, company) VALUES ($1, 'ws-leftover-p05', 'leftover')`, id)
	t.Cleanup(func() { ownerExec(t, `DELETE FROM leads WHERE workspace_id = 'ws-leftover-p05'`) })
}

// The leads id serial must never hand out an id that already exists (seedLeads inserts ids 1-18
// explicitly and other test files leave rows with explicit ids), whatever order tests run in.
func TestLeadsEnvSequenceClearsExistingIds(t *testing.T) {
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('leads','id'), 1, false)`)
	leftoverLead(t, 1001)
	leftoverLead2 := func() { ownerExec(t, `INSERT INTO leads (id, workspace_id, company) VALUES (5000, 'ws-leftover-p05', 'leftover2')`) }
	leftoverLead2()
	for name, mk := range map[string]func(*testing.T) leadsEnv{"newLeadsEnv": newLeadsEnv, "newWriteEnv": newWriteEnv} {
		t.Run(name, func(t *testing.T) {
			e := mk(t)
			e.exec(t, "ws-alpha", `INSERT INTO leads (workspace_id, company, city, score, status, source)
				SELECT 'ws-alpha', 'seqcheck ' || g, 'seqcity', 1, 'new', 'x' FROM generate_series(1, 6000) g`)
		})
	}
}
