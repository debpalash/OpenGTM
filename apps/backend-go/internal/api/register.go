// Package api wires the HTTP route groups of cmd/api.
package api

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
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

	// Settings routes (plan 03). RegisterSettings is a no-op when any is nil.
	Registry *settings.Registry
	Envelope *secrets.Envelope
	Egress   *http.Client // SSRF-guarded outbound client
	Getenv   func(string) string
	// OpenRouterModelsURL overrides the OpenRouter catalogue URL (tests only).
	OpenRouterModelsURL string
}

func (d Deps) getenv() func(string) string {
	if d.Getenv != nil {
		return d.Getenv
	}
	return os.Getenv
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
}
