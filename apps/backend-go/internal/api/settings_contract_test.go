package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// retainedSourcesRoutes stay on FastAPI (they read the Python leadgen registry
// and source-health layer); nginx never sends them to Go.
var retainedSourcesRoutes = []string{
	"GET /api/settings/sources",
	"GET /api/settings/sources/health",
	"PUT /api/settings/sources/{source_id}",
	"POST /api/settings/sources/{source_id}/health/reset",
	"GET /api/settings/sources/registry",
}

// contractAreas are the fixture directories that belong to the settings and
// billing routes (integrations live in their own directory).
var contractAreas = []string{"settings", "integrations", "billing"}

// settingsBillingRouter registers the settings and billing routes without a
// database: handlers are never invoked, only the route table is inspected.
func settingsBillingRouter(t *testing.T) *httpx.Router {
	t.Helper()
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httpx.NewRouter()
	d := Deps{Registry: reg, Envelope: envl, Egress: egress.NewClientForEnv("test", nil)}
	RegisterSettings(r, d)
	RegisterBilling(r, d)
	return r
}

// contractFixtureNames lists "<area>/<case>" for every fixture file in area,
// minus the recorder's token_* login captures (they are setup, not routes).
func contractFixtureNames(t testing.TB, area string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "contract", area, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if strings.HasPrefix(name, "token_") {
			continue
		}
		out = append(out, area+"/"+name)
	}
	sort.Strings(out)
	return out
}

var paramSeg = regexp.MustCompile(`\{[^}/]+\}`)

// patternRegexp turns "METHOD /a/{id}/b" into a matcher over "METHOD /a/x/b".
func patternRegexp(pattern string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(pattern)
	quoted = regexp.MustCompile(`\\\{[^}/]+\\\}`).ReplaceAllString(quoted, `[^/]+`)
	return regexp.MustCompile("^" + quoted + "$")
}

func TestEveryMigratedRouteHasAFixture(t *testing.T) {
	retained := map[string]bool{}
	for _, p := range retainedSourcesRoutes {
		retained[p] = true
	}
	var want []string
	for _, p := range settingsBillingRouter(t).Patterns() {
		if !retained[p] {
			want = append(want, p)
		}
	}
	if len(want) == 0 {
		t.Fatal("no settings/billing routes registered")
	}

	covered := map[string]bool{}
	for _, area := range contractAreas {
		for _, name := range contractFixtureNames(t, area) {
			fx := contract.Load(t, name)
			path, _, _ := strings.Cut(fx.Request.Path, "?")
			hit := false
			for _, p := range want {
				if patternRegexp(p).MatchString(fx.Request.Method + " " + path) {
					covered[p] = true
					hit = true
				}
			}
			if !hit {
				t.Errorf("fixture %s (%s %s) matches no migrated route", name, fx.Request.Method, path)
			}
		}
	}
	for _, p := range want {
		if !covered[p] {
			t.Errorf("route %q has no recorded fixture", p)
		}
	}
}

func TestSourcesRoutesNotRegistered(t *testing.T) {
	r := settingsBillingRouter(t)
	registered := map[string]bool{}
	for _, p := range r.Patterns() {
		registered[p] = true
	}
	h := r.Handler()
	for _, p := range retainedSourcesRoutes {
		if registered[p] {
			t.Errorf("%q must stay on FastAPI but Go registers it", p)
		}
		method, path, _ := strings.Cut(p, " ")
		path = paramSeg.ReplaceAllString(path, "abc")
		req := httptest.NewRequest(method, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusNotFound || body["detail"] != "Not Found" {
			t.Errorf("%s %s: %d %s, want 404 Not Found", method, path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}
