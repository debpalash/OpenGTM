//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// cleanMisc removes the global keys the perf / enrichment-provider routes write.
func (e settingsEnv) cleanMisc(t *testing.T) {
	t.Helper()
	del := func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM app_settings WHERE key LIKE 'ENRICH\_%' OR key IN ('HUNTER_API_KEY','APOLLO_API_KEY')`)
	}
	del()
	t.Cleanup(del)
}

func (e settingsEnv) expect(t *testing.T, method, path, body string, hdr map[string]string, fixture string, opts ...contract.Option) {
	t.Helper()
	fx := contract.Load(t, "settings/"+fixture)
	_, got := e.do(method, path, "application/json", body, hdr)
	contract.AssertEqual(t, got, fx.Response, opts...)
}

func TestPerfRoutesMatchFixtures(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanMisc(t)
	_, admin := e.seedAdmin(t, "t6-adm")
	e.seed(t, "t6-plain", pythonHash, true, "user", false)
	plain := bearer("t6-plain")

	e.expect(t, "GET", "/api/settings/enrichment-perf", "", plain, "perf_get_defaults")
	e.expect(t, "PUT", "/api/settings/enrichment-perf",
		`{"row_concurrency":999,"provider_workers":0,"provider_timeout":50,"max_providers":null,"retry_passes":-4}`, admin, "perf_put_clamp")
	e.expect(t, "GET", "/api/settings/enrichment-perf", "", plain, "perf_get_after_put")
	e.expect(t, "PUT", "/api/settings/enrichment-perf", `{"row_concurrency":3}`, plain, "perf_put_non_admin")
	if v, _ := e.raw(t, "ENRICH_ROW_CONCURRENCY"); v != "64" {
		t.Fatalf("non-admin PUT changed the setting: %q", v)
	}
	if _, got := e.do("GET", "/api/settings/enrichment-perf", "", "", nil); got.Status != 401 {
		t.Fatalf("no token: %d", got.Status)
	}
}

func TestPerfPutValidation(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanMisc(t)
	_, admin := e.seedAdmin(t, "t6-adm2")
	rec, _ := e.put(t, admin, "/api/settings/enrichment-perf", `{"row_concurrency":"abc"}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"int_parsing"`) || !strings.Contains(rec.Body.String(), `"row_concurrency"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.put(t, admin, "/api/settings/enrichment-perf", `{"row_concurrency":"7","provider_workers":9.0}`)
	if rec.Code != 200 {
		t.Fatalf("lax ints: %d %s", rec.Code, rec.Body.String())
	}
	if v, _ := e.raw(t, "ENRICH_ROW_CONCURRENCY"); v != "7" {
		t.Fatalf("stored %q", v)
	}
	rec, _ = e.put(t, admin, "/api/settings/enrichment-perf", `{}`)
	if rec.Code != 200 {
		t.Fatalf("empty body: %d", rec.Code)
	}
}

func TestSystemRouteMatchesFixture(t *testing.T) {
	e := newSettingsEnv(t)
	e.seed(t, "t6-plain2", pythonHash, true, "user", false)
	e.expect(t, "GET", "/api/settings/system", "", bearer("t6-plain2"), "system_get")
}

func TestModelsRoutesMatchFixtures(t *testing.T) {
	e := newSettingsEnv(t)
	e.seed(t, "t6-plain3", pythonHash, true, "user", false)
	h := bearer("t6-plain3")
	e.expect(t, "GET", "/api/settings/models/anthropic", "", h, "models_anthropic")
	e.expect(t, "GET", "/api/settings/models/google_ai", "", h, "models_google_ai")
	e.expect(t, "GET", "/api/settings/models/github_models?free_only=true", "", h, "models_github_models")
	e.expect(t, "GET", "/api/settings/models/nope", "", h, "models_unknown")
	rec, _ := e.do("GET", "/api/settings/models/groq?free_only=maybe", "", "", h)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "bool_parsing") {
		t.Fatalf("bad free_only: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEnrichmentProvidersRoutesMatchFixtures(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanMisc(t)
	_, admin := e.seedAdmin(t, "t6-adm3")
	e.seed(t, "t6-plain4", pythonHash, true, "user", false)
	plain := bearer("t6-plain4")

	e.expect(t, "GET", "/api/settings/enrichment-providers", "", plain, "enrichment_providers_before")
	e.expect(t, "PUT", "/api/settings/enrichment-providers/hunter_io", `{"api_key":"hunter-contract-key-1234567890"}`, admin, "enrichment_provider_put_ok")
	e.expect(t, "PUT", "/api/settings/enrichment-providers/apollo_io", `{"api_key":"short-key"}`, admin, "enrichment_provider_put_short_key")
	e.expect(t, "GET", "/api/settings/enrichment-providers", "", plain, "enrichment_providers_after")
	e.expect(t, "PUT", "/api/settings/enrichment-providers/nope", `{"api_key":"x"}`, admin, "enrichment_provider_put_unknown")
	e.expect(t, "PUT", "/api/settings/enrichment-providers/hunter_io", `{"api_key":"x"}`, plain, "enrichment_provider_put_non_admin")

	raw, ok := e.raw(t, "HUNTER_API_KEY")
	if !ok || !strings.HasPrefix(raw, "enc:") || strings.Contains(raw, "hunter-contract") {
		t.Fatalf("HUNTER_API_KEY must be stored as ciphertext, got %q", raw)
	}
}

// --- llm-usage -----------------------------------------------------------------

func (e settingsEnv) usageWorkspace(t *testing.T, ws, user string) map[string]string {
	t.Helper()
	ctx := context.Background()
	uid, hdr := e.seedAdmin(t, user)
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::varchar,$1::text,$1,$2)`, ws, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, ws, uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(c, `DELETE FROM llm_usage_daily WHERE workspace_id=$1`, ws)
			return nil
		})
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_members WHERE workspace_id=$1`, ws)
		_, _ = e.pool.Exec(c, `DELETE FROM workspaces WHERE id=$1`, ws)
	})
	h := map[string]string{"X-Workspace-Id": ws}
	for k, v := range hdr {
		h[k] = v
	}
	return h
}

func (e settingsEnv) insertUsage(t *testing.T, ws, provider, date string, calls, tokens, limit, remaining int, reset string) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO llm_usage_daily (workspace_id, provider, model, date, calls, total_tokens, rate_limit, rate_remaining, rate_reset, updated_at)
			VALUES ($1,$2,'m1',$3,$4,$5,$6,$7,$8,'2026-01-01T00:00:00')`, ws, provider, date, calls, tokens, limit, remaining, reset)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMUsageEmptyShape(t *testing.T) {
	e := newSettingsEnv(t)
	h := e.usageWorkspace(t, "t6-ws-empty", "t6-usr-empty")
	e.expect(t, "GET", "/api/settings/llm-usage", "", h, "llm_usage_empty")
	e.expect(t, "GET", "/api/settings/llm-usage?date=2026-01-02", "", h, "llm_usage_with_date")
	_, got := e.do("GET", "/api/settings/llm-usage", "", "", nil)
	contract.AssertEqual(t, got, contract.Load(t, "settings/llm_usage_no_auth").Response)
}

func TestLLMUsageTenantIsolation(t *testing.T) {
	e := newSettingsEnv(t)
	ha := e.usageWorkspace(t, "t6-ws-a", "t6-usr-a")
	hb := e.usageWorkspace(t, "t6-ws-b", "t6-usr-b")
	e.insertUsage(t, "t6-ws-a", "groq", "2026-03-01", 250, 900, 0, 0, "")
	e.insertUsage(t, "t6-ws-a", "openrouter", "2026-03-01", 10, 20, 200, 150, "10s")
	e.insertUsage(t, "t6-ws-a", "groq", "2026-03-02", 5, 50, 0, 0, "")
	e.insertUsage(t, "t6-ws-b", "groq", "2026-03-01", 4000, 4000, 0, 0, "")

	type usage struct {
		Providers []struct {
			Provider   string `json:"provider"`
			Model      string `json:"model"`
			Calls      int    `json:"calls"`
			Tokens     int    `json:"tokens"`
			DailyLimit int    `json:"daily_limit"`
			LimitLabel string `json:"limit_label"`
			Remaining  int    `json:"remaining"`
			Pct        int    `json:"pct"`
			RateReset  string `json:"rate_reset"`
			Live       bool   `json:"live"`
		} `json:"providers"`
		TodayCalls    int `json:"today_calls"`
		TodayTokens   int `json:"today_tokens"`
		AllTimeCalls  int `json:"all_time_calls"`
		AllTimeTokens int `json:"all_time_tokens"`
	}
	get := func(h map[string]string, q string) usage {
		_, got := e.do("GET", "/api/settings/llm-usage"+q, "", "", h)
		if got.Status != 200 {
			t.Fatalf("status %d %s", got.Status, got.Body)
		}
		var u usage
		if err := json.Unmarshal(got.Body, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	a := get(ha, "?date=2026-03-01")
	if len(a.Providers) != 2 || a.Providers[0].Provider != "groq" || a.Providers[0].Calls != 250 || a.Providers[0].LimitLabel != "1K RPD" ||
		a.Providers[0].Pct != 25 || a.Providers[0].Remaining != 750 || a.Providers[0].Live || a.Providers[0].Model != "m1" || a.Providers[0].Tokens != 900 {
		t.Fatalf("a rows: %+v", a.Providers)
	}
	if p := a.Providers[1]; p.Provider != "openrouter" || !p.Live || p.LimitLabel != "200 RPD (live)" || p.Remaining != 150 || p.Pct != 5 || p.RateReset != "10s" {
		t.Fatalf("a live row: %+v", p)
	}
	if a.TodayCalls != 260 || a.TodayTokens != 920 || a.AllTimeCalls != 265 || a.AllTimeTokens != 970 {
		t.Fatalf("a totals: %+v", a)
	}
	b := get(hb, "?date=2026-03-01")
	if len(b.Providers) != 1 || b.Providers[0].Calls != 4000 || b.Providers[0].Pct != 100 || b.AllTimeCalls != 4000 {
		t.Fatalf("b: %+v", b)
	}
	if other := get(ha, "?date=2026-03-09"); len(other.Providers) != 0 || other.AllTimeCalls != 265 {
		t.Fatalf("a other date: %+v", other)
	}
	// Default date is today (UTC): rows of another date do not appear.
	if today := get(ha, ""); len(today.Providers) != 0 || today.AllTimeCalls != 265 {
		t.Fatalf("a today: %+v", today)
	}
}

func TestLLMUsageRequiresMembership(t *testing.T) {
	e := newSettingsEnv(t)
	e.usageWorkspace(t, "t6-ws-c", "t6-usr-c")
	e.seed(t, "t6-outsider", pythonHash, true, "user", false)
	h := bearer("t6-outsider")
	h["X-Workspace-Id"] = "t6-ws-c"
	_, got := e.do("GET", "/api/settings/llm-usage", "", "", h)
	if got.Status != 403 {
		t.Fatalf("outsider: %d %s", got.Status, got.Body)
	}
}
