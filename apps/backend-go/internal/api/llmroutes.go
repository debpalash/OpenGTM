package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/nlcolumn"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// chatSource is the slice of llm.Client the lead enrich route uses to find the chat provider.
type chatSource interface {
	ChatChain(ctx context.Context) []llm.ChatProvider
	ActiveChatProvider(ctx context.Context) llm.ChatProvider
}

// llmBundle is the LLM surface of one request: ordered JSON extraction and the chat provider chain.
type llmBundle struct {
	Extract nlcolumn.LLM
	Chat    chatSource
}

// llmRoutes holds the four LLM-backed routes (plan 15 WP-LLMROUTES): POST /api/campaigns/generate, POST
// /api/v2/workbooks/{id}/generate-column, GET /api/workbooks/meta/provider-accuracy and POST /api/lead/{id}/enrich.
// Every dependency that is not Postgres is a field so tests (and later work packages) can replace it.
type llmRoutes struct {
	d   Deps
	log *slog.Logger

	// newLLM builds the per-workspace LLM client. Default: llm.New over the global settings (as Python read
	// them), the operator egress client and llm_usage_daily accounting.
	newLLM func(ctx context.Context, workspaceID string) llmBundle

	enrichOnce    sync.Once
	enrichLimited http.Handler

	// store reads and writes leads (Postgres by default).
	store leadStore

	// httpc is the OPERATOR egress client (EGRESS_ALLOW_HOSTS honoured): only for operator-configured endpoints, the
	// chat stream and the Nominatim lookup. userHTTP is the strict client for anything that fetches user data (lead
	// websites): it is handed to providers, and lead URLs are checked against appEnv/allowHosts first.
	httpc     *http.Client
	userHTTP  *http.Client
	appEnv    string
	allowHost []string
	// nominatimURL is the geocoder endpoint (tests only).
	nominatimURL string

	// Seams to work packages that have not landed. A nil/absent seam makes the matching enrich action answer a
	// terminal SSE error step; see leadenrich.go.
	providerLookup providerLookup // WP-PROVIDERS: ddg_email_provider and website_scraper by registry name
	searcher       Searcher       // WP-WEB: web search (find_phone, the find_address fallback)
}

// RegisterLLMRoutes registers the four LLM-backed routes behind GO_LLM_ROUTES_ENABLED (default off; the routes then
// stay on FastAPI). nginx keeps sending them to FastAPI until WP-RETIRE-INFRA, so enabling the flag alone does not
// move traffic.
func RegisterLLMRoutes(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil || d.Registry == nil || d.Envelope == nil {
		return
	}
	on, err := config.LLMRoutesFrom(d.getenv())
	if err != nil {
		l := d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("GO_LLM_ROUTES_ENABLED is invalid; LLM-backed routes are not registered")
		return
	}
	if !on {
		return
	}
	h := newLLMRoutes(d)
	h.register(r)
}

func newLLMRoutes(d Deps) *llmRoutes {
	appEnv := d.Config.AppEnv
	if appEnv == "" {
		appEnv = d.getenv()("APP_ENV")
	}
	h := &llmRoutes{d: d, log: d.Log, httpc: egress.NewOperatorClient(d.EgressAllowHosts),
		userHTTP: egress.NewClientForEnv(appEnv, d.EgressAllowHosts), appEnv: appEnv, allowHost: d.EgressAllowHosts, nominatimURL: "https://nominatim.openstreetmap.org/search",
		providerLookup: registryProviders{}, store: pgLeadStore{pool: d.Pool}}
	if h.log == nil {
		h.log = slog.Default()
	}
	h.newLLM = h.defaultLLM
	return h
}

func (h *llmRoutes) register(r *httpx.Router) {
	d := h.d
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	writer := identity.RequireRole("tables.write", "editor", "admin")
	// Both routes spend LLM tokens per request (campaigns: up to 10 sequential calls), so they are limited per
	// workspace like the other LLM routes (the limiter keys by workspace, then user). Python had no limit.
	r.Handle("POST /api/campaigns/generate", h.limit(llmGenerateRate, http.HandlerFunc(h.campaignGenerate)), authn, workspace,
		identity.RequireRole("campaigns.write", "editor", "admin"))
	r.Handle("POST /api/v2/workbooks/{workbook_id}/generate-column", h.limit(llmGenerateRate, http.HandlerFunc(h.generateColumn)), authn, workspace, writer)
	r.Handle("GET /api/workbooks/meta/provider-accuracy", http.HandlerFunc(h.providerAccuracy), authn, workspace)
	// The 30/minute limit is applied inside leadEnrich, after path validation (slowapi order).
	r.Handle("POST /api/lead/{lead_id}/enrich", http.HandlerFunc(h.leadEnrich), authn, workspace, writer)
}

// llmGenerateRate is the per-workspace budget of /campaigns/generate and /generate-column.
const llmGenerateRate = "20/minute"

// limit wraps next in the shared limiter (a no-op when the deployment has none).
func (h *llmRoutes) limit(spec string, next http.Handler) http.Handler {
	if h.d.Limiter == nil {
		return next
	}
	return h.d.Limiter.Limit(spec)(next)
}

func (h *llmRoutes) defaultLLM(ctx context.Context, ws string) llmBundle {
	cfg, err := config.LLMFrom(h.d.getenv())
	if err != nil {
		h.log.Error("LLM settings invalid; using defaults", "err_type", errTypeName(err))
		cfg, _ = config.LLMFrom(func(string) string { return "" })
	}
	app := &settings.App{Store: secrets.NewPgStore(h.d.Pool), Env: h.d.Envelope, Secret: h.d.Registry.SecretSet(), Getenv: h.d.getenv()}
	c := llm.New(llm.Options{Settings: app, HTTP: h.httpc, Usage: &llm.PgUsage{Pool: h.d.Pool}, WorkspaceID: ws, Config: cfg,
		Registry: h.d.Registry, Log: h.log})
	return llmBundle{Extract: orderedLLM{c: c}, Chat: c}
}

func (h *llmRoutes) wsID(r *http.Request) string {
	wc, _ := identity.WorkspaceFrom(r.Context())
	return wc.WorkspaceID
}

func (h *llmRoutes) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error("llm routes: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}
