// Package autocaps reserves per-rule and workspace daily action/spend caps for
// automation actions (Python services/automations/caps.py).
//
// All functions run in the caller's tenancy.InWorkspace transaction, so a
// reservation commits or rolls back with the caller's work. D5: reservers are
// serialised by a transaction-scoped advisory lock per (workspace, UTC day),
// taken before any read; Python's FOR UPDATE over the day's rows does not
// serialise the first reserver of a day.
package autocaps

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// nowUTC is the clock for the day bucket (a var so tests can roll the day).
var nowUTC = func() time.Time { return time.Now().UTC() }

// Reservation is the outcome of TryReserve. ID is non-zero only for OK
// reservations; IdempotencyKey is set on OK reservations and on a refused
// released key.
type Reservation struct {
	OK             bool
	Reason         string // "cap" when refused
	ID             int64
	IdempotencyKey string
}

// TriggerCaps are the per-rule limits; nil means unlimited.
type TriggerCaps struct {
	ID                string
	MaxActionsPerDay  *int32
	MaxSpendUSDPerDay *float64
}

// DayUTC is the YYYY-MM-DD UTC day bucket of now.
func DayUTC(now time.Time) string { return now.UTC().Format("2006-01-02") }

// round6 rounds half-to-even on the exact binary value, like Python round(x, 6).
func round6(x float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 6, 64), 64)
	return v
}

func refused(key string) Reservation { return Reservation{Reason: "cap", IdempotencyKey: key} }

// TryReserve atomically reserves one action (and cost USD) against the per-rule
// action cap, the per-rule spend cap and the workspace global daily spend cap
// (globalCap <= 0 disables the latter). A key already held or settled returns
// the existing reservation; a released key is refused ("cap"). Fail-closed
// deviations (documented): a non-finite cost or non-finite spend cap refuses
// instead of comparing false and over-admitting.
func TryReserve(ctx context.Context, tx pgx.Tx, t TriggerCaps, ws string, cost float64, idem string, globalCap float64) (Reservation, error) {
	day := DayUTC(nowUTC())
	q := gen.New(tx)
	if err := q.AutoCapLock(ctx, gen.AutoCapLockParams{WorkspaceID: ws, DayUtc: day}); err != nil {
		return Reservation{}, fmt.Errorf("autocaps: lock: %w", err)
	}

	existing, err := q.AutoCapByKey(ctx, gen.AutoCapByKeyParams{WorkspaceID: ws, IdempotencyKey: idem})
	switch {
	case err == nil:
		if existing.State == "released" {
			return refused(idem), nil
		}
		return Reservation{OK: true, ID: int64(existing.ID), IdempotencyKey: idem}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Reservation{}, fmt.Errorf("autocaps: lookup: %w", err)
	}

	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return Reservation{OK: false, Reason: "cap"}, nil
	}
	cost = round6(math.Max(0, cost))

	if t.MaxActionsPerDay != nil {
		used, err := q.AutoCapActions(ctx, gen.AutoCapActionsParams{WorkspaceID: ws, TriggerID: t.ID, DayUtc: day})
		if err != nil {
			return Reservation{}, fmt.Errorf("autocaps: count: %w", err)
		}
		if used+1 > int64(*t.MaxActionsPerDay) {
			return Reservation{Reason: "cap"}, nil
		}
	}
	if t.MaxSpendUSDPerDay != nil && cost > 0 {
		c := *t.MaxSpendUSDPerDay
		if math.IsNaN(c) {
			return Reservation{Reason: "cap"}, nil
		}
		used, err := q.AutoCapRuleSpend(ctx, gen.AutoCapRuleSpendParams{WorkspaceID: ws, TriggerID: t.ID, DayUtc: day})
		if err != nil {
			return Reservation{}, fmt.Errorf("autocaps: rule spend: %w", err)
		}
		if round6(round6(used)+cost) > c {
			return Reservation{Reason: "cap"}, nil
		}
	}
	if cost > 0 && (globalCap > 0 || math.IsNaN(globalCap)) {
		if math.IsNaN(globalCap) {
			return Reservation{Reason: "cap"}, nil
		}
		used, err := q.AutoCapWorkspaceSpend(ctx, gen.AutoCapWorkspaceSpendParams{WorkspaceID: ws, DayUtc: day})
		if err != nil {
			return Reservation{}, fmt.Errorf("autocaps: workspace spend: %w", err)
		}
		if round6(round6(used)+cost) > globalCap {
			return Reservation{Reason: "cap"}, nil
		}
	}

	id, err := q.AutoCapInsert(ctx, gen.AutoCapInsertParams{
		WorkspaceID: ws, TriggerID: t.ID, DayUtc: day, ReservedUsd: cost, IdempotencyKey: idem,
	})
	if err != nil {
		return Reservation{}, fmt.Errorf("autocaps: insert: %w", err)
	}
	return Reservation{OK: true, ID: int64(id), IdempotencyKey: idem}, nil
}

// Settle marks a held reservation settled. Only a held row changes; a refused
// or empty reservation is ignored.
func Settle(ctx context.Context, tx pgx.Tx, r Reservation) error {
	if !r.OK || r.ID == 0 {
		return nil
	}
	if _, err := gen.New(tx).AutoCapSettle(ctx, int32(r.ID)); err != nil {
		return fmt.Errorf("autocaps: settle: %w", err)
	}
	return nil
}

// Release frees a held reservation; only a held row changes.
func Release(ctx context.Context, tx pgx.Tx, r Reservation) error {
	if !r.OK || r.ID == 0 {
		return nil
	}
	if _, err := gen.New(tx).AutoCapRelease(ctx, int32(r.ID)); err != nil {
		return fmt.Errorf("autocaps: release: %w", err)
	}
	return nil
}

// ActionsToday is the held+settled action count of a rule for the UTC day.
func ActionsToday(ctx context.Context, tx pgx.Tx, triggerID, ws, day string) (int64, error) {
	if day == "" {
		day = DayUTC(nowUTC())
	}
	n, err := gen.New(tx).AutoCapActions(ctx, gen.AutoCapActionsParams{WorkspaceID: ws, TriggerID: triggerID, DayUtc: day})
	if err != nil {
		return 0, fmt.Errorf("autocaps: actions today: %w", err)
	}
	return n, nil
}
