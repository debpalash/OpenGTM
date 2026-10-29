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

// fallbackWeb is the process leadweb when Deps.Web is not set (tests, tools): built once from the environment.
var (
	fallbackWebOnce sync.Once
	fallbackWeb     *leadweb.Web
)

// leadweb is the api process's one leadweb instance (one proxy pool, rate limiter, breaker and search cache;
// plan 15 R6/D5): Deps.Web (cmd/api shares it with the in-process providers), else one built from the environment.
func (d Deps) leadweb() *leadweb.Web {
	if d.Web != nil {
		return d.Web
	}
	fallbackWebOnce.Do(func() {
		cfg, err := config.LeadwebFrom(d.getenv())
		if err != nil {
			d.logger().Error("leadweb settings invalid; the defaults are used", "err_type", errTypeName(err))
			cfg, _ = config.LeadwebFrom(func(string) string { return "" })
		}
		var bc *browserclient.Client
		if b := d.browser(); b.Configured() {
			bc = b
		}
		appEnv := d.Config.AppEnv
		if appEnv == "" {
			appEnv = d.getenv()("APP_ENV")
		}
		fallbackWeb = leadweb.New(cfg, bc, appEnv, d.EgressAllowHosts)
	})
	return fallbackWeb
}

// webStats is leadweb.Web.Stats of the process instance; tests replace it.
var webStats = func(d Deps, ctx context.Context) leadweb.SystemStats { return d.leadweb().Stats(ctx) }

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
