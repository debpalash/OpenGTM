// Package api wires the HTTP route groups of cmd/api.
package api

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Deps is everything a route group may need. cmd/api builds it once.
type Deps struct {
	Pool     *pgxpool.Pool
	Signer   auth.Signer
	Identity *identity.Store
	Config   config.APIConfig
	Log      *slog.Logger
}

// RegisterAll registers every route group on r. Route tasks 9-14 each add one
// Register<Area>(r, d) line below; cmd/api/main.go is not edited by them.
func RegisterAll(r *httpx.Router, d Deps) {
	_ = r
	_ = d
	RegisterAuth(r, d)
	// Task 10+: ...
}
