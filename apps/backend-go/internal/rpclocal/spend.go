package rpclocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

// Paid provider calls made outside a workbook (provcall.SpendScope on the context) are attempts of the spend
// ledger's workspace scope: reserved at the catalog price before the call, dispatched, then settled from the
// answer; a call that fails after dispatch is uncertain and a hold that is never dispatched is released. Python
// ran these calls outside any ledger. Without a ledger a scoped paid call is refused (fail closed).

const (
	reasonNoLedger = "spend_ledger_unavailable"
	reasonDown     = "spend_unavailable"
)

func spendRefusal(reason string) provcall.Result {
	return provcall.Result{Status: "error", Error: reason}
}

// executeLedgered is execute, wrapped in the ledger when the call is scoped and paid.
func (c *Caller) executeLedgered(ctx context.Context, call provcall.Call) (provcall.Result, error) {
	scope, ok := provcall.SpendScopeFrom(ctx)
	if !ok || !vendorcatalog.IsPaid(call.Provider) {
		return c.execute(ctx, call)
	}
	if c.d.SpendPool == nil {
		return spendRefusal(reasonNoLedger), nil
	}
	exposure, err := spend.ExposureFromUSD(c.d.Cost(call.Provider))
	if err != nil {
		return spendRefusal(reasonDown), nil
	}
	limit := scope.LimitUSD
	if limit <= 0 {
		limit = provcall.DefaultSpendLimitUSD
	}
	cellLimit, err := spend.ExposureFromUSD(limit)
	if err != nil {
		return spendRefusal(reasonDown), nil
	}
	// One attempt per dispatched call: the call id is unique per call, so a retry after a failure is a new
	// attempt (bounded by the scope's limit) while the flight cache already dedupes a concurrent twin.
	run, row := truncID(scope.Run), truncID(scope.Row)
	key, err := canonjson.Marshal([]any{call.WorkspaceID, spend.WorkspaceLedger, run, row, "provider_call", call.Provider, call.CallID}, false)
	if err != nil {
		return spendRefusal(reasonDown), nil
	}
	sum := sha256.Sum256(key)
	r := spend.Reservation{WorkspaceID: call.WorkspaceID, WorkbookID: spend.WorkspaceLedger, RunID: run, RowIdentity: row,
		ColumnID: "provider_call", Provider: call.Provider, AttemptKey: hex.EncodeToString(sum[:]),
		ExposureMicroUSD: exposure, CellLimitMicroUSD: cellLimit,
		CostBasis: map[string]any{"kind": "catalog_estimate", "provider": call.Provider},
		Operation: map[string]any{"call_id": truncID(call.CallID)}}
	var adm spend.Admission
	if err := tenancy.InWorkspace(ctx, c.d.SpendPool, call.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		adm, e = spend.ReserveWorkspace(ctx, tx, r)
		return e
	}); err != nil {
		if ctx.Err() != nil {
			return provcall.Result{}, ctx.Err()
		}
		return spendRefusal(reasonDown), nil
	}
	if !adm.OK {
		return spendRefusal("spend_" + adm.Reason), nil
	}
	if adm.Reused {
		return spendRefusal("spend_attempt_already_used"), nil
	}
	id := spend.Identity{WorkspaceID: call.WorkspaceID, WorkbookID: spend.WorkspaceLedger, AttemptID: adm.ID, ContractHash: adm.ContractHash}
	if ok, err := c.transition(ctx, id, "dispatch"); err != nil || !ok {
		c.transitionDetached(ctx, id, "release_before_dispatch")
		if ctx.Err() != nil {
			return provcall.Result{}, ctx.Err()
		}
		return spendRefusal("spend_attempt_not_dispatchable"), nil
	}
	res, err := c.execute(ctx, call)
	c.settle(ctx, id, call.Provider, exposure, res, err)
	return res, err
}

func (c *Caller) transition(ctx context.Context, id spend.Identity, action string) (bool, error) {
	var ok bool
	err := tenancy.InWorkspace(ctx, c.d.SpendPool, id.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		ok, e = spend.Transition(ctx, tx, id, action)
		return e
	})
	return ok, err
}

func (c *Caller) transitionDetached(ctx context.Context, id spend.Identity, action string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spendTimeout)
	defer cancel()
	_, _ = c.transition(dctx, id, action)
}

// settle accounts a dispatched call from its answer; anything that cannot be accounted is uncertain.
func (c *Caller) settle(ctx context.Context, id spend.Identity, provider string, exposure int64, res provcall.Result, callErr error) {
	if callErr != nil || res.Status == "timeout" || res.HTTPStatus >= http.StatusInternalServerError {
		c.transitionDetached(ctx, id, "mark_uncertain") // it may have reached the vendor
		return
	}
	env, err := spend.AccountingEnvelope(provider, spend.ProviderResponse{"provider": provider, "success": res.Status == "ok"}, exposure)
	if err != nil {
		c.transitionDetached(ctx, id, "mark_uncertain")
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spendTimeout)
	defer cancel()
	var sr spend.SettleResult
	err = tenancy.InWorkspace(dctx, c.d.SpendPool, id.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		sr, e = spend.SettleWorkspace(dctx, tx, id, env.ChargedMicroUSD, env.AccountingBasis, env.Result)
		return e
	})
	if err != nil || !sr.OK {
		c.transitionDetached(ctx, id, "mark_uncertain")
	}
}

// truncID keeps an identity within the ledger's 255-character bound.
func truncID(s string) string {
	if len(s) <= 255 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
