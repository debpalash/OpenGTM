package httpx

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Router wraps http.ServeMux with FastAPI-style 404/405 bodies.
type Router struct {
	mux          *http.ServeMux
	global       []Middleware
	explicitHead bool
	patterns     []string
	overrides    []override
}

// override is a route that takes precedence over the mux (see Override).
type override struct {
	method    string
	pathMatch func(escapedPath string) bool
	h         http.Handler
}

var probeMethods = []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"}

// NewRouter returns a router whose unmatched requests answer FastAPI's
// {"detail":"Not Found"} / {"detail":"Method Not Allowed"} bodies.
func NewRouter() *Router {
	r := &Router{mux: http.NewServeMux()}
	r.mux.HandleFunc("/", r.fallback)
	return r
}

// Handle registers h for pattern "METHOD /path/{param}". mws wrap h; the first
// is outermost.
func (r *Router) Handle(pattern string, h http.Handler, mws ...Middleware) {
	if strings.HasPrefix(pattern, "HEAD ") {
		r.explicitHead = true
	}
	r.patterns = append(r.patterns, pattern)
	r.mux.Handle(exactPattern(pattern), chain(h, mws))
}

// Override registers a route the mux cannot hold: pattern is "METHOD /path/{param}" (recorded for Patterns, as
// Handle does), pathMatch decides from the escaped request path whether the route owns the path, and h serves it.
// Overrides are consulted after the canonical-path guard and before the mux, inside the global middleware, so
// they win over any overlapping mux pattern (FastAPI resolves such overlaps by declaration order, and ServeMux
// refuses to register two patterns where neither is more specific). A request for an owned path with another
// method goes to the mux when a mux route accepts that method (static sibling routes win), else answers 405 with Allow: METHOD. mws wrap h; the first is outermost.
func (r *Router) Override(pattern string, pathMatch func(escapedPath string) bool, h http.Handler, mws ...Middleware) {
	method, _, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || method != strings.ToUpper(method) {
		panic("httpx: Override pattern must be \"METHOD /path\": " + pattern)
	}
	r.patterns = append(r.patterns, pattern)
	r.overrides = append(r.overrides, override{method: method, pathMatch: pathMatch, h: chain(h, mws)})
}

// Patterns returns every pattern passed to Handle, sorted ("METHOD /path/{param}").
// Contract tests use it to compare the route table against recorded fixtures.
func (r *Router) Patterns() []string {
	out := append([]string(nil), r.patterns...)
	sort.Strings(out)
	return out
}

// exactPattern turns a trailing-slash pattern into an exact match. ServeMux
// treats "GET /p/" as a subtree; FastAPI/Starlette match "/p/" only.
func exactPattern(pattern string) string {
	if strings.HasSuffix(pattern, "/") {
		return pattern + "{$}"
	}
	return pattern
}

// safeRedirectPath reports whether redirecting from p to toggled is safe: p
// must already be canonical, and toggled must be a single-slash-rooted path
// without backslashes, so Location can never be protocol-relative.
func safeRedirectPath(p, toggled string) bool {
	if strings.Contains(p, "\\") || strings.Contains(toggled, "\\") {
		return false
	}
	if !strings.HasPrefix(toggled, "/") || strings.HasPrefix(toggled, "//") {
		return false
	}
	base := strings.TrimSuffix(p, "/")
	return base != "" && path.Clean(base) == base
}

func (r *Router) matches(method, path, host string) bool {
	probe := &http.Request{Method: method, URL: &url.URL{Path: path}, Host: host, Header: http.Header{}}
	h, pat := r.mux.Handler(probe)
	if pat == "" || pat == "/" {
		return false
	}
	// ServeMux answers "/p" with its own redirect when "/p/..." is registered;
	// that is not a real route match.
	return fmt.Sprintf("%T", h) != "*http.redirectHandler"
}

// Use adds global middleware (applied around the whole mux, including 404/405
// responses). The first registered is outermost.
func (r *Router) Use(mws ...Middleware) { r.global = append(r.global, mws...) }

// Handler returns the composed http.Handler.
func (r *Router) Handler() http.Handler { return chain(http.HandlerFunc(r.serve), r.global) }

// serve answers non-canonical paths with 404 before ServeMux can redirect
// them (FastAPI does not clean paths, and a cleaned redirect could be
// protocol-relative).
func (r *Router) serve(w http.ResponseWriter, req *http.Request) {
	if p := req.URL.Path; p != "/" && !safeRedirectPath(p, "/") {
		Detail(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, o := range r.overrides {
		if !o.pathMatch(req.URL.EscapedPath()) {
			continue
		}
		if req.Method == o.method {
			o.h.ServeHTTP(w, req)
			return
		}
		// Another method on an owned path may be a real mux route (GET /inbound/health is the "health" of the
		// destination "inbound"): the mux serves it; only a path no mux route accepts answers 405.
		if r.matches(req.Method, req.URL.Path, req.Host) {
			break
		}
		Detail(w, http.StatusMethodNotAllowed, "Method Not Allowed", Header{"Allow", o.method})
		return
	}
	r.mux.ServeHTTP(w, req)
}

func chain(h http.Handler, mws []Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// fallback runs only for requests no specific pattern matched.
func (r *Router) fallback(w http.ResponseWriter, req *http.Request) {
	// Starlette redirect_slashes: retry with the trailing slash toggled.
	if p := req.URL.Path; p != "/" {
		toggled := strings.TrimSuffix(p, "/")
		if toggled == p {
			toggled = p + "/"
		}
		if safeRedirectPath(p, toggled) && r.matches(req.Method, toggled, req.Host) {
			loc := (&url.URL{Path: toggled, RawQuery: req.URL.RawQuery}).String()
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
	}
	var allowed []string
	for _, m := range probeMethods {
		if m == "HEAD" && !r.explicitHead {
			continue
		}
		if r.matches(m, req.URL.Path, req.Host) {
			allowed = append(allowed, m)
		}
	}
	if len(allowed) > 0 {
		sort.Strings(allowed)
		Detail(w, http.StatusMethodNotAllowed, "Method Not Allowed", Header{"Allow", strings.Join(allowed, ", ")})
		return
	}
	Detail(w, http.StatusNotFound, "Not Found")
}
