package workbook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// LLM spend of the native AI columns (review wave 3b M1): every ai_formula, research and
// batch LLM call is a spend-ledger attempt of the workbook, reserved and dispatched
// (lease-fenced) before the call and settled after it, so the workbook budget bounds LLM
// cost like paid provider cost. A cell's LLM attempts have no ledger cell limit of their
// own: the research budget is enforced per call in aicol.

// noCellLimit is the cell limit of an LLM attempt (only the workbook budget applies).
const noCellLimit = math.MaxInt64

// llmHold is a dispatched LLM attempt.
type llmHold struct {
	rs *runState
	id spend.Identity
}

func (h llmHold) Settle(ctx context.Context, charged int64, receipt map[string]any) {
	var res spend.SettleResult
	err := tenancy.InWorkspace(ctx, h.rs.deps.Pool, h.id.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		res, e = spend.Settle(ctx, tx, h.id, charged, "catalog_estimate", receipt)
		return e
	})
	if err != nil || !res.OK {
		h.rs.log.Error("llm spend settlement failed", "attempt_id", h.id.AttemptID, "reason", res.Reason, "err", err)
		h.rs.markUncertain(ctx, h.id)
	}
}

func (h llmHold) Uncertain(ctx context.Context) { h.rs.markUncertain(ctx, h.id) }

// llmReserve reserves and dispatches one LLM attempt. build makes the reservation inside
// the reserving transaction (an in-request run resolves its live job first, as
// reservedCall does). beforeDispatch, when set, runs between reserve and dispatch (and for a
// reused attempt); its error releases the hold. A refusal is *aicol.SpendRefused; adm
// reports a reused attempt's status ("dispatched" when this call dispatched it).
func (rs *runState) llmReserve(ctx context.Context, build func() spend.Reservation, beforeDispatch func() error) (llmHold, spend.Admission, error) {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	if err := rs.clientErr(); err != nil {
		return llmHold{}, spend.Admission{}, err
	}
	var adm spend.Admission
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		if rs.claim == nil {
			if e := rs.resolveLiveJob(ctx, tx); e != nil {
				return e
			}
		}
		var e error
		adm, e = spend.Reserve(ctx, tx, build())
		return e
	})
	if err != nil {
		return llmHold{}, adm, err
	}
	if !adm.OK {
		return llmHold{}, adm, &aicol.SpendRefused{Reason: adm.Reason}
	}
	h := llmHold{rs: rs, id: spend.Identity{WorkspaceID: ws, WorkbookID: wb, AttemptID: adm.ID,
		ContractHash: adm.ContractHash, Lease: rs.leaseFence()}}
	if beforeDispatch != nil {
		if err := beforeDispatch(); err != nil {
			if adm.Status == "reserved" {
				rs.releaseBeforeDispatch(ctx, h.id)
			}
			return llmHold{}, adm, err
		}
	}
	if adm.Reused && adm.Status != "reserved" {
		return h, adm, nil // dispatched by an earlier attempt of this job (a resumed batch)
	}
	if err := rs.clientErr(); err != nil {
		rs.releaseBeforeDispatch(ctx, h.id)
		return llmHold{}, adm, err
	}
	var dispatched bool
	err = tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		var e error
		dispatched, e = spend.Transition(ctx, tx, h.id, "dispatch")
		return e
	})
	if err != nil {
		rs.releaseBeforeDispatch(ctx, h.id)
		return llmHold{}, adm, err
	}
	if !dispatched {
		return llmHold{}, adm, &aicol.SpendRefused{Reason: "attempt_not_dispatchable"}
	}
	rs.paidDispatched.Store(true)
	adm.Status = "dispatched"
	return h, adm, nil
}

// llmReservation is one LLM attempt of the run.
func (rs *runState) llmReservation(rowIdentity, columnID, provider string, exposure int64, op map[string]any, call string) spend.Reservation {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	return spend.Reservation{WorkspaceID: ws, WorkbookID: wb, RunID: rs.spendRunID(), RowIdentity: rowIdentity,
		ColumnID: columnID, Provider: provider, AttemptKey: spend.AttemptKey(ws, wb, rs.spendJobID(), rowIdentity, columnID, provider),
		ExposureMicroUSD: exposure, CellLimitMicroUSD: noCellLimit,
		CostBasis: map[string]any{"kind": "llm_token_estimate", "call": call}, Operation: op}
}

// cellSpend is the aicol.Spend of one native AI cell. Each call is its own attempt (a
// random suffix in the provider identity): a retried call is a new, paid call.
type cellSpend struct {
	rs          *runState
	col         column
	rowIdentity string
}

func (rs *runState) cellSpend(col column, rowID, leadID int64) aicol.Spend {
	ri := fmt.Sprintf("row:%d", rowID)
	if rowID == 0 {
		ri = fmt.Sprintf("lead:%d", leadID)
	}
	return &cellSpend{rs: rs, col: col, rowIdentity: ri}
}

func (s *cellSpend) Begin(ctx context.Context, call string, exposure int64) (aicol.SpendHold, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	provider := "llm:" + call + ":" + hex.EncodeToString(nonce[:])
	h, _, err := s.rs.llmReserve(ctx, func() spend.Reservation {
		return s.rs.llmReservation(s.rowIdentity, s.col.ID, provider, exposure,
			map[string]any{"llm_call": call, "column": s.col.cfg}, call)
	}, nil)
	if err != nil {
		var ref *aicol.SpendRefused
		if errors.As(err, &ref) {
			return nil, err
		}
		if errors.Is(err, spend.ErrInvalidInput) {
			return nil, &aicol.SpendRefused{Reason: "invalid_reservation"}
		}
		return nil, err
	}
	return h, nil
}
