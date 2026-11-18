//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Issue 114 closure (MCP production wiring): enrich_company through the runtime openRuntime assembles for the shipping
// binary (real pool, real secrets resolver, real rpclocal provider caller, real website_scraper provider and egress
// guard; only the browser service that fetches pages is a local fake). internal/mcp/tools_enrich_test.go proves the
// tool against a mocked port; this proves the port production injects runs the configured chain.
func TestEnrichCompanyRunsConfiguredChainThroughProductionRuntime(t *testing.T) {
	s := seed(t)
	ctx := context.Background()

	// website_scraper fetches pages through the browser service (tier 2 "impersonate"); this fake serves the site's
	// HTML and records every render request.
	const siteURL = "http://wired-co.example.org"
	var hits, misrouted atomic.Int32
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/render" || r.Header.Get("Authorization") != "Bearer t114-browser-token" {
			misrouted.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "engine": "impersonate", "html": `<html><head><title>Wired Co</title></head><body><h1>Wired Co</h1>
			<p>Email <a href="mailto:sales@wired-co.example.org">sales@wired-co.example.org</a> phone +1 415 555 0142</p>
			<a href="https://www.linkedin.com/company/wired-co">LinkedIn</a></body></html>`})
	}))
	t.Cleanup(browser.Close)

	// A second workspace owning a lead with the same site: the caller's token must never spend on it.
	sfx := strconv.FormatInt(time.Now().UnixNano(), 10)
	other := "t114-other-" + sfx
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug, owner_id)
		SELECT $1::varchar,$1::text,$1::text, owner_id FROM workspaces WHERE id=$2`, other, s.ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, s.pool, other, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM leads`, `DELETE FROM mcp_audit_log`, `DELETE FROM trigger_cap_reservations`} {
				_, _ = tx.Exec(ctx, q)
			}
			return nil
		})
		_, _ = s.pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, other)
	})
	addLead := func(ws, company string) int64 {
		t.Helper()
		var id int64
		if err := tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, website, source, created_at, updated_at)
				VALUES ($1,$2,$3,'t114','2026-08-29T10:00:00','2026-08-29T10:00:00') RETURNING id`, ws, company, siteURL).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mine, mine2, foreign := addLead(s.ws, "Wired Co"), addLead(s.ws, "Wired Two"), addLead(other, "Foreign Co")

	env := map[string]string{
		"DATABASE_URL":           appURL(t),
		"APP_ENV":                "test",
		"MCP_MAX_ENRICH_PER_DAY": "1",
		"BROWSER_SERVICE_URL":    browser.URL,
		"BROWSER_SERVICE_TOKEN":  "t114-browser-token",
	}
	get := func(k string) string { return env[k] }
	cfg, err := config.LoadMCPFrom(get)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := openRuntime(ctx, cfg, get)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	t.Cleanup(rt.close)

	call := func(ws string, lead int64) string {
		t.Helper()
		raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"enrich_company","arguments":{"lead_id":%d}}}`, lead)
		resp, ok := rt.srv.Handle(ctx, mcp.Ctx{WorkspaceID: ws, Capabilities: map[string]bool{"leads:read": true}}, []byte(raw))
		if !ok {
			t.Fatal("no response")
		}
		var r struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(resp, &r); err != nil || len(r.Result.Content) != 1 {
			t.Fatalf("bad response %s: %v", resp, err)
		}
		return r.Result.Content[0].Text
	}
	count := func(ws, q string) int {
		t.Helper()
		var n int
		if err := tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error { return tx.QueryRow(ctx, q).Scan(&n) }); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 1. Tenant denial happens before any spend: no page fetch, no cap slot, no accounting in either workspace.
	if got := call(s.ws, foreign); !strings.Contains(got, "Lead not found") {
		t.Fatalf("cross-tenant lead: %s", got)
	}
	if hits.Load() != 0 {
		t.Fatalf("cross-tenant enrichment rendered %d pages", hits.Load())
	}
	for _, ws := range []string{s.ws, other} {
		if n := count(ws, `SELECT count(*) FROM trigger_cap_reservations WHERE trigger_id='__mcp_enrich__'`); n != 0 {
			t.Fatalf("%s holds %d enrich reservations after a denied call", ws, n)
		}
	}

	// 2. The configured chain runs: website_scraper (first provider of the email, phone and linkedin chains)
	// produced the email and linkedin_url fields over the production provider caller, and the call is accounted once.
	got := call(s.ws, mine)
	var out struct {
		Fields   map[string]string `json:"enriched_fields"`
		Provider string            `json:"provider"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("enrich result %q: %v", got, err)
	}
	if out.Provider != "website_scraper" || out.Fields["email"] != "sales@wired-co.example.org" ||
		!strings.Contains(out.Fields["linkedin_url"], "linkedin.com/company/wired-co") {
		t.Fatalf("chain result %s", got)
	}
	if hits.Load() == 0 || misrouted.Load() != 0 {
		t.Fatalf("browser service renders %d, unauthenticated or misrouted %d", hits.Load(), misrouted.Load())
	}
	if n := count(s.ws, `SELECT count(*) FROM trigger_cap_reservations WHERE trigger_id='__mcp_enrich__' AND state='settled'`); n != 1 {
		t.Fatalf("settled enrich reservations %d, want 1", n)
	}
	if n := count(s.ws, `SELECT count(*) FROM mcp_audit_log WHERE tool_name='enrich_company' AND result_status='ok'`); n != 1 {
		t.Fatalf("ok audit rows %d, want 1", n)
	}

	// 3. MCP_MAX_ENRICH_PER_DAY=1 is enforced on the production path: the next call is refused before any fetch.
	before := hits.Load()
	if got := call(s.ws, mine2); !strings.Contains(got, "mcp enrich cap exceeded") {
		t.Fatalf("second call over the cap: %s", got)
	}
	if hits.Load() != before {
		t.Fatalf("a capped call rendered pages (%d -> %d)", before, hits.Load())
	}
	if n := count(s.ws, `SELECT count(*) FROM mcp_audit_log WHERE tool_name='enrich_company' AND result_status='capped'`); n != 1 {
		t.Fatalf("capped audit rows %d, want 1", n)
	}
	// The cap is per workspace: the other workspace's bucket is untouched.
	if n := count(other, `SELECT count(*) FROM trigger_cap_reservations WHERE trigger_id='__mcp_enrich__'`); n != 0 {
		t.Fatalf("other workspace charged %d enrich slots", n)
	}
}
