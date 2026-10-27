package workbook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// CredentialSource resolves a workspace credential (credentials.Client).
type CredentialSource interface {
	Get(ctx context.Context, workspaceID, envVar string) (string, error)
}

// ManifestSource serves the signature-verified manifest for a provider name
// together with its sha256 (credentials.Client). credentials.ErrManifestNotFound
// is a definitive "no such manifest"; any other error is transient.
type ManifestSource interface {
	Manifest(ctx context.Context, name string) (raw json.RawMessage, sha256 string, err error)
}

// Deps are the executor's collaborators.
type Deps struct {
	Pool   *pgxpool.Pool
	Client *http.Client // SSRF-guarded egress client (egress.NewClientForEnv)
	// URLCheck vets a rendered provider URL (egress.CheckURLFor bound to the
	// app env and allowlist); nil means egress.CheckURL(url, true).
	URLCheck func(rawURL string) error
	Creds    CredentialSource
	// Manifests is required: the payload's manifests are only claims, and the
	// executor runs the fetched copy whose sha256 matches the payload's.
	Manifests ManifestSource
	Progress  progress.Publisher // nil = no broadcasts
	Log       *slog.Logger
	// Runner runs every enrichment provider call (providerrun.Runner: Go
	// providers in process, others on the Python worker). nil runs only the
	// payload's verified declarative manifests (tests, standalone use).
	Runner ProviderRunner
	// Py is the Python worker RPC for ai_formula, research and agent columns
	// and the AI batch prepass. nil hands such jobs back to Python.
	Py PyCaller
	// AIBatch configures the ai_formula batch prepass (off by default).
	AIBatch BatchConfig
	// Emitter receives the run-end on_row_changed deltas (automationhook.Emitter; nil: none).
	// AUTOMATIONS_ENABLED is the emitter's own switch.
	Emitter RowChangeEmitter
	// StopCheckInterval throttles the pause/cancel probe (Python: ~2s).
	StopCheckInterval time.Duration

	// FullRun is GO_EXECUTOR_FULL_RUN (plan 15 WP-WBEXEC, default off): conditions, email
	// verification (Verify) and output columns (Output) run here instead of being refused.
	FullRun bool
	// Provenance is PROVENANCE_TRACKING_ENABLED: per-fact provenance on produced cells and lead
	// write-backs. It only takes effect with FullRun (Python refuses provenance runs otherwise).
	Provenance bool
	// Now is the provenance clock (nil: time.Now).
	Now func() time.Time
	// Verify is the email verify cascade (emailverify.Cascade with its live deps). nil refuses
	// columns that need it, even with FullRun.
	Verify EmailVerifier
	// Output are the output-column destination clients. nil refuses output columns, even with FullRun.
	Output *OutputPorts
}

// EmailVerifier runs the email verify cascade (email_verify_cascade.verify_email).
type EmailVerifier interface {
	Verify(ctx context.Context, workspaceID, email string) (emailverify.Result, error)
}

// VerifyFunc adapts a function to EmailVerifier.
type VerifyFunc func(ctx context.Context, workspaceID, email string) (emailverify.Result, error)

func (f VerifyFunc) Verify(ctx context.Context, workspaceID, email string) (emailverify.Result, error) {
	return f(ctx, workspaceID, email)
}

func (rs *runState) now() time.Time {
	if rs.deps.Now != nil {
		return rs.deps.Now()
	}
	return time.Now()
}

// provenanceOn is PROVENANCE_TRACKING_ENABLED for a run this executor may carry out.
func (rs *runState) provenanceOn() bool { return rs.deps.FullRun && rs.deps.Provenance }

// RowChangeEmitter is the on_row_changed slice of automationhook.Emitter.
type RowChangeEmitter interface {
	RowsChanged(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, changes []automationhook.Change) (int, error)
}

// PyCaller is the Python worker RPC (pyworker.Client).
type PyCaller interface {
	Call(ctx context.Context, call pyworker.ProviderCall) (pyworker.ProviderResult, error)
}

// BatchConfig controls the ai_formula batch prepass (WORKBOOK_BATCH_*).
type BatchConfig struct {
	Enabled bool
	MinRows int // fewer eligible cells than this run synchronously (default 20)
}

// Result is the run summary persisted as the job's execution_result receipt.
type Result struct {
	Completed, Errors, Total, Rows int
	Stopped                        bool

	// skipReceipt mirrors Python returning {"error": "workbook_not_found"},
	// which persist_run_result ignores.
	skipReceipt bool
}

const defaultStopCheckInterval = 2 * time.Second

// runState is the per-run context shared by every cell goroutine.
type runState struct {
	deps  Deps
	claim *queue.Claim
	p     *runPayload
	pl    planner
	log   *slog.Logger
	stop  *stopProbe
	// rowsDone is the number of rows the first pass has finished (written by
	// the single coordinating goroutine only).
	rowsDone int
	// allCols is the whole columns_config as a graph (dependency checks look at
	// every column, not just the ones this run executes); cycleBlocked caches
	// colgraph.CycleBlocked over it.
	runner ProviderRunner
	// cell identifies the spend run of an in-request cell run (claim == nil).
	cell *cellIdentity
	// client is the caller of an in-request cell run: its cancellation stops the run only
	// until the first paid dispatch (the run itself is detached from it).
	client context.Context
	// paidDispatched latches at the first paid dispatch: the client no longer gates the run.
	paidDispatched atomic.Bool
	changeMu       sync.Mutex
	changes        []rowChange // pending on_row_changed deltas, flushed after the run commits
	noRetryMu      sync.Mutex
	noRetry        map[cellKey]bool // cells whose RPC timed out: no automatic retry
	cols           []column         // every configured column (name aliases for {column} references)
	legacy         bool             // no Deps.Runner: only the payload's manifests exist
	allCols        []colgraph.Col
	// outputExec replaces executeOutput (journal tests only).
	outputExec func(ctx context.Context, lead map[string]any, col column, leadID int64) (outResult, error)
	cycleBlocked   map[string]bool
}

func (rs *runState) publish(ctx context.Context, msg map[string]any) {
	if rs.deps.Progress != nil {
		rs.deps.Progress.Publish(ctx, rs.p.WorkbookID, msg)
	}
}

// stopProbe is _should_stop: latched, and the underlying read is throttled so
// many cells do not spam the database.
type stopProbe struct {
	mu        sync.Mutex
	stopped   bool
	checkedAt time.Time
	interval  time.Duration
	check     func() bool
}

func (s *stopProbe) shouldStop() bool {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return true
	}
	now := time.Now()
	if !s.checkedAt.IsZero() && now.Sub(s.checkedAt) < s.interval {
		s.mu.Unlock()
		return false
	}
	s.checkedAt = now
	s.mu.Unlock()
	stop := s.check()
	if stop {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
	}
	return stop
}
