package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
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
// data in the pool counters; tripped_domains names are shared infrastructure state). Go has no in-process pool, so
// they come from the pyworker RPC POST /internal/leadgen/system-stats, which takes no workspace input.
func registerSystemStats(r *httpx.Router, d Deps) {
	h := systemStatsRoute{d: d}
	// plan 15 WP-ROUTES-B (D5): with GO_SCRAPE_ROUTES_ENABLED the counters come from this process (leadweb) and the
	// browser service instead of the Python worker RPC.
	if on, err := config.ScrapeRoutesFrom(d.getenv()); err == nil && on {
		h.goStats = newGoStats(d)
	}
	r.Handle("GET /api/system-stats", http.HandlerFunc(h.get),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity))
}

type systemStatsRoute struct {
	d Deps
	// goStats, when set, replaces the worker RPC (see registerSystemStats).
	goStats func(ctx context.Context) (pool, limiter *wbcrud.Obj, browser any)
}

func (h systemStatsRoute) get(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	var jobs []collectledger.Job
	// the tenant tx ends before the RPC: no connection is held while waiting on the worker
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
	var browser any
	var pool, limiter *wbcrud.Obj
	ok := true
	if h.goStats != nil {
		pool, limiter, browser = h.goStats(r.Context())
	} else {
		pool, limiter, ok = h.workerStats(r.Context())
	}
	if !ok {
		// deviation D5: Python cannot fail here (in-process); a worker outage degrades instead of erroring
		pool, limiter = zeroProxyPool(), zeroRateLimiter()
		w.Header().Set("X-Stats-Source", "degraded")
	}
	if !wc.User.IsGlobalAdmin() {
		// the worker's limiter is process-global: its tripped domains were scraped for any tenant (Python leaks
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

// workerStats fetches the two objects from the worker. Anything but a 200 carrying both as JSON objects (outage,
// timeout, bad token, garbage) is reported as not ok; the upstream text never reaches the client or the log.
func (h systemStatsRoute) workerStats(ctx context.Context) (pool, limiter *wbcrud.Obj, ok bool) {
	secs := h.d.Flags.Leadgen.SystemStatsRPCTimeoutSeconds
	if secs <= 0 {
		secs = 3
	}
	raw, err := h.d.PyWorker.SystemStats(ctx, time.Duration(secs)*time.Second)
	if err != nil {
		h.d.logger().Warn("system-stats: worker unavailable, answering degraded", "err_type", errTypeName(err))
		return nil, nil, false
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		h.d.logger().Warn("system-stats: worker answer is not a JSON object")
		return nil, nil, false
	}
	pv, _ := obj.Get("proxy_pool")
	lv, _ := obj.Get("rate_limiter")
	pool, pok := pv.(*wbcrud.Obj)
	limiter, lok := lv.(*wbcrud.Obj)
	if !pok || !lok {
		h.d.logger().Warn("system-stats: worker answer has the wrong shape")
		return nil, nil, false
	}
	return pool, limiter, true
}

func zeroProxyPool() *wbcrud.Obj {
	o := wbcrud.NewObj()
	for _, k := range []string{"total", "http", "socks5", "socks4", "blocked", "domain_assignments"} {
		o.Set(k, 0)
	}
	return o
}

func zeroRateLimiter() *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("tracked_domains", 0)
	o.Set("tripped_domains", []any{})
	return o
}
