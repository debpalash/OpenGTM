package refresh

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ScanBatchSize is WORKBOOK_REFRESH_SCAN_BATCH_SIZE.
const ScanBatchSize = 1000

// Col is the part of an enrichment column that staleness depends on.
type Col struct {
	ID          string
	TargetField string
}

// StaleRowColumns mirrors _stale_row_columns: the exact (row, column) cells
// that are missing, not complete, undated or older than their field's TTL
// (ttlDays keyed by target_field else column id; default 30). Rows are keyed
// by workbook row id; columns keep config order. Tenancy is RLS plus the
// explicit workbook id; tx must be inside tenancy.InWorkspace.
func StaleRowColumns(ctx context.Context, tx pgx.Tx, wbID string, cols []Col, ttlDays map[string]float64, now time.Time) (map[int64][]string, error) {
	return StaleRowColumnsBatch(ctx, tx, wbID, cols, ttlDays, now, ScanBatchSize)
}

// StaleRowColumnsBatch is StaleRowColumns with an explicit keyset page size;
// the result does not depend on it.
func StaleRowColumnsBatch(ctx context.Context, tx pgx.Tx, wbID string, cols []Col, ttlDays map[string]float64, now time.Time, batch int) (map[int64][]string, error) {
	var cids []string
	var cutoffs []pgtype.Timestamp
	seen := map[string]struct{}{}
	for _, c := range cols {
		if c.ID == "" {
			continue
		}
		if _, dup := seen[c.ID]; dup {
			continue
		}
		seen[c.ID] = struct{}{}
		key := c.TargetField
		if key == "" {
			key = c.ID
		}
		ttl, ok := ttlDays[key]
		if !ok || math.IsNaN(ttl) {
			ttl = DefaultStalenessDays
		}
		cids = append(cids, c.ID)
		cutoffs = append(cutoffs, pgtype.Timestamp{Time: cutoffFor(now, ttl), Valid: true})
	}
	out := map[int64][]string{}
	if len(cids) == 0 {
		return out, nil
	}
	if batch < 1 {
		batch = 1
	}
	q := gen.New(tx)
	var after int32
	for {
		b, err := q.RefreshPageBounds(ctx, gen.RefreshPageBoundsParams{WorkbookID: wbID, After: after, PageSize: int32(batch)})
		if err != nil {
			return nil, fmt.Errorf("refresh: page bounds: %w", err)
		}
		if b.N == 0 {
			break
		}
		pairs, err := q.RefreshStalePairs(ctx, gen.RefreshStalePairsParams{
			WorkbookID: wbID, After: after, Hi: b.Hi, Cids: cids, Cutoffs: cutoffs,
		})
		if err != nil {
			return nil, fmt.Errorf("refresh: stale pairs: %w", err)
		}
		for _, p := range pairs {
			out[int64(p.RowID)] = append(out[int64(p.RowID)], p.ColumnID)
		}
		if b.N < int64(batch) {
			break
		}
		after = b.Hi
	}
	return out, nil
}

// cutoffFor is now - ttl days as a naive UTC timestamp (microsecond precision,
// like Python's timedelta); absurd TTLs clamp instead of overflowing.
func cutoffFor(now time.Time, ttlDays float64) time.Time {
	const limit = 100000 // days (~273y): inside time.Duration and timestamp range
	ttlDays = math.Max(-limit, math.Min(limit, ttlDays))
	d := time.Duration(math.Round(ttlDays*24*float64(time.Hour/time.Microsecond))) * time.Microsecond
	return now.UTC().Add(-d).Truncate(time.Microsecond)
}
