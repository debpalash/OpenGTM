package leadpeople

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Contact execution statuses (contact_executions.status).
const (
	ExecRunning   = "running"
	ExecCompleted = "completed"
	ExecFailed    = "failed"
	ExecUncertain = "uncertain"
	ExecConflict  = "conflict"
)

// ErrBadExecutionArgs is returned for a missing workspace or an unbounded action key.
var ErrBadExecutionArgs = errors.New("leadpeople: a workspace and bounded action key are required")

// ContactExecutor is the durable spend boundary for Chat contact enrichment
// (contact_execution.py). A claim row is committed before any provider
// network I/O and results replay across processes. A lost process may have
// billed a provider without recording its response: such claims expire to an
// explicit uncertain outcome, never back to runnable; a new approved action key
// is required after reviewing provider usage. Every statement runs inside
// tenancy.InWorkspace.
type ContactExecutor struct {
	Pool *pgxpool.Pool
	Now  func() time.Time // nil = time.Now
}

func (e *ContactExecutor) epoch() float64 {
	t := time.Now()
	if e.Now != nil {
		t = e.Now()
	}
	return float64(t.UnixNano()) / 1e9
}

func blocked(actionID, status string, retryAfter int64) map[string]any {
	errName, recovery := "contact_action_outcome_uncertain",
		"Provider usage may have been billed. Review usage and saved results before approving a new action key."
	if status == ExecRunning {
		errName = "contact_action_in_progress"
		recovery = "Check this action again after the retry interval; no additional provider calls were started."
	}
	return map[string]any{
		"ok": false, "action_id": actionID, "execution_status": status, "error": errName, "reused": true,
		"automatic_retry_allowed": false, "retry_after_seconds": retryAfter, "recovery": recovery,
	}
}

func replay(result []byte, status string) (map[string]any, error) {
	out := map[string]any{}
	if len(result) > 0 && string(result) != "null" {
		if err := json.Unmarshal(result, &out); err != nil {
			return nil, fmt.Errorf("leadpeople: stored contact result: %w", err)
		}
	}
	out["reused"] = true
	out["execution_status"] = status
	return out, nil
}

// Fingerprint is the contract hash: sha256 of the canonical JSON of the
// contract (sorted keys, compact separators, ASCII escapes), as Python.
func Fingerprint(contract any) (string, error) {
	b, err := canonjson.Marshal(canonical(contract), true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type claim struct {
	claimed bool
	reply   map[string]any
}

// ExecuteOnce atomically claims actionID before running op and replays a
// recorded outcome otherwise. op's result must have an "ok" key: a truthy
// value stores the execution as completed, otherwise as failed. A business
// outcome (replay, conflict, blocked) is a map and a nil error; the error is
// non-nil for bad arguments, an unfingerprintable contract, database failures
// and a cancelled ctx (the claim is then marked uncertain).
func (e *ContactExecutor) ExecuteOnce(ctx context.Context, workspaceID, actionID string, contract any,
	op func(context.Context) (map[string]any, error), timeout time.Duration) (map[string]any, error) {
	if workspaceID == "" || actionID == "" || utf8.RuneCountInString(actionID) > 255 {
		return nil, ErrBadExecutionArgs
	}
	fingerprint, err := Fingerprint(contract)
	if err != nil {
		return nil, fmt.Errorf("leadpeople: contact contract: %w", err)
	}
	now := e.epoch()
	claimID := uuid.NewString()
	var c claim
	err = tenancy.InWorkspace(ctx, e.Pool, workspaceID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO contact_executions
			(id, workspace_id, action_id, contract_hash, status, started_at, expires_at)
			VALUES ($1, $2, $3, $4, 'running', $5, $6)
			ON CONFLICT (workspace_id, action_id) DO NOTHING`,
			claimID, workspaceID, actionID, fingerprint, now, now+timeout.Seconds()+30)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			c.claimed = true
			return nil
		}
		var (
			prevID, hash, status string
			expiresAt            float64
			result               []byte
		)
		if err := tx.QueryRow(ctx, `SELECT id, contract_hash, status, expires_at, result::text
			FROM contact_executions WHERE workspace_id = $1 AND action_id = $2 FOR UPDATE`,
			workspaceID, actionID).Scan(&prevID, &hash, &status, &expiresAt, &result); err != nil {
			return err
		}
		if hash != fingerprint {
			c.reply = map[string]any{"ok": false, "error": "Idempotency key conflicts with a different contact contract",
				"action_id": actionID, "execution_status": ExecConflict}
			return nil
		}
		if status == ExecCompleted || status == ExecFailed {
			c.reply, err = replay(result, status)
			return err
		}
		if status == ExecRunning && expiresAt > now {
			c.reply = blocked(actionID, ExecRunning, max(1, int64(expiresAt-now)))
			return nil
		}
		// Conditional update cannot overwrite a concurrently completed result.
		if _, err := tx.Exec(ctx, `UPDATE contact_executions SET status = 'uncertain'
			WHERE id = $1 AND workspace_id = $2 AND status = 'running' AND expires_at <= $3`,
			prevID, workspaceID, now); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status, result::text FROM contact_executions WHERE id = $1 AND workspace_id = $2`,
			prevID, workspaceID).Scan(&status, &result); err != nil {
			return err
		}
		if status == ExecCompleted || status == ExecFailed {
			c.reply, err = replay(result, status)
			return err
		}
		c.reply = blocked(actionID, ExecUncertain, 0)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !c.claimed {
		return c.reply, nil
	}

	finish := func(status string, result []byte) error {
		// Not tied to ctx: a cancelled caller must still leave the claim resolved.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return tenancy.InWorkspace(fctx, e.Pool, workspaceID, func(tx pgx.Tx) error {
			var res any
			if result != nil {
				res = string(result)
			}
			_, err := tx.Exec(fctx, `UPDATE contact_executions SET status = $1, result = $2::json
				WHERE id = $3 AND workspace_id = $4`, status, res, claimID, workspaceID)
			return err
		})
	}
	uncertain := func() (map[string]any, error) {
		if err := finish(ExecUncertain, nil); err != nil {
			return nil, err
		}
		return blocked(actionID, ExecUncertain, 0), nil
	}

	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		res map[string]any
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := op(octx)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		if ctx.Err() != nil {
			if err := finish(ExecUncertain, nil); err != nil {
				return nil, err
			}
			return nil, ctx.Err()
		}
		if o.err != nil || o.res == nil {
			return uncertain()
		}
		b, err := json.Marshal(o.res)
		if err != nil {
			return uncertain()
		}
		status := ExecFailed
		if providers.Truthy(o.res["ok"]) {
			status = ExecCompleted
		}
		if err := finish(status, b); err != nil {
			return nil, err
		}
		out := cloneMap(o.res)
		out["execution_status"] = status
		return out, nil
	case <-octx.Done():
		if ctx.Err() != nil {
			if err := finish(ExecUncertain, nil); err != nil {
				return nil, err
			}
			return nil, ctx.Err()
		}
		return uncertain()
	}
}
