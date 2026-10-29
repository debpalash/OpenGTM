package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	jobNotFound = "Job not found"
	// jobLeadsLimit is store.get_leads(limit=10_000) in get_job_leads.
	jobLeadsLimit = 10000
	// jobEventsHistory is progress.recent(500): the most events the route can ever see.
	jobEventsHistory = 500
	jobEventsDefault = 100
)

type jobsReadRoutes struct{ d Deps }

// registerJobsRead mounts GET /api/jobs, /api/jobs/{job_id} and its /stages, /leads and /events (routers/leads.py
// jobs_router). Every route sits behind current_workspace only: any member, viewers included, may read. A job of
// another workspace is "Job not found" (the ledger and leads are tenant-scoped by RLS).
func registerJobsRead(r *httpx.Router, d Deps) {
	h := jobsReadRoutes{d: d}
	mw := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	r.Handle("GET /api/jobs", http.HandlerFunc(h.list), mw...)
	r.Handle("GET /api/jobs/{job_id}", http.HandlerFunc(h.detail), mw...)
	r.Handle("GET /api/jobs/{job_id}/stages", http.HandlerFunc(h.stages), mw...)
	r.Handle("GET /api/jobs/{job_id}/leads", http.HandlerFunc(h.leads), mw...)
	r.Handle("GET /api/jobs/{job_id}/events", http.HandlerFunc(h.events), mw...)
}

// serverError is Starlette's bare 500 for an unhandled exception (also what Python answers when a stored
// intent_details/details holds NaN/Infinity: json.loads accepts them, the JSON response render refuses).
func serverError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte("Internal Server Error"))
}

func (h jobsReadRoutes) inTx(w http.ResponseWriter, r *http.Request, what string, fn func(pgx.Tx) error) (ok bool) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, fn)
	switch {
	case err == nil:
		return true
	case errors.Is(err, collectledger.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, jobNotFound)
	case errors.Is(err, collectledger.ErrNotRenderable):
		serverError(w)
	default:
		h.d.logger().Error("jobs: "+what+" failed", "err_type", errTypeName(err))
		serverError(w)
	}
	return false
}

// jobExists is get_job_detail(...) used as an existence check only: nothing stored is parsed, so a job whose
// JSON columns hold NaN still answers its /stages, /leads and /events routes.
func jobExists(r *http.Request, tx pgx.Tx, id string) error {
	if _, err := gen.New(tx).CJGet(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		return collectledger.ErrNotFound
	} else {
		return err
	}
}

func (h jobsReadRoutes) list(w http.ResponseWriter, r *http.Request) {
	raw, _ := lastQuery(r.URL.Query(), "status") // last value wins like FastAPI; "" and __all__ mean no filter
	status := cleanFilter(raw)
	var jobs []collectledger.Job
	if !h.inTx(w, r, "list", func(tx pgx.Tx) (err error) {
		jobs, err = collectledger.List(r.Context(), tx, status, 0)
		return err
	}) {
		return
	}
	writePy(w, jobs)
}

func (h jobsReadRoutes) detail(w http.ResponseWriter, r *http.Request) {
	var job *collectledger.JobDetail
	if !h.inTx(w, r, "detail", func(tx pgx.Tx) (err error) {
		job, err = collectledger.Detail(r.Context(), tx, r.PathValue("job_id"))
		return err
	}) {
		return
	}
	writePy(w, job)
}

func (h jobsReadRoutes) stages(w http.ResponseWriter, r *http.Request) {
	var stages []collectledger.Stage
	if !h.inTx(w, r, "stages", func(tx pgx.Tx) (err error) {
		stages, err = collectledger.Stages(r.Context(), tx, r.PathValue("job_id"))
		return err
	}) {
		return
	}
	writePy(w, stages)
}

func (h jobsReadRoutes) leads(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	var leads []leadsread.Lead
	if !h.inTx(w, r, "leads", func(tx pgx.Tx) error {
		if err := jobExists(r, tx, id); err != nil {
			return err
		}
		var err error
		leads, err = leadsread.CollectionJobLeads(r.Context(), tx, id, jobLeadsLimit)
		return err
	}) {
		return
	}
	writePy(w, leads)
}

// eventsLimit reads `limit` (default 100). Python slices events[-limit:], which for limit <= 0 returns all events
// ([-0:]) or drops the first ([1:], limit=-1): a Python bug. Go requires limit >= 1 (422 otherwise). Values above
// the history size return every event, whatever their magnitude.
func eventsLimit(v *queryValidator) int {
	raw, present := v.last("limit")
	if !present {
		return jobEventsDefault
	}
	if n, ok := wbcrud.ParseIntBig(raw); ok && n.Sign() > 0 && !n.IsInt64() {
		return jobEventsHistory
	}
	ge := int64(1)
	n := v.integer("limit", jobEventsDefault, &ge, nil)
	if n > jobEventsHistory {
		return jobEventsHistory
	}
	return int(n)
}

func (h jobsReadRoutes) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	v := &queryValidator{q: r.URL.Query()}
	limit := eventsLimit(v)
	if v.fail(w) {
		return
	}
	if !h.inTx(w, r, "events", func(tx pgx.Tx) error { return jobExists(r, tx, id) }) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	recent := progressBus(h.d).RecentEvents(r.Context(), wc.WorkspaceID, jobEventsHistory) // Redis trouble falls back to the local deque, never an error
	events := make([][]byte, 0, len(recent))
	for _, e := range recent {
		if jid, _ := e.Event["job_id"].(string); jid != id || !eventTruthy(e.Event["message"]) {
			continue
		}
		// The stored text, compacted Python-style: keys keep the stored (Python insertion) order.
		enc, ok := compactStoredEvent(e.Raw)
		if !ok {
			enc, _ = json.Marshal(e.Event)
		}
		events = append(events, enc)
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	body := append([]byte(`{"events":[`), bytes.Join(events, []byte(","))...)
	body = append(body, ']', '}')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// eventTruthy is Python's bool(x) for a decoded JSON value (json.Number from Redis, native types from the
// in-process fallback deque).
func eventTruthy(x any) bool {
	if x == nil {
		return false
	}
	switch t := x.(type) {
	case bool:
		return t
	case string:
		return t != ""
	}
	if n, ok := x.(json.Number); ok {
		f, _ := new(big.Float).SetString(n.String())
		return f == nil || f.Sign() != 0
	}
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array, reflect.String:
		return rv.Len() > 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() != 0
	}
	return true
}
