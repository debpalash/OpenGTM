package api

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/watchsvc"
)

// registerWatchesPoll registers POST /api/watches/{watch_id}/poll (editor; signals.write) and
// GET /api/watches/{watch_id}/signals (any workspace member). Every route answers 404 `intent poller
// disabled`, then 409 `intent_poller_requires_pg_lead_store`, after the role gate and the query
// validation (FastAPI resolves dependencies and query params before the handler body).
func registerWatchesPoll(r *httpx.Router, d Deps) {
	p := watchPollRoutes{d: d, svc: d.pollWatchService()}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), identity.RequireRole("signals.write", "editor", "admin"))
	r.Handle("POST /api/watches/{watch_id}/poll", http.HandlerFunc(p.poll), write...)
	r.Handle("GET /api/watches/{watch_id}/signals", http.HandlerFunc(p.signals), read...)
}

// pollWatchService builds the watch service the poll and signals routes use. Poll-now enqueues
// with the cohort executor of the watch_poll job (go_executor.watch_poll_executor).
func (d Deps) pollWatchService() *watchsvc.Service {
	a := d.Flags.Automations
	c := config.ExecutorCohortFrom(d.getenv())
	return &watchsvc.Service{
		Pool: d.Pool,
		Cfg: watchsvc.Config{DefaultInterval: a.PollerDefaultInterval, MaxWatchesPerWS: a.PollerMaxWatchesPerWS,
			JobChangeMaxContacts: a.PollerJobChangeMaxContacts, PollNowMinIntervalSec: a.PollerPollNowMinIntervalSec,
			PollNowDailyQuota: a.PollerPollNowDailyQuota, AutomationsEnabled: d.Flags.AutomationsEnabled,
			WebhookAllowlist: a.WebhookDomainAllowlist},
		ExecFor: automationhook.CohortExecutor(c.Enabled, a.GoExecutorWatchPoll, c.Workspaces),
		Log:     d.logger(),
	}
}

// watchCursorMax is the `cursor` query max_length of the signals feed.
const watchCursorMax = 1024

type watchPollRoutes struct {
	d   Deps
	svc *watchsvc.Service
}

// gate is _require_enabled: false after answering when the poller or the PG lead store is off.
func (p watchPollRoutes) gate(w http.ResponseWriter) bool {
	if he := watchsvc.RequireEnabled(p.d.Flags.IntentPollerEnabled, p.d.Flags.PGLeadStore); he != nil {
		httpx.Detail(w, he.Status, he.Detail.(string))
		return false
	}
	return true
}

func (p watchPollRoutes) fail(w http.ResponseWriter, what string, err error) {
	var he *autosvc.HTTPError
	if errors.As(err, &he) {
		if s, ok := he.Detail.(string); ok {
			httpx.Detail(w, he.Status, s)
		} else {
			httpx.WriteJSON(w, he.Status, map[string]any{"detail": he.Detail})
		}
		return
	}
	p.d.logger().Error("watches: "+what+" failed", "err", err)
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (p watchPollRoutes) poll(w http.ResponseWriter, r *http.Request) {
	if !p.gate(w) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	res, err := p.svc.PollNow(r.Context(), wc.WorkspaceID, r.PathValue("watch_id"))
	if err != nil {
		p.fail(w, "poll-now", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (p watchPollRoutes) signals(w http.ResponseWriter, r *http.Request) {
	limit, ok := queryInt(w, r, "limit", 50, intp(1), intp(200))
	if !ok {
		return
	}
	offset, ok := queryInt(w, r, "offset", 0, intp(0), nil)
	if !ok {
		return
	}
	q := r.URL.Query()
	if c := q.Get("cursor"); utf8.RuneCountInString(c) > watchCursorMax {
		httpx.ValidationError(w, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", "cursor"}, Input: c,
			Msg: "String should have at most 1024 characters", Ctx: map[string]any{"max_length": watchCursorMax}})
		return
	}
	if !p.gate(w) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	page, err := p.svc.Signals(r.Context(), wc.WorkspaceID, r.PathValue("watch_id"), watchsvc.SignalsQuery{
		SignalType: q.Get("signal_type"), Limit: limit, Offset: offset, Cursor: q.Get("cursor")})
	if err != nil {
		p.fail(w, "signals", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}
