package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// The durable-queue handlers: collect (job_runner.handle_collect), its failure hook
// (reconcile_collect_job_failure) and bulk_enrich (handle_bulk_enrich, bulk.go).

const (
	JobTypeCollect    = "collect"
	JobTypeBulkEnrich = "bulk_enrich"
)

// ErrPayload is handle_collect's ValueError (a payload without workspace_id or slug).
var ErrPayload = errors.New("collect payload requires workspace_id and slug")

// Deps are the job handlers' dependencies.
type Deps struct {
	Pool *pgxpool.Pool
	// For returns the Runner of a workspace (its web search cache salt, LLM usage account).
	For func(ws string) *Runner
	// Emit returns the progress emitter of a workspace.
	Emit func(ws string) Emitter
}

type collectPayload struct {
	JobID       string `json:"job_id"`
	Query       string `json:"query"`
	Intent      string `json:"intent"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
}

// claimTenant decodes the payload into dst and returns the job's tenant. The tenant comes ONLY
// from the claimed row (jobs.workspace_id): a row without one, or a payload naming another
// workspace, is refused permanently. *ws is set to the tenant; *slug must be non-empty (legacy
// payload guard).
func claimTenant(c *queue.Claim, dst any, ws, slug *string) (string, error) {
	if err := json.Unmarshal(c.Payload, dst); err != nil {
		return "", &queue.PermanentError{Err: fmt.Errorf("collect: payload: %w", err)}
	}
	if c.WorkspaceID == "" {
		return "", &queue.PermanentError{Err: errors.New("collect: job workspace missing")}
	}
	if *ws != "" && *ws != c.WorkspaceID {
		return "", &queue.PermanentError{Err: errors.New("collect: payload workspace differs from the job's")}
	}
	if *slug == "" {
		return "", &queue.PermanentError{Err: ErrPayload}
	}
	*ws = c.WorkspaceID
	return c.WorkspaceID, nil
}

// HandleCollect is handle_collect: the ledger row must exist (its intent wins), a cancelled job is
// a no-op, the pipeline runs, and any outcome but done / cancelled / intent-blocked is an error
// so the durable queue retries (it is the retry authority; the failure hook mirrors its decision
// into the ledger).
func HandleCollect(ctx context.Context, d Deps, c *queue.Claim) error {
	var p collectPayload
	p.Intent = "market_search"
	ws, err := claimTenant(c, &p, &p.WorkspaceID, &p.Slug)
	if err != nil {
		return err
	}
	led := PgLedger{d.Pool, ws}
	status, _, intent, found, err := led.Detail(ctx, p.JobID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("collection job %s not found in workspace ledger", p.JobID)
	}
	if intent != "" {
		p.Intent = intent
	}
	if status == "cancelled" {
		return nil
	}
	r := d.For(ws)
	err = r.Process(ctx, Job{ID: p.JobID, Query: p.Query, Intent: p.Intent, WorkspaceID: ws,
		Ledger: led, Store: PgStore{d.Pool, ws}, Emit: d.Emit(ws)})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrCancelled) {
		return nil
	}
	st, msg, _, _, rerr := led.Detail(context.WithoutCancel(ctx), p.JobID)
	if rerr != nil {
		if err != nil {
			return err
		}
		return rerr
	}
	if strings.HasPrefix(msg, "collection_intent_blocked:") {
		return nil
	}
	if st == "done" || st == "cancelled" {
		return nil
	}
	if msg != "" {
		return errors.New(msg)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("collection ended in %s", or(st, "unknown"))
}

// RegisterFailureHook registers reconcile_collect_job_failure for collect jobs: after the queue
// persisted a failed attempt (crash, timeout, error) the ledger row follows it (requeued with
// "Queue retry scheduled: ..." or failed with "Final failure: ..."); missing and cancelled jobs
// are left alone.
func RegisterFailureHook(pool *pgxpool.Pool) { queue.RegisterFailureHook(JobTypeCollect, failureHook(pool)) }

// failureHook is the hook; the tenant is the failed row's workspace_id only (a row without one
// is left alone, a payload naming another workspace is an error).
func failureHook(pool *pgxpool.Pool) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		if n.WorkspaceID == "" {
			return nil
		}
		var p collectPayload
		_ = json.Unmarshal(n.Payload, &p)
		if p.WorkspaceID == "" {
			p.WorkspaceID = n.WorkspaceID
		}
		if err := collectledger.CheckFailurePayload(p.JobID, p.WorkspaceID, p.Slug); err != nil {
			return err
		}
		if p.WorkspaceID != n.WorkspaceID {
			return errors.New("collect: failure payload workspace differs from the job's")
		}
		return tenancy.InWorkspace(ctx, pool, n.WorkspaceID, func(tx pgx.Tx) error {
			return collectledger.Reconcile(ctx, tx, p.JobID, n.WillRetry, n.Reason, time.Now().UTC())
		})
	}
}
