package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Test seams. nil means the production behaviour.
var (
	jobsCancelQueue  func(ctx context.Context, tx pgx.Tx, ws, id string, now time.Time) (int, error)
	jobsRetryEnqueue func(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (id int64, created bool, err error)
)

const (
	jobsWriteNotFound = "Job not found"
)

var (
	errJobsNotRetryable = errors.New("jobs: only failed or cancelled jobs can be retried")
	errJobsQueue        = errors.New("jobs: collection queue unavailable")
)

type jobsWriteRoutes struct{ d Deps }

// registerJobsWrite mounts POST /api/jobs/{job_id}/cancel, DELETE /api/jobs/{job_id} and POST
// /api/jobs/{job_id}/retry (routers/leads.py jobs_router): require_editor (tables.write; owner, admin, editor).
// FastAPI resolves the role dependency before the query parameter is validated, so a viewer gets 403 even with a
// bad keep_leads, and a bad keep_leads is a 422 even for an unknown job.
func registerJobsWrite(r *httpx.Router, d Deps) {
	h := jobsWriteRoutes{d: d}
	mw := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin")}
	r.Handle("POST /api/jobs/{job_id}/cancel", http.HandlerFunc(h.cancel), mw...)
	r.Handle("DELETE /api/jobs/{job_id}", notImplemented("DELETE /api/jobs/{job_id}"), mw...)
	r.Handle("POST /api/jobs/{job_id}/retry", http.HandlerFunc(h.retry), mw...)
}

type jobsOKBody struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type jobsRetryBody struct {
	OK         bool   `json:"ok"`
	QueueJobID int64  `json:"queue_job_id"`
	Message    string `json:"message"`
}

// collectRetryPayload is the queue payload in Python's dict order (EnqueueOnce appends fire_key).
type collectRetryPayload struct {
	JobID       string `json:"job_id"`
	Query       string `json:"query"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
}

// parseKeepLeads is pydantic's bool parsing of the keep_leads query value; the last occurrence wins, absent is False.
func parseKeepLeads(values []string) (bool, *httpx.FieldError) {
	if len(values) == 0 {
		return false, nil
	}
	v := values[len(values)-1]
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, &httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", "keep_leads"},
		Msg: "Input should be a valid boolean, unable to interpret input", Input: v}
}

// run opens the tenant transaction and maps the outcome: a missing (or foreign) job is the 404, a rollback keeps
// the ledger and the queue consistent. fn answers 200 through the value it returns.
func (h jobsWriteRoutes) run(w http.ResponseWriter, r *http.Request, what string, fn func(ctx context.Context, tx pgx.Tx, ws string) (any, error)) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	var body any
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		body, err = fn(r.Context(), tx, wc.WorkspaceID)
		return err
	})
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, body)
	case errors.Is(err, collectledger.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, jobsWriteNotFound)
	case errors.Is(err, errJobsNotRetryable):
		httpx.Detail(w, http.StatusConflict, "Only failed or cancelled jobs can be retried")
	case errors.Is(err, errJobsQueue):
		httpx.Detail(w, http.StatusServiceUnavailable, "Collection queue unavailable")
	default:
		h.d.logger().Error("jobs: "+what+" failed", "err_type", errTypeName(err))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Internal Server Error"))
	}
}

// getJob is get_job_detail used for its existence (and status/query): nothing stored is parsed.
func getJob(ctx context.Context, tx pgx.Tx, id string) (gen.CJGetRow, error) {
	if strings.IndexByte(id, 0) >= 0 { // no job id holds a NUL
		return gen.CJGetRow{}, collectledger.ErrNotFound
	}
	row, err := gen.New(tx).CJGet(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, collectledger.ErrNotFound
	}
	return row, err
}

func cancelQueue(ctx context.Context, tx pgx.Tx, ws, id string) error {
	f := collectledger.CancelQueueJob
	if jobsCancelQueue != nil {
		f = jobsCancelQueue
	}
	_, err := f(ctx, tx, ws, id, time.Now().UTC())
	return err
}

// cancel: the ledger half (guarded: only running or pending move) and the queue half commit together; a job that
// is already terminal still answers ok (Python reports neither guard).
func (h jobsWriteRoutes) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	h.run(w, r, "cancel", func(ctx context.Context, tx pgx.Tx, ws string) (any, error) {
		if _, err := getJob(ctx, tx, id); err != nil {
			return nil, err
		}
		if _, err := collectledger.Cancel(ctx, tx, id, time.Now().UTC()); err != nil {
			return nil, err
		}
		if err := cancelQueue(ctx, tx, ws, id); err != nil {
			return nil, err
		}
		return jobsOKBody{OK: true, Message: "Job " + id + " cancelled"}, nil
	})
}

// retry: reset a failed or cancelled job and enqueue its collect job in one transaction. A failing enqueue rolls
// the reset back (Python marks the job failed instead, because its two stores cannot share a transaction) and
// answers 503. Two concurrent retries: the loser finds the job no longer failed/cancelled (409), and the fire-key
// lock plus the active-row check keep one queue row either way.
func (h jobsWriteRoutes) retry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	wc, _ := identity.WorkspaceFrom(r.Context())
	h.run(w, r, "retry", func(ctx context.Context, tx pgx.Tx, ws string) (any, error) {
		job, err := getJob(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if job.Status != "failed" && job.Status != "cancelled" {
			return nil, errJobsNotRetryable
		}
		changed, err := collectledger.Retry(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if !changed { // lost the race to another retry
			return nil, errJobsNotRetryable
		}
		enqueue := func(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (int64, bool, error) {
			return queue.EnqueueOnce(ctx, tx, "collect", ws, payload, fireKey, "python")
		}
		if jobsRetryEnqueue != nil {
			enqueue = jobsRetryEnqueue
		}
		fireKey := collectledger.CollectFireKey(ws, id)
		qid, created, err := enqueue(ctx, tx, ws, collectRetryPayload{JobID: id, Query: job.Query, WorkspaceID: ws, Slug: wc.Slug}, fireKey)
		if err != nil {
			h.d.logger().Error("jobs: retry enqueue failed", "err_type", errTypeName(err))
			return nil, errJobsQueue
		}
		if !created { // an active queue row for this job already exists: that is the one that will run
			if qid, err = activeQueueID(ctx, tx, fireKey); err != nil {
				return nil, err
			}
		}
		return jobsRetryBody{OK: true, QueueJobID: qid, Message: "Job " + id + " queued for retry"}, nil
	})
}

func activeQueueID(ctx context.Context, tx pgx.Tx, fireKey string) (int64, error) {
	id, err := gen.New(tx).ActiveJobByFireKey(ctx, fireKey)
	return int64(id), err
}
