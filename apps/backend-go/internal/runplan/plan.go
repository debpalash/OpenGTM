package runplan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Workbook is the stored workbook a run is planned for.
type Workbook = wbcrud.Workbook

// Perf is get_enrichment_settings(): the performance knobs stamped into the
// job payload.
type Perf struct {
	RowConcurrency, MaxProviders, RetryPasses, ProviderWorkers, ProviderTimeout int
}

// Config is everything Build reads besides the request and the workbook. All
// of it is injected: Build touches no package-level state.
type Config struct {
	WorkspaceID string
	Perf        Perf
	// DefaultConcurrency is enrichment.DEFAULT_CONCURRENCY (WORKBOOK_RUN_CONCURRENCY, 12).
	DefaultConcurrency int

	// Executor routing (go_executor.go_eligibility).
	GoExecutorEnabled  bool
	GoCohort           map[string]bool // GO_EXECUTOR_WORKSPACES
	ProvenanceTracking bool
	AutomationsEnabled bool
	// ExtraColumnTypes is GO_EXECUTOR_COLUMN_TYPES: computed (formula, http) and
	// python-run (ai_formula, research, agent) column types the Go executor may
	// take besides enrichment/waterfall. output is never allowed.
	ExtraColumnTypes map[string]bool
	// NativeProviders is GO_RUN_NATIVE_PROVIDERS (Go-only, default off = Python parity): chains
	// with providers outside the manifest index (Go-native or pyworker) stay on Go instead of
	// being handed back as non_declarative_provider.
	NativeProviders bool
	// FullRun is GO_EXECUTOR_FULL_RUN (Go-only, default off = Python parity): the Go executor
	// writes provenance, emits automations, evaluates conditions, verifies emails, delivers
	// output columns and runs formula/http columns and column references, so those refusals close.
	FullRun bool
	// Index is the manifest index (manifest.LoadIndex); nil means no declarative providers.
	Index map[string]manifest.Entry

	// Leads reads a v1 (legacy leads-filter) workbook's leads. Required only for such workbooks.
	Leads wbcrud.LeadReader
	// Cost is the per-lookup provider cost written into the go block; nil uses vendorcatalog.BaseCost.
	Cost func(provider string) float64
	// NewRunID returns a fresh uuid4 hex; nil generates one.
	NewRunID func() string
}

// Plan is what a run will do. A Skipped plan carries only Message.
type Plan struct {
	Skipped bool
	Message string // "No enrichment columns to run" | "No rows to process" | "Enqueued run: N rows × M columns"

	Columns   []colgraph.Col // selected columns, output columns last
	ColumnIDs []string
	// Rows are the resolved v2 row ids (nil for a v1 workbook), LeadIDs the
	// resolved v1 lead ids (nil for v2); the executor never re-derives either.
	Rows    []int64
	LeadIDs []int64
	// RowColumns is the explicit cell scope restricted to Rows (nil = every column of every row).
	RowColumns map[string][]string

	MatchedRows int // rows (or leads) the run covers
	TotalJobs   int // cells

	RunID    string
	Executor string // "go" | "python"
	Reason   string // why Python was kept (or "ok")
	Payload  json.RawMessage

	// Billing inputs: the provider chain per column key and the worst-case
	// platform-billed USD (billing.ProjectedPlatformCost), zero when nothing is platform billed.
	ProvidersByCol map[string][]string
	ColumnCounts   map[string]int // nil unless the run has a cell scope
	ProjectedUSD   float64
}

func unprocessable(detail string) error { return &wbcrud.GuardError{Status: 422, Detail: detail} }
func conflict(detail string) error      { return &wbcrud.GuardError{Status: 409, Detail: detail} }

func skipped(msg string) Plan { return Plan{Skipped: true, Message: msg} }

// toCols reads the workbook's column configs as colgraph columns.
func toCols(cols []wbcrud.Column) ([]colgraph.Col, error) {
	out := make([]colgraph.Col, len(cols))
	for i, c := range cols {
		raw, err := c.MarshalJSON()
		if err != nil {
			return nil, err
		}
		if out[i], err = colgraph.ParseCol(raw); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Build plans a run: workbooks.py run_workbook up to (not including) the
// debit, the workbook status update and the enqueue, plus the executor choice
// and payload of go_executor.enqueue_run_workbook. Request problems are
// *wbcrud.GuardError (422 cell scope or cycle, 409 drift or legacy view, 404
// or 409 from a saved view); database failures are returned as is.
func Build(ctx context.Context, tx pgx.Tx, wb Workbook, req RunRequest, cfg Config) (Plan, error) {
	all, err := toCols(wb.Columns)
	if err != nil {
		return Plan{}, fmt.Errorf("runplan: columns: %w", err)
	}
	cols := SelectColumns(all, req.ColumnIDs)
	var colIDs []string
	allowed := map[string]bool{}
	for _, c := range cols {
		if id, ok := colID(c); ok {
			colIDs = append(colIDs, id)
			allowed[id] = true
		}
	}

	if req.RowColumns != nil {
		if err := ValidateCellScope(req.RowColumns); err != nil {
			return Plan{}, unprocessable(err.Error())
		}
		if req.RowIDs == nil || !sameKeys(req.RowColumns, req.RowIDKeys()) {
			return Plan{}, unprocessable("Cell scope requires matching explicit row IDs")
		}
		for _, ids := range req.RowColumns {
			for _, id := range ids {
				if !allowed[id] {
					return Plan{}, unprocessable("Cell scope references an unavailable run column")
				}
			}
		}
	}
	if len(cols) == 0 {
		return skipped("No enrichment columns to run"), nil
	}

	selected := map[string]bool{}
	if req.RowColumns != nil {
		for _, ids := range req.RowColumns {
			for _, id := range ids {
				selected[id] = true
			}
		}
	} else {
		for id := range allowed {
			selected[id] = true
		}
	}
	var cyclic []string
	blocked := colgraph.CycleBlocked(all)
	for id := range selected {
		if blocked[id] {
			cyclic = append(cyclic, id)
		}
	}
	if len(cyclic) > 0 {
		sort.Strings(cyclic)
		return Plan{}, unprocessable("Selected columns depend on a circular reference: " + strings.Join(cyclic, ", ") +
			". Edit their column references to break the cycle before running.")
	}

	rows, leadIDs, err := resolveRows(ctx, tx, wb, req, cfg)
	if err != nil {
		return Plan{}, err
	}

	if req.RowColumns != nil {
		if !sameResolved(rows, req.RowIDKeys()) {
			return Plan{}, conflict("Selected workbook rows changed. Refresh before running.")
		}
		kept := rows[:0:0]
		for _, id := range rows {
			if len(req.RowColumns[strconv.FormatInt(id, 10)]) > 0 {
				kept = append(kept, id)
			}
		}
		rows = kept
	}
	matched := len(rows)
	if rows == nil {
		matched = len(leadIDs)
	}
	if req.ExpectedRows != nil && int64(matched) != *req.ExpectedRows {
		return Plan{}, conflict("The matching row count changed. Refresh the run review before starting.")
	}
	if matched == 0 {
		return skipped("No rows to process"), nil
	}

	plan := Plan{Columns: cols, ColumnIDs: colIDs, Rows: rows, LeadIDs: leadIDs, MatchedRows: matched}
	plan.ProvidersByCol = providersByCol(cols)
	var scope ordered
	if req.RowColumns != nil {
		plan.RowColumns = make(map[string][]string, len(rows))
		plan.ColumnCounts = map[string]int{}
		for _, id := range rows {
			key := strconv.FormatInt(id, 10)
			ids := req.RowColumns[key]
			plan.RowColumns[key] = ids
			scope = append(scope, kv{key, ids})
			distinct := map[string]bool{}
			for _, c := range ids {
				distinct[c] = true
			}
			plan.TotalJobs += len(distinct)
			for c := range distinct {
				plan.ColumnCounts[c]++
			}
		}
		if scope == nil {
			scope = ordered{}
		}
	} else {
		plan.TotalJobs = matched * len(cols)
	}
	plan.ProjectedUSD = billing.ProjectedPlatformCost(matched, plan.ProvidersByCol, plan.ColumnCounts)
	plan.Message = fmt.Sprintf("Enqueued run: %d rows × %d columns", matched, len(cols))

	plan.RunID = newRunID(cfg)
	fields := jobFields{
		WorkbookID: wb.ID, WorkspaceID: cfg.WorkspaceID, RunID: plan.RunID, ColumnIDs: colIDs,
		RowColumns: scope, RowIDs: rows, LeadIDs: leadIDs, Perf: cfg.Perf,
		FillMissing: req.FillMissing, Force: req.Force,
	}
	plan.Executor = "python"
	var goBlock []byte
	ok, reason := Eligible(colsByID(all, colIDs), cfg.Index, cfg)
	plan.Reason = reason
	if ok {
		cost := cfg.Cost
		if cost == nil {
			cost = vendorcatalog.BaseCost
		}
		if goBlock, err = GoBlockRequiring(colsByID(all, colIDs), cfg.Index, cost, cfg.DefaultConcurrency,
			RequiredCapabilities(colsByID(all, colIDs), cfg)); err != nil {
			goBlock, plan.Reason = nil, ReasonEligibilityError // fail safe: Python executes
		} else {
			plan.Executor = "go"
		}
	}
	if plan.Payload, err = jobPayload(fields, goBlock); err != nil {
		return Plan{}, fmt.Errorf("runplan: payload: %w", err)
	}
	return plan, nil
}

func newRunID(cfg Config) string {
	if cfg.NewRunID != nil {
		return cfg.NewRunID()
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

func sameKeys(m map[string][]string, sortedKeys []string) bool {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return equalStrings(keys, dedupe(sortedKeys))
}

func dedupe(sorted []string) []string {
	var out []string
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameResolved is set(resolved_row_ids) == set(body.row_ids).
func sameResolved(rows []int64, sortedKeys []string) bool {
	keys := make([]string, len(rows))
	for i, id := range rows {
		keys[i] = strconv.FormatInt(id, 10)
	}
	sort.Strings(keys)
	return equalStrings(dedupe(keys), dedupe(sortedKeys))
}

// providersByCol is {c.get("id") or target: chain} for billing: a configured
// waterfall (even empty) wins, else the single provider, else the default chain.
func providersByCol(cols []colgraph.Col) map[string][]string {
	out := map[string][]string{}
	for _, c := range cols {
		key := target(c)
		if truthy(c.Get("id")) {
			key = manifest.PyStr(c.Get("id"))
		}
		chain, err := providerChain(c)
		if err != nil {
			chain = nil
		}
		out[key] = chain
	}
	return out
}

// resolveRows resolves the exact row set of the run, once: explicit row ids,
// else lead ids, else a saved view and/or search, else every row; a workbook
// without v2 rows falls back to its legacy leads.
func resolveRows(ctx context.Context, tx pgx.Tx, wb Workbook, req RunRequest, cfg Config) (rows, leadIDs []int64, err error) {
	q := gen.New(tx)
	count, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: wb.ID, WorkspaceID: cfg.WorkspaceID})
	if err != nil {
		return nil, nil, err
	}
	hasSearch := manifest.PyStrip(req.Search) != ""
	if count > 0 {
		switch {
		case req.RowIDs != nil:
			rows, err = q.RunplanRowIDsByRowIDs(ctx, gen.RunplanRowIDsByRowIDsParams{WorkbookID: wb.ID, WorkspaceID: cfg.WorkspaceID, RowIds: req.RowIDs})
		case req.LeadIDs != nil:
			rows, err = q.RunplanRowIDsByLeadIDs(ctx, gen.RunplanRowIDsByLeadIDsParams{WorkbookID: wb.ID, WorkspaceID: cfg.WorkspaceID, LeadIds: req.LeadIDs})
		case req.ViewID != "" || hasSearch:
			rows, err = viewRows(ctx, tx, wb, req)
		default:
			rows, err = q.RunplanRowIDs(ctx, gen.RunplanRowIDsParams{WorkbookID: wb.ID, WorkspaceID: cfg.WorkspaceID})
		}
		if err != nil {
			return nil, nil, err
		}
		if rows == nil {
			rows = []int64{}
		}
		return rows, nil, nil
	}
	switch {
	case req.RowIDs != nil: // an explicit v2 selection never falls back to unrelated legacy leads
		return []int64{}, nil, nil
	case req.LeadIDs != nil && len(req.LeadIDs) == 0:
		return nil, []int64{}, nil
	case !wb.UsesLegacyLeads(): // an empty v2 workbook is not an invitation to scan legacy leads
		return []int64{}, nil, nil
	}
	if req.ViewID != "" || hasSearch {
		return nil, nil, conflict("Migrate this legacy workbook before running a saved view or search")
	}
	if cfg.Leads == nil {
		return nil, nil, fmt.Errorf("runplan: no lead reader configured for legacy workbook %s", wb.ID)
	}
	filter, ferr := wbcrud.FilterFromCriteria(wb.FilterCriteria)
	if ferr != nil {
		return nil, nil, ferr
	}
	leads, _, err := cfg.Leads.QueryLeads(ctx, tx, filter, 1, 10000)
	if err != nil {
		return nil, nil, err
	}
	var want map[int64]bool
	if req.LeadIDs != nil {
		want = make(map[int64]bool, len(req.LeadIDs))
		for _, id := range req.LeadIDs {
			want[id] = true
		}
	}
	leadIDs = []int64{}
	for _, l := range leads {
		if want == nil || want[l.ID] {
			leadIDs = append(leadIDs, l.ID)
		}
	}
	return nil, leadIDs, nil
}

// viewRows runs the workbook row query (saved view filters and sort, search)
// of _workbook_rows_query and returns the matching row ids.
func viewRows(ctx context.Context, tx pgx.Tx, wb Workbook, req RunRequest) ([]int64, error) {
	var view *wbcrud.QueryView
	if req.ViewID != "" {
		v, g, err := wbcrud.LoadView(ctx, tx, wb.ID, req.ViewID)
		if err != nil {
			return nil, err
		}
		if g != nil {
			return nil, g
		}
		view = v
	}
	rq, g := wbcrud.BuildRowQuery(wbcrud.QueryWorkbook{ID: wb.ID, Columns: wb.Columns}, view, req.Search)
	if g != nil {
		return nil, g
	}
	rs, err := tx.Query(ctx, rq.SQL, rq.Args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var ids []int64
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			return nil, err
		}
		switch id := vals[0].(type) {
		case int32:
			ids = append(ids, int64(id))
		case int64:
			ids = append(ids, id)
		}
	}
	return ids, rs.Err()
}
