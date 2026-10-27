package refresh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// DefaultExecutor runs the recurring refresh chain Go enqueues: a Go worker
// handled this job, so its workspace is in the Go cohort.
const DefaultExecutor = "go"

// Deps are the handler dependencies.
type Deps struct {
	Pool *pgxpool.Pool
	// Now overrides Clock (tests).
	Now func() time.Time
	// NextExecutor is the executor of the next refresh_workbook; default "go".
	NextExecutor string
	// SourceWait makes one cycle wait for the source_workbook jobs it enqueued
	// (up to this long) and enrich the rows they added in the same run job
	// (Python refresh.py semantics). Zero: do not wait; new rows are then picked
	// up as stale by the next cycle.
	SourceWait time.Duration
	// NextExecutorFor resolves the executor of a re-seeded chain by workspace (failure hook);
	// nil falls back to NextExecutor / "go".
	NextExecutorFor func(workspaceID string) string
	// SourcePoll is the poll interval while waiting (default 2s).
	SourcePoll time.Duration
	// SourceExecutorFor picks the executor of the source_workbook jobs a cycle enqueues;
	// nil: config.SourceWorkbookExecutor over config.Getenv (GO_ALL_NATIVE applies) ("python" unless
	// the GO_EXECUTOR_SOURCE_WORKBOOK cohort covers the workspace).
	SourceExecutorFor func(workspaceID string) string
}

func (d Deps) sourceExecutor(ws string) string {
	if d.SourceExecutorFor != nil {
		if e := d.SourceExecutorFor(ws); e == "go" || e == "python" {
			return e
		}
		return "python"
	}
	return config.SourceWorkbookExecutor(config.Getenv, ws)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return Clock().UTC()
}

func (d Deps) executor() string {
	if d.NextExecutor != "" {
		return d.NextExecutor
	}
	return DefaultExecutor
}

// enrichmentTypes are the column types the stale scan covers
// (enrichment_cols in refresh.py); runnable types are ENRICHMENT_COL_TYPES.
var (
	staleScanTypes = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true}
	runnableTypes  = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true, "research": true,
		"agent": true, "http": true, "formula": true, "output": true}
)

func colType(c wbcrud.Column) string { s, _ := c.Get("type").(string); return s }
func colID(c wbcrud.Column) string   { s, _ := c.Get("id").(string); return s }

func isReactive(c wbcrud.Column) bool {
	t := colType(c)
	if !runnableTypes[t] {
		return false
	}
	if v, present := c.Lookup("reactive"); present {
		return pyTruthy(v)
	}
	return t != "output"
}

func decodePolicy(raw []byte) map[string]any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var p map[string]any
	if err := dec.Decode(&p); err != nil || p == nil {
		return map[string]any{}
	}
	return p
}

func ttlMap(policy map[string]any) map[string]float64 {
	out := map[string]float64{}
	m, _ := policy["staleness_ttl_days"].(map[string]any)
	for k, v := range m {
		switch x := v.(type) {
		case json.Number:
			if f, err := x.Float64(); err == nil {
				out[k] = f
			}
		case float64:
			out[k] = x
		case int:
			out[k] = float64(x)
		}
	}
	return out
}

func ts(t time.Time) pgtype.Timestamp { return pgtype.Timestamp{Time: t.UTC(), Valid: true} }

// enqueueNext inserts the next refresh_workbook at now+minutes, once per
// fireKey (the key is also advisory-locked like EnqueueOnce). It reports
// whether a row was created.
func enqueueNext(ctx context.Context, tx pgx.Tx, wbID, ws, executor, fireKey string, minutes int, now time.Time) (bool, error) {
	body, err := json.Marshal(struct {
		WorkbookID  string `json:"workbook_id"`
		WorkspaceID string `json:"workspace_id"`
		FireKey     string `json:"fire_key"`
	}{wbID, ws, fireKey})
	if err != nil {
		return false, err
	}
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, fireKey); err != nil {
		return false, fmt.Errorf("refresh: lock next: %w", err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, fireKey); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("refresh: look up next: %w", err)
	}
	_, err = q.InsertRefreshJobAt(ctx, gen.InsertRefreshJobAtParams{
		Executor: executor, Payload: body, WorkspaceID: ws, FireKey: fireKey,
		NextRunAt: ts(now.Add(time.Duration(minutes) * time.Minute)),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("refresh: enqueue next: %w", err)
	}
	return true, nil
}

// SetPolicy mirrors set_refresh_policy: store the policy and, when it is
// enabled with a usable interval and no refresh chain is active for the
// workbook, start the recurring chain. An unknown (or other-tenant) workbook
// answers {"error": "workbook_not_found"}. tx must be inside
// tenancy.InWorkspace. The stored JSON is the compact encoding of policy (map
// keys sorted; Python keeps insertion order).
func SetPolicy(ctx context.Context, tx pgx.Tx, wbID string, policy map[string]any) (map[string]any, error) {
	return SetPolicyExecutor(ctx, tx, wbID, policy, DefaultExecutor)
}

// SetPolicyExecutor is SetPolicy with the executor of the chain job it may
// start: Python uses refresh_executor(workspace_id), so the API passes the
// cohort resolution (config.ExecutorSettings.RefreshExecutor). An empty
// executor is DefaultExecutor.
func SetPolicyExecutor(ctx context.Context, tx pgx.Tx, wbID string, policy map[string]any, executor string) (map[string]any, error) {
	if executor == "" {
		executor = DefaultExecutor
	}
	q := gen.New(tx)
	wb, err := q.RefreshWorkbook(ctx, wbID)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{"error": "workbook_not_found"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("refresh: load workbook: %w", err)
	}
	if policy == nil {
		policy = map[string]any{}
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("refresh: encode policy: %w", err)
	}
	if _, err := q.SetRefreshPolicy(ctx, gen.SetRefreshPolicyParams{ID: wbID, Policy: body}); err != nil {
		return nil, fmt.Errorf("refresh: store policy: %w", err)
	}
	minutes, ok := IntervalMinutes(policy)
	enabled := pyTruthy(policy["enabled"])
	if enabled && schedulable(minutes, ok) {
		// A pending or claimed refresh already continues the enabled policy;
		// saving again must not fork another self-reenqueuing chain.
		_, err := q.ActiveRefreshJobForWorkbook(ctx, wbID)
		if errors.Is(err, pgx.ErrNoRows) {
			now := Clock().UTC()
			key := fmt.Sprintf("refresh_workbook:%s:policy:%d", wbID, now.Unix())
			if _, err := enqueueNext(ctx, tx, wbID, wb.WorkspaceID, executor, key, minutes, now); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, fmt.Errorf("refresh: active chain: %w", err)
		}
	}
	var next any
	if enabled && ok {
		next = minutes
	}
	return map[string]any{"refresh_policy": policy, "next_in_minutes": next}, nil
}

type payload struct {
	WorkbookID  string `json:"workbook_id"`
	WorkspaceID string `json:"workspace_id"`
	Reason      string `json:"reason"`
	// Written by sourceFirst on the claim's own row (retry state).
	SourcesEnqueued bool  `json:"sources_enqueued"`
	PreMaxID        int32 `json:"pre_max_id"`
}

// workbookTenant resolves the tenant of a refresh job whose jobs.workspace_id is empty from the database: the
// workbook's own workspace_id (a narrow cross-tenant lookup, since workbooks is FORCE RLS), never the payload's.
// A payload workspace_id that is present and differs, or a workbook that does not exist, is a PermanentError.
//
// This fallback exists ONLY for refresh jobs enqueued by Python (refresh.py _enqueue_next and _enqueue_next_now
// put workspace_id in the payload but not in the jobs column) during the canary. It is removed at cutover, when
// Handle and OnFailure take the tenant from the claim alone (progress.md, CUTOVER).
func workbookTenant(ctx context.Context, d Deps, p payload) (string, error) {
	ws, err := gen.New(d.Pool).WorkbookWorkspaceByID(ctx, p.WorkbookID)
	if err != nil {
		return "", fmt.Errorf("refresh: workbook tenant lookup: %w", err)
	}
	if ws == "" {
		return "", &queue.PermanentError{Err: errors.New("refresh: job workspace missing and the workbook does not exist")}
	}
	if p.WorkspaceID != "" && p.WorkspaceID != ws {
		return "", &queue.PermanentError{Err: errors.New("refresh: payload workspace_id does not match the workbook's workspace")}
	}
	return ws, nil
}

// Handle mirrors handle_refresh_workbook. It reads the workbook inside the
// tenant, then in one transaction: queues one source_workbook (executor per
// SourceExecutorFor, Python by default) per source column, queues one run_workbook (Python executor) for
// the exact stale cells and their reactive dependants, logs a refresh activity
// row, and, when the policy is enabled with an interval, enqueues the next
// refresh_workbook once per fire key. A missing (or other-tenant) workbook
// completes quietly; a missing workspace fails loudly like workspace_scope.
func Handle(ctx context.Context, d Deps, c *queue.Claim) error {
	var p payload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		return fmt.Errorf("refresh: payload: %w", err)
	}
	if p.WorkbookID == "" {
		return errors.New("refresh: payload has no workbook_id")
	}
	// The tenant is the job's workspace_id column; the payload is data the enqueuer controls, so a non-empty claim
	// workspace is strict (a differing payload workspace is refused).
	ws := c.WorkspaceID
	if ws == "" {
		var err error
		if ws, err = workbookTenant(ctx, d, p); err != nil {
			return err
		}
	} else if p.WorkspaceID != "" && p.WorkspaceID != ws {
		return &queue.PermanentError{Err: errors.New("refresh: payload workspace_id does not match the job's workspace")}
	}
	reason := p.Reason
	if reason == "" {
		reason = "scheduled"
	}
	now := d.now()
	pre, err := d.sourceFirst(ctx, ws, p.WorkbookID, c.ID, p)
	if err != nil {
		return err
	}
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		wb, err := gen.New(tx).RefreshWorkbook(ctx, p.WorkbookID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("refresh: load workbook: %w", err)
		}
		cols, err := wbcrud.ParseColumns(wb.ColumnsConfig)
		if err != nil {
			return fmt.Errorf("refresh: columns_config: %w", err)
		}
		policy := decodePolicy(wb.RefreshPolicy)

		// 1) Re-source: the Python executor appends only genuinely new entities.
		sourceCols := 0
		var staleCols []Col
		for _, col := range cols {
			id := colID(col)
			switch t := colType(col); {
			case t == "source":
				if id == "" {
					continue
				}
				sourceCols++
				if pre.done { // enqueued, and waited for, before this transaction
					continue
				}
				if err := enqueueSource(ctx, tx, ws, p.WorkbookID, id, c.ID, d.sourceExecutor(ws)); err != nil {
					return err
				}
			case staleScanTypes[t]:
				tf, _ := col.Get("target_field").(string)
				staleCols = append(staleCols, Col{ID: id, TargetField: tf})
			}
		}

		// 2) Re-enrich only stale cells plus their reactive dependants.
		stale := map[int64][]string{}
		if len(staleCols) > 0 {
			if stale, err = StaleRowColumns(ctx, tx, p.WorkbookID, staleCols, ttlMap(policy), now); err != nil {
				return err
			}
		}
		// Same cycle: rows our own sourcing added get every runnable cell
		// (sourcing initializes them; outputs are not dispatched).
		if len(pre.addedRows) > 0 {
			var newCols []string
			for _, col := range cols {
				if id := colID(col); id != "" && runnableTypes[colType(col)] && colType(col) != "output" {
					newCols = append(newCols, id)
				}
			}
			if len(newCols) > 0 {
				for _, id := range pre.addedRows {
					stale[id] = newCols
				}
			}
		}
		rowIDs := make([]int64, 0, len(stale))
		for id := range stale {
			rowIDs = append(rowIDs, id)
		}
		sort.Slice(rowIDs, func(i, j int) bool { return rowIDs[i] < rowIDs[j] })
		rowColumns := map[string][]string{}
		var columnIDs []string
		seenCol := map[string]bool{}
		cells := 0
		for _, id := range rowIDs {
			ids := stale[id]
			dependents := wbcrud.DownstreamColumns(cols, ids, isReactive)
			merged := append([]string(nil), ids...)
			have := map[string]bool{}
			for _, x := range ids {
				have[x] = true
			}
			for _, dc := range dependents {
				if x := colID(dc); x != "" && !have[x] {
					have[x] = true
					merged = append(merged, x)
				}
			}
			rowColumns[fmt.Sprint(id)] = merged
			cells += len(merged)
			for _, x := range merged {
				if !seenCol[x] {
					seenCol[x] = true
					columnIDs = append(columnIDs, x)
				}
			}
		}
		if len(rowIDs) > 0 {
			_, _, err := queue.EnqueueOnce(ctx, tx, queue.JobTypeRunWorkbook, ws, map[string]any{
				"workbook_id": p.WorkbookID, "workspace_id": ws,
				"row_ids": rowIDs, "column_ids": columnIDs, "row_columns": rowColumns,
			}, fmt.Sprintf("refresh_run:%s:%d", p.WorkbookID, c.ID), "python")
			if err != nil {
				return fmt.Errorf("refresh: enqueue run: %w", err)
			}
		}

		if err := gen.New(tx).InsertRefreshActivity(ctx, gen.InsertRefreshActivityParams{
			WorkbookID: p.WorkbookID, WorkspaceID: ws, Kind: "refresh",
			Message: fmt.Sprintf("%s: queued %d source columns, re-enrich %d stale cells in %d rows",
				reason, sourceCols, cells, len(rowIDs)),
		}); err != nil {
			return fmt.Errorf("refresh: log activity: %w", err)
		}

		// 3) Chain the next refresh once per (workbook, job).
		minutes, ok := IntervalMinutes(policy)
		if pyTruthy(policy["enabled"]) && schedulable(minutes, ok) {
			// Deviation: Python chains unconditionally, so every extra refresh
			// job (signal-triggered, retried) starts a permanent extra chain.
			// Go chains unless another refresh is pending or a lower-id one is processing
			// (the lowest id always chains, so concurrent jobs never all skip).
			_, err := gen.New(tx).OtherActiveRefreshJob(ctx, gen.OtherActiveRefreshJobParams{
				WorkbookID: p.WorkbookID, SelfID: int32(c.ID),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				key := fmt.Sprintf("refresh_workbook:%s:after:%d", p.WorkbookID, c.ID)
				if _, err := enqueueNext(ctx, tx, p.WorkbookID, ws, d.executor(), key, minutes, now); err != nil {
					return err
				}
			} else if err != nil {
				return fmt.Errorf("refresh: active chain: %w", err)
			}
		}
		return nil
	})
}

func enqueueSource(ctx context.Context, tx pgx.Tx, ws, wb, colID string, jobID int64, executor string) error {
	_, _, err := queue.EnqueueOnce(ctx, tx, "source_workbook", ws, map[string]any{
		"workbook_id": wb, "column_id": colID, "workspace_id": ws,
	}, sourceKey(wb, jobID, colID), executor)
	if err != nil {
		return fmt.Errorf("refresh: enqueue source: %w", err)
	}
	return nil
}

func sourceKey(wb string, jobID int64, colID string) string {
	return fmt.Sprintf("refresh_source:%s:%d:%s", wb, jobID, colID)
}

type preSource struct {
	done      bool    // sources were enqueued and awaited here
	addedRows []int64 // rows created in the workbook while waiting
}

// sourceFirst is step 1 of a cycle when SourceWait is set: enqueue the source
// jobs in their own transaction (so the Python executor can claim them), wait
// for them to finish (bounded by SourceWait and ctx), and report the rows that
// appeared. A timeout is not an error: rows added later are enriched as stale
// by the next cycle. Rows written concurrently by other writers during the
// wait are included (Python tracks exact ids from materialize_source).
func (d Deps) sourceFirst(ctx context.Context, ws, wbID string, jobID int64, p payload) (preSource, error) {
	if d.SourceWait <= 0 {
		return preSource{}, nil
	}
	var keys []string
	var maxID int32
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		wb, err := gen.New(tx).RefreshWorkbook(ctx, wbID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("refresh: load workbook: %w", err)
		}
		cols, err := wbcrud.ParseColumns(wb.ColumnsConfig)
		if err != nil {
			return fmt.Errorf("refresh: columns_config: %w", err)
		}
		for _, col := range cols {
			if id := colID(col); colType(col) == "source" && id != "" {
				keys = append(keys, sourceKey(wbID, jobID, id))
			}
		}
		if len(keys) == 0 {
			return nil
		}
		if p.SourcesEnqueued {
			// Retry: the first attempt already enqueued the sources; keep its
			// high-water mark and do not source again.
			maxID = p.PreMaxID
			return nil
		}
		if maxID, err = gen.New(tx).MaxWorkbookRowID(ctx, wbID); err != nil {
			return fmt.Errorf("refresh: max row id: %w", err)
		}
		if _, err := gen.New(tx).StoreRefreshSourcing(ctx, gen.StoreRefreshSourcingParams{ID: int32(jobID), PreMaxID: maxID}); err != nil {
			return fmt.Errorf("refresh: store sourcing state: %w", err)
		}
		for _, col := range cols {
			if id := colID(col); colType(col) == "source" && id != "" {
				if err := enqueueSource(ctx, tx, ws, wbID, id, jobID, d.sourceExecutor(ws)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil || len(keys) == 0 {
		return preSource{}, err
	}
	poll := d.SourcePoll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(d.SourceWait)
	for {
		var n int64
		err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) (e error) {
			n, e = gen.New(tx).ActiveJobsByFireKeys(ctx, keys)
			return e
		})
		if err != nil {
			return preSource{}, fmt.Errorf("refresh: wait for sources: %w", err)
		}
		if n == 0 || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return preSource{}, ctx.Err()
		case <-time.After(poll):
		}
	}
	out := preSource{done: true}
	err = tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		ids, err := gen.New(tx).WorkbookRowsAfter(ctx, gen.WorkbookRowsAfterParams{WorkbookID: wbID, AfterID: maxID})
		for _, id := range ids {
			out.addedRows = append(out.addedRows, int64(id))
		}
		return err
	})
	return out, err
}

// RegisterFailureHook re-seeds the refresh chain when a refresh_workbook job fails for good.
func RegisterFailureHook(d Deps) {
	queue.RegisterFailureHook("refresh_workbook", func(ctx context.Context, n queue.FailureNotice) error {
		return OnFailure(ctx, d, n)
	})
}

// OnFailure keeps a recurring policy alive: a refresh job that exhausted its retries never
// reached its chaining step, so, unless a retry is coming, another refresh job is already
// active, or the policy is off, it enqueues the next occurrence (once per failed job id).
func OnFailure(ctx context.Context, d Deps, n queue.FailureNotice) error {
	if n.WillRetry {
		return nil
	}
	var p payload
	if err := json.Unmarshal(n.Payload, &p); err != nil || p.WorkbookID == "" {
		return nil
	}
	ws := n.WorkspaceID
	if ws == "" {
		var err error
		if ws, err = workbookTenant(ctx, d, p); err != nil {
			var perm *queue.PermanentError
			if errors.As(err, &perm) {
				return nil // unknown or inconsistent tenant: no re-seed
			}
			return err
		}
	} else if p.WorkspaceID != "" && p.WorkspaceID != ws {
		return nil
	}
	exec := d.executor()
	if d.NextExecutorFor != nil {
		if e := d.NextExecutorFor(ws); e != "" {
			exec = e
		}
	}
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		wb, err := gen.New(tx).RefreshWorkbook(ctx, p.WorkbookID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("refresh: failure hook load: %w", err)
		}
		policy := decodePolicy(wb.RefreshPolicy)
		minutes, ok := IntervalMinutes(policy)
		if !pyTruthy(policy["enabled"]) || !schedulable(minutes, ok) {
			return nil
		}
		// Any other pending or processing refresh will chain by itself.
		_, err = gen.New(tx).OtherActiveRefreshJob(ctx, gen.OtherActiveRefreshJobParams{
			WorkbookID: p.WorkbookID, SelfID: math.MaxInt32,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("refresh: failure hook active chain: %w", err)
		}
		key := fmt.Sprintf("refresh_workbook:%s:after:%d", p.WorkbookID, n.JobID)
		_, err = enqueueNext(ctx, tx, p.WorkbookID, ws, exec, key, minutes, d.now())
		return err
	})
}
