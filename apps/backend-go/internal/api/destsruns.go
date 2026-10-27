package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Destination sync, run history, health, delivery ledger, retry and cancel (routers/destinations.py start_sync,
// list_runs, destination_health, list_deliveries, retry_run, cancel_run). Like FastAPI the order is authentication,
// workspace, role (403, writes only), query validation (422), then the lookup (404) and the state check (409).

const (
	drDestNotFound = "Destination not found"
	drRunNotFound  = "Destination run not found"
	drCursorMax    = 1024
)

// drDeliveryStatuses is the delivery status vocabulary list_deliveries accepts.
var drDeliveryStatuses = map[string]bool{"pending": true, "in_flight": true, "success": true, "failed": true, "cancelled": true}

// registerDestsRuns registers the six run routes. The static /runs/... paths and the /{destination_id}/... ones never
// match the same request (different segment counts or literals), and Go's mux prefers the literal pattern anyway.
func registerDestsRuns(r *httpx.Router, d Deps) {
	h := destRuns{audCrud: audCrud{d: d}}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), identity.RequireRole("activation.write", "editor", "admin"))
	r.Handle("GET "+drPrefix+"/runs/{run_id}/deliveries", http.HandlerFunc(h.deliveries), read...)
	r.Handle("POST "+drPrefix+"/runs/{run_id}/retry", http.HandlerFunc(h.retry), write...)
	r.Handle("POST "+drPrefix+"/runs/{run_id}/cancel", http.HandlerFunc(h.cancel), write...)
	r.Handle("POST "+drPrefix+"/{destination_id}/sync", http.HandlerFunc(h.sync), write...)
	r.Handle("GET "+drPrefix+"/{destination_id}/runs", http.HandlerFunc(h.runs), read...)
	r.Handle("GET "+drPrefix+"/{destination_id}/health", http.HandlerFunc(h.health), read...)
}

const drPrefix = "/api/audience-destinations"

type destRuns struct{ audCrud }

// drTime is a naive UTC column as FastAPI renders a datetime: microseconds only when non-zero.
func drTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return &s
}

// drRun is DestinationRun.to_api(); the field order is the key order.
type drRun struct {
	ID            string  `json:"id"`
	DestinationID string  `json:"destination_id"`
	Status        string  `json:"status"`
	Attempted     int     `json:"attempted"`
	Succeeded     int     `json:"succeeded"`
	Failed        int     `json:"failed"`
	Skipped       int     `json:"skipped"`
	Error         *string `json:"error"`
	StartedAt     *string `json:"started_at"`
	FinishedAt    *string `json:"finished_at"`
	CreatedAt     string  `json:"created_at"`
}

func drRunOf(r deststore.Run) drRun {
	return drRun{ID: r.ID, DestinationID: r.DestinationID, Status: r.Status, Attempted: r.Attempted, Succeeded: r.Succeeded,
		Failed: r.Failed, Skipped: r.Skipped, Error: r.Error, StartedAt: drTime(r.StartedAt), FinishedAt: drTime(r.FinishedAt),
		CreatedAt: *drTime(&r.CreatedAt)}
}

// drDelivery is DestinationDelivery.to_api() (the payload and idempotency key are not exposed).
type drDelivery struct {
	ID            int     `json:"id"`
	RunID         string  `json:"run_id"`
	DestinationID string  `json:"destination_id"`
	LeadID        int     `json:"lead_id"`
	Operation     string  `json:"operation"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	ExternalID    *string `json:"external_id"`
	Summary       *string `json:"summary"`
	Error         *string `json:"error"`
	DeliveredAt   *string `json:"delivered_at"`
	CreatedAt     string  `json:"created_at"`
}

func drDeliveryOf(d deststore.Delivery) drDelivery {
	return drDelivery{ID: d.ID, RunID: d.RunID, DestinationID: d.DestinationID, LeadID: d.LeadID, Operation: d.Operation, Status: d.Status,
		Attempts: d.Attempts, ExternalID: d.ExternalID, Summary: d.Summary, Error: d.Error, DeliveredAt: drTime(d.DeliveredAt),
		CreatedAt: *drTime(&d.CreatedAt)}
}

// drFloat renders a Python float: shortest repr, with ".0" on an integral value.
type drFloat float64

func (f drFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(f), 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return []byte(s), nil
}

// drHealth is the destination_health body; the field order is the key order (fixtures sort keys, Python does not).
type drHealth struct {
	DestinationID string         `json:"destination_id"`
	HealthStatus  string         `json:"health_status"`
	LastError     *string        `json:"last_error"`
	LastSuccessAt *string        `json:"last_success_at"`
	RunCounts     map[string]int `json:"run_counts"`
	Deliveries    struct {
		Total     int `json:"total"`
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
	} `json:"delivery_counts"`
	SuccessRate              *drFloat     `json:"success_rate"`
	ConsecutiveUnhealthyRuns int          `json:"consecutive_unhealthy_runs"`
	RecentFailures           []drDelivery `json:"recent_failures"`
	LatestRun                *drRun       `json:"latest_run"`
}

// pathID is a path id. Ids are opaque strings; one Postgres cannot store (NUL, invalid UTF-8) matches nothing.
func (h destRuns) pathID(w http.ResponseWriter, r *http.Request, name, notFound string) (string, bool) {
	id := r.PathValue(name)
	if strings.ContainsRune(id, 0) || !utf8.ValidString(id) {
		httpx.Detail(w, http.StatusNotFound, notFound)
		return "", false
	}
	return id, true
}

// fail maps the store sentinels to Python's statuses and details; anything else is a 500 that never echoes the error.
func (h destRuns) fail(w http.ResponseWriter, what string, err error) {
	for _, m := range []struct {
		err    error
		status int
		detail string
	}{
		{deststore.ErrNotFound, http.StatusNotFound, drDestNotFound},
		{deststore.ErrRunNotFound, http.StatusNotFound, drRunNotFound},
		{deststore.ErrDisabled, http.StatusConflict, "Destination is disabled"},
		{deststore.ErrNotRetryable, http.StatusConflict, "Only failed or completed-with-errors runs can be retried"},
		{deststore.ErrNotCancellable, http.StatusConflict, "Only pending or running destination runs can be cancelled"},
	} {
		if errors.Is(err, m.err) {
			httpx.Detail(w, m.status, m.detail)
			return
		}
	}
	h.internal(w, what, err)
}

func (h destRuns) sync(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "destination_id", drDestNotFound)
	if !ok {
		return
	}
	wc := h.ws(r)
	executorFor := goExecutor
	var run deststore.Run
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
		dest, err := deststore.GetDestination(r.Context(), tx, id)
		if err != nil {
			return err
		}
		// a destination's type never changes, so the executor read here is the one the job is enqueued with
		run, _, err = deststore.StartRun(r.Context(), tx, wc.WorkspaceID, id, strconv.Itoa(int(wc.User.ID)), executorFor(dest.Type))
		return err
	})
	if err != nil {
		h.fail(w, "sync", err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, drRunOf(run))
}

func (h destRuns) runs(w http.ResponseWriter, r *http.Request) {
	limit, offset, cursor, errs := pageQuery(r.URL.Query(), 50, 200, drCursorMax)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	id, ok := h.pathID(w, r, "destination_id", drDestNotFound)
	if !ok {
		return
	}
	wc := h.ws(r)
	q := deststore.RunsQuery{Limit: limit, Offset: offset}
	var page deststore.RunsPage
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
		// the destination is looked up before the cursor is decoded: an unknown destination is a 404 even with a bad cursor
		if _, err := deststore.GetDestination(r.Context(), tx, id); err != nil {
			return err
		}
		if cursor != "" {
			at, rid, err := destvalidate.DecodeHistoryCursor(cursor, "destination run")
			if err != nil {
				return drBadCursor{err}
			}
			q.Cursor = &deststore.RunsCursor{CreatedAt: at, ID: rid}
		}
		var err error
		page, err = deststore.ListRuns(r.Context(), tx, id, q)
		return err
	})
	var bc drBadCursor
	if errors.As(err, &bc) {
		httpx.Detail(w, http.StatusUnprocessableEntity, bc.Error())
		return
	}
	if err != nil {
		h.fail(w, "list runs", err)
		return
	}
	out := struct {
		Runs       []drRun `json:"runs"`
		Limit      int     `json:"limit"`
		Offset     *int    `json:"offset"`
		HasMore    bool    `json:"has_more"`
		NextCursor *string `json:"next_cursor"`
	}{Runs: make([]drRun, 0, len(page.Runs)), Limit: limit, HasMore: page.HasMore}
	for _, run := range page.Runs {
		out.Runs = append(out.Runs, drRunOf(run))
	}
	if cursor == "" {
		out.Offset = &offset
	}
	if page.HasMore && len(page.Runs) > 0 {
		last := page.Runs[len(page.Runs)-1]
		c := destvalidate.EncodeHistoryCursor(last.CreatedAt, last.ID)
		out.NextCursor = &c
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// drBadCursor carries the 422 detail out of the transaction.
type drBadCursor struct{ err error }

func (e drBadCursor) Error() string { return e.err.Error() }

func (h destRuns) health(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "destination_id", drDestNotFound)
	if !ok {
		return
	}
	wc := h.ws(r)
	var rep deststore.HealthReport
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		rep, err = deststore.Health(r.Context(), tx, id)
		return err
	})
	if err != nil {
		h.fail(w, "health", err)
		return
	}
	out := drHealth{DestinationID: rep.DestinationID, HealthStatus: rep.HealthStatus, LastError: rep.LastError,
		LastSuccessAt: drTime(rep.LastSuccessAt), RunCounts: rep.RunCounts, ConsecutiveUnhealthyRuns: rep.ConsecutiveUnhealthyRuns,
		RecentFailures: make([]drDelivery, 0, len(rep.RecentFailures))}
	out.Deliveries.Total, out.Deliveries.Succeeded, out.Deliveries.Failed = rep.Deliveries.Total, rep.Deliveries.Succeeded, rep.Deliveries.Failed
	if rep.SuccessRate != nil {
		f := drFloat(*rep.SuccessRate)
		out.SuccessRate = &f
	}
	for _, d := range rep.RecentFailures {
		out.RecentFailures = append(out.RecentFailures, drDeliveryOf(d))
	}
	if rep.LatestRun != nil {
		lr := drRunOf(*rep.LatestRun)
		out.LatestRun = &lr
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// drDeliveryQuery validates after_id (int, optional) and limit (int, default 100) types; the status vocabulary and the
// limit range are handler checks in Python (422 with a string detail, after the run lookup).
func drDeliveryQuery(q url.Values) (status string, hasStatus bool, afterID *int, limit int, errs []httpx.FieldError) {
	if v, ok := q["status"]; ok && len(v) > 0 {
		status, hasStatus = v[len(v)-1], true
	}
	if q.Has("after_id") {
		n, e := wbcrud.QueryInt(q, "after_id", nil, nil, nil)
		if e != nil {
			errs = append(errs, *e)
		} else {
			a := int(n)
			afterID = &a
		}
	}
	def := int64(100)
	n, e := wbcrud.QueryInt(q, "limit", &def, nil, nil)
	if e != nil {
		errs = append(errs, *e)
	}
	return status, hasStatus, afterID, int(n), errs
}

func (h destRuns) deliveries(w http.ResponseWriter, r *http.Request) {
	status, hasStatus, afterID, limit, errs := drDeliveryQuery(r.URL.Query())
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	id, ok := h.pathID(w, r, "run_id", drRunNotFound)
	if !ok {
		return
	}
	wc := h.ws(r)
	var rows []deststore.Delivery
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
		// the run lookup (404) precedes the status (422) and limit (422) checks
		if _, err := deststore.GetRun(r.Context(), tx, id); err != nil {
			return err
		}
		if hasStatus && !drDeliveryStatuses[status] {
			return drBadQuery("Invalid delivery status")
		}
		if limit < 1 || limit > 500 {
			return drBadQuery("limit must be between 1 and 500")
		}
		var err error
		// an empty status filters nothing, as `if status:` does in Python
		rows, err = deststore.ListDeliveries(r.Context(), tx, id, status, afterID, limit)
		return err
	})
	var bq drBadQuery
	if errors.As(err, &bq) {
		httpx.Detail(w, http.StatusUnprocessableEntity, string(bq))
		return
	}
	if err != nil {
		h.fail(w, "deliveries", err)
		return
	}
	out := make([]drDelivery, 0, len(rows))
	for _, d := range rows {
		out = append(out, drDeliveryOf(d))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// drBadQuery is a handler-level 422 (string detail) carried out of the transaction.
type drBadQuery string

func (e drBadQuery) Error() string { return string(e) }

// retry sends a failed or completed_with_errors run back to pending and enqueues its job again on the same fire key; the
// run's counters are kept so deliveries that already succeeded skip on their idempotency keys.
func (h destRuns) retry(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "run_id", drRunNotFound)
	if !ok {
		return
	}
	wc := h.ws(r)
	executorFor := goExecutor
	var run deststore.Run
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		run, err = deststore.RetryRun(r.Context(), tx, id, executorFor)
		return err
	})
	if err != nil {
		h.fail(w, "retry", err)
		return
	}
	audit.SetMetadata(r.Context(), map[string]any{"action": "audience_destination.run.retry", "destination_id": run.DestinationID,
		"run_id": run.ID, "previous_failed": run.Failed})
	httpx.WriteJSON(w, http.StatusAccepted, drRunOf(run))
}

// cancel finishes a pending run at once and asks a running one to stop at the next delivery boundary. The state change is
// conditional on the status read under the row lock, so a run the engine just finished is refused (409), and the engine's
// own transitions are conditional in turn, so it observes the cancellation instead of overwriting it.
func (h destRuns) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "run_id", drRunNotFound)
	if !ok {
		return
	}
	wc := h.ws(r)
	var run deststore.Run
	var previous string
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		run, previous, err = deststore.CancelRun(r.Context(), tx, id)
		return err
	})
	if err != nil {
		h.fail(w, "cancel", err)
		return
	}
	audit.SetMetadata(r.Context(), map[string]any{"action": "audience_destination.run.cancel", "destination_id": run.DestinationID,
		"run_id": run.ID, "previous_status": previous})
	httpx.WriteJSON(w, http.StatusAccepted, drRunOf(run))
}
