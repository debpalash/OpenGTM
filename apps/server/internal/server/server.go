// Package server is the opengtm HTTP front door.
//
// It answers Go-owned routes (health, version, progress events), reverse
// proxies every legacy API path to FastAPI, and serves the React SPA. With it,
// `opengtm serve` replaces the nginx container: the routing table below
// mirrors infra/nginx/default.conf, plus the FastAPI paths nginx never
// exposed (see legacyPrefixes).
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/server/webdist"
)

// Deps are the server's collaborators. Pool, Authz and Hub may be nil in
// tests; the routes that need them then answer 503.
type Deps struct {
	Config  config.Config
	Pool    *pgxpool.Pool
	Authz   *authz.Client
	Hub     *progress.Hub
	Logger  *slog.Logger
	Version string
	// Web overrides the SPA root (tests). Otherwise OPENGTM_WEB_DIR, then the
	// embedded build, is used.
	Web fs.FS
	// Closing is closed when the server starts shutting down, so streaming
	// handlers end and http.Server.Shutdown does not wait on them.
	Closing <-chan struct{}
	// Heartbeat is the SSE keep-alive period (default 15s).
	Heartbeat time.Duration
	// Routes mount Go-owned API routes from domain packages. Their patterns
	// are more specific than the legacy prefixes, so they take precedence.
	Routes []func(*http.ServeMux)
}

// legacyPrefixes are forwarded to FastAPI. nginx forwarded only /api/, /auth/
// and /health; /scim/ (IdP provisioning), /ws/ (scraper and person-intel
// WebSockets), /admin/ (user and operations admin API) and /mcp are FastAPI or
// reserved API paths that nginx silently answered with the SPA instead.
var legacyPrefixes = []string{"/api/", "/auth/", "/scim/", "/ws/", "/admin/", "/mcp/"}

// legacyExact are single legacy paths without a trailing subtree.
var legacyExact = []string{"/health", "/api", "/mcp"}

// New builds the root handler.
func New(d Deps) (http.Handler, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Heartbeat <= 0 {
		d.Heartbeat = 15 * time.Second
	}
	proxy, err := newLegacyProxy(d.Config.LegacyAPIURL, d.Logger)
	if err != nil {
		return nil, err
	}
	web := d.Web
	if web == nil {
		web = webRoot(d.Config.WebDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", readyHandler(d))
	mux.HandleFunc("GET /api/v2/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": d.Version, "go": runtime.Version()})
	})
	mux.Handle("GET /api/v2/events", eventsHandler(d))
	for _, mount := range d.Routes {
		mount(mux)
	}
	for _, p := range legacyPrefixes {
		mux.Handle(p, proxy)
	}
	for _, p := range legacyExact {
		mux.Handle(p, proxy) // no trailing slash: exact match only
	}
	mux.Handle("/", newSPA(web, d.Logger))

	return requestID(accessLog(d.Logger, securityHeaders(mux))), nil
}

func webRoot(dir string) fs.FS {
	if dir != "" {
		return osDirFS(dir)
	}
	return webdist.FS()
}

func readyHandler(d Deps) http.HandlerFunc {
	legacy := &http.Client{Timeout: 2 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		type check struct {
			OK       bool   `json:"ok"`
			Required bool   `json:"required"`
			Error    string `json:"error,omitempty"`
		}
		checks := map[string]check{}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		db := check{Required: true}
		if d.Pool == nil {
			db.Error = "database not configured"
		} else if err := d.Pool.Ping(ctx); err != nil {
			db.Error = err.Error()
		} else {
			db.OK = true
		}
		checks["database"] = db

		// Reported, not required: Go-owned routes keep working while the
		// legacy API restarts, and its own healthcheck gates its traffic.
		api := check{}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.Config.LegacyAPIURL+"/health", nil)
		if resp, err := legacy.Do(req); err != nil {
			api.Error = err.Error()
		} else {
			resp.Body.Close()
			api.OK = resp.StatusCode == http.StatusOK
			if !api.OK {
				api.Error = resp.Status
			}
		}
		checks["legacy_api"] = api

		status, code := "ready", http.StatusOK
		if !db.OK {
			status, code = "not_ready", http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": checks})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
