package api

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// RegisterTrailingSlash adds FastAPI's redirect_slashes behaviour for every registered route (plan 15 F10).
//
// Starlette answers a request whose path matches no route, but whose slash-toggled form matches a route for ANY
// method (a method mismatch still counts as a match), with 307 to the toggled path for every method; a client then
// gets the route's own 405 on the redirected request. httpx.Router's built-in fallback only redirects when the
// toggled path matches the same method, so a GET on a POST-only route's trailing-slash form would 404 here.
// nginx pins these forms to FastAPI today; this lets Go answer them once that pin is gone.
//
// The Location is the path with the original query (relative; FastAPI emits an absolute URL from the Host header,
// which is wrong behind a TLS-terminating proxy). WebSocket-only routes ("/ws") are not redirected, as in Starlette
// (redirects are for http scopes only). Non-canonical paths (double slashes, backslashes) are not redirected;
// the router answers them 404. The route table is read on the first request, after every group has registered.
func RegisterTrailingSlash(r *httpx.Router) {
	var once sync.Once
	var tpls, wsTpls [][]string
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			once.Do(func() { tpls, wsTpls = routeTemplates(r.Patterns()) })
			p := req.URL.Path
			if p == "/" || strings.Contains(p, "//") || strings.Contains(p, `\`) || !strings.HasPrefix(p, "/") {
				next.ServeHTTP(w, req)
				return
			}
			toggled := strings.TrimSuffix(p, "/")
			if toggled == p {
				toggled = p + "/"
			}
			if toggled != "/" && !templatesMatch(tpls, p) && templatesMatch(tpls, toggled) {
				w.Header().Set("Location", (&url.URL{Path: toggled, RawQuery: req.URL.RawQuery}).String())
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if toggled != "/" && !templatesMatch(tpls, p) && templatesMatch(wsTpls, toggled) {
				// websocket-only route: Starlette does not redirect it (and the router's own same-method
				// fallback would), so answer the 404 FastAPI gives.
				httpx.Detail(w, http.StatusNotFound, "Not Found")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
}

// routeTemplates splits every "METHOD /path" pattern into path segments; websocket-only routes ("/ws") are
// returned separately.
func routeTemplates(patterns []string) (plain, ws [][]string) {
	seen := map[string]bool{}
	for _, pat := range patterns {
		_, path, ok := strings.Cut(pat, " ")
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		if strings.HasSuffix(path, "/ws") {
			ws = append(ws, strings.Split(path, "/"))
			continue
		}
		plain = append(plain, strings.Split(path, "/"))
	}
	return plain, ws
}

// templatesMatch reports whether path is matched by any template: literal segments equal, {name} one non-empty
// segment, {name...} one or more segments, {$} the end of a trailing-slash path.
func templatesMatch(tpls [][]string, path string) bool {
	segs := strings.Split(path, "/")
	for _, t := range tpls {
		if segMatch(t, segs) {
			return true
		}
	}
	return false
}

func segMatch(t, s []string) bool {
	for i, seg := range t {
		switch {
		case seg == "{$}":
			return i == len(s)
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}"):
			return i < len(s) && s[i] != ""
		case i >= len(s):
			return false
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			if s[i] == "" {
				return false
			}
		case seg != s[i]:
			return false
		}
	}
	return len(t) == len(s)
}
