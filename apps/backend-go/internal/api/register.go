// Package api wires the HTTP route groups of cmd/api.
package api

import (
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// Deps is everything a route group may need. cmd/api builds it once.
type Deps struct {
	Pool     *pgxpool.Pool
	Signer   auth.Signer
	Identity *identity.Store
	Config   config.APIConfig
	Log      *slog.Logger

	// Workbook CRUD (plan 04).
	Flags   config.Flags
	DataDir string

	// Leads, signals and events (plan 05). Redis may be nil (heartbeat-only SSE).
	Redis         *redis.Client
	SSEMaxStreams int
	// Hardening beyond Python: per-user / per-workspace stream caps (429) and max stream
	// lifetime (zero values use the defaults in eventsroutes.go).
	SSEMaxPerUser, SSEMaxPerWorkspace int
	SSEMaxLifetime                    time.Duration
	// ShutdownCtx is cancelled when the server starts shutting down; SSE streams end then
	// (nil = never) so http.Server.Shutdown does not wait out its grace on open streams.
	ShutdownCtx context.Context

	// Providers serves provider calls in process (cmd/api: the rpclocal composite for the names
	// GO_LOCAL_PROVIDERS lists); nil disables the callers that need it (the email waterfall's website finder).
	// Browser is the process's browser-service client (person intel renders JavaScript pages through it);
	// nil, or an unconfigured one, disables those renders.
	// Limiter may be nil (rate limits then not enforced; cmd/api sets it).
	// Web is the process's one leadweb (search, fetch, proxy pool, rate limiter); nil builds one from the
	// environment on first use.
	Providers         provcall.Caller
	Browser           *browserclient.Client
	Web               *leadweb.Web
	Limiter           *ratelimit.Limiter
	WSMaxMessageBytes int

	// Settings routes (plan 03). RegisterSettings is a no-op when any is nil.
	Registry *settings.Registry
	Envelope *secrets.Envelope
	Egress   *http.Client // SSRF-guarded outbound client
	Getenv   func(string) string
	// OpenRouterModelsURL overrides the OpenRouter catalogue URL (tests only).
	OpenRouterModelsURL string

	// Outreach (plan 09). EgressAllowHosts are the operator-allowed internal SMTP/IMAP hosts
	// (EGRESS_ALLOW_HOSTS); the secret is never logged.
	OutreachPublicBaseURL string
	OutreachWebhookSecret config.Secret
	EgressAllowHosts      []string

	// Audiences and destinations (plan 10). AudienceEvents is required for the audience routes
	// (nil: they do not register, D5); DestPorts are the provider delivery seams.
	AudienceEvents audiencerefresh.Emitter
	DestPorts      destdeliver.Ports

	// ChatStreams caps concurrent copilot chat streams (plan 12). cmd/api builds it with NewCapsShared over
	// the same SSE_MAX_STREAMS semaphore as GET /api/events (SSEStreamSem), so the two draw on one pool.
	// Nil: the chat route builds its own SSE_MAX_STREAMS-sized pool (tests, embedders).
	ChatStreams *ssestream.Caps
	// SSEStreamSem, when set, is the process-wide stream semaphore GET /api/events uses (cmd/api shares it
	// with ChatStreams). Nil: the events route makes its own, sized by SSEMaxStreams.
	SSEStreamSem chan struct{}

	// SetupToken is the first-admin setup token (ResolveSetupToken at API start). Empty: setup needs no token.
	SetupToken string
}

// browser is the process's browser-service client: Deps.Browser, else one built from BROWSER_SERVICE_URL /
// BROWSER_SERVICE_TOKEN (unconfigured when they are unset; its calls then answer browserclient.ErrUnavailable).
func (d Deps) browser() *browserclient.Client {
	if d.Browser != nil {
		return d.Browser
	}
	sc := config.ScrapeFrom(d.getenv())
	return browserclient.New(sc.BrowserURL, sc.BrowserToken.Reveal(), nil)
}

func (d Deps) getenv() func(string) string {
	if d.Getenv != nil {
		return d.Getenv
	}
	return config.Getenv
}

// workbooksEnabled reports whether the workbook route groups may register.
// Go supports only the Postgres lead store; otherwise nothing registers (and
// the reason is logged) so an nginx canary cannot be enabled by mistake.
func (d Deps) workbooksEnabled(area string) bool {
	if d.Flags.PGLeadStore {
		return true
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("workbook routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
	return false
}

// RegisterAll registers every route group on r. Route tasks 9-14 each add one
// Register<Area>(r, d) line below; cmd/api/main.go is not edited by them.
func RegisterAll(r *httpx.Router, d Deps) {
	_ = r
	_ = d
	RegisterAuth(r, d)
	RegisterUsers(r, d)
	RegisterWorkspaces(r, d)
	RegisterMCPTokens(r, d)
	RegisterSCIM(r, d)
	RegisterSettings(r, d)
	RegisterBilling(r, d)
	wbcrud.SetLimits(wbcrud.Limits{MaxAddRows: d.Flags.WorkbookMaxAddRows, MaxRowKeys: d.Flags.WorkbookMaxRowKeys,
		MaxViewFilters: d.Flags.WorkbookMaxViewFilters, MaxViewSorts: d.Flags.WorkbookMaxViewSorts, MaxColumns: d.Flags.WorkbookMaxColumns})
	RegisterWorkbooks(r, d)
	RegisterWorkbookDetail(r, d)
	RegisterWorkbookColumns(r, d)
	RegisterWorkbookRows(r, d)
	RegisterWorkbookImport(r, d)
	RegisterWorkbookViews(r, d)
	RegisterWorkbookTelemetry(r, d)
	RegisterWorkbookMeta(r, d)
	RegisterTemplates(r, d)
	RegisterFunctions(r, d)
	RegisterEntities(r, d)
	RegisterAnalytics(r, d)
	RegisterSystem(r, d)
	RegisterLeads(r, d)
	RegisterSignals(r, d)
	RegisterEvents(r, d)
	RegisterWorkbookRuns(r, d)
	RegisterIngest(r, d)
	RegisterWorkbookWS(r, d)
	RegisterConnectors(r, d)
	RegisterHubspot(r, d)
	RegisterDataCRM(r, d)
	RegisterAmbitionBox(r, d)
	RegisterAutomations(r, d)
	RegisterWatches(r, d)
	RegisterOutreach(r, d)
	RegisterAudiences(r, d)
	RegisterDestinations(r, d)
	RegisterGovernance(r, d)
	RegisterPlaybooks(r, d)
	RegisterCopilot(r, d)
	RegisterPersonIntel(r, d)
	RegisterCollect(r, d)
	RegisterJobs(r, d)
	RegisterLeadUtils(r, d)
	RegisterSourceSettings(r, d)
	RegisterCapabilityRoutes(r, d) // plan 15 WP-ROUTES-A: behind GO_CAPABILITY_ROUTES_ENABLED
	RegisterLLMRoutes(r, d)        // plan 15 WP-LLMROUTES: behind GO_LLM_ROUTES_ENABLED
	RegisterScrapeRoutes(r, d)     // plan 15 WP-ROUTES-B: behind GO_SCRAPE_ROUTES_ENABLED
}
