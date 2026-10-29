package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// statsJobWindow is get_jobs(limit=100) in system_stats.
const statsJobWindow = 100

// registerSystemStats mounts GET /api/system-stats (routers/leads.py system_stats): current_workspace only, so any
// member, viewers included, reads. The jobs object counts the workspace's newest 100 ledger rows (tenant tx);
// proxy_pool and rate_limiter are process-global in Python (ProxyPool() and RateLimiter() singletons, no tenant
// data in the pool counters; tripped_domains names are shared infrastructure state). Plan 15 WP-ROUTES-B (D5): they
// come from this process (leadweb) and the browser service.
func registerSystemStats(r *httpx.Router, d Deps) {
	h := systemStatsRoute{d: d, goStats: newGoStats(d)}
	r.Handle("GET /api/system-stats", http.HandlerFunc(h.get),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity))
}

type systemStatsRoute struct {
	d Deps
	// goStats reads the process and browser-service counters (see registerSystemStats).
	goStats func(ctx context.Context) (pool, limiter *wbcrud.Obj, browser any)
}

func (h systemStatsRoute) get(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	var jobs []collectledger.Job
	// the tenant tx ends before the counters are read: no connection is held while waiting on the browser service
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		jobs, err = collectledger.List(r.Context(), tx, "", statsJobWindow)
		return err
	})
	if err != nil {
		h.d.logger().Error("system-stats: jobs failed", "err_type", errTypeName(err))
		serverError(w)
		return
	}
	counts := wbcrud.NewObj()
	counts.Set("total", len(jobs))
	for _, s := range []string{"pending", "running", "done", "failed"} {
		n := 0
		for _, j := range jobs {
			if j.Status == s {
				n++
			}
		}
		counts.Set(s, n)
	}

	out := wbcrud.NewObj()
	pool, limiter, browser := h.goStats(r.Context())
	if !wc.User.IsGlobalAdmin() {
		// the limiter is process-global: its tripped domains were scraped for any tenant (Python leaks
		// them to every member; deviation). The key stays so the shape is Python's.
		limiter.Set("tripped_domains", []any{})
	}
	out.Set("proxy_pool", pool)
	out.Set("rate_limiter", limiter)
	out.Set("jobs", counts)
	if browser != nil {
		// addition (D5): the browser service's own per-replica counters
		out.Set("browser", browser)
	}
	writePy(w, out)
}
