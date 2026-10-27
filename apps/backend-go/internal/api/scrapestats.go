package api

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// statsWeb is the one leadweb instance of the api process (its proxy pool and rate limiter state is per process,
// plan 15 R6/D5), built on first use from the environment.
var (
	statsWebOnce sync.Once
	statsWeb     *leadweb.Web
)

// webStats is leadweb.Web.Stats; tests replace it.
var webStats = func(d Deps, ctx context.Context) leadweb.SystemStats {
	statsWebOnce.Do(func() {
		cfg, err := config.LeadwebFrom(d.getenv())
		if err != nil {
			d.logger().Error("leadweb settings invalid; system-stats counters stay empty", "err_type", errTypeName(err))
			cfg, _ = config.LeadwebFrom(func(string) string { return "" })
		}
		sc := config.ScrapeFrom(d.getenv())
		var bc *browserclient.Client
		if sc.BrowserURL != "" {
			bc = browserclient.New(sc.BrowserURL, sc.BrowserToken.Reveal(), nil)
		}
		appEnv := d.Config.AppEnv
		if appEnv == "" {
			appEnv = d.getenv()("APP_ENV")
		}
		statsWeb = leadweb.New(cfg, bc, appEnv, d.EgressAllowHosts)
	})
	return statsWeb.Stats(ctx)
}

// newGoStats builds the system-stats source over leadweb: proxy_pool and rate_limiter in the legacy shape, plus the
// browser service's counters when it answers.
func newGoStats(d Deps) func(ctx context.Context) (pool, limiter *wbcrud.Obj, browser any) {
	return func(ctx context.Context) (*wbcrud.Obj, *wbcrud.Obj, any) {
		st := webStats(d, ctx)
		pool := wbcrud.NewObj()
		pool.Set("total", st.ProxyPool.Total)
		pool.Set("http", st.ProxyPool.HTTP)
		pool.Set("socks5", st.ProxyPool.Socks5)
		pool.Set("socks4", st.ProxyPool.Socks4)
		pool.Set("blocked", st.ProxyPool.Blocked)
		pool.Set("domain_assignments", st.ProxyPool.DomainAssignments)
		limiter := wbcrud.NewObj()
		limiter.Set("tracked_domains", st.RateLimiter.TrackedDomains)
		tripped := make([]any, 0, len(st.RateLimiter.TrippedDomains))
		for _, t := range st.RateLimiter.TrippedDomains {
			tripped = append(tripped, t)
		}
		limiter.Set("tripped_domains", tripped)
		var browser any
		if st.Browser != nil {
			// round-trip through JSON so the object keeps the client's field names and order
			if b, err := json.Marshal(st.Browser); err == nil {
				if o, errs := wbcrud.DecodeBodyObject(b); errs == nil {
					browser = o
				}
			}
		}
		return pool, limiter, browser
	}
}
