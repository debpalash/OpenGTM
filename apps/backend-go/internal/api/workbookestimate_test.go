//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

type estimateEnv struct{ detailEnv }

func newEstimateEnv(t *testing.T) estimateEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	registerWorkbookEstimate(r, d)
	e := estimateEnv{detailEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}
	e.purge()
	e.cleanEnrich()
	t.Cleanup(func() { e.cleanEnrich(); e.purge() })
	e.seedDetail(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	e.seedEnrich(t)
	return e
}

func (e estimateEnv) cleanEnrich() {
	ctx := context.Background()
	_ = tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM jobs WHERE type = 'run_workbook'`,
			`DELETE FROM workbook_views WHERE workbook_id LIKE 'wb-enrich-%' OR workbook_id LIKE 'wb-t23-%'`,
			`DELETE FROM workbook_rows WHERE workbook_id LIKE 'wb-enrich-%' OR workbook_id LIKE 'wb-t23-%'`,
			`DELETE FROM workbooks WHERE id LIKE 'wb-enrich-%' OR id LIKE 'wb-t23-%'`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	_ = tenancy.InWorkspace(ctx, e.pool, "ws-beta", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM jobs WHERE type = 'run_workbook'`)
		return err
	})
	_, _ = e.pool.Exec(ctx, `DELETE FROM provider_stats`)
}

type enrichSeedColumns struct {
	Columns []json.RawMessage `json:"columns"`
	BigCols json.RawMessage   `json:"big_columns"`
}

func (e estimateEnv) exec(t *testing.T, ws, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e estimateEnv) workbook(t *testing.T, id string, cols string, rows int) {
	t.Helper()
	e.exec(t, "ws-alpha", `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
		filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads)
		VALUES ($1, $1, '', 'idle', 'ws-alpha', 'manual', '{}'::json, '{}'::json, $2::json, 0, 0, false)`, id, cols)
	for k := 0; k < rows; k++ {
		e.exec(t, "ws-alpha", `INSERT INTO workbook_rows (workbook_id, "position", data, enrichments, workspace_id)
			VALUES ($1,$2,$3::json,'{}'::json,'ws-alpha')`, id, k, fmt.Sprintf(`{"company":"Co %d"}`, k))
	}
}

// seedEnrich recreates the contract seed's enrichment workbooks (scripts/contract_seed.py _seed_enrichment).
func (e estimateEnv) seedEnrich(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("../runplan/testdata/seed_columns.json")
	if err != nil {
		t.Fatal(err)
	}
	var s enrichSeedColumns
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(s.Columns)
	var live []json.RawMessage
	for _, c := range s.Columns {
		var h struct{ ID string }
		_ = json.Unmarshal(c, &h)
		if h.ID == "company" || h.ID == "email_enr" || h.ID == "src_icp" {
			live = append(live, c)
		}
	}
	liveJSON, _ := json.Marshal(live)
	e.workbook(t, "wb-enrich-1", string(all), 0)
	sites := []string{"https://globex.example.com", "https://initech.example.com", "", "https://umbrella.example.com", "https://hooli.example.com", "https://stark.example.com"}
	companies := []string{"Globex", "Initech", "Soylent", "Umbrella", "Hooli", "Stark Industries"}
	for k := range companies {
		e.exec(t, "ws-alpha", `INSERT INTO workbook_rows (workbook_id, "position", data, enrichments, workspace_id)
			VALUES ('wb-enrich-1',$1,$2::json,'{}'::json,'ws-alpha')`, k,
			fmt.Sprintf(`{"company":%q,"website":%q,"email":""}`, companies[k], sites[k]))
	}
	e.workbook(t, "wb-enrich-big", string(s.BigCols), 250)
	e.workbook(t, "wb-enrich-live", string(liveJSON), 3)
	e.exec(t, "ws-alpha", `INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config)
		VALUES ('view-enrich-1','ws-alpha','wb-enrich-1','Has website',$1::json)`,
		`{"filters":[{"column":"website","op":"not_empty","value":null}],"hidden_columns":[]}`)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO provider_stats
		(provider, field, attempts, hits, total_confidence, total_latency_ms, total_cost_usd, cooldown_until, accuracy_score, accuracy_samples)
		VALUES ('hunter_io','email',40,28,22.4,18000.0,1.6,NULL,0.91,25),
		       ('apollo_io','email',12,5,3.5,9600.0,0.36,'2026-08-29 12:00:00',NULL,0),
		       ('ipinfo','company_size',7,0,0.0,700.0,0.0,NULL,NULL,0)`); err != nil {
		t.Fatal(err)
	}
}

func estimateActor(name string) (who, ws string) {
	switch name {
	case "estimate_no_token":
		return "", ""
	case "estimate_viewer_ok":
		return "bob", "ws-alpha"
	case "estimate_foreign":
		return "bob", "ws-beta"
	case "estimate_live":
		return "erin", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func (e estimateEnv) replayEstimate(t *testing.T, name string) {
	t.Helper()
	fx := contract.Load(t, "estimate/"+name)
	who, ws := estimateActor(name)
	rec := e.get(who, ws, fx.Request.Path)
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
}

func TestEstimateMatchesFixtures(t *testing.T) {
	e := newEstimateEnv(t)
	n := 0
	for _, name := range contractFixtureNames(t, "estimate") {
		name = strings.TrimPrefix(name, "estimate/")
		if name == "estimate_billing" { // recorded with RESEARCH_VENDOR_COST=1
			vendorcatalog.SetResearchFlag(func() bool { return true })
		}
		e.replayEstimate(t, name)
		if name == "estimate_billing" {
			vendorcatalog.SetResearchFlag(nil)
		}
		n++
	}
	if n < 15 {
		t.Fatalf("expected the recorded estimate fixtures, got %d", n)
	}
}

func TestEstimateEmptyWaterfallDisablesProviders(t *testing.T) {
	e := newEstimateEnv(t)
	cols := `[{"id":"a","type":"enrichment","target_field":"email","waterfall":[]},
	          {"id":"b","type":"enrichment","target_field":"email","waterfall":null},
	          {"id":"c","type":"enrichment","target_field":"email","provider":"hunter_io"},
	          {"id":"d","type":"lead_field","lead_field":"company"}]`
	e.workbook(t, "wb-t23-wf", cols, 2)
	rec := e.get("alice", "ws-alpha", "/api/workbooks/wb-t23-wf/run/estimate")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Rows      int `json:"rows"`
		Breakdown []struct {
			Column string   `json:"column"`
			Paid   []string `json:"paid_providers"`
		} `json:"breakdown"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Rows != 2 || len(got.Breakdown) != 3 {
		t.Fatalf("%+v", got)
	}
	if got.Breakdown[0].Column != "a" || len(got.Breakdown[0].Paid) != 0 {
		t.Errorf("explicit empty waterfall must disable providers: %+v", got.Breakdown[0])
	}
	if got.Breakdown[1].Column != "b" || len(got.Breakdown[1].Paid) == 0 {
		t.Errorf("null waterfall uses the default chain for the target: %+v", got.Breakdown[1])
	}
	if got.Breakdown[2].Column != "c" || len(got.Breakdown[2].Paid) != 1 || got.Breakdown[2].Paid[0] != "hunter_io" {
		t.Errorf("single provider: %+v", got.Breakdown[2])
	}
}

func TestEstimateSearchTooLong422(t *testing.T) {
	e := newEstimateEnv(t)
	rec := e.get("alice", "ws-alpha", "/api/workbooks/wb-enrich-1/run/estimate?search="+strings.Repeat("a", 501))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "string_too_long") {
		t.Fatalf("%d %.200s", rec.Code, rec.Body.String())
	}
	if rec := e.get("alice", "ws-alpha", "/api/workbooks/wb-enrich-1/run/estimate?search="+strings.Repeat("a", 500)); rec.Code != 200 {
		t.Fatalf("500 chars must pass: %d", rec.Code)
	}
}

func (e estimateEnv) job(t *testing.T, ws, typ, status, wb string) int {
	t.Helper()
	var id int
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO jobs (type, payload, status, priority, retry_count, max_retries, workspace_id, created_at)
			VALUES ($1, $2::json, $3, 0, 0, 3, $4, now() at time zone 'utc') RETURNING id`,
			typ, fmt.Sprintf(`{"workbook_id":%q}`, wb), status, ws).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e estimateEnv) jobState(t *testing.T, ws string, id int) (status string, errText *string) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, error FROM jobs WHERE id=$1`, id).Scan(&status, &errText)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

func (e estimateEnv) wbStatus(t *testing.T, ws, id string) (st string) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM workbooks WHERE id=$1`, id).Scan(&st)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

func (e estimateEnv) post(who, ws, path string) (int, string) {
	hdr := bearer(who)
	hdr["X-Workspace-Id"] = ws
	rec, _ := e.do("POST", path, "", "", hdr)
	return rec.Code, rec.Body.String()
}

func TestStopCancelsPendingAndRunning(t *testing.T) {
	e := newEstimateEnv(t)
	pending := e.job(t, "ws-alpha", "run_workbook", "pending", "wb-enrich-1")
	running := e.job(t, "ws-alpha", "run_workbook", "processing", "wb-enrich-1")
	done := e.job(t, "ws-alpha", "run_workbook", "completed", "wb-enrich-1")
	other := e.job(t, "ws-alpha", "run_workbook", "pending", "wb-enrich-live")
	notRun := e.job(t, "ws-alpha", "source_workbook", "pending", "wb-enrich-1")
	code, body := e.post("erin", "ws-alpha", "/api/workbooks/wb-enrich-1/stop")
	if code != 200 || strings.TrimSpace(body) != `{"status":"paused"}` {
		t.Fatalf("%d %s", code, body)
	}
	for _, id := range []int{pending, running} {
		if st, msg := e.jobState(t, "ws-alpha", id); st != "cancelled" || msg == nil || *msg != "stopped by user" {
			t.Errorf("job %d: %s %v", id, st, msg)
		}
	}
	for id, want := range map[int]string{done: "completed", other: "pending", notRun: "pending"} {
		if st, _ := e.jobState(t, "ws-alpha", id); st != want {
			t.Errorf("job %d: %s, want %s untouched", id, st, want)
		}
	}
	if st := e.wbStatus(t, "ws-alpha", "wb-enrich-1"); st != "paused" {
		t.Errorf("workbook status %q", st)
	}
	if st := e.wbStatus(t, "ws-alpha", "wb-enrich-live"); st != "idle" {
		t.Errorf("other workbook status %q", st)
	}
}

func TestStopForeignWorkbook404(t *testing.T) {
	e := newEstimateEnv(t)
	pending := e.job(t, "ws-alpha", "run_workbook", "pending", "wb-enrich-1")
	// a beta job that names the alpha workbook id must stay untouched as well
	betaJob := e.job(t, "ws-beta", "run_workbook", "pending", "wb-enrich-1")
	for _, who := range []string{"bob"} {
		code, body := e.post(who, "ws-beta", "/api/workbooks/wb-enrich-1/stop")
		if code != 404 || !strings.Contains(body, "Workbook not found") {
			t.Fatalf("%s: %d %s", who, code, body)
		}
	}
	if code, _ := e.post("alice", "ws-alpha", "/api/workbooks/does-not-exist/stop"); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
	if st, _ := e.jobState(t, "ws-alpha", pending); st != "pending" {
		t.Errorf("alpha job %s", st)
	}
	if st, _ := e.jobState(t, "ws-beta", betaJob); st != "pending" {
		t.Errorf("beta job %s", st)
	}
	if st := e.wbStatus(t, "ws-alpha", "wb-enrich-1"); st != "idle" {
		t.Errorf("status %q", st)
	}
}

func TestStopViewerForbidden(t *testing.T) {
	e := newEstimateEnv(t)
	pending := e.job(t, "ws-alpha", "run_workbook", "pending", "wb-enrich-1")
	code, body := e.post("bob", "ws-alpha", "/api/workbooks/wb-enrich-1/stop")
	if code != 403 {
		t.Fatalf("%d %s", code, body)
	}
	if st, _ := e.jobState(t, "ws-alpha", pending); st != "pending" {
		t.Errorf("job %s", st)
	}
	if st := e.wbStatus(t, "ws-alpha", "wb-enrich-1"); st != "idle" {
		t.Errorf("status %q", st)
	}
	hdr := map[string]string{}
	if rec, _ := e.do("POST", "/api/workbooks/wb-enrich-1/stop", "", "", hdr); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
}

func TestProviderStatsMatchesFixture(t *testing.T) {
	e := newEstimateEnv(t)
	for _, name := range []string{"provider_stats", "provider_stats_editor", "provider_stats_no_token"} {
		fx := contract.Load(t, "meta/"+name)
		who, ws := metaActor(name)
		hdr := map[string]string{}
		if who != "" {
			hdr = bearer(who)
			hdr["X-Workspace-Id"] = ws
		}
		rec, got := e.do("GET", fx.Request.Path, "", "", hdr)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
}

func TestProviderStatsTieOrder(t *testing.T) {
	e := newEstimateEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `DELETE FROM provider_stats`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO provider_stats (provider, field, attempts, hits, total_confidence, total_latency_ms, total_cost_usd)
		VALUES ('p_first','f',5,1,0.5,50,0.1), ('p_big','f',9,1,0.5,50,0.1), ('p_second','f',5,1,0.5,50,0.1), ('p_third','f',5,0,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	rec := e.get("alice", "ws-alpha", "/api/workbooks/meta/provider-stats")
	var got struct {
		Stats []struct {
			Provider string `json:"provider"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range got.Stats {
		order = append(order, s.Provider)
	}
	if strings.Join(order, ",") != "p_big,p_first,p_second,p_third" {
		t.Errorf("order %v: attempts desc, ties in insertion order", order)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"stats":[`) {
		t.Errorf("body %.80s", rec.Body.String())
	}
}
