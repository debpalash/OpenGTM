package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// registerSetup registers the first-admin setup routes (stub: filled in by the GREEN commit).
func registerSetup(r *httpx.Router, d Deps) {
	nyi := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.Detail(w, http.StatusNotImplemented, "not implemented")
	})
	r.Handle("GET /auth/setup/status", nyi)
	r.Handle("POST /auth/setup", nyi)
}

// ResolveSetupToken decides the setup token at API start (stub).
func ResolveSetupToken(_ context.Context, _ *pgxpool.Pool, _ func(string) string, _ string, _ *slog.Logger) (string, error) {
	return "", nil
}
