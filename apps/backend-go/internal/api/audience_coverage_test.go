package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencesched"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Plan 10 coverage (Task 22): every route of routers/audiences.py and routers/destinations.py is ported (23, one of
// them a router override instead of a mux pattern) or retained on Python (2, D1); every ported route has a recorded
// fixture; both audience job types are dispatched by the Go worker; every destination type has a Go driver or port; no
// seeded secret leaks into fixtures, transcripts or logs; the deviations and the cutover runbook are written down.

const (
	retainedAudienceFile = "../../testdata/contract/retained_audience_routes.txt"
	audienceReadme       = "../../testdata/contract/README.md"
	audienceRunbook      = "../../../../docs/go-port/runbooks/audiences-destinations-cutover.md"
	audienceNginxConf    = "../../../../infra/nginx/go-routes.conf"
	// inboundRoutePattern is the public CRM callback. It overlaps two literal routes, so it is a router override
	// (httpx.Router.Override, destsinbound.go) rather than a mux pattern, but it is still listed in Router.Patterns().
	inboundRoutePattern = "POST /api/audience-destinations/inbound/{destination_id}"
)

type audCovEmitter struct{}

func (audCovEmitter) AudienceEvents(context.Context, pgx.Tx, string, []audiencerefresh.MembershipEvent) (int, error) {
	return 0, nil
}

// audienceGoDeps has every flag on; the pool never connects (lazy, refused address).
func audienceGoDeps(t *testing.T, log *slog.Logger) Deps {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return Deps{Log: log, Pool: pool, Identity: identity.NewStore(pool), AudienceEvents: audCovEmitter{},
		Flags: config.Flags{PGLeadStore: true, Audiences: config.AudienceSettings{GoAudienceRoutesEnabled: true, GoDestRoutesEnabled: true}}}
}

func audienceGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	d := audienceGoDeps(t, nil)
	r := httpx.NewRouter()
	RegisterAudiences(r, d)
	RegisterDestinations(r, d)
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

func readRetainedAudiences(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(retainedAudienceFile)
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

func pythonAPIFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(testutil.LegacyPath(t, append([]string{"apps", "api"}, parts...)...))
	if err != nil {
		t.Skipf("legacy Python tree not present (%v)", err)
	}
	return string(b)
}

// pythonAudienceRoutes parses routers/audiences.py and routers/destinations.py.
func pythonAudienceRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, name := range []string{"audiences.py", "destinations.py"} {
		src := pythonAPIFile(t, "routers", name)
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

func TestEveryAudienceDestinationRouteIsPortedOrRetained(t *testing.T) {
	routes := pythonAudienceRoutes(t)
	patterns := audienceGoPatterns(t)
	retained := readRetainedAudiences(t)
	ported := map[string]bool{}
	for p := range patterns {
		ported[p] = true
	}
	if len(patterns) != 23 || !patterns[inboundRoutePattern] || len(routes) != 25 || len(ported) != 23 || len(retained) != 2 {
		t.Fatalf("want 25 Python routes = 23 ported (22 mux patterns + the inbound override) + 2 retained, got %d = %d (%d patterns) + %d",
			len(routes), len(ported), len(patterns), len(retained))
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

func TestRetainedAudienceRoutesNotRegisteredInGo(t *testing.T) {
	patterns := audienceGoPatterns(t)
	retained := readRetainedAudiences(t)
	for _, want := range []string{"GET /api/audiences/capabilities", "GET /api/audience-destinations/types"} {
		if !retained[want] {
			t.Errorf("%s must be retained on Python (D1)", want)
		}
	}
	for r := range retained {
		if patterns[r] {
			t.Errorf("retained route %s is registered in Go", r)
		}
	}
}

// The inbound callback is a router override listed in Router.Patterns() (not a mux pattern), its overlapping literals
// are not claimed by it, and any other method on its path answers 405 with Allow: POST (I1).
func TestInboundCallbackIsRouterOverride(t *testing.T) {
	d := audienceGoDeps(t, nil)
	r := httpx.NewRouter()
	RegisterDestinations(r, d)
	found := false
	for _, p := range r.Patterns() {
		found = found || p == inboundRoutePattern
	}
	if !found {
		t.Errorf("%s missing from Router.Patterns()", inboundRoutePattern)
	}
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/api/audience-destinations/inbound/dst-1", true},
		{"/api/audience-destinations/inbound/sync", false},
		{"/api/audience-destinations/inbound/sy%6ec", false},
		{"/api/audience-destinations/inbound/inbound-token", false},
		{"/api/audience-destinations/inbound/dst-1/x", false},
		{"/api/audience-destinations/inbound/a%2Fb", false},
	} {
		if _, got := inboundPathID(c.path); got != c.want {
			t.Errorf("inboundPathID(%s) = %v, want %v", c.path, got, c.want)
		}
	}
	for _, method := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/audience-destinations/inbound/dst-1", nil))
		if rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d Allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestEveryAudienceDestinationRouteHasFixtureOrIsRetained(t *testing.T) {
	ported := audienceGoPatterns(t)
	retained := readRetainedAudiences(t)
	rest := map[string]bool{}
	for p := range ported {
		if !retained[p] {
			rest[p] = true
		}
	}
	assertFixturesCover(t, rest, fixtureRequests(t, "audiences", "destinations"))
}

// audience_refresh and audience_destination_sync are the only job types the plan 10 wiring dispatches; Python
// registers both, and nothing else (scheduler, drafting, other workers) is claimed by it.
func TestEveryAudienceJobTypeHandledOrPython(t *testing.T) {
	reg := pythonAPIFile(t, "services", "job_registry.py")
	py := map[string]bool{}
	for _, m := range pyJobKey.FindAllStringSubmatch(reg, -1) {
		py[m[1]] = true
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "worker", "handlers_audiences.go"))
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{"audiencesched.JobType": audiencesched.JobType, "deststore.JobType": deststore.JobType}
	body := regexp.MustCompile(`(?s)handlers := map\[string\]runner\{(.*?)\n\t\}`).FindStringSubmatch(string(src))
	if body == nil {
		t.Fatal("handlers map not found in cmd/worker/handlers_audiences.go")
	}
	got := map[string]bool{}
	resolve := func(name string) {
		v, ok := consts[name]
		if !ok {
			t.Fatalf("cannot resolve job type constant %s", name)
		}
		got[v] = true
	}
	for _, m := range goHandlerKy.FindAllStringSubmatch(body[1], -1) {
		resolve(m[1])
	}
	for _, m := range regexp.MustCompile(`handlers\[([\w.]+)\]\s*=`).FindAllStringSubmatch(string(src), -1) {
		resolve(m[1])
	}
	if len(got) != 2 || !got["audience_refresh"] || !got["audience_destination_sync"] {
		t.Errorf("audience wiring must dispatch exactly audience_refresh and audience_destination_sync, got %v", got)
	}
	for ty := range got {
		if !py[ty] {
			t.Errorf("Go dispatches %q, which Python does not register", ty)
		}
	}
	// The scheduler is Python-only (D9).
	for _, ty := range []string{"audience_schedule", "bootstrap_audience_schedules", "audience_tick"} {
		if got[ty] {
			t.Errorf("job type %s must stay on the Python worker", ty)
		}
	}
}

// The 12 destination types are Python's TYPES set. Ads and warehouse_http deliver natively in Go; every other type
// needs its port wired (plan 07/08) and its name in GO_DEST_TYPES, otherwise its runs go to Python.
func TestEveryDestinationTypeHasDriverOrPort(t *testing.T) {
	src := pythonAPIFile(t, "routers", "destinations.py")
	blk := regexp.MustCompile(`(?s)\nTYPES = \{(.*?)\}`).FindStringSubmatch(src)
	if blk == nil {
		t.Fatal("TYPES not found in routers/destinations.py")
	}
	var py []string
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(blk[1], -1) {
		py = append(py, m[1])
	}
	sort.Strings(py)
	goTypes := append([]string(nil), destvalidate.Types...)
	sort.Strings(goTypes)
	if len(py) != 12 || strings.Join(py, ",") != strings.Join(goTypes, ",") {
		t.Fatalf("destination types differ: python %v, go %v", py, goTypes)
	}
	native := map[string]bool{"meta_ads": true, "google_ads": true, "linkedin_ads": true, "warehouse_http": true}
	var none destdeliver.Ports
	all := destdeliver.Ports{Webhook: struct{ destdeliver.WebhookSender }{}, CRM: struct{ destdeliver.CRMPusher }{},
		Instantly: struct{ destdeliver.CampaignAdder }{}, Smartlead: struct{ destdeliver.CampaignAdder }{},
		Sheets: struct{ destdeliver.SheetsUpserter }{}, Airtable: struct{ destdeliver.AirtableUpserter }{}}
	for _, ty := range py {
		if !destvalidate.IsType(ty) {
			t.Errorf("destvalidate rejects Python type %s", ty)
		}
		if got := none.Supports(ty); got != native[ty] {
			t.Errorf("type %s: Supports with no ports = %v, want %v (only ads and warehouse_http are native)", ty, got, native[ty])
		}
		if !all.Supports(ty) {
			t.Errorf("type %s has no driver or port even with every port wired", ty)
		}
	}
	if err := none.ValidateTypes([]string{"warehouse_http", "meta_ads"}); err != nil {
		t.Errorf("native types must validate without ports: %v", err)
	}
	if err := none.ValidateTypes([]string{"slack"}); err == nil {
		t.Error("ValidateTypes must reject a type without a wired port (the worker warns and hands those rows back; it does not call this at boot)")
	}
}

// Seeded TEST-ONLY secrets (scripts/contract_seed.py): workspace secret values, ad tokens and the raw inbound tokens.
var audienceSeededSecrets = []string{
	"ctk_warehouse_auth_secret_0001", "ctk_meta_access_token_0001", "ctk_linkedin_access_token_0001",
	"ctk_google_ads_access_token_0001", "ctk_google_ads_developer_token_0001", "ctk_beta_only_secret_0001",
	"https://hooks.slack.test/services/T0000/B0000/ctkplaceholder0001",
	"dsi_hubA0001_contract_active_token_hubspot", "dsi_hubR0001_contract_revoked_token_hubspot",
	"dsi_sfE00001_contract_expired_token_salesforce", "dsi_hubD0001_contract_active_token_disabled",
	"dsi_betaA001_contract_active_token_beta",
}

func TestNoSecretsInFixturesOrLogs(t *testing.T) {
	check := func(name string, b []byte) {
		for _, s := range audienceSeededSecrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains a seeded secret (%.8s...)", name, s)
			}
		}
	}
	root := filepath.Join("..", "..", "testdata")
	n := 0
	for _, dir := range []string{"contract/audiences", "contract/destinations"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no fixtures in %s (%v)", dir, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			check(f, b)
			n++
		}
	}
	// Ad transcripts carry the TEST-ONLY secret map as the resolver's INPUT; requests and responses must only hold
	// {{secret:NAME}} placeholders, so everything except that input is checked.
	tr, _ := filepath.Glob(filepath.Join(root, "transcripts", "ads", "*.json"))
	if len(tr) == 0 {
		t.Fatal("no ad transcripts")
	}
	for _, f := range tr {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		delete(m, "secrets")
		rest, _ := json.Marshal(m)
		check(f+" (outside secrets input)", rest)
		n++
	}
	if n < 200 {
		t.Errorf("suspiciously few files checked: %d", n)
	}

	// What the public inbound callback logs while it refuses or fails a request must not hold the bearer token or body.
	var logBuf bytes.Buffer
	d := audienceGoDeps(t, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r := httpx.NewRouter()
	RegisterDestinations(r, d)
	h := r.Handler()
	body := `{"external_event_id":"evt-1","fields":{"note":"ctk_warehouse_auth_secret_0001"}}`
	for _, tok := range []string{"dsi_hubA0001_contract_active_token_hubspot", "dsi_hubR0001_contract_revoked_token_hubspot", "wrong"} {
		for _, path := range []string{"/api/audience-destinations/inbound/dst-act-hubspot", "/api/audience-destinations/inbound/sync"} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	check("captured log", logBuf.Bytes())
}

type nginxLoc struct {
	mod, pat, block string
}

// audienceNginxSection returns the locations of the audiences-destinations section of go-routes.conf.
func audienceNginxSection(t *testing.T) []nginxLoc {
	t.Helper()
	b, err := os.ReadFile(audienceNginxConf)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "# BEGIN audiences-destinations")
	j := strings.Index(s, "# END audiences-destinations")
	if i < 0 || j < i {
		t.Fatal("go-routes.conf has no BEGIN/END audiences-destinations section")
	}
	var locs []nginxLoc
	re := regexp.MustCompile(`(?m)^location\s+(=|~|\^~)\s+(\S+)\s+\{`)
	sec := s[i:j]
	for _, m := range re.FindAllStringSubmatchIndex(sec, -1) {
		end := strings.Index(sec[m[1]:], "\n}")
		locs = append(locs, nginxLoc{mod: sec[m[2]:m[3]], pat: sec[m[4]:m[5]], block: sec[m[1] : m[1]+end]})
	}
	return locs
}

// nginxPick is nginx's selection inside the section: exact first, then the first matching regex.
func nginxPick(locs []nginxLoc, path string) *nginxLoc {
	for i, l := range locs {
		if l.mod == "=" && l.pat == path {
			return &locs[i]
		}
	}
	for i, l := range locs {
		if l.mod == "~" && regexp.MustCompile(l.pat).MatchString(path) {
			return &locs[i]
		}
	}
	return nil
}

// Every Python route reaches the right upstream rule: retained paths are pinned to api_backend, ported paths follow a
// bucket, the sticky mutations use the workspace bucket, and every block forwards the client address.
func TestAudienceNginxSectionCoversRoutes(t *testing.T) {
	locs := audienceNginxSection(t)
	retained := readRetainedAudiences(t)
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, r := range pythonAudienceRoutes(t) {
		_, path, _ := strings.Cut(r, " ")
		l := nginxPick(locs, param.ReplaceAllString(path, "x1"))
		if l == nil {
			t.Errorf("no nginx location for %s", r)
			continue
		}
		if retained[r] {
			if !strings.Contains(l.block, "proxy_pass http://api_backend;") {
				t.Errorf("retained %s is not pinned to api_backend", r)
			}
			continue
		}
		if !strings.Contains(l.block, "$auth_upstream") && !strings.Contains(l.block, "$aud_up") {
			t.Errorf("ported %s does not follow a bucket: %s", r, l.pat)
		}
		sticky := r == "POST /api/audiences" || r == "PATCH /api/audiences/{audience_id}" || r == "POST /api/audiences/{audience_id}/refresh"
		if sticky && !strings.Contains(l.block, "$ws_upstream") {
			t.Errorf("%s must be sticky per workspace ($ws_upstream)", r)
		}
		if !strings.Contains(l.block, "proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;") {
			t.Errorf("%s does not forward X-Forwarded-For", r)
		}
	}
	// Nothing newly exposed: siblings, trailing slashes and lookalikes match no location in the section.
	for _, p := range []string{"/api/audiences/", "/api/audiences/a/other", "/api/audiences/a/members/", "/api/audiencesx",
		"/api/audience-destinations/", "/api/audience-destinations/a/other", "/api/audience-destinations/runs/r/other",
		"/api/audience-destinations/a/inbound/b", "/api/audience-destination"} {
		if l := nginxPick(locs, p); l != nil {
			t.Errorf("%s unexpectedly matches nginx location %s %s", p, l.mod, l.pat)
		}
	}
}

// The deviations the runbook and README promise operators must be written down.
func TestAudienceDeviationsDocumented(t *testing.T) {
	b, err := os.ReadFile(audienceReadme)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan 10 (audiences", "d3f5a7b9c1e2", "NFKC", "resourceName", "1 MiB", "WorkspaceOnly", "external_id", "GO_DEST_TYPES",
		"retained_audience_routes.txt", "serialize", "pre-mux", "120", "lax", "UTC session", "114", "completed_with_errors", "D3", "D4", "D5", "D6", "D7", "D8"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("README missing plan 10 deviation text %q", want)
		}
	}
	rb, err := os.ReadFile(audienceRunbook)
	if err != nil {
		t.Fatalf("runbook missing: %v", err)
	}
	for _, want := range []string{"GO_AUDIENCE_ROUTES_ENABLED", "GO_DEST_ROUTES_ENABLED", "GO_EXECUTOR_AUDIENCE_REFRESH", "GO_DEST_TYPES", "warehouse_http",
		"AudienceEvents", "Rollback", "scheduler", "executor='go'", "OnFailure", "inbound", "rotate", "READ COMMITTED", "per workspace"} {
		if !strings.Contains(string(rb), want) {
			t.Errorf("runbook missing %q", want)
		}
	}
}
