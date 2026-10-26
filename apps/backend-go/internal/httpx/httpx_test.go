package httpx

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ok(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]string{"ok": "yes"}) }

func TestUnknownPathIs404Detail(t *testing.T) {
	r := NewRouter()
	r.Handle("GET /a", http.HandlerFunc(ok))
	rec := do(r.Handler(), "GET", "/nope", nil)
	if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not Found"}` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestWrongMethodIs405WithAllow(t *testing.T) {
	r := NewRouter()
	r.Handle("GET /a", http.HandlerFunc(ok))
	r.Handle("POST /a", http.HandlerFunc(ok))
	rec := do(r.Handler(), "DELETE", "/a", nil)
	if rec.Code != 405 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Method Not Allowed"}` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if a := rec.Header().Get("Allow"); a != "GET, POST" {
		t.Fatalf("allow %q", a)
	}
}

func TestPatternWildcardParams(t *testing.T) {
	r := NewRouter()
	var got string
	r.Handle("GET /w/{ws_id}/x", http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		got = q.PathValue("ws_id")
	}))
	do(r.Handler(), "GET", "/w/42/x", nil)
	if got != "42" {
		t.Fatalf("got %q", got)
	}
}

func TestRouteMiddlewareAndUseOrder(t *testing.T) {
	r := NewRouter()
	var order []string
	mk := func(n string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				order = append(order, n)
				next.ServeHTTP(w, q)
			})
		}
	}
	r.Use(mk("g1"), mk("g2"))
	r.Handle("GET /a", http.HandlerFunc(ok), mk("r1"), mk("r2"))
	do(r.Handler(), "GET", "/a", nil)
	if strings.Join(order, ",") != "g1,g2,r1,r2" {
		t.Fatalf("order %v", order)
	}
}

func TestRequestIDHonoursSafeHeaderRejectsUnsafe(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { seen = RequestIDFrom(q.Context()) }))
	rec := do(h, "GET", "/", map[string]string{"X-Request-Id": "abc.1:2_x-y"})
	if seen != "abc.1:2_x-y" || rec.Header().Get("X-Request-Id") != seen {
		t.Fatalf("seen %q hdr %q", seen, rec.Header().Get("X-Request-Id"))
	}
	for _, bad := range []string{"a b", strings.Repeat("a", 65), "a/b"} {
		rec = do(h, "GET", "/", map[string]string{"X-Request-Id": bad})
		id := rec.Header().Get("X-Request-Id")
		if id == bad || len(id) != 36 || id[14] != '4' || seen != id {
			t.Fatalf("bad %q -> %q (ctx %q)", bad, id, seen)
		}
	}
}

func TestCORSPreflight(t *testing.T) {
	h := CORS("")(http.HandlerFunc(ok))
	rec := do(h, "OPTIONS", "/x", map[string]string{
		"Origin": "http://e.com", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization, x-workspace-id",
	})
	if rec.Code != 200 || rec.Body.String() != "OK" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	hd := rec.Header()
	if hd.Get("Access-Control-Allow-Origin") != "*" || hd.Get("Access-Control-Max-Age") != "600" ||
		hd.Get("Access-Control-Allow-Headers") != "authorization, x-workspace-id" ||
		!strings.Contains(hd.Get("Access-Control-Allow-Methods"), "PATCH") ||
		hd.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("headers %v", hd)
	}
	rec = do(h, "GET", "/x", map[string]string{"Origin": "http://e.com"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Code != 200 {
		t.Fatalf("simple: %v", rec.Header())
	}
	rec = do(h, "GET", "/x", nil)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no origin must add no header")
	}
}

func TestCORSSpecificOriginList(t *testing.T) {
	h := CORS("https://a.com, https://b.com")(http.HandlerFunc(ok))
	rec := do(h, "GET", "/x", map[string]string{"Origin": "https://b.com"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://b.com" || rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("headers %v", rec.Header())
	}
	rec = do(h, "GET", "/x", map[string]string{"Origin": "https://evil.com"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Code != 200 {
		t.Fatalf("headers %v", rec.Header())
	}
	rec = do(h, "OPTIONS", "/x", map[string]string{"Origin": "https://evil.com", "Access-Control-Request-Method": "GET"})
	if rec.Code != 400 || rec.Body.String() != "Disallowed CORS origin" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestGzipSkipsSSEAndSmallBodies(t *testing.T) {
	big := strings.Repeat("x", 5000)
	mk := func(ct string, body string, extra func(http.Header)) http.Handler {
		return Gzip(1000)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			if extra != nil {
				extra(w.Header())
			}
			io.WriteString(w, body)
		}))
	}
	ae := map[string]string{"Accept-Encoding": "gzip"}
	rec := do(mk("application/json", big, nil), "GET", "/", ae)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("big: %v", rec.Header())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != big {
		t.Fatal("roundtrip mismatch")
	}
	rec = do(mk("application/json", "small", nil), "GET", "/", ae)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "small" {
		t.Fatalf("small: %v %q", rec.Header(), rec.Body.String())
	}
	rec = do(mk("text/event-stream", big, nil), "GET", "/", ae)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != big {
		t.Fatal("sse must not be compressed")
	}
	rec = do(mk("application/json", big, func(h http.Header) { h.Set("Content-Encoding", "br") }), "GET", "/", ae)
	if rec.Header().Get("Content-Encoding") != "br" || rec.Body.String() != big {
		t.Fatal("pre-encoded must pass through")
	}
	rec = do(mk("application/json", big, nil), "GET", "/", nil)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != big {
		t.Fatal("no accept-encoding must not compress")
	}
}

func TestValidationErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	ValidationError(rec, FieldError{Type: "missing", Loc: []any{"body", "username"}, Msg: "Field required"})
	if rec.Code != 422 {
		t.Fatalf("code %d", rec.Code)
	}
	want := `{"detail":[{"type":"missing","loc":["body","username"],"msg":"Field required","input":null}]}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("got %s", rec.Body.String())
	}
}

func TestDetailHeadersAndJSONNoHTMLEscape(t *testing.T) {
	rec := httptest.NewRecorder()
	Detail(rec, 401, "a<b", Header{"WWW-Authenticate", "Bearer"})
	var m map[string]string
	json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" || m["detail"] != "a<b" || !strings.Contains(rec.Body.String(), "a<b") {
		t.Fatalf("got %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}
