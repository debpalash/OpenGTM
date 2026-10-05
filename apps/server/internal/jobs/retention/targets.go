package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// storage is how a target's time column holds its value, which decides the
// type of the cutoff parameter (Python's "datetime", "date" and "epoch").
type storage int

const (
	storeTimestamp storage = iota // timestamp / timestamptz column
	storeDateText                 // 'YYYY-MM-DD' varchar column
	storeEpoch                    // double precision seconds since the epoch
)

// target is one table a category purges: the same eight (category, model,
// column, storage) tuples as _targets in retention.py, in the same order.
type target struct {
	category string
	table    string
	column   string
	storage  storage
}

var targets = []target{
	{"audit", "governance_audit_events", "created_at", storeTimestamp},
	{"llm_usage", "llm_usage_daily", "date", storeDateText},
	{"signals", "signals", "created_at", storeEpoch},
	{"activation", "destination_deliveries", "created_at", storeTimestamp},
	{"activation", "destination_inbound_receipts", "created_at", storeTimestamp},
	{"audience_history", "audience_membership_events", "created_at", storeTimestamp},
	{"agent_results", "playbook_results", "created_at", storeTimestamp},
	{"outreach_history", "outreach_sends", "created_at", storeTimestamp},
}

// cutoffArg converts the cutoff into the bind value and SQL cast for t.
func (t target) cutoffArg(cutoff time.Time) (arg any, cast string) {
	switch t.storage {
	case storeDateText:
		return cutoff.Format("2006-01-02"), "text"
	case storeEpoch:
		return epochSeconds(cutoff), "float8"
	}
	return cutoff, "timestamp"
}

// deleteSQL deletes one bounded batch. The inner SELECT picks at most $3
// primary keys; the outer DELETE repeats the tenant and cutoff predicates so
// a row updated concurrently after it was picked is re-checked (as a single
// DELETE ... WHERE would) rather than purged on stale evidence.
func (t target) deleteSQL(cast string) string {
	return fmt.Sprintf(`DELETE FROM %[1]s
WHERE id = ANY(ARRAY(SELECT id FROM %[1]s WHERE workspace_id = $1 AND %[2]s < $2::%[3]s LIMIT $3))
  AND workspace_id = $1 AND %[2]s < $2::%[3]s`, t.table, t.column, cast)
}

// deleteBatches removes every expired row of t for the workspace in batches
// of at most size rows, inside the caller's tenant transaction. The batches
// bound each statement's memory and WAL burst while the transaction keeps
// the whole purge atomic, so a failure leaves no partial deletion behind
// (the Python handler's single DELETE had the same all-or-nothing property).
func deleteBatches(ctx context.Context, tx pgx.Tx, t target, workspaceID string, cutoff time.Time, size int) (int64, error) {
	arg, cast := t.cutoffArg(cutoff)
	sql := t.deleteSQL(cast)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		tag, err := tx.Exec(ctx, sql, workspaceID, arg, size)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
	}
}
