package workbook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// AIColumns runs ai_formula, research and agent cells and the ai_formula batch pre-pass in
// process (GO_NATIVE_AI_COLUMNS, plan 15 WP-AICOLS) instead of on the Python worker. nil
// keeps the Python worker path.
type AIColumns struct {
	// For returns the column collaborators of a workspace: an LLM client that accounts
	// usage to the workspace, operator settings, search, page fetching and the URL guard.
	For func(ctx context.Context, workspaceID string) aicol.Deps
}

// aiDeps builds the run's column collaborators once.
func (rs *runState) aiDeps(ctx context.Context) aicol.Deps {
	rs.aiOnce.Do(func() { rs.ai = rs.deps.AICols.For(ctx, rs.p.WorkspaceID) })
	return rs.ai
}

// aiState is embedded in runState.
type aiState struct {
	aiOnce sync.Once
	ai     aicol.Deps
}

// nativeColumn runs one ai_formula / research / agent cell in process and answers like the
// column RPC: status ok/error/timeout and the decoded fields.
func (rs *runState) nativeColumn(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (status string, out pyOutput, err error) {
	if col.Type == rpcAgent {
		// The overall deadline of the RPC path (review wave 3b L9): per-tool timeouts and an
		// unbounded policy.max_steps alone do not bound the cell. A paid call cut by it is
		// left uncertain by reservedCall.
		actx, cancel := context.WithTimeout(ctx, agentTimeout)
		defer cancel()
		out, err := rs.agentCell(actx, lead, col, rowID, leadID)
		if ctx.Err() != nil {
			return "", pyOutput{}, context.Cause(ctx)
		}
		if errors.Is(actx.Err(), context.DeadlineExceeded) {
			return "timeout", pyOutput{}, nil
		}
		if err != nil {
			if a := abortErr(ctx, err); a != nil {
				return "", pyOutput{}, a
			}
			rs.log.Error("agent cell failed", "col", col.ID, "row", rowID, "err", err)
			return "error", pyOutput{}, nil
		}
		return "ok", out, nil
	}
	d := rs.aiDeps(ctx)
	limit := aiTimeout
	if col.Type == rpcResearch {
		limit = researchTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	row := rowForColumn(lead)
	prompt := ""
	if p := col.cfg["prompt"]; p != nil {
		prompt = manifest.PyStr(p)
	}
	var res aicol.Result
	switch col.Type {
	case rpcAIFormula:
		res, err = aicol.AIFormula(cctx, d, prompt, row, rs.allCols)
	case rpcResearch:
		v, present := col.cfg["max_steps"]
		steps, serr := aicol.MaxStepsOf(v, present)
		budget, berr := aicol.BudgetOf(col.cfg["cell_budget_usd"])
		if serr != nil || berr != nil {
			return "error", pyOutput{}, nil // Python: the provider raised
		}
		res, err = aicol.Research(cctx, d, aicol.ResearchInput{Template: prompt, Row: row, Cols: rs.allCols,
			MaxSteps: steps, Budget: budget})
	}
	if ctx.Err() != nil {
		return "", pyOutput{}, context.Cause(ctx)
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return "timeout", pyOutput{}, nil
	}
	if err != nil {
		return "error", pyOutput{}, nil
	}
	out = pyOutput{Value: res.Value, HasValue: true, Error: res.Error}
	if res.Metadata != nil {
		out.Metadata = res.Metadata.ToMap()
	}
	return "ok", out, nil
}

// ErrBatchRecovery is batch_attempts.BatchRecoveryRequired: an AI batch attempt of this job
// exists (or its outcome is unknown), so the cells must not silently fall back to
// synchronous calls; the job fails and a retry retrieves the existing vendor batch.
var ErrBatchRecovery = errors.New("workbook: AI batch recovery required")

func batchRecovery(msg string) error { return fmt.Errorf("%w: %s", ErrBatchRecovery, msg) }

const batchAttemptKey = "ai_batch_attempt"

// batchContractHash is claim_batch_attempt's digest over the requests and the provider
// identity (the API key is not part of the Go contract).
func batchContractHash(reqs []llm.BatchRequest, p llm.Provider) (string, error) {
	rl := make([]any, len(reqs))
	for i, r := range reqs {
		rl[i] = map[string]any{"custom_id": r.CustomID, "prompt": r.Prompt, "system": r.System, "max_tokens": int64(r.MaxTokens)}
	}
	b, err := canonjson.Marshal(map[string]any{"requests": rl,
		"provider": map[string]any{"id": p.ID, "model": p.Model, "base_url": p.BaseURL}}, true)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// batchAttempt reads (and with fn, edits) the job's ai_batch_attempt under the lease.
func (rs *runState) batchAttempt(ctx context.Context, fn func(payload map[string]any) (bool, error)) error {
	c := rs.claim
	return tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		raw, err := q.LockOwnedRunWorkbookJob(ctx, gen.LockOwnedRunWorkbookJobParams{
			ID: int32(c.ID), WorkspaceID: c.WorkspaceID, WorkerID: c.WorkerID,
			LockedAt: pgtype.Timestamp{Time: c.LockedAt, Valid: true}, LeaseToken: c.LeaseToken,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		payload, err := jsonObject(raw)
		if err != nil {
			return err
		}
		if wb, _ := payload["workbook_id"].(string); wb != rs.p.WorkbookID {
			return ErrLeaseLost
		}
		changed, err := fn(payload)
		if err != nil || !changed {
			return err
		}
		b, err := canonjson.Marshal(canonValue(payload), false)
		if err != nil {
			return err
		}
		return q.SetRunWorkbookJobPayload(ctx, gen.SetRunWorkbookJobPayloadParams{Payload: b, ID: int32(c.ID), WorkspaceID: c.WorkspaceID})
	})
}

func (rs *runState) priorBatchAttempt(ctx context.Context) (map[string]any, error) {
	var prior map[string]any
	err := rs.batchAttempt(ctx, func(p map[string]any) (bool, error) {
		if v, ok := p[batchAttemptKey]; ok && v != nil {
			m, ok := v.(map[string]any)
			if !ok {
				return false, batchRecovery("malformed batch attempt")
			}
			prior = m
		}
		return false, nil
	})
	return prior, err
}

// nativeBatch ports _run_ai_batch_prepass's dispatch over the Go LLM client with the
// durable claim of batch_attempts.py (queued runs): one vendor batch per job, the vendor id
// acknowledged and every result checkpointed in jobs.payload before a cell is written, so a
// retried job retrieves the batch instead of paying for a second one. nil results (no error)
// means "run synchronously".
func (rs *runState) nativeBatch(ctx context.Context, cells []aicol.BatchCell, index map[string]batchKey) (map[string]string, error) {
	d := rs.aiDeps(ctx)
	var prior map[string]any
	if rs.claim != nil {
		var err error
		if prior, err = rs.priorBatchAttempt(ctx); err != nil {
			if a := abortErr(ctx, err); a != nil || errors.Is(err, ErrLeaseLost) {
				return nil, err
			}
			return nil, batchRecovery("cannot verify prior batch ownership; no fallback permitted")
		}
	}
	prov, ok := d.LLM.AnthropicProvider(ctx)
	if !ok {
		if prior != nil {
			return nil, batchRecovery("batch provider changed with an existing attempt; reconcile before retrying")
		}
		return nil, nil
	}
	reqs, err := aicol.BatchRequests(ctx, d, cells, rs.allCols)
	if err != nil {
		if prior != nil {
			return nil, batchRecovery("existing batch failed recovery; synchronous fallback is disabled")
		}
		rs.log.Warn("ai batch pre-pass errored, continuing synchronously", "err", err)
		return nil, nil
	}
	opts := llm.BatchOptions{Provider: &prov, ShouldStop: rs.stop.shouldStop}
	var cached map[string]string
	var hash string
	var ackErr error
	if rs.claim != nil {
		if hash, err = batchContractHash(reqs, prov); err != nil {
			return nil, err
		}
		action := "dispatch"
		err = rs.batchAttempt(ctx, func(p map[string]any) (bool, error) {
			if v, ok := p[batchAttemptKey]; ok && v != nil {
				m, _ := v.(map[string]any)
				if h, _ := m["contract_hash"].(string); m == nil || h != hash {
					return false, batchRecovery("batch input contract changed")
				}
				prior = m
				action = "reconcile"
				if vid, _ := m["vendor_batch_id"].(string); vid != "" {
					action, opts.BatchID = "retrieve", vid
				}
				return false, nil
			}
			p[batchAttemptKey] = map[string]any{"contract_hash": hash, "state": "dispatch_claimed", "vendor_batch_id": nil}
			return true, nil
		})
		if err != nil {
			return nil, err
		}
		if action == "reconcile" {
			return nil, batchRecovery("batch submission outcome unknown; reconcile before retrying")
		}
		if r, ok := prior["results"].(map[string]any); ok {
			cached = map[string]string{}
			for k, v := range r {
				if s, ok := v.(string); ok {
					cached[k] = s
				}
			}
		}
		opts.StrictResults = true
		for k := range cached {
			opts.KnownResultIDs = append(opts.KnownResultIDs, k)
		}
		opts.OnSubmitted = func(vendorID string) {
			ackErr = rs.batchAttempt(ctx, func(p map[string]any) (bool, error) {
				m, _ := p[batchAttemptKey].(map[string]any)
				if h, _ := m["contract_hash"].(string); m == nil || h != hash {
					return false, batchRecovery("batch claim does not match acknowledgement")
				}
				if vid, _ := m["vendor_batch_id"].(string); vid != "" && vid != vendorID {
					return false, batchRecovery("batch already has a different vendor identity")
				}
				m["state"], m["vendor_batch_id"] = "accepted", vendorID
				return true, nil
			})
		}
	}
	complete := cached != nil && len(cached) == len(index)
	if complete {
		for k := range index {
			if _, ok := cached[k]; !ok {
				complete = false
			}
		}
	}
	results := cached
	if !complete {
		got, err := d.LLM.BatchComplete(ctx, reqs, opts)
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if ackErr != nil {
			if abortErr(ctx, ackErr) != nil || errors.Is(ackErr, ErrLeaseLost) {
				return nil, ackErr
			}
			return nil, batchRecovery("batch acknowledgement failed: " + ackErr.Error())
		}
		if err != nil {
			if rs.claim != nil {
				return nil, batchRecovery("batch results unavailable; retrieve or reconcile the existing attempt")
			}
			rs.log.Warn("ai batch submission failed, continuing synchronously", "err", err)
			return nil, nil
		}
		merged := map[string]string{}
		for k, v := range cached {
			merged[k] = v
		}
		for k, v := range got {
			merged[k] = v
		}
		results = merged
	}
	if rs.claim == nil {
		return results, nil
	}
	if len(results) != len(index) {
		return nil, batchRecovery("batch has unresolved cells; automatic synchronous fallback is disabled")
	}
	for k := range results {
		if _, ok := index[k]; !ok {
			return nil, batchRecovery("batch returned an unexpected request identity")
		}
	}
	if !complete {
		err := rs.batchAttempt(ctx, func(p map[string]any) (bool, error) {
			m, _ := p[batchAttemptKey].(map[string]any)
			if h, _ := m["contract_hash"].(string); m == nil || h != hash || m["state"] != "accepted" {
				return false, batchRecovery("batch result has no matching accepted attempt")
			}
			prev, _ := m["results"].(map[string]any)
			out := map[string]any{}
			for k, v := range prev {
				out[k] = v
			}
			for k, v := range results {
				if old, ok := out[k]; ok && old != v {
					return false, batchRecovery("batch result checkpoint conflicts with committed results")
				}
				out[k] = v
			}
			m["results"] = out
			return true, nil
		})
		if err != nil {
			if a := abortErr(ctx, err); a != nil || errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrBatchRecovery) {
				return nil, err
			}
			return nil, batchRecovery("batch results could not be checkpointed; retry retrieval, not submission")
		}
	}
	return results, nil
}
