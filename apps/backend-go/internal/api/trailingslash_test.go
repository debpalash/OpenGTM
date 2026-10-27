package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Plan 15 WP-ROUTES-A (F10): FastAPI's redirect_slashes answers the trailing-slash form of any route, for every
// method (including a method the route does not serve), with 307 to the route's own form. nginx pins these forms
// to FastAPI today; once Go is the only API they must be answered here. Cases are recorded from FastAPI.

func slashHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

func slashRouter() *httpx.Router {
	r := httpx.NewRouter()
	h := slashHandler(200)
	for _, p := range []string{
		"POST /api/workbooks/{workbook_id}/run", "GET /api/workbooks/{workbook_id}/run/estimate",
		"GET /api/workbooks/meta/provider-stats", "GET /api/workbooks/{workbook_id}/ws",
		"POST /api/workbooks/{workbook_id}/rows/{row_id}/cells/{col_id}/run", "PUT /api/workbooks/{workbook_id}/refresh-policy",
		"GET /api/with-slash/", "GET /api/files/{rest...}",
	} {
		r.Handle(p, h)
	}
	RegisterTrailingSlash(r)
	return r
}

func TestTrailingSlashRedirectsAnyMethod(t *testing.T) {
	h := slashRouter().Handler()
	for _, c := range []struct{ method, path, want string }{
		{"POST", "/api/workbooks/abc/run/", "/api/workbooks/abc/run"},
		{"GET", "/api/workbooks/abc/run/", "/api/workbooks/abc/run"}, // wrong method: FastAPI still redirects, then 405
		{"DELETE", "/api/workbooks/abc/run/", "/api/workbooks/abc/run"},
		{"GET", "/api/workbooks/abc/run/estimate/", "/api/workbooks/abc/run/estimate"},
		{"GET", "/api/workbooks/meta/provider-stats/", "/api/workbooks/meta/provider-stats"},
		{"POST", "/api/workbooks/abc/rows/r1/cells/c1/run/", "/api/workbooks/abc/rows/r1/cells/c1/run"},
		{"PUT", "/api/workbooks/abc/refresh-policy/", "/api/workbooks/abc/refresh-policy"},
		{"GET", "/api/with-slash", "/api/with-slash/"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != 307 || rec.Header().Get("Location") != c.want {
			t.Errorf("%s %s: %d %q, want 307 %q", c.method, c.path, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}
}

func TestTrailingSlashKeepsQueryAndSkipsUnknown(t *testing.T) {
	h := slashRouter().Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/workbooks/abc/run/?a=1&b=x+y", nil))
	if rec.Code != 307 || rec.Header().Get("Location") != "/api/workbooks/abc/run?a=1&b=x+y" {
		t.Errorf("query: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, p := range []string{"/api/nope/", "/api/workbooks/abc/run/extra/", "/api/workbooks/abc/", "/", "//evil.example/run/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 404 || rec.Header().Get("Location") != "" {
			t.Errorf("%s: %d location %q, want plain 404", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	// A path that is itself a route is never redirected; a WebSocket-only path answers 404 in its slash form.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/with-slash/", nil))
	if rec.Code != 200 {
		t.Errorf("own route: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/workbooks/abc/ws/", nil))
	if rec.Code != 404 {
		t.Errorf("ws slash form: %d, want 404 (FastAPI: websocket routes are not redirected)", rec.Code)
	}
}

// Never an open redirect: the Location is a path, never protocol-relative, whatever the request path holds.
func TestTrailingSlashLocationNeverProtocolRelative(t *testing.T) {
	h := slashRouter().Handler()
	for _, p := range []string{"/%2Fevil.example/api/workbooks/abc/run/", "/api/workbooks/%5Cevil/run/", "/api/files/a//b/"} {
		u, err := url.Parse("http://x" + p)
		if err != nil {
			continue
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", u.String(), nil))
		if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, "//") || strings.Contains(loc, `\`) {
			t.Errorf("%s: unsafe Location %q", p, loc)
		}
	}
}

// Recorded FastAPI cases against the real route tables of the pinned workbook groups plus the capability and health
// routes. A case whose route is not ported to Go yet (generate-column, ingest-token, rows/ingest, preview,
// provider-accuracy: retained on Python) has no Go pattern to redirect to and is skipped here; the double-slash
// forms are a deliberate deviation (Go refuses non-canonical paths with 404, hardening, see the report).
func TestTrailingSlashMatchesRecordedFastAPI(t *testing.T) {
	r := httpx.NewRouter()
	d, restore := capDeps(t, true)
	defer restore()
	RegisterCapabilityRoutes(r, d)
	registerEnrich(r, enrichDepsWithPool(t))
	ported := map[string]bool{}
	for _, p := range r.Patterns() {
		_, path, _ := strings.Cut(p, " ")
		ported[pyParam.ReplaceAllString(path, "{}")] = true
	}
	checked := 0
	for _, c := range loadCapFixtures(t).TrailingSlash {
		if c.Status != 307 {
			continue
		}
		loc, err := url.Parse(c.Location)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(c.Path, "//") || !goRouteExists(ported, loc.Path) {
			continue
		}
		rel := loc.RequestURI()
		u := c.Path
		if q := strings.Index(rel, "?"); q >= 0 {
			u += rel[q:]
		}
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(c.Method, u, nil))
		if rec.Code != 307 || rec.Header().Get("Location") != rel {
			t.Errorf("%s %s: %d %q, FastAPI 307 -> %q (relative form)", c.Method, u, rec.Code, rec.Header().Get("Location"), rel)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d recorded slash cases exercised", checked)
	}
}

func goRouteExists(ported map[string]bool, path string) bool {
	segs := strings.Split(path, "/")
	for p := range ported {
		ps := strings.Split(p, "/")
		if len(ps) != len(segs) {
			continue
		}
		ok := true
		for i := range ps {
			if ps[i] != segs[i] && !(ps[i] == "{}" && segs[i] != "") {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// The Location is built from the raw (escaped) path: an escaped character is not decoded on the way through.
func TestTrailingSlashLocationKeepsEscaping(t *testing.T) {
	h := slashRouter().Handler()
	for _, c := range []struct{ path, want string }{
		{"/api/workbooks/a%2Cb/run/", "/api/workbooks/a%2Cb/run"},
		{"/api/workbooks/a%2Fb/run/", "/api/workbooks/a%2Fb/run"},
		{"/api/workbooks/a%20b/run/?q=%2F", "/api/workbooks/a%20b/run?q=%2F"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, nil))
		if rec.Code != 307 || rec.Header().Get("Location") != c.want {
			t.Errorf("%s: %d %q, want 307 %q", c.path, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}
}

// GO_TRAILING_SLASH_COMPAT, not GO_CAPABILITY_ROUTES_ENABLED, switches the redirect behaviour of every route.
func TestTrailingSlashCompatHasItsOwnFlag(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want int
	}{
		{map[string]string{}, 404},
		{map[string]string{"GO_CAPABILITY_ROUTES_ENABLED": "true"}, 404},
		{map[string]string{"GO_TRAILING_SLASH_COMPAT": "true"}, 307},
	} {
		d, restore := capDeps(t, false)
		d.Getenv = func(k string) string { return tc.env[k] }
		r := httpx.NewRouter()
		RegisterCapabilityRoutes(r, d)
		r.Handle("GET /health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		RegisterTrailingSlashCompat(r, d)
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/health/", nil))
		restore()
		if rec.Code != tc.want {
			t.Errorf("env %v: POST /health/ = %d, want %d", tc.env, rec.Code, tc.want)
		}
	}
}
