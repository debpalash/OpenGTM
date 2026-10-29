package leadpeople

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Paid contact lookups and the spend ledger (review wave 3b M4). Every paid email discovery
// and the deliverability verification of a contacts action is an attempt of the spend
// ledger's workspace scope (spend.WorkspaceLedger: the action belongs to no workbook):
// reserved at the catalog price before the call, dispatched, then settled from the
// provider's accounting envelope; a call that fails after dispatch is uncertain; a hold
// that is never dispatched is released. Python ran these calls outside any ledger.

// ContactAttempt is one paid call of a contacts action.
type ContactAttempt struct {
	WorkspaceID, ActionID, PersonID string
	Stage, Provider                 string // "discovery"/"verification", the provider name
	ExposureMicroUSD                int64  // catalog price of the call
	LimitMicroUSD                   int64  // bound of every paid call for the person
}

// ContactSpend reserves the attempts of a contacts action. A refusal is *SpendRefusedError.
type ContactSpend interface {
	Reserve(ctx context.Context, a ContactAttempt) (ContactHold, error)
}

// ContactHold is a reserved attempt.
type ContactHold interface {
	Dispatch(ctx context.Context) error // a refusal is *SpendRefusedError; then Release
	Release(ctx context.Context)
	Settle(ctx context.Context, chargedMicroUSD int64, basis string, result map[string]any)
	Uncertain(ctx context.Context)
}

// SpendRefusedError is a reservation or dispatch the ledger refused.
type SpendRefusedError struct{ Reason string }

func (e *SpendRefusedError) Error() string { return "contact spend refused: " + e.Reason }

// DefaultMaxPeople bounds the people of one contacts call (PEOPLE_CONTACTS_MAX_SELECTED).
const DefaultMaxPeople = 25

func (s *Service) maxPeople() int {
	if s.MaxPeople > 0 {
		return s.MaxPeople
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PEOPLE_CONTACTS_MAX_SELECTED"))); err == nil && n > 0 {
		return n
	}
	return DefaultMaxPeople
}

// LedgerSpend is the Postgres ContactSpend (workbook_spend_attempts, workspace scope).
type LedgerSpend struct{ Pool *pgxpool.Pool }

const ledgerTimeout = 10 * time.Second

func (l LedgerSpend) Reserve(ctx context.Context, a ContactAttempt) (ContactHold, error) {
	run := "contacts:" + a.ActionID
	row := "person:" + a.PersonID
	const col = "people_contacts"
	prov := a.Stage + ":" + a.Provider
	key, err := canonjson.Marshal([]any{a.WorkspaceID, spend.WorkspaceLedger, run, row, col, prov}, false)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	r := spend.Reservation{WorkspaceID: a.WorkspaceID, WorkbookID: spend.WorkspaceLedger, RunID: truncBytes(run, 255),
		RowIdentity: truncBytes(row, 255), ColumnID: col, Provider: prov, AttemptKey: hex.EncodeToString(sum[:]),
		ExposureMicroUSD: a.ExposureMicroUSD, CellLimitMicroUSD: a.LimitMicroUSD,
		CostBasis: map[string]any{"kind": "catalog_estimate", "provider": a.Provider},
		Operation: map[string]any{"action_id": a.ActionID, "person_id": a.PersonID, "stage": a.Stage}}
	var adm spend.Admission
	if err := tenancy.InWorkspace(ctx, l.Pool, a.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		adm, e = spend.ReserveWorkspace(ctx, tx, r)
		return e
	}); err != nil {
		return nil, err
	}
	if !adm.OK {
		return nil, &SpendRefusedError{Reason: adm.Reason}
	}
	if adm.Reused {
		// The same action/person/call was already reserved (a replay): never pay twice.
		return nil, &SpendRefusedError{Reason: "attempt_already_used"}
	}
	return &ledgerHold{pool: l.Pool, id: spend.Identity{WorkspaceID: a.WorkspaceID, WorkbookID: spend.WorkspaceLedger,
		AttemptID: adm.ID, ContractHash: adm.ContractHash}}, nil
}

type ledgerHold struct {
	pool *pgxpool.Pool
	id   spend.Identity
}

func (h *ledgerHold) transition(ctx context.Context, action string) (bool, error) {
	var ok bool
	err := tenancy.InWorkspace(ctx, h.pool, h.id.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		ok, e = spend.Transition(ctx, tx, h.id, action)
		return e
	})
	return ok, err
}

func (h *ledgerHold) Dispatch(ctx context.Context) error {
	ok, err := h.transition(ctx, "dispatch")
	if err != nil {
		return err
	}
	if !ok {
		return &SpendRefusedError{Reason: "attempt_not_dispatchable"}
	}
	return nil
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), ledgerTimeout)
}

func (h *ledgerHold) Release(ctx context.Context) {
	dctx, cancel := detached(ctx)
	defer cancel()
	_, _ = h.transition(dctx, "release_before_dispatch")
}

func (h *ledgerHold) Uncertain(ctx context.Context) {
	dctx, cancel := detached(ctx)
	defer cancel()
	_, _ = h.transition(dctx, "mark_uncertain")
}

func (h *ledgerHold) Settle(ctx context.Context, charged int64, basis string, result map[string]any) {
	dctx, cancel := detached(ctx)
	defer cancel()
	var res spend.SettleResult
	err := tenancy.InWorkspace(dctx, h.pool, h.id.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		res, e = spend.SettleWorkspace(dctx, tx, h.id, charged, basis, result)
		return e
	})
	if err != nil || !res.OK {
		_, _ = h.transition(dctx, "mark_uncertain")
	}
}

func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// refusedReason is the attempt detail of a refused paid call.
func refusedReason(err error) (string, bool) {
	var ref *SpendRefusedError
	if errors.As(err, &ref) {
		return "spend_" + ref.Reason, true
	}
	return "", false
}
