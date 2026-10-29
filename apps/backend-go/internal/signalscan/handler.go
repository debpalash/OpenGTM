package signalscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// dailyMinutes is refresh.INTERVAL_MINUTES["daily"], the default scan interval.
const dailyMinutes = 1440

var defaultFiredTypes = []string{"hiring", "funding", "tech_change", "news"}

// HandleScan is refresh.handle_signal_scan, the global signal_scan job: run the scan, then
// for each workspace (its own tenancy.InWorkspace transaction, never a cross-tenant
// enumeration) refresh the workbooks whose refresh_policy.on_signal lists a fired type, and
// finally queue the next periodic scan. A failing scan or workspace is logged and skipped.
func HandleScan(ctx context.Context, d Deps, c *queue.Claim) error {
	var payload map[string]json.RawMessage
	if c != nil && len(c.Payload) > 0 {
		_ = json.Unmarshal(c.Payload, &payload)
	}
	scan, err := Run(ctx, d)
	if err != nil {
		d.log().Warn("signal scan failed", "err", err.Error())
	}

	fired := firedTypes(payload["signal_types"])
	triggered := 0
	wsIDs, err := gen.New(d.Pool).ScanListWorkspaceIDs(ctx)
	if err != nil {
		d.log().Warn("signal_scan: could not list workspaces", "err", err.Error())
		wsIDs = nil
	}
	for _, ws := range wsIDs {
		if ctx.Err() != nil {
			break
		}
		if err := d.triggerPass(ctx, ws, fired, &triggered); err != nil {
			d.log().Warn("signal_scan: workspace trigger pass failed", "workspace_id", ws, "err", err.Error())
		}
	}

	interval, err := intervalMinutes(payload["interval_minutes"])
	if err != nil {
		return err
	}
	if err := d.enqueueNextScan(ctx, interval); err != nil {
		return err
	}
	d.log().Info("signal_scan done", "job_id", jobID(c), "scanned", scan.Scanned, "signals", scan.SignalsFound,
		"triggered", triggered, "next_in_minutes", interval)
	return nil
}

func jobID(c *queue.Claim) int64 {
	if c == nil {
		return 0
	}
	return c.ID
}

// firedTypes is payload.get("signal_types") or the default list.
func firedTypes(raw json.RawMessage) map[string]bool {
	var list []any
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		list = nil
	}
	out := map[string]bool{}
	if list == nil {
		for _, s := range defaultFiredTypes {
			out[s] = true
		}
		return out
	}
	for _, v := range list {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out
}

// intervalMinutes is int(payload.get("interval_minutes", 1440)).
func intervalMinutes(raw json.RawMessage) (int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		if len(raw) == 0 {
			return dailyMinutes, nil
		}
		return 0, errors.New("signalscan: interval_minutes must be a number")
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) || f > 1e9 || f < -1e9 {
			return 0, errors.New("signalscan: interval_minutes out of range")
		}
		return int(f), nil // int() truncates toward zero
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("signalscan: interval_minutes %q is not an integer", s)
		}
		return n, nil
	}
	return 0, errors.New("signalscan: interval_minutes must be a number")
}

// truthy is Python truthiness of a decoded JSON value.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// triggerPass runs one workspace's trigger pass in a single transaction (Python: one session,
// one commit). triggered is incremented as workbooks are queued, like Python.
func (d Deps) triggerPass(ctx context.Context, ws string, fired map[string]bool, triggered *int) error {
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		wbs, err := q.ScanListWorkbooks(ctx, ws)
		if err != nil {
			return err
		}
		executor := "go"
		if d.RefreshExecutor != nil {
			executor = d.RefreshExecutor(ws)
		}
		for _, wb := range wbs {
			var policy map[string]any
			if len(wb.RefreshPolicy) > 0 {
				_ = json.Unmarshal(wb.RefreshPolicy, &policy)
			}
			if !truthy(policy["enabled"]) {
				continue
			}
			var hits []string
			seen := map[string]bool{}
			if list, ok := policy["on_signal"].([]any); ok {
				for _, v := range list {
					if s, ok := v.(string); ok && fired[s] && !seen[s] {
						seen[s] = true
						hits = append(hits, s)
					}
				}
			}
			if len(hits) == 0 {
				continue
			}
			sort.Strings(hits)
			body, err := encode(struct {
				WorkbookID  string `json:"workbook_id"`
				Reason      string `json:"reason"`
				WorkspaceID string `json:"workspace_id"`
			}{wb.ID, "signal", ws})
			if err != nil {
				return err
			}
			if err := q.InsertSignalRefreshJob(ctx, gen.InsertSignalRefreshJobParams{Executor: executor, Payload: body, WorkspaceID: ws}); err != nil {
				return err
			}
			if err := q.InsertRefreshActivity(ctx, gen.InsertRefreshActivityParams{
				WorkbookID: wb.ID, WorkspaceID: ws, Kind: "signal",
				Message: "signal trigger → refresh (" + strings.Join(hits, ", ") + ")",
			}); err != nil {
				return err
			}
			*triggered++
		}
		return nil
	})
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// enqueueNextScan is enqueue_job_once for the next periodic signal_scan: advisory lock on the
// fire key, no second pending/processing row for it, delayed by the interval.
func (d Deps) enqueueNextScan(ctx context.Context, intervalMinutes int) error {
	next := d.now().UTC().Add(time.Duration(intervalMinutes) * time.Minute)
	// Python: _now() is tz-aware UTC, so isoformat() carries "+00:00".
	fireKey := "signal_scan:" + next.Truncate(time.Second).Format("2006-01-02T15:04:05") + "+00:00"
	body, err := encode(struct {
		IntervalMinutes int    `json:"interval_minutes"`
		FireKey         string `json:"fire_key"`
	}{intervalMinutes, fireKey})
	if err != nil {
		return err
	}
	executor := "go"
	if d.ScanExecutor != nil {
		executor = d.ScanExecutor()
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("signalscan: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, fireKey); err != nil {
		return fmt.Errorf("signalscan: lock: %w", err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, fireKey); err == nil {
		return tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("signalscan: look up next scan: %w", err)
	}
	if _, err := q.InsertSignalScanJobAt(ctx, gen.InsertSignalScanJobAtParams{
		Executor: executor, Payload: body, FireKey: fireKey, NextRunAt: pgtype.Timestamp{Time: next, Valid: true},
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("signalscan: enqueue next scan: %w", err)
	}
	return tx.Commit(ctx)
}
