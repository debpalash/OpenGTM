package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Synthetic per-workspace "rules" under which MCP calls are counted in
// trigger_cap_reservations. Writes use MCP_MAX_WRITES_PER_DAY, cost-bearing read
// tools (task 9) use MCP_MAX_ENRICH_PER_DAY; each rule is its own daily bucket.
const (
	mcpWriteRule  = "__mcp_write__"
	mcpEnrichRule = "__mcp_enrich__"

	// Exported for the write pipeline and the enrichment tools.
	WriteRule  = mcpWriteRule
	EnrichRule = mcpEnrichRule
)

// nowUTC is the clock for the daily bucket (a var so tests can roll the day).
var nowUTC = func() time.Time { return time.Now().UTC() }

// Reservation is the outcome of reserving one call against a daily cap.
//
//	OK && !Replay: a fresh slot is held; the caller applies the write in the SAME
//	               transaction, then Settle (commit) or rolls back (frees the slot).
//	OK && Replay:  the key already holds or settled a slot; do NOT re-apply.
//	!OK:           the cap is exhausted (Reason "cap").
type Reservation struct {
	OK, Replay bool
	Reason     string
	Key        string
	id         int64
}

// Reserve reserves one action for workspace ws under rule against cap (cap <= 0
// means unlimited) inside tx, which must be a tenancy.InWorkspace transaction for
// ws. A pg_advisory_xact_lock on (workspace, UTC day) serialises reservers, so the
// cap and the key dedupe are exact under concurrency. An empty key is replaced by
// a random "mcpw_<32 hex>" so every call counts. A key in state held or settled is
// a Replay; a key in state released may be reserved again (D5): the released row is
// reopened, because (workspace_id, idempotency_key) is unique.
//
// D5: the caller keeps tx open across the mutation, so a failed write rolls the
// reservation back with it; Release exists for callers that must free a slot that
// was committed earlier.
func Reserve(ctx context.Context, tx pgx.Tx, ws, rule string, cap int, key string) (Reservation, error) {
	if key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Reservation{}, fmt.Errorf("mcp caps: random key: %w", err)
		}
		key = "mcpw_" + hex.EncodeToString(b[:])
	}
	day := nowUTC().Format("2006-01-02")
	q := gen.New(tx)
	if err := q.McpCapLock(ctx, gen.McpCapLockParams{WorkspaceID: ws, DayUtc: day}); err != nil {
		return Reservation{}, fmt.Errorf("mcp caps: lock: %w", err)
	}

	existing, err := q.McpCapByKey(ctx, gen.McpCapByKeyParams{WorkspaceID: ws, IdempotencyKey: key})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, fmt.Errorf("mcp caps: lookup: %w", err)
	}
	if found && (existing.State == "held" || existing.State == "settled") {
		return Reservation{OK: true, Replay: true, Key: key, id: int64(existing.ID)}, nil
	}

	if cap > 0 {
		used, err := q.McpCapActionsToday(ctx, gen.McpCapActionsTodayParams{WorkspaceID: ws, TriggerID: rule, DayUtc: day})
		if err != nil {
			return Reservation{}, fmt.Errorf("mcp caps: count: %w", err)
		}
		if used+1 > int64(cap) {
			return Reservation{OK: false, Reason: "cap", Key: key}, nil
		}
	}

	if found { // released: reopen the same row
		n, err := q.McpCapReopen(ctx, gen.McpCapReopenParams{TriggerID: rule, DayUtc: day, ID: existing.ID})
		if err != nil {
			return Reservation{}, fmt.Errorf("mcp caps: reopen: %w", err)
		}
		if n != 1 {
			return Reservation{}, errors.New("mcp caps: reopen: reservation changed concurrently")
		}
		return Reservation{OK: true, Key: key, id: int64(existing.ID)}, nil
	}
	id, err := q.McpCapInsert(ctx, gen.McpCapInsertParams{WorkspaceID: ws, TriggerID: rule, DayUtc: day, IdempotencyKey: key})
	if err != nil {
		return Reservation{}, fmt.Errorf("mcp caps: insert: %w", err)
	}
	return Reservation{OK: true, Key: key, id: int64(id)}, nil
}

// Settle marks a held reservation settled after a successful write. State
// transitions only leave "held": settling anything else is a no-op. A reservation
// that is not a fresh hold (OK false or Replay) is ignored.
func Settle(ctx context.Context, tx pgx.Tx, r Reservation) error {
	if !r.OK || r.Replay || r.id == 0 {
		return nil
	}
	if _, err := gen.New(tx).McpCapSettle(ctx, int32(r.id)); err != nil {
		return fmt.Errorf("mcp caps: settle: %w", err)
	}
	return nil
}

// Release frees a held reservation (state released); only a held row changes.
func Release(ctx context.Context, tx pgx.Tx, r Reservation) error {
	if !r.OK || r.Replay || r.id == 0 {
		return nil
	}
	if _, err := gen.New(tx).McpCapRelease(ctx, int32(r.id)); err != nil {
		return fmt.Errorf("mcp caps: release: %w", err)
	}
	return nil
}
