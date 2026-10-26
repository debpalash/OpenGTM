// Command api is the Go HTTP API (auth, workspaces, SCIM).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/api"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

const (
	shutdownGrace  = 30 * time.Second
	gzipMinSize    = 1000 // Python GZipMiddleware(minimum_size=1000)
	dbLockTimeout  = "30s"
	dbStmtTimeout  = "60s"
	headerReadTime = 10 * time.Second
	readTimeout    = 30 * time.Second
	idleTimeout    = 120 * time.Second
)

func main() {
	// Distroless has no shell/wget: compose healthchecks run `/api -healthcheck`.
	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		if err := healthcheck(healthURL(os.Getenv("API_ADDR"))); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}
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
	// RLS is silently inert for superuser/BYPASSRLS roles: refuse to start
	// (Python assert_rls_role); PG_RLS_REQUIRE_SAFE_ROLE=false downgrades to CRITICAL.
	safe, err := queue.AssertSafeRole(ctx, pool, cfg.RLSRequireSafeRole)
	if err != nil {
		return err
	}
	if !safe {
		log.Error("CRITICAL: database role is superuser/BYPASSRLS; Row-Level Security is inert (PG_RLS_REQUIRE_SAFE_ROLE=false)")
	}

	// Cancelled when shutdown starts so open SSE streams end instead of pinning Shutdown.
	streamCtx, endStreams := context.WithCancel(context.Background())
	defer endStreams()
	router, err := buildRouter(cfg, log, pool, streamCtx)
	if err != nil {
		return err
	}
	handler := router.Handler()
	srv := newServer(cfg.Addr, handler)
	srv.RegisterOnShutdown(endStreams)
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

// healthURL targets the local /healthz on the port of API_ADDR (default 8000).
func healthURL(addr string) string {
	port := "8000"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	return "http://127.0.0.1:" + port + "/healthz"
}

// healthcheck GETs url and returns nil only on HTTP 200.
func healthcheck(url string) error {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// newServer applies read/idle timeouts. There is deliberately no WriteTimeout
// so long-lived responses (future SSE) are not cut off.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: h,
		ReadHeaderTimeout: headerReadTime, ReadTimeout: readTimeout, IdleTimeout: idleTimeout,
	}
}

// buildRouter assembles the global middleware chain and routes. pool may be nil
// in tests that only exercise routes that do not touch the database.
func buildRouter(cfg config.APIConfig, log *slog.Logger, pool *pgxpool.Pool, streamCtx context.Context) (*httpx.Router, error) {
	r := httpx.NewRouter()
	// Outermost first. audit.Middleware sits after RequestID (it needs the id)
	// and inside CORS/Gzip; it owns the holder identity.Workspace fills.
	r.Use(httpx.RequestID(), httpx.CORS(cfg.CORSOrigins), httpx.Gzip(gzipMinSize), audit.Middleware(pool, log))

	// Distinct from FastAPI's /health, which nginx keeps routing to Python.
	r.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	var st *identity.Store
	if pool != nil {
		st = identity.NewStore(pool)
	}
	deps := api.Deps{
		Pool: pool,
		Signer: auth.Signer{
			Secret:     []byte(cfg.SecretKey),
			AccessTTL:  cfg.AccessTTL,
			RefreshTTL: cfg.RefreshTTL,
		},
		Identity: st,
		Config:   cfg,
		Flags:    cfg.Flags,
		DataDir:  cfg.DataDir,
		Log:      log,
	}
	// Settings routes need the registry, the secrets envelope and the guarded
	// egress client; without a pool they stay unregistered (route tests). With a
	// pool, a bad config fails startup rather than silently 404ing the routes.
	if pool != nil {
		sd, err := newSettingsDeps(cfg, settings.Load)
		if err != nil {
			return nil, err
		}
		deps.Registry, deps.Envelope, deps.Egress = sd.Registry, sd.Envelope, sd.Egress
	}
	if err := applySSE(&deps, cfg, streamCtx); err != nil {
		return nil, err
	}
	api.RegisterAll(r, deps)
	return r, nil
}

// applySSE wires the events route: Redis (nil without REDIS_URL = heartbeat-only), stream
// caps, max lifetime and the shutdown context. A bad REDIS_URL fails startup without echoing it.
func applySSE(d *api.Deps, cfg config.APIConfig, streamCtx context.Context) error {
	d.SSEMaxStreams, d.SSEMaxPerUser, d.SSEMaxPerWorkspace = cfg.SSEMaxStreams, cfg.SSEMaxPerUser, cfg.SSEMaxPerWorkspace
	d.SSEMaxLifetime, d.ShutdownCtx = cfg.SSEMaxLifetime, streamCtx
	if cfg.RedisURL == "" {
		return nil
	}
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return errors.New("parse REDIS_URL: invalid connection string")
	}
	d.Redis = redis.NewClient(opt)
	return nil
}

// newSettingsDeps builds the settings registry, secrets envelope and egress
// client. Errors never include secret values (NewEnvelope reports names only).
func newSettingsDeps(cfg config.APIConfig, load func() (*settings.Registry, error)) (api.Deps, error) {
	reg, err := load()
	if err != nil {
		return api.Deps{}, fmt.Errorf("settings registry: %w", err)
	}
	env, err := secrets.NewEnvelope(secrets.Config{
		Provider: os.Getenv("SECRETS_PROVIDER"), MasterKey: os.Getenv("SECRETS_MASTER_KEY"),
		SecretKey: cfg.SecretKey, AppEnv: cfg.AppEnv,
		VaultAddr: os.Getenv("VAULT_ADDR"), VaultToken: os.Getenv("VAULT_TOKEN"),
		VaultTokenFile: os.Getenv("VAULT_TOKEN_FILE"), VaultTransitKey: os.Getenv("VAULT_TRANSIT_KEY"),
		VaultNamespace: os.Getenv("VAULT_NAMESPACE"), VaultCACert: os.Getenv("VAULT_CACERT"),
	}, nil)
	if err != nil {
		return api.Deps{}, fmt.Errorf("secrets envelope: %w", err)
	}
	return api.Deps{Registry: reg, Envelope: env, Egress: egress.NewClientForEnv(cfg.AppEnv, nil)}, nil
}
