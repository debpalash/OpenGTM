package refresh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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
			if _, err := enqueueNext(ctx, tx, wbID, wb.WorkspaceID, DefaultExecutor, key, minutes, now); err != nil {
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
}

// Handle mirrors handle_refresh_workbook. It reads the workbook inside the
// tenant, then in one transaction: queues one source_workbook (Python
// executor) per source column, queues one run_workbook (Python executor) for
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
	ws := p.WorkspaceID
	if ws == "" {
		ws = c.WorkspaceID
	}
	if ws == "" {
		return errors.New("refresh: workspace_id is required (empty would yield zero rows under RLS and mask the bug)")
	}
	if c.WorkspaceID != "" && c.WorkspaceID != ws {
		return errors.New("refresh: payload workspace_id does not match the job's workspace")
	}
	reason := p.Reason
	if reason == "" {
		reason = "scheduled"
	}
	now := d.now()
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
				_, _, err := queue.EnqueueOnce(ctx, tx, "source_workbook", ws, map[string]any{
					"workbook_id": p.WorkbookID, "column_id": id, "workspace_id": ws,
				}, fmt.Sprintf("refresh_source:%s:%d:%s", p.WorkbookID, c.ID, id), "python")
				if err != nil {
					return fmt.Errorf("refresh: enqueue source: %w", err)
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
			// Go chains only when no other refresh is pending or running.
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
