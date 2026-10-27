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
	"reflect"
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
	audienceNginxConf    = "../../../../infra/nginx/default.conf"
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
	// Plan 15 WP-ROUTES-A: the two capability catalogs moved from the retained list into Go.
	for p := range capabilityPatterns(t, "/api/audiences/", "/api/audience-destinations/") {
		out[p] = true
	}
	return out
}

// capabilityRoutesNginxPinned are the capability catalogs, recorded under testdata/contract/capabilities rather than
// the audience fixtures (the name is historical: nginx pinned them to Python until plan 15).
var capabilityRoutesNginxPinned = map[string]bool{
	"GET /api/audiences/capabilities": true, "GET /api/audience-destinations/types": true,
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
	if len(patterns) != 25 || !patterns[inboundRoutePattern] || len(routes) != 25 || len(ported) != 25 || len(retained) != 0 {
		t.Fatalf("want 25 Python routes = 25 ported (22 mux patterns + the inbound override + 2 capability catalogs, plan 15) + 0 retained, got %d = %d (%d patterns) + %d",
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
	// Plan 15 WP-ROUTES-A ported the two capability catalogs (D1 retained them only for want of internal/certification).
	if len(retained) != 0 {
		t.Errorf("retained list must be empty after plan 15 WP-ROUTES-A: %v", retained)
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
		// The capability catalogs are recorded under testdata/contract/capabilities (capabilityroutes_test.go).
		if !retained[p] && !capabilityRoutesNginxPinned[p] {
			rest[p] = true
		}
	}
	assertFixturesCover(t, rest, fixtureRequests(t, "audiences", "destinations"))
}

// audience_refresh and audience_destination_sync are claimable and handled by the Go worker unconditionally
// (the plan 15 cutover retired the cohort flags and the Python worker).
func TestEveryAudienceJobTypeHandledByGoWorker(t *testing.T) {
	const file = "handlers_audiences.go"
	allowed, handled := workerJobTypes(t, file, map[string]string{"audiencesched.JobType": audiencesched.JobType, "deststore.JobType": deststore.JobType})
	assertWorkerHandlesExactly(t, file, allowed, handled, "audience_refresh", "audience_destination_sync")
}

// workerJobTypes reads one cmd/worker source file and returns the job types it makes claimable
// (queue.AllowJobType) and those it maps to a handler (map literal keys of handlers and handlers[k] = ...),
// resolving the identifiers through consts.
func workerJobTypes(t *testing.T, file string, consts map[string]string) (allowed, handled map[string]bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "worker", file))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	resolve := func(set map[string]bool, name string) {
		v, ok := consts[name]
		if !ok || v == "" {
			t.Fatalf("%s: cannot resolve job type %s", file, name)
		}
		set[v] = true
	}
	allowed, handled = map[string]bool{}, map[string]bool{}
	for _, m := range regexp.MustCompile(`queue\.AllowJobType\(([\w.]+)\)`).FindAllStringSubmatch(src, -1) {
		resolve(allowed, m[1])
	}
	for _, m := range regexp.MustCompile(`handlers\[([\w.]+)\]\s*=`).FindAllStringSubmatch(src, -1) {
		resolve(handled, m[1])
	}
	// keys of every map[string]runner literal: the literal is cut out by brace matching, and a key is an
	// identifier at nesting depth 0 followed by a colon (values may hold func literals with their own braces)
	const open = "map[string]runner{"
	for i := strings.Index(src, open); i >= 0; {
		start, depth, j := i+len(open), 0, i+len(open)
		for ; j < len(src) && depth >= 0; j++ {
			switch src[j] {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				depth--
			}
			if depth == 0 && (src[j] == ',' || src[j] == '\n') || j == start {
				if m := regexp.MustCompile(`^[\s,]*([\w.]+):\s`).FindStringSubmatch(src[j:]); m != nil {
					resolve(handled, m[1])
				}
			}
		}
		next := strings.Index(src[j:], open)
		if next < 0 {
			break
		}
		i = j + next
	}
	return allowed, handled
}

// assertWorkerHandlesExactly checks that the worker makes exactly want claimable and gives each a Go handler: since
// the plan 15 cutover there is no Python worker to hand a job type back to.
func assertWorkerHandlesExactly(t *testing.T, file string, allowed, handled map[string]bool, want ...string) {
	t.Helper()
	w := map[string]bool{}
	for _, ty := range want {
		w[ty] = true
	}
	if !reflect.DeepEqual(allowed, w) {
		t.Errorf("%s: claimable job types %v, want %v", file, allowed, w)
	}
	if !reflect.DeepEqual(handled, w) {
		t.Errorf("%s: handled job types %v, want %v", file, handled, w)
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

// nginxLocations returns the location blocks of the server in infra/nginx/default.conf (one level of nesting).
func nginxLocations(t *testing.T) (server string, locs []nginxLoc) {
	t.Helper()
	b, err := os.ReadFile(audienceNginxConf)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	re := regexp.MustCompile(`(?m)^\s*location\s+(?:(=|~|~\*|\^~)\s+)?(\S+)\s+\{`)
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		end := strings.Index(s[m[1]:], "\n    }")
		if end < 0 {
			t.Fatalf("unterminated location at offset %d", m[0])
		}
		mod := ""
		if m[2] >= 0 {
			mod = s[m[2]:m[3]]
		}
		locs = append(locs, nginxLoc{mod: mod, pat: s[m[4]:m[5]], block: s[m[1] : m[1]+end]})
	}
	return s, locs
}

// nginxPick is nginx's location selection: an exact match, else the longest prefix when it is ^~, else the first
// matching regex, else the longest prefix.
func nginxPick(locs []nginxLoc, path string) *nginxLoc {
	var prefix *nginxLoc
	for i, l := range locs {
		switch l.mod {
		case "=":
			if l.pat == path {
				return &locs[i]
			}
		case "", "^~":
			if strings.HasPrefix(path, l.pat) && (prefix == nil || len(l.pat) > len(prefix.pat)) {
				prefix = &locs[i]
			}
		}
	}
	if prefix != nil && prefix.mod == "^~" {
		return prefix
	}
	for i, l := range locs {
		if (l.mod == "~" || l.mod == "~*") && regexp.MustCompile(l.pat).MatchString(path) {
			return &locs[i]
		}
	}
	return prefix
}

// Every Python audience/destination route reaches the Go API through the single gateway (plan 15: the Python
// upstream is gone), and the server forwards the client address to every location.
func TestAudienceNginxSectionCoversRoutes(t *testing.T) {
	server, locs := nginxLocations(t)
	if strings.Contains(server, "api_backend") {
		t.Fatal("default.conf still names the Python upstream api_backend")
	}
	if !strings.Contains(server, "proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;") {
		t.Error("default.conf does not forward X-Forwarded-For at server level")
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, r := range pythonAudienceRoutes(t) {
		_, path, _ := strings.Cut(r, " ")
		l := nginxPick(locs, param.ReplaceAllString(path, "x1"))
		if l == nil {
			t.Errorf("no nginx location for %s", r)
			continue
		}
		if !strings.Contains(l.block, "proxy_pass http://api_go;") {
			t.Errorf("%s is not proxied to api_go (location %s %s)", r, l.mod, l.pat)
		}
		if strings.Contains(l.block, "proxy_set_header") {
			t.Errorf("%s: location %s sets proxy headers, dropping the server-level X-Forwarded-For", r, l.pat)
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
