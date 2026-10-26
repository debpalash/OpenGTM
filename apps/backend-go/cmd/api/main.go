// Command api is the Go HTTP API (auth, workspaces, SCIM).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/api"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

const (
	shutdownGrace  = 30 * time.Second
	gzipMinSize    = 1000 // Python GZipMiddleware(minimum_size=1000)
	dbLockTimeout  = "30s"
	dbStmtTimeout  = "60s"
	headerReadTime = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(os.Stdout)
	log.Info("starting api", "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return errors.New("parse DATABASE_URL: invalid connection string")
	}
	pcfg.MaxConns = int32(cfg.DBMaxConns)
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["lock_timeout"] = dbLockTimeout
	pcfg.ConnConfig.RuntimeParams["statement_timeout"] = dbStmtTimeout
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Schema gate: refuse to start below the Alembic revision Go requires.
	if err := queue.CheckSchema(ctx, pool, config.RequiredGoSchemaRevision); err != nil {
		return err
	}

	handler := buildRouter(cfg, log, pool).Handler()
	srv := &http.Server{Addr: cfg.Addr, Handler: handler, ReadHeaderTimeout: headerReadTime}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.Addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutdown: draining", "grace", shutdownGrace)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// buildRouter assembles the global middleware chain and routes. pool may be nil
// in tests that only exercise routes that do not touch the database.
func buildRouter(cfg config.APIConfig, log *slog.Logger, pool *pgxpool.Pool) *httpx.Router {
	r := httpx.NewRouter()
	// Outermost first. TODO(task 7): add audit.Middleware here, after
	// RequestID (it needs the request id) and inside CORS/Gzip.
	r.Use(httpx.RequestID(), httpx.CORS(cfg.CORSOrigins), httpx.Gzip(gzipMinSize))

	// Distinct from FastAPI's /health, which nginx keeps routing to Python.
	r.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	var st *identity.Store
	if pool != nil {
		st = identity.NewStore(pool)
	}
	api.RegisterAll(r, api.Deps{
		Pool: pool,
		Signer: auth.Signer{
			Secret:     []byte(cfg.SecretKey),
			AccessTTL:  cfg.AccessTTL,
			RefreshTTL: cfg.RefreshTTL,
		},
		Identity: st,
		Config:   cfg,
		Log:      log,
	})
	return r
}
