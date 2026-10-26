// Command worker is the Go job executor: it claims executor='go' jobs from the
// shared Postgres queue, heartbeats leases, and finalizes outcomes.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

const (
	idleSleep      = time.Second
	cancelInterval = 2 * time.Second
)

// claimNext claims one job using the shared per-workspace cap from cfg.
func claimNext(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, workerID string) (*queue.Claim, error) {
	return queue.ClaimNext(ctx, pool, workerID, cfg.MaxActivePerWorkspace)
}

// runner executes a claimed job; a nil error means completed.
type runner func(ctx context.Context, c *queue.Claim) error

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(os.Stdout)
	log.Info("starting worker", "config", cfg)

	rootCtx, stop := context.WithCancel(context.Background())
	defer stop()

	pcfg, err := poolConfig(cfg)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(rootCtx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := queue.CheckSchema(rootCtx, pool, config.RequiredGoSchemaRevision); err != nil {
		return err
	}
	if _, err := telemetry.StartDebugServer(rootCtx, cfg.DebugAddr); err != nil {
		return err
	}

	// claimCtx stops claiming on SIGTERM/SIGINT; jobCtx is cancelled only
	// after the shutdown grace expires.
	claimCtx, stopClaiming := signal.NotifyContext(rootCtx, syscall.SIGTERM, syscall.SIGINT)
	defer stopClaiming()
	jobCtx, cancelJobs := context.WithCancel(rootCtx)
	defer cancelJobs()

	workerID := fmt.Sprintf("go-%s-%d", hostname(), os.Getpid())
	rn, closeDeps, err := buildRunner(cfg, pool, log)
	if err != nil {
		return err
	}
	defer closeDeps()
	var wg sync.WaitGroup
	for i := 0; i < cfg.WorkerConcurrency; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			slotLoop(claimCtx, jobCtx, pool, cfg, log.With("slot", slot), workerID, rn)
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-claimCtx.Done():
		log.Info("shutdown: draining", "grace", cfg.ShutdownGrace)
		select {
		case <-done:
		case <-time.After(cfg.ShutdownGrace):
			log.Warn("shutdown grace expired; cancelling jobs, leaving rows for the reaper")
			cancelJobs()
			<-done
		}
	}
	return nil
}

// Session limits on every pooled connection: a stuck lock or runaway statement
// must fail the one query, not hold a slot (and its lease) forever.
const (
	dbLockTimeout      = "30s"
	dbStatementTimeout = "60s"
	finalizeTimeout    = 30 * time.Second
	defaultJobTimeout  = 1800 * time.Second
)

// poolConfig is the pgx pool configuration for cfg.
func poolConfig(cfg config.Config) (*pgxpool.Config, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pcfg.MaxConns = int32(cfg.DBMaxConns)
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["lock_timeout"] = dbLockTimeout
	pcfg.ConnConfig.RuntimeParams["statement_timeout"] = dbStatementTimeout
	return pcfg, nil
}

// runWithDeadline runs rn under the per-job wall-clock limit (Python:
// JOB_TIMEOUTS["run_workbook"]). When the limit, not a cancel from above, ends
// the run, the error is "job timed out ...": a failed attempt through the
// normal retry path, like a Python job timeout.
func runWithDeadline(ctx context.Context, log *slog.Logger, rn runner, c *queue.Claim, limit time.Duration) error {
	if limit <= 0 {
		limit = defaultJobTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := guarded(tctx, log, rn, c)
	if err != nil && tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("job timed out after %s", limit)
	}
	return err
}

// guarded runs rn and converts a panic into a failed-job error. The stack is
// logged (no payload or secrets in it); the error carries only the panic value,
// truncated, so the process survives and the job fails through the normal
// retry path.
func guarded(ctx context.Context, log *slog.Logger, rn runner, c *queue.Claim) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("job panicked", "stack", string(debug.Stack()))
			msg := "panic: " + fmt.Sprint(r)
			if rs := []rune(msg); len(rs) > 200 {
				msg = string(rs[:200])
			}
			err = errors.New(msg)
		}
	}()
	return rn(ctx, c)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func slotLoop(claimCtx, jobCtx context.Context, pool *pgxpool.Pool, cfg config.Config, log *slog.Logger, workerID string, rn runner) {
	for claimCtx.Err() == nil {
		c, err := claimNext(claimCtx, pool, cfg, workerID)
		if err != nil {
			if claimCtx.Err() == nil {
				log.Error("claim failed", "err", err)
			}
		}
		if err != nil || c == nil {
			select {
			case <-claimCtx.Done():
				return
			case <-time.After(idleSleep):
			}
			continue
		}
		processJob(jobCtx, pool, cfg, telemetry.JobLogger(log, c.ID, c.WorkspaceID, c.LeaseToken), c, rn)
	}
}

// processJob runs one claim with a heartbeat/cancel watcher, then finalizes.
// If the job context is cancelled by shutdown it returns without finalizing.
func processJob(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, log *slog.Logger, c *queue.Claim, rn runner) {
	jctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errLeaseLost := errors.New("lease lost or job cancelled")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		hb := time.NewTicker(cfg.HeartbeatInterval)
		cc := time.NewTicker(cancelInterval)
		defer hb.Stop()
		defer cc.Stop()
		for {
			select {
			case <-jctx.Done():
				return
			case <-hb.C:
				active, err := queue.Heartbeat(jctx, pool, c)
				if err != nil {
					log.Warn("heartbeat failed", "err", err)
				} else if !active {
					log.Warn("lease lost")
					cancel(errLeaseLost)
					return
				}
			case <-cc.C:
				if cancelled, err := queue.IsCancelled(jctx, pool, c); err == nil && cancelled {
					log.Info("job cancelled externally")
					cancel(errLeaseLost)
					return
				}
			}
		}
	}()

	limit := cfg.JobTimeout
	if c.Type == ambitionimport.JobType {
		limit = ambitionimport.JobTimeout // JOB_TIMEOUTS["ambitionbox_import"]
	}
	runErr := runWithDeadline(jctx, log, rn, c, limit)
	cancel(nil)
	wg.Wait()

	if errors.Is(runErr, workbook.ErrHandedBack) {
		// The executor already returned the job to Python with a fenced
		// update; finalizing would fail a job that is now pending elsewhere.
		log.Warn("job handed back to python", "reason", runErr.Error())
		return
	}
	if ctx.Err() != nil {
		log.Warn("shutdown: leaving claim for reaper")
		return
	}
	if context.Cause(jctx) == errLeaseLost {
		// Lease lost: Finalize is fenced and would no-op; a cancelled row
		// still needs ownership cleared, which Finalize handles.
		runErr = errLeaseLost
	}
	out := queue.Outcome{Status: "completed"}
	if runErr != nil {
		// A permanent failure (e.g. run not found) skips the retry budget.
		out = queue.Outcome{Status: "failed", Error: runErr.Error(), NoRetry: queue.IsPermanent(runErr)}
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer fcancel()
	persisted, err := queue.FinalizeAndNotify(fctx, pool, log, c, out)
	if err != nil {
		log.Error("finalize failed", "err", err)
		return
	}
	log.Info("finalized", "status", out.Status, "persisted", persisted)
}

// buildRunner wires the run_workbook executor. GO_WORKER_TEST_BLOCK_SECONDS
// blocks a job before it runs, honored only when APP_ENV=test (interop tests
// SIGKILL the worker during the block).
func buildRunner(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (runner, func(), error) {
	api := credentials.New(cfg.InternalAPIURL, cfg.InternalServiceToken, nil)
	deps := workbook.Deps{
		Pool:   pool,
		Client: egress.NewClientForEnv(cfg.AppEnv, cfg.EgressAllowHosts),
		URLCheck: func(raw string) error {
			return egress.CheckURLFor(cfg.AppEnv, cfg.EgressAllowHosts, raw, true)
		},
		Creds:     api,
		Manifests: api,
		Log:       log,
	}
	if cfg.CredentialsSource == config.CredentialsSourceLocal {
		env, err := secrets.NewEnvelope(secrets.Config{
			Provider: cfg.SecretsProvider, MasterKey: cfg.SecretsMasterKey, SecretKey: cfg.SecretKey, AppEnv: cfg.AppEnv,
			VaultAddr: cfg.VaultAddr, VaultToken: cfg.VaultToken, VaultTokenFile: cfg.VaultTokenFile,
			VaultTransitKey: cfg.VaultTransitKey, VaultNamespace: cfg.VaultNamespace, VaultCACert: cfg.VaultCACert,
		}, nil)
		if err != nil {
			return nil, nil, err
		}
		// Manifests still come from the API; their auth.env_var set is the
		// allow-list for in-process resolution.
		allow := credentials.NewManifestAllow(api)
		deps.Manifests = allow
		deps.Creds = credentials.NewLocal(secrets.NewResolver(env, secrets.NewPgStore(pool), os.Getenv), allow.Allowed, nil)
	}
	closeFn := func() {}
	var prog progress.Publisher
	if cfg.RedisURL != "" {
		pub, err := progress.NewRedis(cfg.RedisURL, log)
		if err != nil {
			return nil, nil, err
		}
		deps.Progress = pub
		prog = pub
		closeFn = func() { _ = pub.Close() }
	}
	handle := workbook.Handler(deps)
	// ambitionbox_import: the client is built on the SSRF-guarded egress client;
	// the failure hook reconciles run and workbook after a persisted failure.
	abDeps := ambitionimport.Deps{Pool: pool, Fetcher: ambitionbox.NewEgress(cfg.AppEnv, cfg.EgressAllowHosts),
		Hook: automationhook.New(cfg.Flags.AutomationsEnabled, log), Progress: prog, Log: log}
	queue.AllowJobType(ambitionimport.JobType)
	ambitionimport.RegisterFailureHook(abDeps)
	block := time.Duration(0)
	if cfg.AppEnv == "test" {
		if s, err := strconv.Atoi(os.Getenv("GO_WORKER_TEST_BLOCK_SECONDS")); err == nil && s > 0 {
			block = time.Duration(s) * time.Second
		}
	}
	return func(ctx context.Context, c *queue.Claim) error {
		if block > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(block):
			}
		}
		if c.Type == ambitionimport.JobType {
			return ambitionimport.Handle(ctx, abDeps, c)
		}
		return handle(ctx, c)
	}, closeFn, nil
}
