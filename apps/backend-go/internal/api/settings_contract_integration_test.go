//go:build integration

package api

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// replayer replays recorded fixtures against one environment and remembers
// which ones it ran, so a fixture nobody replays fails the suite.
type replayer struct {
	do   func(method, path, body string, hdr map[string]string) contract.Resp
	seen map[string]bool
}

func fixtureBody(fx contract.Fixture) string {
	b := strings.TrimSpace(string(fx.Request.Body))
	if b == "null" {
		return ""
	}
	return b
}

// replay sends the fixture's own request (method, path, body, non-redacted
// headers) with auth as the "<redacted>" Authorization/X-Workspace-Id stand-in.
func (rp *replayer) replay(t *testing.T, name string, auth map[string]string, opts ...contract.Option) {
	t.Helper()
	fx := contract.Load(t, name)
	rp.seen[name] = true
	hdr := map[string]string{}
	for k, v := range fx.Request.Headers {
		if v == "<redacted>" {
			continue
		}
		hdr[k] = v
	}
	if _, redacted := fx.Request.Headers["Authorization"]; redacted {
		for k, v := range auth {
			hdr[k] = v
		}
	}
	got := rp.do(fx.Request.Method, fx.Request.Path, fixtureBody(fx), hdr)
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
}

// assertAllReplayed fails when a fixture file in areas was never replayed.
func (rp *replayer) assertAllReplayed(t *testing.T, areas ...string) {
	t.Helper()
	var missing []string
	for _, a := range areas {
		for _, n := range contractFixtureNames(t, a) {
			if !rp.seen[n] {
				missing = append(missing, n)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("fixtures never replayed: %v", missing)
	}
}

func TestSettingsRoutesMatchRecordedFixtures(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanMisc(t)
	rp := &replayer{seen: map[string]bool{}, do: func(m, p, b string, h map[string]string) contract.Resp {
		_, got := e.do(m, p, "application/json", b, h)
		return got
	}}
	_, alice := e.seedAdmin(t, "ct-alice")
	e.seed(t, "ct-bob", pythonHash, true, "user", false)
	bob := bearer("ct-bob")
	ignoreTS := contract.IgnorePaths("$.created_at")

	// providers: stateful, order matters (admin checks, then a write, then the masked list).
	rp.replay(t, "settings/providers_list", bob, ignoreTS)
	rp.replay(t, "settings/provider_put_unknown", alice, ignoreTS)
	rp.replay(t, "settings/provider_put_non_admin", bob, ignoreTS)
	rp.replay(t, "settings/provider_put_no_auth", nil, ignoreTS)
	rp.replay(t, "settings/provider_test_unconfigured", alice, ignoreTS)
	rp.replay(t, "settings/provider_test_unknown", alice, ignoreTS)
	rp.replay(t, "settings/provider_test_non_admin", bob, ignoreTS)
	rp.replay(t, "settings/provider_put_ok", alice, ignoreTS)
	rp.replay(t, "settings/provider_put_short_key", alice, ignoreTS)
	rp.replay(t, "settings/providers_list_after", bob, ignoreTS)

	// enrichment perf
	rp.replay(t, "settings/perf_get_defaults", bob, ignoreTS)
	rp.replay(t, "settings/perf_put_clamp", alice, ignoreTS)
	rp.replay(t, "settings/perf_get_after_put", bob, ignoreTS)
	rp.replay(t, "settings/perf_put_non_admin", bob, ignoreTS)

	// system and models (read-only)
	rp.replay(t, "settings/system_get", bob, ignoreTS)
	for _, n := range []string{"models_anthropic", "models_google_ai", "models_github_models", "models_unknown"} {
		rp.replay(t, "settings/"+n, bob, ignoreTS)
	}

	// enrichment providers
	rp.replay(t, "settings/enrichment_providers_before", bob, ignoreTS)
	rp.replay(t, "settings/enrichment_provider_put_ok", alice, ignoreTS)
	rp.replay(t, "settings/enrichment_provider_put_short_key", alice, ignoreTS)
	rp.replay(t, "settings/enrichment_providers_after", bob, ignoreTS)
	rp.replay(t, "settings/enrichment_provider_put_unknown", alice, ignoreTS)
	rp.replay(t, "settings/enrichment_provider_put_non_admin", bob, ignoreTS)

	// llm usage (tenant table: needs a workspace the caller belongs to)
	usage := e.usageWorkspace(t, "ct-ws-usage", "ct-usage-adm")
	rp.replay(t, "settings/llm_usage_empty", usage, ignoreTS)
	rp.replay(t, "settings/llm_usage_with_date", usage, ignoreTS)
	rp.replay(t, "settings/llm_usage_no_auth", nil, ignoreTS)

	// global and workspace integrations (its own seeded users and workspace)
	for _, n := range replayIntegrationFixtures(t, e) {
		rp.seen[n] = true
	}

	rp.assertAllReplayed(t, "settings", "integrations")
}

type billingCase struct {
	name  string
	setup func(t *testing.T, e mcpEnv, ws string)
}

func TestBillingRoutesMatchRecordedFixtures(t *testing.T) {
	seeded := func(t *testing.T, e mcpEnv, ws string) {
		// Rows equivalent to the ones the fixture recorder inserted (ids differ).
		err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
			ctx := context.Background()
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ($1, 24.87655)`, ws); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO credit_ledger_entries (workspace_id, amount_usd, reason, run_id, idempotency_key, created_at) VALUES
				($1, 25.0, 'stripe_topup', NULL, 'stripe:ct-bal-1', '2026-08-29 10:40:00'),
				($1, -0.12345, 'run_debit', 'run-fixture-1', 'run:ct-bal-1', '2026-08-29 10:41:00')`, ws)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	groups := []struct {
		label string
		cfg   config.APIConfig
		cases []billingCase
	}{
		{"disabled", billingCfg(false, "", ""), []billingCase{
			{name: "balance_empty_disabled"}, {name: "balance_no_token"},
			{name: "checkout_disabled"}, {name: "checkout_amount_numeric_string"},
			{name: "checkout_amount_zero"}, {name: "checkout_amount_negative"}, {name: "checkout_amount_missing"},
			{name: "checkout_amount_not_number"}, {name: "checkout_amount_null"}, {name: "checkout_url_not_string"},
			{name: "webhook_disabled"},
		}},
		{"enabled_no_key", billingCfg(true, "", ""), []billingCase{
			{name: "balance_empty_enabled"}, {name: "balance_with_entries", setup: seeded},
			{name: "checkout_unconfigured"}, {name: "webhook_unconfigured"},
		}},
		{"enabled_with_key", billingCfg(true, "sk_test_x", ""), []billingCase{
			{name: "webhook_bad_signature"}, {name: "webhook_no_signature"},
		}},
	}
	seen := map[string]bool{}
	for _, g := range groups {
		g := g
		// One subtest per config: each environment holds the shared DB lock until its test ends.
		t.Run(g.label, func(t *testing.T) {
			ws, usr := "ct-bill-ws-"+g.label, "ct-bill-adm-"+g.label
			e := newBillingEnv(t, g.cfg)
			_, hdr := e.admin(t, ws, usr)
			e.cleanCredits(t, ws)
			rp := &replayer{seen: seen, do: func(m, p, b string, h map[string]string) contract.Resp {
				_, got := e.do(m, p, "application/json", b, h)
				return got
			}}
			for _, c := range g.cases {
				if c.setup != nil {
					c.setup(t, e, ws)
				}
				rp.replay(t, "billing/"+c.name, hdr, contract.IgnorePaths("$.workspace_id",
					"$.entries[0].id", "$.entries[1].id", "$.entries[0].workspace_id", "$.entries[1].workspace_id"))
			}
		})
	}
	(&replayer{seen: seen}).assertAllReplayed(t, "billing")
}
