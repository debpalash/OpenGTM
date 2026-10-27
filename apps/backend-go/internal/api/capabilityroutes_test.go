package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Plan 15 WP-ROUTES-A: the four certification capability routes, /health and /api, recorded from FastAPI with a
// test certification set (testdata/contract/capabilities, scripts/record_capability_routes.py) and compared byte for byte.

const capFixtureDir = "../../testdata/contract/capabilities"

type capCase struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	Location    string `json:"location"`
	Allow       string `json:"allow"`
	ContentType string `json:"content_type"`
	BodyRaw     string `json:"body_raw"`
}

type capFixtures struct {
	Capabilities  []capCase `json:"capabilities"`
	Root          []capCase `json:"root"`
	TrailingSlash []capCase `json:"trailing_slash"`
}

func loadCapFixtures(t *testing.T) capFixtures {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(capFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f capFixtures
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func capMeta(t *testing.T) (key, build string, now time.Time) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(capFixtureDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct{ Key, Build, Now string }
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	m.Key, m.Build, m.Now = raw["key"], raw["build_sha"], raw["now"]
	now, err = time.Parse(time.RFC3339, m.Now)
	if err != nil {
		t.Fatal(err)
	}
	return m.Key, m.Build, now
}

func capDeps(t *testing.T, enabled bool) (Deps, func()) {
	t.Helper()
	key, build, now := capMeta(t)
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	prev := capabilityClock
	capabilityClock = func() time.Time { return now }
	env := map[string]string{}
	if enabled {
		env["GO_CAPABILITY_ROUTES_ENABLED"] = "true"
	}
	d := Deps{Pool: pool, Identity: identity.NewStore(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Getenv: func(k string) string { return env[k] },
		Flags: config.Flags{CertificationPath: filepath.Join(capFixtureDir, "certifications.json"),
			CertificationKey: config.Secret(key), BuildSHA: build}}
	return d, func() { capabilityClock = prev }
}

// The handlers (behind auth in production) must produce FastAPI's exact bytes for every recorded catalog.
func TestCapabilityBodiesByteEqualPython(t *testing.T) {
	f := loadCapFixtures(t)
	d, restore := capDeps(t, true)
	defer restore()
	h := newCapabilityRoutes(d)
	routes := map[string]http.HandlerFunc{
		"/api/audiences/capabilities":         h.audiences,
		"/api/audience-destinations/types":    h.destinationTypes,
		"/api/governance/capabilities":        h.governance,
		"/api/research-playbooks/capabilities": h.playbooks,
	}
	if len(f.Capabilities) != len(routes) {
		t.Fatalf("fixture has %d capability cases, want %d", len(f.Capabilities), len(routes))
	}
	for _, c := range f.Capabilities {
		fn, ok := routes[c.Path]
		if !ok {
			t.Fatalf("no handler for %s", c.Path)
		}
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest("GET", c.Path, nil))
		if rec.Code != c.Status || rec.Body.String() != c.BodyRaw {
			t.Errorf("%s: %d\n got %s\nwant %s", c.Path, rec.Code, rec.Body.String(), c.BodyRaw)
		}
		if ct := rec.Header().Get("Content-Type"); ct != c.ContentType {
			t.Errorf("%s: content type %q want %q", c.Path, ct, c.ContentType)
		}
	}
}

// With no certification evidence every capability is beta with a null certification (fail closed).
func TestCapabilityFailsClosedWithoutEvidence(t *testing.T) {
	d, restore := capDeps(t, true)
	defer restore()
	d.Flags.CertificationPath = filepath.Join(t.TempDir(), "missing.json")
	rec := httptest.NewRecorder()
	newCapabilityRoutes(d).governance(rec, httptest.NewRequest("GET", "/api/governance/capabilities", nil))
	var out struct {
		Capabilities []struct {
			Maturity      string `json:"maturity"`
			Certification any    `json:"certification"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Capabilities) != 2 {
		t.Fatalf("%v %s", err, rec.Body.String())
	}
	for _, c := range out.Capabilities {
		if c.Maturity != "beta" || c.Certification != nil {
			t.Errorf("not fail-closed: %+v", c)
		}
	}
}

func capRouter(t *testing.T, enabled bool) (*httpx.Router, func()) {
	d, restore := capDeps(t, enabled)
	r := httpx.NewRouter()
	RegisterCapabilityRoutes(r, d)
	return r, restore
}

func serveCap(r *httpx.Router, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// Off by default: nothing is registered, so no route changes until WP-CUTOVER flips the flag.
func TestCapabilityRoutesOffByDefault(t *testing.T) {
	r, restore := capRouter(t, false)
	defer restore()
	if p := r.Patterns(); len(p) != 0 {
		t.Fatalf("registered with the flag off: %v", p)
	}
}

func TestCapabilityRoutesRegisteredAndAuthenticated(t *testing.T) {
	r, restore := capRouter(t, true)
	defer restore()
	want := map[string]bool{
		"GET /api/audiences/capabilities": true, "GET /api/audience-destinations/types": true,
		"GET /api/governance/capabilities": true, "GET /api/research-playbooks/capabilities": true,
		"GET /health": true, "GET /api": true,
	}
	for _, p := range r.Patterns() {
		if !want[p] {
			t.Errorf("unexpected pattern %s", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("missing pattern %s", p)
	}
	for _, p := range []string{"/api/audiences/capabilities", "/api/audience-destinations/types",
		"/api/governance/capabilities", "/api/research-playbooks/capabilities"} {
		rec := serveCap(r, "GET", p)
		if rec.Code != 401 || rec.Body.String() != `{"detail":"Not authenticated"}` {
			t.Errorf("%s unauthenticated: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// /health and /api are public (no auth dependency in FastAPI) and carry the recorded bodies; other methods 405.
func TestHealthAndRootMatchPython(t *testing.T) {
	r, restore := capRouter(t, true)
	defer restore()
	for _, c := range loadCapFixtures(t).Root {
		if c.Method == "HEAD" {
			continue // net/http answers HEAD for a GET route (every route group); see the report
		}
		rec := serveCap(r, c.Method, c.Path)
		if rec.Code != c.Status || rec.Body.String() != c.BodyRaw || rec.Header().Get("Allow") != c.Allow ||
			(c.BodyRaw != "" && rec.Header().Get("Content-Type") != c.ContentType) {
			t.Errorf("%s %s: %d %q allow=%q ct=%q\nwant %d %q allow=%q ct=%q", c.Method, c.Path, rec.Code, rec.Body.String(),
				rec.Header().Get("Allow"), rec.Header().Get("Content-Type"), c.Status, c.BodyRaw, c.Allow, c.ContentType)
		}
	}
}
