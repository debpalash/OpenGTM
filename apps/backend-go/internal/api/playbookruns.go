package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govaudit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// registerPlaybookRuns registers the playbook run, results, retry and cancel routes (Task 17). Reads need
// workspace membership (current_workspace); start, retry and cancel need agents.write (require_editor).
// The static /runs/{run_id}/... paths and /{playbook_id}/runs differ in segment count, so neither shadows the other.
func registerPlaybookRuns(r *httpx.Router, d Deps) {
	h := playbookRoutes{d: d}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), govadmin.Gate("agents.write"))
	r.Handle("POST /api/research-playbooks/{playbook_id}/runs", http.HandlerFunc(h.startRun), write...)
	r.Handle("GET /api/research-playbooks/{playbook_id}/runs", http.HandlerFunc(h.listRuns), read...)
	r.Handle("GET /api/research-playbooks/runs/{run_id}/results", http.HandlerFunc(h.listResults), read...)
}

// runError maps the run sentinels (and a bad cursor) to Python's statuses and details; false when err is not one.
func (h playbookRoutes) runError(w http.ResponseWriter, err error) bool {
	var ce *govaudit.CursorError
	if errors.As(err, &ce) {
		httpx.Detail(w, ce.Status(), ce.Detail)
		return true
	}
	for _, m := range []struct {
		err    error
		status int
		detail string
	}{
		{playbooks.ErrNotFound, http.StatusNotFound, "Playbook not found"},
		{playbooks.ErrRunNotFound, http.StatusNotFound, "Playbook run not found"},
		{playbooks.ErrAudienceNotFound, http.StatusNotFound, "Audience not found"},
		{playbooks.ErrPlaybookDisabled, http.StatusConflict, "Playbook is disabled"},
	} {
		if errors.Is(err, m.err) {
			httpx.Detail(w, m.status, m.detail)
			return true
		}
	}
	return false
}

// runExecutor is go_executor.executor_for("research_playbook_run", ws): the cohort rule, read per request.
func (h playbookRoutes) runExecutor(ws string) string {
	return playbooks.ExecutorFor(config.ExecutorCohortFrom(h.d.getenv()), h.d.Flags.Governance, ws)
}

// startRun snapshots the playbook into a pending run and enqueues its job in the same transaction.
func (h playbookRoutes) startRun(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, errs := playbooks.ParseRunCreate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var run playbooks.Run
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		pb, err := playbooks.Get(r.Context(), tx, c.WorkspaceID, r.PathValue("playbook_id"))
		if err != nil {
			return err
		}
		if run, err = playbooks.StartRun(r.Context(), tx, c.WorkspaceID, pb, in.AudienceID, in.MaxMembers, strconv.Itoa(int(c.User.ID))); err != nil {
			return err
		}
		return playbooks.EnqueueRun(r.Context(), tx, c.WorkspaceID, run.ID, h.runExecutor(c.WorkspaceID))
	})
	if err != nil {
		if !h.runError(w, err) {
			h.fail(w, "start run", err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, run)
}

// pageQuery validates limit, offset and cursor in declaration order; every error is reported.
func pageQuery(q url.Values, defLimit, maxLimit int64, maxCursor int) (limit, offset int, cursor string, errs []httpx.FieldError) {
	one, zero := int64(1), int64(0)
	hi, offMax := maxLimit, int64(100000)
	def, defOff := defLimit, int64(0)
	l, e := wbcrud.QueryInt(q, "limit", &def, &one, &hi)
	if e != nil {
		errs = append(errs, *e)
	}
	o, e := wbcrud.QueryInt(q, "offset", &defOff, &zero, &offMax)
	if e != nil {
		errs = append(errs, *e)
	}
	if v, ok := q["cursor"]; ok && len(v) > 0 {
		cursor = v[len(v)-1]
		if utf8.RuneCountInString(cursor) > maxCursor {
			errs = append(errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", "cursor"}, Input: cursor,
				Msg: "String should have at most " + strconv.Itoa(maxCursor) + " characters", Ctx: map[string]any{"max_length": maxCursor}})
		}
	}
	if len(errs) > 0 {
		return 0, 0, "", errs
	}
	return int(l), int(o), cursor, nil
}

func (h playbookRoutes) listRuns(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	limit, offset, cursor, errs := pageQuery(r.URL.Query(), 50, 200, govaudit.MaxCursorLen)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var page playbooks.RunPage
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		page, err = playbooks.ListRuns(r.Context(), tx, c.WorkspaceID, r.PathValue("playbook_id"), limit, offset, cursor)
		return err
	})
	if err != nil {
		if !h.runError(w, err) {
			h.fail(w, "list runs", err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (h playbookRoutes) listResults(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	limit, offset, cursor, errs := pageQuery(r.URL.Query(), 100, 500, 32)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var page playbooks.ResultPage
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		page, err = playbooks.ListResults(r.Context(), tx, c.WorkspaceID, r.PathValue("run_id"), limit, offset, cursor)
		return err
	})
	if err != nil {
		if !h.runError(w, err) {
			h.fail(w, "list results", err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}
