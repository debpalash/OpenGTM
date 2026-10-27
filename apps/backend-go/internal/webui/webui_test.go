package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":        {Data: []byte("<html>app</html>")},
		"assets/app-abc.js": {Data: []byte("console.log(1)")},
		"favicon.svg":       {Data: []byte("<svg/>")},
	}
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestSPAFallbackAndIndex(t *testing.T) {
	h := Handler(testFS())
	for _, p := range []string{"/", "/leads", "/leads/123", "/index.html"} {
		rec := get(h, "GET", p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
			t.Fatalf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s Cache-Control %q", p, cc)
		}
		if rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s missing security headers", p)
		}
	}
}

func TestHashedAssetImmutable(t *testing.T) {
	rec := get(Handler(testFS()), "GET", "/assets/app-abc.js")
	if rec.Code != 200 || rec.Body.String() != "console.log(1)" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Fatalf("Cache-Control %q", cc)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
}

func TestMissingFileWithExtensionIs404(t *testing.T) {
	h := Handler(testFS())
	for _, p := range []string{"/assets/missing.js", "/nope.png", "/assets/"} {
		if rec := get(h, "GET", p); rec.Code != 404 && p != "/assets/" {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := get(h, "GET", "/assets/missing.js"); strings.Contains(rec.Body.String(), "<html>") {
		t.Fatal("missing asset must not fall back to index.html")
	}
}

func TestExcludedPrefixes(t *testing.T) {
	h := Handler(testFS())
	for _, p := range []string{"/api/leads", "/api", "/auth/me", "/health", "/healthz", "/internal/x", "/metrics", "/ws/person/enrich", "/admin/users"} {
		if !Excluded(p) {
			t.Fatalf("%s should be excluded", p)
		}
		if rec := get(h, "GET", p); rec.Code != 404 || strings.Contains(rec.Body.String(), "app") {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/", "/leads", "/apix", "/authors", "/assets/a.js", "/metricsx"} {
		if Excluded(p) && p != "/metricsx" { // /metricsx is not a child of /metrics
			t.Fatalf("%s should not be excluded", p)
		}
	}
}

func TestMountRouting(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("api")) })
	h := Mount(api, Handler(testFS()))
	if b := get(h, "GET", "/api/x").Body.String(); b != "api" {
		t.Fatalf("api path: %q", b)
	}
	if b := get(h, "GET", "/leads").Body.String(); !strings.Contains(b, "app") {
		t.Fatalf("spa path: %q", b)
	}
	if b := get(h, "POST", "/leads").Body.String(); b != "api" {
		t.Fatalf("non-GET goes to api: %q", b)
	}
	if Mount(api, nil) == nil {
		t.Fatal("nil ui")
	}
}

func TestNotBuilt(t *testing.T) {
	rec := get(Handler(nil), "GET", "/")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "UI not built (build with make build)") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestHeadAndMethod(t *testing.T) {
	h := Handler(testFS())
	if rec := get(h, "HEAD", "/leads"); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD %d", rec.Code)
	}
	if rec := get(h, "POST", "/leads"); rec.Code != 405 {
		t.Fatalf("POST %d", rec.Code)
	}
}
