//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// newAnalyticsEnv is the leads environment (18 seeded leads, alice/bob, ws-alpha/ws-beta) with the analytics
// routes mounted and erin, a user in no workspace, added for the 403 fixture.
func newAnalyticsEnv(t *testing.T) leadsEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	r := httpx.NewRouter()
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	RegisterAnalytics(r, d)
	e.h = r.Handler()
	return e
}

func analyticsActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case strings.HasSuffix(name, "_beta_bob"):
		return "bob", "ws-beta"
	case strings.HasSuffix(name, "_viewer_bob_alpha"):
		return "bob", "ws-alpha"
	case strings.HasSuffix(name, "_nonmember_erin_beta"):
		return "erin", "ws-beta"
	}
	return "alice", ""
}

// sortUnorderedLists orders by_city by (count desc, city) and source_quality by source on both sides: Python's
// GROUP BY has no ORDER BY for sources and leaves equal city counts in heap order, while Go orders sources by name and
// breaks city ties by name (documented deviation).
func sortUnorderedLists(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		City     *string `json:"city"`
		Count    int     `json:"count"`
		AvgScore *int    `json:"avg_score"`
	}
	if err := json.Unmarshal(m["by_city"], &rows); err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		a, b := "", ""
		if rows[i].City != nil {
			a = *rows[i].City
		}
		if rows[j].City != nil {
			b = *rows[j].City
		}
		return a < b
	})
	m["by_city"], _ = json.Marshal(rows)
	var src []map[string]any
	if err := json.Unmarshal(m["source_quality"], &src); err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(src, func(i, j int) bool { return src[i]["source"].(string) < src[j]["source"].(string) })
	m["source_quality"], _ = json.Marshal(src)
	out, _ := json.Marshal(m)
	return out
}

func TestAnalyticsFixturesMatchPython(t *testing.T) {
	e := newAnalyticsEnv(t)
	replayed := 0
	for _, full := range contractFixtureNames(t, "analytics") {
		name := strings.TrimPrefix(full, "analytics/")
		fx := contract.Load(t, full)
		who, ws := analyticsActor(name)
		rec := e.get(who, ws, fx.Request.Path)
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		replayed++
		if strings.HasPrefix(name, "enrichment_") && rec.Code == 200 {
			got.Body, fx.Response.Body = sortUnorderedLists(t, got.Body), sortUnorderedLists(t, fx.Response.Body)
		}
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if replayed != 25 {
		t.Fatalf("only %d analytics fixtures replayed", replayed)
	}
}

// TestOverviewMatchesFixture pins the overview key order, which the order-insensitive comparator cannot see.
func TestOverviewMatchesFixture(t *testing.T) {
	e := newAnalyticsEnv(t)
	rec := e.get("alice", "ws-alpha", "/api/analytics/overview")
	body := rec.Body.String()
	order := []string{"total_leads", "leads_this_week", "leads_this_month", "avg_score", "tiers", "enrichment",
		"email_confidence", "jobs", "success_rate"}
	last := -1
	for _, k := range order {
		i := strings.Index(body, `"`+k+`"`)
		if i <= last {
			t.Fatalf("key %q out of order in %s", k, body)
		}
		last = i
	}
	if !strings.Contains(body, `"email_pct":66.7`) || !strings.Contains(body, `"phone_pct":40.0`) {
		t.Errorf("pct formatting: %s", body)
	}
}

func TestOverviewEmptyWorkspaceUsesIntZero(t *testing.T) {
	e := newAnalyticsEnv(t)
	e.purge()
	rec := e.get("alice", "ws-alpha", "/api/analytics/overview")
	want := `{"total_leads":0,"leads_this_week":0,"leads_this_month":0,"avg_score":0.0,"tiers":{},"enrichment":{"total":0,` +
		`"with_email":0,"with_phone":0,"with_website":0,"with_contact":0,"with_linkedin":0,"email_pct":0,"phone_pct":0,` +
		`"website_pct":0,"contact_pct":0},"email_confidence":{},"jobs":{"total":0,"completed":0,"failed":0,"success_rate":0}}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

// TestOtherRoutesEmptyShapesMatchFixtures: key order of the degraded sections (the comparator ignores it).
func TestOtherRoutesEmptyShapesMatchFixtures(t *testing.T) {
	e := newAnalyticsEnv(t)
	for path, want := range map[string]string{
		"/api/analytics/collection": `{"jobs_by_day":[],"leads_by_day":[],"avg_leads_per_job":0}`,
		"/api/analytics/llm":        `{"by_day":[],"by_provider":[],"total_tokens":0,"total_calls":0}`,
	} {
		if rec := e.get("alice", "ws-alpha", path); rec.Code != 200 || rec.Body.String() != want {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := e.get("alice", "ws-alpha", "/api/analytics/pipeline")
	if !strings.HasSuffix(rec.Body.String(), `,"status_tiers":{}}`) || !strings.HasPrefix(rec.Body.String(), `{"statuses":{`) {
		t.Errorf("pipeline: %s", rec.Body.String())
	}
	rec = e.get("alice", "ws-alpha", "/api/analytics/enrichment")
	if !strings.Contains(rec.Body.String(), `{"city":"Mumbai","count":4,"avg_score":null}`) ||
		!strings.Contains(rec.Body.String(), `"source":"import","count":5,"avg_score":null,"with_email":null,"with_phone":null,"with_contact":null`) ||
		!strings.HasSuffix(rec.Body.String(), `"score_distribution":[]}`) {
		t.Errorf("enrichment: %s", rec.Body.String())
	}
}

// A NULL city is a JSON null (Python None) and a NULL/empty source is "unknown".
func TestEnrichmentNullCityAndSource(t *testing.T) {
	e := newAnalyticsEnv(t)
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-beta", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE leads SET city = NULL, source = NULL WHERE id = (SELECT min(id) FROM leads)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := e.get("bob", "ws-beta", "/api/analytics/enrichment").Body.String()
	if !strings.Contains(body, `{"city":null,"count":1,"avg_score":null}`) || !strings.Contains(body, `"source":"unknown","count":1`) {
		t.Errorf("%s", body)
	}
}

func TestAnalyticsTenantIsolation(t *testing.T) {
	e := newAnalyticsEnv(t)
	var alpha, beta struct {
		Total int `json:"total_leads"`
	}
	_ = json.Unmarshal(e.get("alice", "ws-alpha", "/api/analytics/overview").Body.Bytes(), &alpha)
	_ = json.Unmarshal(e.get("bob", "ws-beta", "/api/analytics/overview").Body.Bytes(), &beta)
	if alpha.Total != 15 || beta.Total != 3 {
		t.Fatalf("alpha %d beta %d (want 15, 3)", alpha.Total, beta.Total)
	}
	e.purge()
	var rec *httptest.ResponseRecorder = e.get("bob", "ws-beta", "/api/analytics/pipeline")
	if rec.Body.String() != `{"statuses":{},"status_tiers":{}}` {
		t.Errorf("after purge: %s", rec.Body.String())
	}
}

func TestAnalyticsRequiresWorkspace(t *testing.T) {
	e := newAnalyticsEnv(t)
	for _, p := range []string{"overview", "pipeline", "collection", "enrichment", "llm"} {
		path := "/api/analytics/" + p
		if rec := e.get("", "", path); rec.Code != 401 {
			t.Errorf("%s without token: %d", p, rec.Code)
		}
		if rec := e.get("erin", "ws-beta", path); rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace access denied") {
			t.Errorf("%s non-member: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}
