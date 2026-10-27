package api

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/domainintel"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// domainIntelRate is the per-workspace-key budget of the analysis. Python's route has no limiter (deviation): one
// analysis makes about fifteen outbound requests (DoH, RDAP, crt.sh with retries), so an editor could otherwise use
// the server as a request amplifier.
const domainIntelRate = "30/minute"

type domainIntelCtxKey struct{}

type domainIntelRoute struct {
	d      Deps
	once   sync.Once
	client *http.Client
}

// domainIntelMaxInFlight bounds concurrent analyses process-wide: the limiter is per workspace and workspaces are
// self-service, so many tenants together must not fan out an unbounded number of upstream requests (each analysis
// is up to ~15 requests to DoH, RDAP and crt.sh, and those hosts rate-limit by server IP).
const domainIntelMaxInFlight = 8

var domainIntelSlots = make(chan struct{}, domainIntelMaxInFlight)

// registerDomainIntel mounts POST /api/leads/domain-intel (routers/leads.py domain_intelligence): require_editor
// (tables.write; owner, admin, editor). The request value is only ever a query term of the three fixed upstreams
// (cloudflare DoH, rdap.org, crt.sh) behind the egress guard (no private ranges, no proxies, redirects checked by
// internal/domainintel): it is never a destination.
//
// Like POST /api/collect, validation (422 / 400) runs before the limiter, so a rejected request neither costs
// outbound work nor budget.
func registerDomainIntel(r *httpx.Router, d Deps) {
	h := &domainIntelRoute{d: d}
	var inner http.Handler = http.HandlerFunc(h.analyze)
	if d.Limiter != nil {
		inner = d.Limiter.Limit(domainIntelRate)(inner)
	}
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain, ok := h.parse(w, r)
		if !ok {
			return
		}
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), domainIntelCtxKey{}, domain)))
	})
	r.Handle("POST /api/leads/domain-intel", outer, identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"))
}

// parse is `body: dict` plus the two 400 checks of the handler; it returns the cleaned domain.
func (h *domainIntelRoute) parse(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCollectBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return "", false
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		for i := range errs {
			if errs[i].Type == "model_attributes_type" { // the annotation is a plain dict
				errs[i].Type, errs[i].Msg = "dict_type", "Input should be a valid dictionary"
			}
		}
		httpx.ValidationError(w, errs...)
		return "", false
	}
	v, _ := obj.Get("domain")
	s, isStr := v.(string)
	if !isStr || collectintent.PyStrip(s) == "" {
		httpx.Detail(w, http.StatusBadRequest, "domain is required")
		return "", false
	}
	domain := domainintel.Clean(s)
	if domain == "" {
		httpx.Detail(w, http.StatusBadRequest, "domain must be a valid domain name or URL")
		return "", false
	}
	return domain, true
}

func (h *domainIntelRoute) http() *http.Client {
	h.once.Do(func() {
		h.client = h.d.Egress
		if h.client == nil {
			h.client = egress.NewClientForEnv(h.d.Config.AppEnv, h.d.EgressAllowHosts)
		}
	})
	return h.client
}

// analyze answers the analysis verbatim (key order of analyze_domain). Upstream failures are already folded into
// empty sections by domainintel.Analyze (the recorded partial result); a non-object crt.sh row is skipped. An
// error is a cancelled request, answered with a bare 500 (no upstream text).
func (h *domainIntelRoute) analyze(w http.ResponseWriter, r *http.Request) {
	select {
	case domainIntelSlots <- struct{}{}:
		defer func() { <-domainIntelSlots }()
	default:
		httpx.Detail(w, http.StatusServiceUnavailable, "Too many domain analyses in progress; retry shortly")
		return
	}
	domain, _ := r.Context().Value(domainIntelCtxKey{}).(string)
	res, err := domainintel.Analyze(r.Context(), h.http(), domain)
	if err != nil {
		if r.Context().Err() == nil {
			h.d.logger().Warn("domain-intel: analysis failed", "err_type", errTypeName(err))
		}
		serverError(w)
		return
	}
	writePy(w, res)
}
