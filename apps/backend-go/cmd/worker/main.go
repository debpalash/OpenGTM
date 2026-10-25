// Command worker is the Go job executor: it claims executor='go' jobs from the
// shared Postgres queue, heartbeats leases, and finalizes outcomes.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
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

	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pcfg.MaxConns = int32(cfg.DBMaxConns)
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
	rn := placeholderRunner(cfg.AppEnv)
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

	runErr := rn(jctx, c)
	cancel(nil)
	wg.Wait()

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
		out = queue.Outcome{Status: "failed", Error: runErr.Error()}
	}
	persisted, err := queue.Finalize(context.WithoutCancel(ctx), pool, c, out)
	if err != nil {
		log.Error("finalize failed", "err", err)
		return
	}
	log.Info("finalized", "status", out.Status, "persisted", persisted)
}

// placeholderRunner stands in for the real run_workbook executor (Task 12).
// GO_WORKER_TEST_BLOCK_SECONDS blocks the job, honored only when APP_ENV=test.
func placeholderRunner(appEnv string) runner {
	block := time.Duration(0)
	if appEnv == "test" {
		if s, err := strconv.Atoi(os.Getenv("GO_WORKER_TEST_BLOCK_SECONDS")); err == nil && s > 0 {
			block = time.Duration(s) * time.Second
		}
	}
	return func(ctx context.Context, _ *queue.Claim) error {
		if block > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(block):
			}
		}
		return errors.New("run_workbook executor not implemented")
	}
}
