// Command scheduler is the Go port of apps/api/scheduler.py: the dedicated reconciliation loop for
// recurring durable jobs. Every SCHEDULER_RECONCILE_SECONDS (min 10, default 60) it runs the nine
// bootstraps of internal/schedboot; a failing one never starves the rest. A Postgres session advisory
// lock keeps exactly one scheduler active, so a second replica (or the Python scheduler, which must take
// the same lock) idles.
//
// The scheduler only runs with GO_SCHEDULER_ENABLED=true (default false: it prints why and exits 0).
// apps/api/scheduler.py does not take the lease, so WP-RETIRE-INFRA enables this binary and removes the Python
// scheduler in one step; until then two reconcilers must never coexist.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/schedboot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scheduler:", err)
		if errors.Is(err, errDisabled) {
			os.Exit(0) // intentionally idle: not a failure, so a restart policy does not loop on it
		}
		os.Exit(1)
	}
}

// errDisabled is returned while GO_SCHEDULER_ENABLED is not true.
var errDisabled = errors.New("disabled: set GO_SCHEDULER_ENABLED=true to run the Go scheduler (it must be the only scheduler: stop the Python scheduler first)")

// gate refuses to run unless GO_SCHEDULER_ENABLED is a true boolean; an invalid value is a configuration error.
func gate(get func(string) string) error {
	switch strings.ToLower(strings.TrimSpace(get("GO_SCHEDULER_ENABLED"))) {
	case "true", "1", "yes", "on", "t", "y":
		return nil
	case "", "false", "0", "no", "off", "f", "n":
		return errDisabled
	}
	return errors.New("GO_SCHEDULER_ENABLED: not a boolean")
}

// strictRole is PG_RLS_REQUIRE_SAFE_ROLE (default true; only an explicit false value downgrades the refusal).
func strictRole(get func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(get("PG_RLS_REQUIRE_SAFE_ROLE"))) {
	case "false", "0", "no", "off", "f", "n":
		return false
	}
	return true
}

// checkRole refuses a superuser or BYPASSRLS database role (Row-Level Security would be inert), like api and
// worker; PG_RLS_REQUIRE_SAFE_ROLE=false downgrades the refusal to a CRITICAL log.
func checkRole(ctx context.Context, pool *pgxpool.Pool, strict bool, log *slog.Logger) error {
	safe, err := queue.AssertSafeRole(ctx, pool, strict)
	if err != nil {
		return err
	}
	if !safe {
		log.Error("CRITICAL: database role is superuser/BYPASSRLS; Row-Level Security is inert (PG_RLS_REQUIRE_SAFE_ROLE=false)")
	}
	return nil
}

func run() error {
	if err := gate(config.Getenv); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := config.CheckSecretKey(cfg.SecretKey, cfg.AppEnv); err != nil {
		return err
	}
	log := telemetry.NewLogger(os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pcfg, err := poolConfig(cfg)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrate.CheckSchema(ctx, pool, config.RequiredGoSchemaVersion); err != nil {
		return err
	}
	if err := checkRole(ctx, pool, strictRole(os.Getenv), log); err != nil {
		return err
	}
	imap, err := imapChecker(cfg, pool)
	if err != nil {
		return err
	}
	d := schedboot.Deps{Pool: pool, Cfg: cfg, Log: log, IMAPConfigured: imap}
	return serve(ctx, pool, d, schedboot.ReconcileInterval(config.Getenv), log)
}

// serve runs the reconcile loop while this process holds the scheduler lease, and releases it on exit.
func serve(ctx context.Context, pool *pgxpool.Pool, d schedboot.Deps, interval time.Duration, log *slog.Logger) error {
	lease := schedboot.NewLease(pool)
	defer lease.Release(ctx)
	return schedboot.Run(ctx, schedboot.RunConfig{
		Interval: interval, Log: log, Subsystems: d.Subsystems(), Hold: lease.Hold,
	})
}

// Session limits on every pooled connection, as in the worker: a stuck lock or runaway statement fails
// the one query, never the loop.
const (
	dbLockTimeout      = "30s"
	dbStatementTimeout = "60s"
)

func poolConfig(cfg config.Config) (*pgxpool.Config, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pcfg.MaxConns = 4 // one is parked on the lease; the bootstraps run one at a time
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["lock_timeout"] = dbLockTimeout
	pcfg.ConnConfig.RuntimeParams["statement_timeout"] = dbStatementTimeout
	pcfg.ConnConfig.RuntimeParams["TimeZone"] = "UTC" // naive timestamps are UTC whatever the server zone
	pcfg.ConnConfig.RuntimeParams["application_name"] = "opengtm-scheduler"
	return pcfg, nil
}

// imapChecker is is_imap_configured on the in-process secret resolver the worker uses. Under
// CREDENTIALS_SOURCE=http there is no resolver, so the inbound bootstrap reports that it cannot run
// (the other eight are unaffected).
func imapChecker(cfg config.Config, pool *pgxpool.Pool) (func(context.Context, string) (bool, error), error) {
	if cfg.CredentialsSource != config.CredentialsSourceLocal {
		return nil, nil
	}
	env, err := secrets.NewEnvelope(secrets.Config{
		Provider: cfg.SecretsProvider, MasterKey: cfg.SecretsMasterKey, SecretKey: cfg.SecretKey, AppEnv: cfg.AppEnv,
		VaultAddr: cfg.VaultAddr, VaultToken: cfg.VaultToken, VaultTokenFile: cfg.VaultTokenFile,
		VaultTransitKey: cfg.VaultTransitKey, VaultNamespace: cfg.VaultNamespace, VaultCACert: cfg.VaultCACert,
	}, nil)
	if err != nil {
		return nil, err
	}
	res := secrets.NewResolver(env, secrets.NewPgStore(pool), os.Getenv)
	return func(ctx context.Context, ws string) (bool, error) {
		return outreachcfg.LoadIMAP(ctx, res, ws).Configured(), nil
	}, nil
}
