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
	"strings"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signalscan"
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
	if err := config.CheckSecretKey(cfg.SecretKey, cfg.AppEnv); err != nil {
		return err
	}
	log := telemetry.NewLogger(os.Stdout)
	log.Info("starting worker", "config", cfg)
	logAutomationFlags(log, cfg)

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
	pcfg.ConnConfig.RuntimeParams["TimeZone"] = "UTC" // naive timestamps (DEFAULT now(), casts) are UTC whatever the server zone
	return pcfg, nil
}

// Job types this worker executes besides run_workbook and ambitionbox_import.
const (
	jobRefreshWorkbook = "refresh_workbook"
	jobSignalScan      = "signal_scan"
	// sourceWait bounds how long one refresh cycle waits for its Python
	// source_workbook jobs before enriching the rows found so far (the
	// refresh_workbook limit is 900s).
	sourceWait = 600 * time.Second
)

// jobLimit is the wall-clock limit per job type: Python JOB_TIMEOUTS
// (run_workbook 1800 configurable, ambitionbox_import 900, refresh_workbook 900,
// signal_scan 300).
func jobLimit(cfg config.Config, jobType string) time.Duration {
	if d, ok := governanceLimit(jobType); ok {
		return d
	}
	if d, ok := audienceLimit(jobType); ok {
		return d
	}
	if d, ok := leadgenLimit(jobType); ok {
		return d
	}
	switch jobType {
	case ambitionimport.JobType:
		return ambitionimport.JobTimeout
	case jobRefreshWorkbook:
		return 900 * time.Second
	case jobSignalScan:
		return 300 * time.Second
	case jobTriggerEval, jobWatchPoll:
		return automationTimeout
	case jobSend:
		return outreachsend.DefaultJobTimeout // Python JOB_TIMEOUTS["send"]
	case outreachinbound.JobType:
		return outreachInboundTimeout
	}
	return cfg.JobTimeout
}

// dispatch routes a claim to the handler of its type. A type without a handler
// fails permanently (ClaimNext only claims registered types, so this is a
// backstop).
func dispatch(handlers map[string]runner) runner {
	return func(ctx context.Context, c *queue.Claim) error {
		h, ok := handlers[c.Type]
		if !ok {
			return &queue.PermanentError{Err: fmt.Errorf("worker: no handler for job type %q", c.Type)}
		}
		return h(ctx, c)
	}
}

// signalCaps maps the SIGNAL_SCAN_* settings to the scan caps.
func signalCaps(e config.ExecutorSettings) signalscan.Caps {
	return signalscan.Caps{HotLeads: e.SignalHotLeads, WarmLeads: e.SignalWarmLeads,
		PerWorkspace: e.SignalPerWorkspace, Global: e.SignalGlobal}
}

// runWithDeadline runs rn under the per-job wall-clock limit (Python:
// JOB_TIMEOUTS[type]). When the limit, not a cancel from above, ends
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

	runErr := runWithDeadline(jctx, log, rn, c, jobLimit(cfg, c.Type))
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
	var localSecrets *secrets.Resolver
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
		localSecrets = secrets.NewResolver(env, secrets.NewPgStore(pool), os.Getenv)
		deps.Creds = credentials.NewLocal(localSecrets, allow.Allowed, nil)
	}
	var pyRPC *pyworker.Client
	deps.Runner, deps.Py, pyRPC = buildProviderRunner(cfg, pool, log, api, localSecrets)
	deps.AIBatch = workbook.BatchConfig{Enabled: envBool("GO_AI_BATCH_ENABLED"), MinRows: envInt("WORKBOOK_BATCH_MIN_ROWS", 20)}
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
	// One emitter for every Go write site: run-end on_rows_changed, poller signals, ambitionbox
	// rows and signal scan all enqueue trigger_eval for the executor the cohort names (F7).
	emitter := automationEmitter(cfg, log)
	deps.Emitter = emitter
	handle := workbook.Handler(deps)
	// ambitionbox_import: the client is built on the SSRF-guarded egress client;
	// the failure hook reconciles run and workbook after a persisted failure.
	abDeps := ambitionimport.Deps{Pool: pool, Fetcher: ambitionbox.NewEgress(cfg.AppEnv, cfg.EgressAllowHosts),
		Hook: emitter, Progress: prog, Log: log}
	queue.AllowJobType(ambitionimport.JobType)
	ambitionimport.RegisterFailureHook(abDeps)
	block := time.Duration(0)
	if cfg.AppEnv == "test" {
		if s, err := strconv.Atoi(os.Getenv("GO_WORKER_TEST_BLOCK_SECONDS")); err == nil && s > 0 {
			block = time.Duration(s) * time.Second
		}
	}
	// refresh_workbook / signal_scan: the next occurrence's executor follows the
	// go_executor cohort flags (a Go worker handled this job, but the flags may
	// have changed since it was enqueued).
	rfDeps := refresh.Deps{Pool: pool, SourceWait: sourceWait, NextExecutor: ""}
	ssDeps := signalscan.Deps{Pool: pool, Hook: abDeps.Hook, Log: log, Caps: ptr(signalCaps(cfg.Executor)),
		RefreshExecutor: cfg.Executor.RefreshExecutor, ScanExecutor: cfg.Executor.SignalScanExecutor}
	if pyRPC != nil {
		ssDeps.Py = pyRPC
	}
	rfFail := rfDeps
	rfFail.NextExecutorFor = cfg.Executor.RefreshExecutor
	refresh.RegisterFailureHook(rfFail) // a refresh job that exhausts its retries re-seeds the chain
	queue.AllowJobType(jobRefreshWorkbook)
	queue.AllowJobType(jobSignalScan)
	handlers := map[string]runner{
		queue.JobTypeRunWorkbook: handle,
		ambitionimport.JobType:   func(ctx context.Context, c *queue.Claim) error { return ambitionimport.Handle(ctx, abDeps, c) },
		jobRefreshWorkbook: func(ctx context.Context, c *queue.Claim) error {
			d := rfDeps
			d.NextExecutor = cfg.Executor.RefreshExecutor(c.WorkspaceID)
			return refresh.Handle(ctx, d, c)
		},
		jobSignalScan: func(ctx context.Context, c *queue.Claim) error { return signalscan.HandleScan(ctx, ssDeps, c) },
	}
	var sec autoactions.SecretGetter = apiSecrets{api}
	if localSecrets != nil {
		sec = localSecrets
	}
	for typ, h := range buildAutomations(cfg, pool, log, deps, sec, localSecrets, pyRPC, emitter) {
		handlers[typ] = h
	}
	for typ, h := range buildOutreach(cfg, pool, log, localSecrets, outreachOpts{}) {
		handlers[typ] = h
	}
	for typ, h := range governanceHandlers(cfg, pool, log, pyRPC, governanceOpts{}) {
		handlers[typ] = h
	}
	for typ, h := range audienceHandlers(cfg, pool, log, sec, emitter, audienceOpts{}) {
		handlers[typ] = h
	}
	for typ, h := range leadgenHandlers(cfg, pool, log, pyRPC, leadgenOpts{}) {
		handlers[typ] = h
	}
	route := dispatch(handlers)
	return func(ctx context.Context, c *queue.Claim) error {
		if block > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(block):
			}
		}
		return route(ctx, c)
	}, closeFn, nil
}

// apiSecrets serves providerrun credentials through the Python credentials
// API; global settings (no workspace) are not served by it, so they fall back
// to the process environment (REACHER_URL, SEC_EDGAR_USER_AGENT, ...).
type apiSecrets struct{ api *credentials.Client }

func (a apiSecrets) Get(ctx context.Context, ws, key string) (string, string, error) {
	if ws == "" {
		if v := os.Getenv(key); v != "" {
			return v, "env", nil
		}
		return "", "", nil
	}
	v, err := a.api.Get(ctx, ws, key)
	return v, secrets.SourceAPI, err
}

// buildProviderRunner wires providerrun.Runner (Go providers in process, the
// rest through the Python worker) and the Python RPC client. The Python client
// is returned as an interface only when PY_WORKER_URL is configured, so a nil
// client can never hide behind a non-nil interface: columns that need Python
// are then handed back.
func buildProviderRunner(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, api *credentials.Client, local *secrets.Resolver) (workbook.ProviderRunner, workbook.PyCaller, *pyworker.Client) {
	var sec providerrun.Secrets = apiSecrets{api}
	if local != nil {
		sec = local
	}
	var py workbook.PyCaller
	var pyRun providerrun.PyCaller
	var client *pyworker.Client
	if cfg.Executor.PyWorkerURL != "" {
		client = pyworker.New(cfg.Executor.PyWorkerURL, cfg.Executor.PyWorkerToken, nil)
		py, pyRun = client, client
	}
	return providerrun.New(providerrun.Deps{Pool: pool, Secrets: sec, Py: pyRun,
		OperatorAllow: cfg.EgressAllowHosts, Log: log}), py, client
}

func ptr[T any](v T) *T { return &v }

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && n > 0 {
		return n
	}
	return def
}
