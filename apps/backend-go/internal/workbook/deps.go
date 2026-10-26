package workbook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
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
	// StopCheckInterval throttles the pause/cancel probe (Python: ~2s).
	StopCheckInterval time.Duration
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
	runner       ProviderRunner
	cols         []column // every configured column (name aliases for {column} references)
	legacy       bool     // no Deps.Runner: only the payload's manifests exist
	allCols      []colgraph.Col
	cycleBlocked map[string]bool
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
