package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxRunBody caps the request body of POST /run (Python has no limit; a run
// names at most the workbook's rows, a few hundred KB for 10^4 ids).
const maxRunBody = 4 << 20

// runDebitReason is check_and_debit's default reason.
const runDebitReason = "run_debit"

// Test seams: the enqueue of the run job and the run id generator.
var (
	runEnqueue = enqueueRunJob
	runNewID   func() string // nil: runplan generates a uuid4 hex
)

// enqueueRunJob inserts the run_workbook job with the planned executor inside tx.
func enqueueRunJob(ctx context.Context, tx pgx.Tx, ws, executor string, payload json.RawMessage) (int64, error) {
	id, err := gen.New(tx).EnqueueRunWorkbookJob(ctx, gen.EnqueueRunWorkbookJobParams{
		Executor: executor, Payload: string(payload), WorkspaceID: ws,
	})
	return int64(id), err
}

// registerWorkbookRun registers POST /api/workbooks/{workbook_id}/run (Task 24,
// routers/workbooks.py run_workbook): require_editor, 20/minute.
//
// Everything the run writes happens in ONE workspace transaction, in Python's
// order but atomically (documented deviation D4; Python commits the debit
// first and loses the credits when the enqueue fails): plan (runplan.Build,
// read only), debit (run:<run_id>, once per run id), workbook status flip,
// supersede of the workbook's in-flight run job and the enqueue with the
// executor the plan chose. Any failure rolls all of it back.
func registerWorkbookRun(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := &runRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}}, index: newManifestIndexer(d.getenv())}
	if d.Registry != nil && d.Envelope != nil {
		app := &settings.App{Store: secrets.NewPgStore(d.Pool), Env: d.Envelope, Secret: d.Registry.SecretSet(), Getenv: d.getenv()}
		h.perf = &settings.Perf{App: app, Reg: d.Registry}
	}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin")}
	if d.Limiter != nil { // slowapi runs after the dependencies: authentication and the role gate come first
		mws = append(mws, d.Limiter.Limit("20/minute"))
	}
	r.Handle("POST /api/workbooks/{workbook_id}/run", http.HandlerFunc(h.run), mws...)
}

type runRoutes struct {
	d     Deps
	svc   *wbcrud.Service
	perf  *settings.Perf
	index *manifestIndexer
}

// runResponse is RunWorkbookResponse in Python field order.
type runResponse struct {
	Status    string  `json:"status"`
	TotalJobs int     `json:"total_jobs"`
	Message   string  `json:"message"`
	JobID     *int64  `json:"job_id"`
	RunID     *string `json:"run_id"`
}

func (h *runRoutes) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h *runRoutes) run(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	req, errs := runplan.ParseRunRequest(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	ctx := r.Context()
	wsCtx, _ := identity.WorkspaceFrom(ctx)
	ws, wbID := wsCtx.WorkspaceID, r.PathValue("workbook_id")
	cfg := h.config(ctx, ws)

	var resp runResponse
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
		wb, err := h.svc.Owned(ctx, tx, ws, wbID, true) // FOR UPDATE: concurrent runs of one workbook serialize
		if errors.Is(err, wbcrud.ErrNotFound) {
			return &wbcrud.GuardError{Status: http.StatusNotFound, Detail: wbcrud.NotFoundDetail}
		}
		if err != nil {
			return err
		}
		plan, err := runplan.Build(ctx, tx, wb, req, cfg)
		if err != nil {
			return err
		}
		if plan.Skipped {
			resp = runResponse{Status: "skipped", Message: plan.Message}
			return nil
		}
		audit.SetMetadata(ctx, runAuditMetadata(wbID, plan, req))

		// Billing: a no-op (and no ledger access at all) unless BILLING_ENABLED and something is platform billed.
		if h.d.Config.BillingEnabled && plan.ProjectedUSD > 0 {
			if _, err := billing.CheckAndDebit(ctx, tx, true, ws, plan.ProjectedUSD, plan.RunID, runDebitReason); err != nil {
				return err
			}
		}
		q := gen.New(tx)
		if err := q.StartWorkbookRun(ctx, gen.StartWorkbookRunParams{TotalRows: int32(plan.MatchedRows), WorkbookID: wbID, WorkspaceID: ws}); err != nil {
			return err
		}
		if err := q.CancelActiveRunJobs(ctx, wbID); err != nil {
			return err
		}
		jobID, err := runEnqueue(ctx, tx, ws, plan.Executor, plan.Payload)
		if err != nil {
			return err
		}
		runID := plan.RunID
		resp = runResponse{Status: "started", TotalJobs: plan.TotalJobs, Message: plan.Message, JobID: &jobID, RunID: &runID}
		return nil
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// fail answers guards as FastAPI's HTTPException, insufficient credits as the
// 402 detail object, anything else as a bare 500 (the error text may carry SQL).
func (h *runRoutes) fail(w http.ResponseWriter, err error) {
	var g *wbcrud.GuardError
	if errors.As(err, &g) {
		httpx.Detail(w, g.Status, g.Detail)
		return
	}
	var ic *billing.InsufficientCreditsError
	if errors.As(err, &ic) {
		body, merr := insufficientBody(ic)
		if merr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write(body)
			return
		}
	}
	h.log().Error("workbook run failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// insufficientBody is {"detail":{"message","balance_usd","required_usd"}} with
// Python's float spelling (10.0). Billing treats Inf as insufficient, and
// Infinity/NaN are not JSON: non-finite amounts are clamped to the largest
// finite float64 (NaN to 0) so the 402 can always be encoded.
func insufficientBody(e *billing.InsufficientCreditsError) ([]byte, error) {
	num := func(f float64) json.Number {
		switch {
		case math.IsNaN(f):
			f = 0
		case math.IsInf(f, 1):
			f = math.MaxFloat64
		case math.IsInf(f, -1):
			f = -math.MaxFloat64
		}
		return json.Number(wbcrud.PyFloatRepr(f))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		Detail struct {
			Message     string      `json:"message"`
			BalanceUSD  json.Number `json:"balance_usd"`
			RequiredUSD json.Number `json:"required_usd"`
		} `json:"detail"`
	}{Detail: struct {
		Message     string      `json:"message"`
		BalanceUSD  json.Number `json:"balance_usd"`
		RequiredUSD json.Number `json:"required_usd"`
	}{e.Error(), num(e.BalanceUSD), num(e.RequiredUSD)}})
	return bytes.TrimRight(buf.Bytes(), "\n"), err
}

func runAuditMetadata(wbID string, plan runplan.Plan, req runplan.RunRequest) map[string]any {
	md := map[string]any{
		"action": "workbook.run", "workbook_id": wbID, "matched_rows": plan.MatchedRows,
		"column_ids": plan.ColumnIDs, "view_id": nil, "search_applied": strings.TrimSpace(req.Search) != "",
		"fill_missing": req.FillMissing, "force": req.Force,
	}
	if plan.RowColumns != nil {
		md["row_columns"] = plan.RowColumns
	} else {
		md["row_columns"] = nil
	}
	if req.ViewID != "" {
		md["view_id"] = req.ViewID
	}
	return md
}

// executorConfig is the Go-executor routing read from the environment.
type executorConfig struct {
	Enabled     bool            // GO_EXECUTOR_ENABLED
	Cohort      map[string]bool // GO_EXECUTOR_WORKSPACES
	Provenance  bool            // PROVENANCE_TRACKING_ENABLED
	ExtraTypes  map[string]bool // GO_EXECUTOR_COLUMN_TYPES
	Concurrency int             // WORKBOOK_RUN_CONCURRENCY (default 12)
}

// goExtraTypes are the column types GO_EXECUTOR_COLUMN_TYPES may add; anything
// else is dropped. NOTE: a non-empty set also lifts runplan's column-reference
// and structured-target hand-backs (the Go executor then orders referenced
// columns), so enabling a type is also enabling those runs for the cohort.
var goExtraTypes = map[string]bool{"formula": true, "http": true, "ai_formula": true, "research": true, "agent": true}

// executorSettings reads the routing flags with pydantic's bool spelling.
func executorSettings(getenv func(string) string) executorConfig {
	truthy := func(k string) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(k))) {
		case "1", "true", "yes", "on", "t", "y":
			return true
		}
		return false
	}
	c := executorConfig{Enabled: truthy("GO_EXECUTOR_ENABLED"), Provenance: truthy("PROVENANCE_TRACKING_ENABLED"),
		Cohort: map[string]bool{}, ExtraTypes: map[string]bool{}, Concurrency: 12}
	for _, w := range strings.Split(getenv("GO_EXECUTOR_WORKSPACES"), ",") {
		if w = strings.TrimSpace(w); w != "" {
			c.Cohort[w] = true
		}
	}
	for _, t := range strings.Split(getenv("GO_EXECUTOR_COLUMN_TYPES"), ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); goExtraTypes[t] {
			c.ExtraTypes[t] = true
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv("WORKBOOK_RUN_CONCURRENCY"))); err == nil && n > 0 {
		c.Concurrency = n
	}
	return c
}

// config assembles runplan.Config for one request.
func (h *runRoutes) config(ctx context.Context, ws string) runplan.Config {
	ex := executorSettings(h.d.getenv())
	perf := wbcrud.DefaultPerf
	if h.perf != nil {
		if p, err := h.perf.Get(ctx); err == nil {
			perf = wbcrud.PerfValues{RowConcurrency: p.RowConcurrency, ProviderWorkers: p.ProviderWorkers,
				ProviderTimeout: p.ProviderTimeout, MaxProviders: p.MaxProviders, RetryPasses: p.RetryPasses}
		}
	}
	return runplan.Config{
		WorkspaceID:        ws,
		Perf:               runplan.Perf{RowConcurrency: perf.RowConcurrency, MaxProviders: perf.MaxProviders, RetryPasses: perf.RetryPasses, ProviderWorkers: perf.ProviderWorkers, ProviderTimeout: perf.ProviderTimeout},
		DefaultConcurrency: ex.Concurrency,
		GoExecutorEnabled:  ex.Enabled, GoCohort: ex.Cohort, ProvenanceTracking: ex.Provenance,
		AutomationsEnabled: h.d.Flags.AutomationsEnabled, ExtraColumnTypes: ex.ExtraTypes,
		Index: h.index.get(), Leads: wbcrud.PGLeads{}, NewRunID: runNewID,
	}
}

// manifestIndexer loads the declarative manifest index once (Python caches it
// for the process): the connectors directory, trust store and signature policy
// are the ones the connectors routes use.
type manifestIndexer struct {
	getenv func(string) string
	once   sync.Once
	idx    map[string]manifest.Entry
}

func newManifestIndexer(getenv func(string) string) *manifestIndexer {
	return &manifestIndexer{getenv: getenv}
}

func (m *manifestIndexer) get() map[string]manifest.Entry {
	m.once.Do(func() {
		dir, _ := connectorsLookupEnv(connectorManifestsEnv)
		trust, _ := connectorsLookupEnv(connectorTrustStoreEnv)
		if dir == "" {
			dir = defaultConnectorManifests
		}
		if trust == "" {
			trust = defaultConnectorTrust
		}
		policy, set := connectorsLookupEnv(connectorPolicyEnv)
		if !set {
			policy = "optional"
		}
		idx, err := manifest.LoadIndex(dir, policy, manifest.LoadTrustStore(trust))
		if err != nil {
			slog.Default().Warn("run: manifest index unavailable, every run stays on python", "err_type", errTypeName(err))
			idx = map[string]manifest.Entry{}
		}
		m.idx = idx
	})
	return m.idx
}

// WireRunPlan connects the vendor catalog, the research flag and billing's
// platform-cost projection (runplan.Wire). cmd/api calls it exactly once at
// startup, before RegisterAll; without it only `research` is platform billed.
func WireRunPlan(d Deps) {
	deps := runplan.WireDeps{Index: newManifestIndexer(d.getenv()).get}
	if d.Pool != nil && d.Registry != nil && d.Envelope != nil {
		app := &settings.App{Store: secrets.NewPgStore(d.Pool), Env: d.Envelope, Secret: d.Registry.SecretSet(), Getenv: d.getenv()}
		deps.AppSetting = app.Get
	}
	runplan.Wire(deps)
}
