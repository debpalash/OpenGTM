package api

import (
	"bytes"
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Plan 09 coverage (Task 26): the 22 /api/outreach routes are ported, POST /api/campaigns/generate (LLM) is
// retained on Python (D1), every ported route has a recorded fixture, and the deviations are written down.

const retainedOutreachFile = "../../testdata/contract/retained_outreach_routes.txt"

func outreachGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Identity: identity.NewStore(pool),
		Flags: config.Flags{PGLeadStore: true, Outreach: config.OutreachSettings{GoRoutesEnabled: true}}}
	r := httpx.NewRouter()
	RegisterOutreach(r, d)
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

func readRetainedOutreach(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(retainedOutreachFile)
	if err != nil {
		t.Fatalf("retained routes list missing: %v", err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out[l] = true
		}
	}
	return out
}

// pythonOutreachRoutes parses routers/outreach.py (prefix /api/outreach) and routers/campaigns.py (/api/campaigns).
func pythonOutreachRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, name := range []string{"outreach.py", "campaigns.py"} {
		b, err := os.ReadFile(testutil.LegacyPath(t, "apps", "api", "routers", name))
		if err != nil {
			t.Skipf("legacy Python tree not present (%v)", err)
		}
		src := string(b)
		prefixes := map[string]string{}
		for _, m := range pyRouterDecl.FindAllStringSubmatch(src, -1) {
			prefixes[m[1]] = ""
			if pm := pyPrefix.FindStringSubmatch(m[2]); pm != nil {
				prefixes[m[1]] = pm[1]
			}
		}
		for _, m := range pyRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on router %q without a router declaration", name, m[1])
			}
			routes = append(routes, strings.ToUpper(m[2])+" "+pre+m[3])
		}
	}
	sort.Strings(routes)
	return routes
}

func TestEveryOutreachRouteIsPortedOrRetained(t *testing.T) {
	routes := pythonOutreachRoutes(t)
	ported := outreachGoPatterns(t)
	retained := readRetainedOutreach(t)
	// POST /api/campaigns/generate moved to llmroutes.go (WP-LLMROUTES, behind GO_LLM_ROUTES_ENABLED).
	for p := range llmRoutePatterns(t) {
		if strings.HasPrefix(p, "POST /api/campaigns/") {
			ported[p] = true
		}
	}
	if len(routes) != 23 || len(ported) != 23 || len(retained) != 0 {
		t.Fatalf("want 23 Python routes = 23 ported + 0 retained, got %d = %d + %d", len(routes), len(ported), len(retained))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r] = true
		if !ported[r] && !retained[r] {
			t.Errorf("Python route in neither the Go router nor the retained list: %s", r)
		}
		if ported[r] && retained[r] {
			t.Errorf("route both ported and retained: %s", r)
		}
	}
	for r := range ported {
		if !seen[r] {
			t.Errorf("Go route with no Python counterpart: %s", r)
		}
	}
	for r := range retained {
		if !seen[r] {
			t.Errorf("stale retained entry: %s", r)
		}
	}
}

func TestRetainedOutreachRoutesNotRegisteredInGo(t *testing.T) {
	ported := outreachGoPatterns(t)
	retained := readRetainedOutreach(t)
	if retained["POST /api/campaigns/generate"] {
		t.Error("POST /api/campaigns/generate moved to llmroutes.go (WP-LLMROUTES); it must not stay in the retained list")
	}
	for r := range retained {
		if ported[r] {
			t.Errorf("retained route %s is registered in Go", r)
		}
	}
	for p := range ported {
		if strings.Contains(p, "/api/campaigns") {
			t.Errorf("RegisterOutreach registers a campaigns route (llmroutes.go owns it): %s", p)
		}
	}
}

func TestEveryOutreachRouteHasFixtureOrIsRetained(t *testing.T) {
	ported := outreachGoPatterns(t)
	retained := readRetainedOutreach(t)
	rest := map[string]bool{}
	for p := range ported {
		if !retained[p] {
			rest[p] = true
		}
	}
	assertFixturesCover(t, rest, fixtureRequests(t, "outreach"))
}

// send and outreach_inbound_poll are claimable and handled by the Go worker unconditionally (the plan 15 cutover
// retired the cohort flags and the Python worker).
func TestEveryOutreachJobTypeHandledByGoWorker(t *testing.T) {
	const file = "outreach.go"
	allowed, handled := workerJobTypes(t, file, map[string]string{"jobSend": "send", "outreachinbound.JobType": "outreach_inbound_poll"})
	assertWorkerHandlesExactly(t, file, allowed, handled, "send", "outreach_inbound_poll")
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The seeded (TEST-ONLY) outreach secrets must appear neither in the committed fixtures nor in what the public
// routes log when they refuse a request.
func TestNoOutreachSecretInFixturesOrLogs(t *testing.T) {
	secrets := []string{pubMasterKey, pubWebhookSecret, "ctk_outreach_smtp_password_0001", "ctk_outreach_imap_password_0001"}
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "contract", "outreach", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no outreach fixtures (%v)", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains a seeded secret", f)
			}
		}
	}
	var logBuf bytes.Buffer
	d := pubDeps(pubOpts{webhookToken: pubWebhookSecret})
	d.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := pubHandler(d)
	for _, q := range []pubReq{
		{method: "POST", path: "/api/outreach/webhooks/bounce", body: `{"message_id":"m","event":"bounce"}`,
			hdr: map[string]string{"X-Webhook-Secret": "wrong", "Content-Type": "application/json"}},
		{method: "POST", path: "/api/outreach/webhooks/bounce", body: `{"message_id":"m"}`,
			hdr: map[string]string{"X-Webhook-Secret": pubWebhookSecret + "x", "Content-Type": "application/json"}},
		{method: "POST", path: "/api/outreach/unsubscribe?token=bad.token"},
		{method: "GET", path: "/api/outreach/unsubscribe?token=bad.token"},
	} {
		_ = q.do(h)
	}
	for _, s := range secrets {
		if strings.Contains(logBuf.String(), s) {
			t.Errorf("captured log contains a seeded secret %q", s[:8])
		}
	}
}

// The deviations the runbook and README promise operators must be written down.
func TestOutreachDeviationsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../testdata/contract/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan 09 (outreach", "plaintext", "SECURITY DEFINER", "TRUST_PROXY_CIDRS", "outreach_send_by_message_id",
		"UTC session", "114", "unlock", "GO_OUTREACH_ROUTES_ENABLED"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("README missing plan 09 deviation text %q", want)
		}
	}
	rb, err := os.ReadFile("../../../../docs/go-port/runbooks/outreach-cutover.md")
	if err != nil {
		t.Fatalf("runbook missing: %v", err)
	}
	for _, want := range []string{"SECRETS_MASTER_KEY", "OUTREACH_PUBLIC_BASE_URL", "OUTREACH_BOUNCE_WEBHOOK_SECRET", "AUTOMATIONS_ENABLED",
		"GO_OUTREACH_ROUTES_ENABLED", "GO_EXECUTOR_OUTREACH_INBOUND", "GO_EXECUTOR_OUTREACH_SEND", "EGRESS_ALLOW_HOSTS", "Rollback"} {
		if !strings.Contains(string(rb), want) {
			t.Errorf("runbook missing %q", want)
		}
	}
}
