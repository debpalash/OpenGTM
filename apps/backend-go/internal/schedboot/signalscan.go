package schedboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// signalScanIntervalMinutes is INTERVAL_MINUTES["daily"].
const signalScanIntervalMinutes = 1440

// SignalScan is bootstrap_signal_scan: enqueue the recurring signal_scan once, unless a scan job is
// already pending or processing. It reports whether it enqueued.
//
// The pending check and the insert run in one transaction under a transaction-level advisory lock: the fire
// key is time-based, so two concurrent passes (a standby that took the lease over, or an overlapping
// reconciler) would otherwise each see no pending scan and enqueue one.
func (d Deps) SignalScan(ctx context.Context) (bool, error) {
	next := d.now().Add(signalScanIntervalMinutes * time.Minute)
	// _now() is tz-aware UTC in Python: isoformat() carries +00:00 and the key drops microseconds.
	fireKey := "signal_scan:" + next.Truncate(time.Second).Format("2006-01-02T15:04:05") + "+00:00"
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		IntervalMinutes int    `json:"interval_minutes"`
		FireKey         string `json:"fire_key"`
	}{signalScanIntervalMinutes, fireKey}); err != nil {
		return false, err
	}
	created := false
	err := inTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('signal_scan'))`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type = 'signal_scan' AND status IN ('pending', 'processing')`).Scan(&n); err != nil {
			return fmt.Errorf("count signal_scan jobs: %w", err)
		}
		if n > 0 {
			return nil
		}
		q := gen.New(tx)
		if err := q.LockJobFireKey(ctx, fireKey); err != nil {
			return err
		}
		if _, err := q.ActiveJobByFireKey(ctx, fireKey); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err := q.InsertSignalScanJobAt(ctx, gen.InsertSignalScanJobAtParams{
			Executor: d.Cfg.Executor.SignalScanExecutor(), Payload: bytes.TrimSpace(buf.Bytes()), FireKey: fireKey,
			NextRunAt: pgtype.Timestamp{Time: next.Truncate(time.Microsecond), Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // lost the unique-index race
		}
		created = err == nil
		return err
	})
	if err != nil {
		return false, fmt.Errorf("schedboot: signal_scan: %w", err)
	}
	return created, nil
}
