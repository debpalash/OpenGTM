package httpx

import (
	"net/http"
	"net/url"
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
	r.mux.Handle(pattern, chain(h, mws))
}

// Use adds global middleware (applied around the whole mux, including 404/405
// responses). The first registered is outermost.
func (r *Router) Use(mws ...Middleware) { r.global = append(r.global, mws...) }

// Handler returns the composed http.Handler.
func (r *Router) Handler() http.Handler { return chain(r.mux, r.global) }

func chain(h http.Handler, mws []Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// fallback runs only for requests no specific pattern matched.
func (r *Router) fallback(w http.ResponseWriter, req *http.Request) {
	var allowed []string
	for _, m := range probeMethods {
		if m == "HEAD" && !r.explicitHead {
			continue
		}
		probe := &http.Request{Method: m, URL: &url.URL{Path: req.URL.Path}, Host: req.Host, Header: http.Header{}}
		if _, pat := r.mux.Handler(probe); pat != "" && pat != "/" {
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
