// Command seed is the opt-in first-run demo (compose "demo" profile), the Go port of
// apps/api/scripts/seed_demo.py. It seeds a demo workbook into the workspace that first-run setup
// created and starts the first enrichment run. It never creates a user: complete /setup in the UI first
// (until then it exits with status 3 and writes nothing, or waits SEED_WAIT_SECONDS for setup).
//
//	SEED_WORKSPACE_SLUG   workspace to seed (default main); SEED_WORKSPACE_ID selects by id instead
//	SEED_START_RUN        0 seeds the workbook only (default 1: start the run through the API)
//	SEED_FORCE_RUN        1 starts a run even when the demo workbook already exists
//	SEED_API_URL          API base for the run (default http://127.0.0.1:8000)
//	SEED_WAIT_SECONDS     wait this long for first-run setup to finish (default 0)
//
// SEED_ADMIN_USERNAME / SEED_ADMIN_PASSWORD of the Python seed are ignored (and reported).
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

// exitNotReady is the status when first-run setup has not created a workspace yet.
const exitNotReady = 3

func main() {
	os.Exit(run(os.Getenv, os.Stdout))
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func run(getenv func(string) string, out io.Writer) int {
	log := telemetry.NewLogger(out)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := config.LoadAPI()
	if err != nil {
		log.Error("seed: config", "err", err.Error())
		return 1
	}
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Error("seed: parse DATABASE_URL")
		return 1
	}
	pcfg.MaxConns = 2
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["TimeZone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		log.Error("seed: database", "err", err.Error())
		return 1
	}
	defer pool.Close()
	if err := checkRole(ctx, pool, cfg.RLSRequireSafeRole, log); err != nil {
		log.Error("seed: " + err.Error())
		return 1
	}
	return seed(ctx, pool, cfg, getenv, log)
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

func seed(ctx context.Context, pool *pgxpool.Pool, cfg config.APIConfig, getenv func(string) string, log *slog.Logger) int {
	if getenv("SEED_ADMIN_USERNAME") != "" || getenv("SEED_ADMIN_PASSWORD") != "" {
		log.Warn("seed: SEED_ADMIN_USERNAME/SEED_ADMIN_PASSWORD are ignored: the first admin is created in the UI (first-run setup)")
	}
	opts := demoseed.Options{WorkspaceID: getenv("SEED_WORKSPACE_ID"), WorkspaceSlug: getenv("SEED_WORKSPACE_SLUG"), Force: truthy(getenv("SEED_FORCE_RUN"))}
	if v := getenv("SEED_START_RUN"); v == "" || truthy(v) {
		base := getenv("SEED_API_URL")
		if base == "" {
			base = "http://127.0.0.1:8000"
		}
		opts.Runner = demoseed.APIRunner{BaseURL: base, Signer: auth.Signer{Secret: []byte(cfg.SecretKey), AccessTTL: 5 * time.Minute}}
	}
	wait, _ := strconv.Atoi(strings.TrimSpace(getenv("SEED_WAIT_SECONDS")))
	deadline := time.Now().Add(time.Duration(max(wait, 0)) * time.Second)
	for {
		res, err := demoseed.Seed(ctx, pool, opts)
		switch {
		case errors.Is(err, demoseed.ErrNoWorkspace):
			if time.Now().Before(deadline) && ctx.Err() == nil {
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
				continue
			}
			log.Warn("seed: " + err.Error())
			return exitNotReady
		case err != nil:
			log.Error("seed failed", "err", err.Error())
			return 1
		}
		log.Info("seed complete", "workspace", res.WorkspaceID, "created", res.Created, "run_started", res.RunStarted)
		if res.RunErr != nil {
			log.Warn("the first run did not start; run the workbook from the UI or rerun the seed", "err", res.RunErr.Error())
		}
		return 0
	}
}
