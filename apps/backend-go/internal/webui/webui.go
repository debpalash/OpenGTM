// Package webui serves the built React UI (apps/web) from the Go API binary.
//
// The UI is embedded only under the `webui` build tag (see embed.go), so plain `go build ./...`
// and `go test ./...` work without a built dist. It is the lowest-precedence catch-all: paths
// the API owns are never answered here.
package webui

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

const notBuilt = "UI not built (build with make build)\n"

// apiPrefixes are the paths the Go API owns (and nginx proxies); the UI never answers them.
// "/health" also covers /healthz; "/metrics" is reserved for the Prometheus endpoint.
var apiPrefixes = []string{"/api", "/auth", "/internal", "/ws", "/admin", "/health", "/metrics"}

// Excluded reports whether p belongs to the API rather than the UI.
func Excluded(p string) bool {
	for _, pre := range apiPrefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	// "/healthz" is a sibling of "/health", not a child.
	return strings.HasPrefix(p, "/health")
}

// Mount returns api with the UI as the catch-all: GET/HEAD requests for non-API paths go to ui,
// everything else (API paths, other methods) goes to api unchanged. A nil ui returns api.
func Mount(api, ui http.Handler) http.Handler {
	if ui == nil {
		return api
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !Excluded(r.URL.Path) {
			ui.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}

// Default is the handler for the compiled-in UI (404 "UI not built" without the webui tag).
func Default() http.Handler { return Handler(FS()) }

// Handler serves fsys: existing files as-is, extensionless unknown paths as index.html (SPA fallback),
// unknown paths with an extension as 404. A nil fsys answers 404 "UI not built".
func Handler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fsys == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, notBuilt)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if Excluded(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if st, err := fs.Stat(fsys, name); err != nil || st.IsDir() {
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		}
		serve(w, r, fsys, name)
	})
}

func serve(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	// Same headers as `location /` in infra/nginx/default.conf.
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-Content-Type-Options", "nosniff")
	switch {
	case strings.HasPrefix(name, "assets/"):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case name == "index.html":
		h.Set("Cache-Control", "no-cache")
	default:
		h.Set("Cache-Control", "public, max-age=3600")
	}
	// ServeContent sets Content-Type by extension and handles HEAD and Range.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
