package workbook

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Agent column (workbook/agent_column.py run_agent_cell), run in process with
// GO_NATIVE_AI_COLUMNS: the agent re-plans the candidate tools every step (planner order,
// cooldowns, the cell/workbook budget left), calls the first untried one and stops at the
// first value of the target field. Paid tools go through the Go spend ledger exactly like a
// waterfall provider (reserve, fenced dispatch, settle; uncertain on any failure after
// dispatch, released when never dispatched), so the agent_paid_tools refusal of the RPC
// path is gone. The provider runner owns the provider_stats ledger.

const (
	agentDefaultMaxSteps = 6
	agentDefaultMaxCost  = 0.10
	maxGoalRunes         = 512 // cell_traces.goal is varchar(512)
)

// agentTarget is _infer_target.
func agentTarget(c column) string {
	if t := c.firstTruthy("target_field", "lead_field"); t != "" {
		return t
	}
	return "email"
}

// agentCandidates is run_agent_cell's tool list: explicit tools (even empty), else the
// target's default chain.
func agentCandidates(c column) []string {
	if raw, ok := c.cfg["tools"]; ok && raw != nil {
		out := []string{}
		if list, ok := raw.([]any); ok {
			for _, e := range list {
				out = append(out, manifest.PyStr(e))
			}
		}
		return out
	}
	return append([]string(nil), defaultWaterfalls[agentTarget(c)]...)
}

func agentPolicy(c column) (maxSteps int, maxCost float64, err error) {
	maxSteps, maxCost = agentDefaultMaxSteps, agentDefaultMaxCost
	policy, _ := c.cfg["policy"].(map[string]any)
	if v, ok := policy["max_steps"]; ok {
		f, ok := asNumber(v)
		if !ok {
			return 0, 0, fmt.Errorf("invalid policy.max_steps")
		}
		maxSteps = int(f)
	}
	if v, ok := policy["max_cost_usd"]; ok {
		f, ok := asNumber(v)
		if !ok {
			return 0, 0, fmt.Errorf("invalid policy.max_cost_usd")
		}
		maxCost = f
	}
	return maxSteps, maxCost, nil
}

func asNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	f := asFloat(canonify(v))
	switch canonify(v).(type) {
	case float64, int64:
		return f, true
	}
	return 0, false
}

// floorMicro is int(floor(Decimal(str(usd)) * 1e6)).
func floorMicro(usd float64) int64 {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(usd, 'e', -1, 64))
	if !ok {
		return 0
	}
	r.Mul(r, big.NewRat(1000000, 1))
	q := new(big.Int).Div(r.Num(), r.Denom())
	if !q.IsInt64() {
		return 0
	}
	return q.Int64()
}

// agentStep is one trace step (insertion order as Python's dict).
func agentStep(i int, provider string, success bool, kv ...any) *aicol.Obj {
	o := aicol.NewObj("step", i, "provider", provider, "success", success)
	for j := 0; j+1 < len(kv); j += 2 {
		o.Set(kv[j].(string), kv[j+1])
	}
	return o
}

type agentResult struct {
	value, provider, outcome string
	found                    bool
}

// agentCell runs one agent cell. A returned error unwinds the run (lease lost, job
// cancelled, provider runner gone) or is a configuration error the caller reports as
// cell_execution_failed (abortErr tells them apart).
func (rs *runState) agentCell(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (pyOutput, error) {
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	target := agentTarget(col)
	goal := "Find " + target
	if g := col.cfg["goal"]; manifest.PyTruthy(g) {
		goal = manifest.PyStr(g)
	}
	maxSteps, maxCost, err := agentPolicy(col)
	if err != nil {
		return pyOutput{}, err
	}
	tools := agentCandidates(col)
	inTools := func(p string) bool {
		for _, t := range tools {
			if t == p {
				return true
			}
		}
		return false
	}
	var wbRemaining *float64
	if err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		b, e := gen.New(tx).GetWorkbookBudget(ctx, gen.GetWorkbookBudgetParams{WorkbookID: wb, WorkspaceID: ws})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if b.BudgetMaxUsd.Valid && b.BudgetMaxUsd.Float64 > 0 {
			r := b.BudgetMaxUsd.Float64 - b.BudgetSpentUsd.Float64
			wbRemaining = &r
		}
		return nil
	}); err != nil {
		return pyOutput{}, err
	}
	rowIdentity := fmt.Sprintf("row:%d", rowID)
	if rowID == 0 {
		rowIdentity = fmt.Sprintf("lead:%d", leadID)
	}
	// Persisted attempts of this run are visited before planning fresh spend: a settled
	// result is replayed even when its charge exhausted the cap.
	var prior []string
	if err := tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		var e error
		prior, e = gen.New(tx).ListPriorAttemptProviders(ctx, gen.ListPriorAttemptProvidersParams{
			WorkspaceID: ws, WorkbookID: wb, RunID: rs.spendRunID(), RowIdentity: rowIdentity, ColumnID: col.ID})
		return e
	}); err != nil {
		return pyOutput{}, err
	}

	steps := []any{}
	tried := map[string]bool{}
	spent := 0.0
	res := agentResult{outcome: "exhausted"}
loop:
	for stepI := 1; stepI <= maxSteps; stepI++ {
		cellBudget := maxCost - spent
		if wbRemaining != nil {
			cellBudget = min(cellBudget, *wbRemaining-spent)
		}
		planned, _, err := rs.pl.plan(ctx, rs.deps.Pool, ws, target, tools, false, &cellBudget)
		if err != nil {
			return pyOutput{}, err
		}
		var ordered []string
		seen := map[string]bool{}
		for _, p := range append(append([]string{}, prior...), planned...) {
			if (!seen[p]) && inTools(p) && !tried[p] {
				seen[p] = true
				ordered = append(ordered, p)
			}
		}
		if len(ordered) == 0 {
			blocked := false
			for _, t := range tools {
				if !tried[t] && rs.pl.paid(t) && rs.pl.cost(t) > cellBudget {
					blocked = true
				}
			}
			if blocked {
				res.outcome = "budget"
			} else {
				res.outcome = "exhausted"
			}
			break
		}
		name := ordered[0]
		if err := checkOwner(ctx, rs.deps.Pool, rs.claim, wb); err != nil {
			return pyOutput{}, err
		}
		if err := rs.clientErr(); err != nil {
			return pyOutput{}, err
		}
		notRegistered := func() {
			steps = append(steps, agentStep(stepI, name, false, "reason", "provider_not_registered"))
			kept := tools[:0:0]
			for _, t := range tools {
				if t != name {
					kept = append(kept, t)
				}
			}
			tools = kept
		}
		if _, declarative := rs.p.Manifests[name]; !declarative && rs.legacy {
			notRegistered()
			continue
		}
		reserved := rs.pl.paid(name)
		if !reserved && !rs.pl.knownCost(name) {
			res.outcome = "provider_price_unknown"
			steps = append(steps, agentStep(stepI, name, false, "reason", res.outcome))
			break
		}
		tried[name] = true
		var call providerCall
		if reserved {
			exposure, err := spend.ExposureFromUSD(rs.pl.cost(name))
			if err != nil {
				return pyOutput{}, err
			}
			if perr := rs.precheck(ctx, name, lead); perr != "" {
				call = providerCall{kind: callOK, resp: responseDict(name, providers.Output{Error: perr})}
			} else if call, err = rs.reservedCall(ctx, name, lead, col, target, rowIdentity, exposure, floorMicro(maxCost)); err != nil {
				return pyOutput{}, err
			}
		} else {
			out, timedOut, rerr := rs.runProvider(ctx, name, lead, target)
			if ctx.Err() != nil {
				return pyOutput{}, context.Cause(ctx)
			}
			switch {
			case errors.Is(rerr, provcall.ErrUnknownProvider):
				call = providerCall{kind: callUnknown}
			case errors.Is(rerr, provcall.ErrUnavailable), errors.Is(rerr, provcall.ErrUnauthorized):
				return pyOutput{}, fmt.Errorf("workbook: %w", rerr)
			case rerr != nil:
				call = providerCall{kind: callException, text: truncRunes(rerr.Error(), maxErrorRunes)}
			case timedOut:
				call = providerCall{kind: callTimeout}
			default:
				call = providerCall{kind: callOK, resp: responseDict(name, out)}
			}
		}
		switch call.kind {
		case callReject, callInvalid:
			res.outcome = call.text
			steps = append(steps, agentStep(stepI, name, false, "reason", res.outcome))
			break loop
		case callUnknown:
			delete(tried, name)
			notRegistered()
			continue
		case callTimeout, callException:
			reason := "timeout"
			if call.kind == callException {
				reason = "error: " + truncRunes(call.text, 160)
				if looksRateLimited(call.text) {
					reason = "rate_limited→reroute"
				}
			}
			steps = append(steps, agentStep(stepI, name, false, "reason", reason))
			if reserved {
				res.outcome = "accounting_uncertain"
				break loop
			}
			continue
		}
		spent += float64(call.charged) / 1e6
		success, _ := call.resp["success"].(bool)
		fields, _ := call.resp["fields"].(map[string]any)
		if v, ok := fields[target]; success && len(fields) > 0 && ok && manifest.PyTruthy(v) {
			res.value, res.provider, res.found, res.outcome = manifest.PyStr(v), name, true, "found"
			steps = append(steps, agentStep(stepI, name, true, "value", truncRunes(res.value, 80),
				"cost", rs.pl.cost(name), "reason", "goal_met"))
			break
		}
		steps = append(steps, agentStep(stepI, name, false, "reason", "no_value"))
	}
	rs.saveTrace(ctx, leadIDForTrace(lead), col.ID, goal, steps, res.outcome, spent)
	out := pyOutput{Provider: res.provider}
	if res.found {
		out.Value, out.HasValue = res.value, true
	} else {
		out.Error = "agent_" + res.outcome
	}
	return out, nil
}

// leadIDForTrace is the AgentProvider lead id: the row's lead id, else its row id, else 0.
func leadIDForTrace(lead map[string]any) int64 {
	if v := lead["__lead_id"]; v != nil {
		return intOf(v)
	}
	if v := lead["__row_id"]; v != nil {
		return intOf(v)
	}
	return 0
}

// saveTrace is _save_trace: the first trace of the cell is overwritten, else one is added.
// It is telemetry: a failure is logged and never fails the cell.
func (rs *runState) saveTrace(ctx context.Context, leadID int64, colID, goal string, steps []any, outcome string, spent float64) {
	stepsJSON, err := aicol.JSON(steps)
	if err != nil {
		rs.log.Warn("agent trace not encodable", "err", err)
		return
	}
	cost, _ := aicol.JSON(aicol.NewObj("spent", round4(spent)))
	ws, wb := rs.p.WorkspaceID, rs.p.WorkbookID
	goal = truncRunes(goal, maxGoalRunes)
	err = tenancy.InWorkspace(ctx, rs.deps.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		n, err := q.UpdateCellTrace(ctx, gen.UpdateCellTraceParams{Goal: goal, Steps: stepsJSON, Outcome: outcome,
			TotalCostUsd: cost, WorkspaceID: ws, WorkbookID: wb, LeadID: int32(leadID), ColumnID: colID})
		if err != nil || n > 0 {
			return err
		}
		return q.InsertCellTrace(ctx, gen.InsertCellTraceParams{WorkbookID: wb, WorkspaceID: ws, LeadID: int32(leadID),
			ColumnID: colID, Goal: goal, Steps: stepsJSON, Outcome: outcome, TotalCostUsd: cost})
	})
	if err != nil {
		rs.log.Warn("agent trace write failed", "col", colID, "err", err)
	}
}
