package workbook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colcompute"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

// Python-executed column types: the cell is computed by the Python worker
// (LLM, research agent, tool-picking agent) behind POST /internal/provider-call
// as the pseudo-providers below. Go owns the cell: it writes the value, the
// overlay and the broadcast exactly like enrich_cell, fenced by the queue lease.
const (
	rpcAIFormula = "ai_formula"
	rpcResearch  = "research"
	rpcAgent     = "agent"
	rpcAIBatch   = "ai_batch"
)

// Per-call RPC deadlines. ai_formula is WORKBOOK_AI_TIMEOUT (default 45 s); the
// research and agent loops are bounded by their own step and budget caps on the
// Python side and by the RPC maximum here. A timeout is a cell error and is
// never retried.
const (
	aiTimeout       = 45 * time.Second
	researchTimeout = 120 * time.Second
	agentTimeout    = 120 * time.Second
	batchTimeout    = 120 * time.Second
)

var pyColumnTypes = map[string]bool{rpcAIFormula: true, rpcResearch: true, rpcAgent: true}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "go-" + hex.EncodeToString(b[:])
}

// checkPyColumn refuses (hands back) a column Go may not run: no Python worker
// is configured, or an agent could spend money outside the Go spend ledger.
func (rs *runState) checkPyColumn(c column) error {
	if !pyColumnTypes[c.Type] {
		return nil
	}
	if rs.deps.Py == nil {
		return refuse("python_worker_unavailable: column %q (%s) runs on the python worker", c.ID, c.Type)
	}
	if c.Type == rpcAgent {
		// The agent picks tools at run time and, without the queue lease, the
		// Python side cannot reserve spend attempts: only all-free toolsets run
		// here, anything that may cost money stays on the Python executor.
		for _, t := range agentTools(c) {
			if vendorcatalog.IsPaid(t) || !vendorcatalog.HasKnownCost(t) {
				return refuse("agent_paid_tools: column %q may call paid provider %q", c.ID, t)
			}
		}
	}
	return nil
}

// agentTools ports run_agent_cell's candidate list: the explicit tools (even
// empty) else the target's default chain.
func agentTools(c column) []string {
	if raw, ok := c.cfg["tools"]; ok && raw != nil {
		var out []string
		if list, ok := raw.([]any); ok {
			for _, e := range list {
				out = append(out, manifest.PyStr(e))
			}
		}
		return out
	}
	return defaultWaterfalls[c.target()]
}

func (rs *runState) columnConfigs() []any {
	out := make([]any, len(rs.cols))
	for i, c := range rs.cols {
		out[i] = c.cfg
	}
	return out
}

// pyOutput is the decoded answer of a column RPC: fields.value / error /
// metadata / provider.
type pyOutput struct {
	Value    any
	HasValue bool
	Error    string
	Metadata map[string]any
	Provider string
}

func decodeColumnOutput(raw json.RawMessage) (pyOutput, error) {
	var o pyOutput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top struct {
		Fields map[string]any `json:"fields"`
	}
	if err := dec.Decode(&top); err != nil {
		return o, err
	}
	o.Value, o.HasValue = top.Fields["value"]
	o.Error, _ = top.Fields["error"].(string)
	o.Metadata, _ = top.Fields["metadata"].(map[string]any)
	o.Provider, _ = top.Fields["provider"].(string)
	return o, nil
}

// pyCall performs one RPC. A returned error means the whole run must unwind
// (job cancelled) or the python worker is not usable; a cell-local failure is
// reported through (status, output).
func (rs *runState) pyCall(ctx context.Context, provider string, input map[string]any, timeout time.Duration) (pyworker.ProviderResult, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return pyworker.ProviderResult{Status: "error", Error: "invalid provider call input"}, nil
	}
	res, err := rs.deps.Py.Call(ctx, pyworker.ProviderCall{CallID: newCallID(), Provider: provider,
		WorkspaceID: rs.p.WorkspaceID, Input: b, Timeout: timeout})
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return res, a
		}
		return res, fmt.Errorf("workbook: python worker: %w", err)
	}
	if ctx.Err() != nil {
		return res, context.Cause(ctx) // a late answer never overwrites a cancelled run
	}
	if res.Err != nil {
		return res, fmt.Errorf("workbook: python worker: %w", res.Err)
	}
	return res, nil
}

// pyCell runs an ai_formula, research or agent cell on the Python worker.
func (rs *runState) pyCell(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, error) {
	if col.Type != rpcAgent && !manifest.PyTruthy(col.cfg["prompt"]) {
		return rs.failDependent(ctx, lead, col, "no_prompt")
	}
	timeout, provider, timeoutErr := aiTimeout, "ai", "ai_timeout"
	switch col.Type {
	case rpcResearch:
		timeout, provider, timeoutErr = researchTimeout, "research", "research_timeout"
	case rpcAgent:
		timeout, provider, timeoutErr = agentTimeout, "agent", "agent_timeout"
	}
	input := map[string]any{
		"prompt": col.cfg["prompt"], "row": rowForColumn(lead), "columns": rs.columnConfigs(), "column": col.cfg,
		"workbook_id": rs.p.WorkbookID, "lead_id": intOrNil(lead["__lead_id"]), "row_id": rowID,
		"provider_timeout": rs.p.ProviderTimeout,
	}
	res, err := rs.pyCall(ctx, col.Type, input, timeout)
	if err != nil {
		return cellOutcome{}, err
	}
	switch res.Status {
	case "timeout":
		rs.markNoRetry(rowID, col.ID) // a timed-out RPC is never re-run automatically
		return rs.commitCell(ctx, col, rowID, leadID, cellResult{Provider: provider, Err: timeoutErr})
	case "ok", "error":
	default:
		return rs.failDependent(ctx, lead, col, "cell_execution_failed")
	}
	out, derr := decodeColumnOutput(res.Output)
	if derr != nil || (res.Status == "error" && !out.HasValue && out.Error == "") {
		// The Python column raised: enrich_cell lets that escape to _run_one_cell.
		return rs.failDependent(ctx, lead, col, "cell_execution_failed")
	}
	if out.Provider != "" && col.Type == rpcAgent {
		provider = out.Provider
	}
	r := cellResult{Provider: provider, Err: out.Error, Meta: nil}
	if col.Type == rpcResearch {
		r.Meta = out.Metadata
	}
	if out.HasValue && out.Value != nil {
		text, raw, serr := colcompute.StoredValue(out.Value)
		if serr != nil {
			return cellOutcome{}, serr
		}
		if !emptyValue(raw) {
			r.Has, r.Value, r.Raw, r.HasRaw = true, text, raw, true
			if _, isStr := out.Value.(string); isStr {
				r.HasRaw = false
			}
		}
	}
	return rs.commitCell(ctx, col, rowID, leadID, r)
}

func intOrNil(v any) any {
	if v == nil {
		return nil
	}
	return intOf(v)
}

// ── AI batch prepass ───────────────────────────────────────────────────────

type batchKey struct {
	row int64
	col string
}

// batchEligible ports _batch_eligible_columns over the whole columns_config:
// independent, text-output ai_formula columns with a prompt.
func (rs *runState) batchEligible() map[string]bool {
	var run []colgraph.Col
	var byID = map[string]column{}
	for _, c := range rs.cols {
		if enrichmentColTypes[c.Type] {
			run = append(run, c.g)
			byID[c.ID] = c
		}
	}
	ok := map[string]bool{}
	for _, g := range colgraph.Independent(run) {
		id, _ := g.Get("id").(string)
		c := byID[id]
		if c.Type != rpcAIFormula {
			continue
		}
		if f := c.firstTruthy("output_format"); f == "json" {
			continue
		}
		if p, _ := c.cfg["prompt"].(string); trim(p) == "" {
			continue
		}
		// A condition is evaluated per cell before it runs (preRunGates); the pre-pass
		// writes complete cells without that gate, so a gated column stays on the per-cell
		// path and an unmet condition never reaches the LLM (review wave 2b M5; Python's
		// pre-pass had the same gap).
		if manifest.PyTruthy(c.cfg["condition"]) {
			continue
		}
		ok[id] = true
	}
	return ok
}

// aiBatchPrepass ports _run_ai_batch_prepass over the Python worker: eligible
// cells of many rows go out as one batch request; every answered cell is
// persisted like a synchronous ai cell. It returns the handled (row, column)
// pairs; any failure is "nothing handled" so the synchronous loop runs those
// cells (the durable attempt-recovery of the Python executor is not reproduced,
// which is why the prepass is off unless Deps.AIBatch.Enabled).
func (rs *runState) aiBatchPrepass(ctx context.Context, items []workItem) (map[batchKey]bool, int, error) {
	cfg := rs.deps.AIBatch
	if !cfg.Enabled || rs.deps.Py == nil {
		return nil, 0, nil
	}
	minRows := cfg.MinRows
	if minRows <= 0 {
		minRows = 20
	}
	eligible := rs.batchEligible()
	var reqs []map[string]any
	index := map[string]batchKey{}
	leads := map[int64]map[string]any{}
	for _, it := range items {
		for _, c := range it.cols {
			if !eligible[c.ID] {
				continue
			}
			rid := it.rowID()
			cid := fmt.Sprintf("row_%d::%s", rid, c.ID)
			index[cid] = batchKey{rid, c.ID}
			leads[rid] = it.lead
			reqs = append(reqs, map[string]any{"custom_id": cid, "row": rowForColumn(it.lead), "column_id": c.ID})
		}
	}
	if len(reqs) < minRows {
		return nil, 0, nil
	}
	res, err := rs.pyCall(ctx, rpcAIBatch, map[string]any{"requests": reqs, "columns": rs.columnConfigs()}, batchTimeout)
	if err != nil {
		if abortErr(ctx, err) != nil {
			return nil, 0, err
		}
		rs.log.Warn("ai batch pre-pass unavailable, continuing synchronously", "err", err)
		return nil, 0, nil
	}
	if res.Status != "ok" {
		rs.log.Warn("ai batch pre-pass failed, continuing synchronously", "status", res.Status)
		return nil, 0, nil
	}
	var out struct {
		Fields struct {
			Results map[string]string `json:"results"`
		} `json:"fields"`
	}
	if json.Unmarshal(res.Output, &out) != nil {
		return nil, 0, nil
	}
	handled := map[batchKey]bool{}
	completed := 0
	for cid, text := range out.Fields.Results {
		k, ok := index[cid]
		if !ok {
			continue
		}
		lead := leads[k.row]
		leadID := intOf(lead["id"])
		w := cellWrite{WorkspaceID: rs.p.WorkspaceID, WorkbookID: rs.p.WorkbookID, LeadID: int32(leadID),
			RowID: int32(k.row), ColumnID: k.col, Status: "complete", Value: &text}
		prov := "ai"
		w.Provider = &prov
		var change *rowChange
		err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) (err error) {
			if err := fence(ctx, tx, rs.claim, rs.p.WorkbookID); err != nil {
				return err
			}
			change, err = setEnrichmentChange(ctx, tx, w)
			return err
		})
		if err == nil {
			rs.queueChange(change)
		}
		if err != nil {
			if a := abortErr(ctx, err); a != nil {
				return handled, completed, a
			}
			if errors.Is(err, ErrLeaseLost) {
				return handled, completed, err
			}
			return handled, completed, err
		}
		handled[k] = true
		completed++
		rs.publish(ctx, map[string]any{"type": "cell_update", "leadId": leadID, "rowId": k.row, "colId": k.col,
			"value": text, "status": "complete", "provider": "ai", "error": nil})
	}
	return handled, completed, nil
}

// applyBatchHandled ports _apply_batch_handled.
func applyBatchHandled(items []workItem, handled map[batchKey]bool) []workItem {
	if len(handled) == 0 {
		return items
	}
	var out []workItem
	for _, it := range items {
		var kept []column
		for _, c := range it.cols {
			if !handled[batchKey{it.rowID(), c.ID}] {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			out = append(out, workItem{lead: it.lead, cols: kept})
		}
	}
	return out
}

// markNoRetry excludes a cell from the automatic retry passes.
func (rs *runState) markNoRetry(rowID int64, colID string) {
	rs.noRetryMu.Lock()
	defer rs.noRetryMu.Unlock()
	if rs.noRetry == nil {
		rs.noRetry = map[cellKey]bool{}
	}
	rs.noRetry[cellKey{rowID, colID}] = true
}

func (rs *runState) isNoRetry(k cellKey) bool {
	rs.noRetryMu.Lock()
	defer rs.noRetryMu.Unlock()
	return rs.noRetry[k]
}
