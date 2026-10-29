package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type playbookRoutes struct{ d Deps }

// registerPlaybooksRoutes registers the playbook CRUD routes (Task 16). Listing needs workspace membership
// (Python's current_workspace); create, patch and delete need agents.write (editor, admin).
func registerPlaybooksRoutes(r *httpx.Router, d Deps) {
	h := playbookRoutes{d: d}
	// PATCH enqueues the next research_playbook_schedule occurrence: its executor follows the same cohort rule
	// as run starts, read per request (the worker wires the same rule through playbooks.WireExecutors).
	playbooks.ScheduleExecutor = goExecutor
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), govadmin.Gate("agents.write"))
	r.Handle("GET /api/research-playbooks", http.HandlerFunc(h.list), read...)
	r.Handle("POST /api/research-playbooks", http.HandlerFunc(h.create), write...)
	r.Handle("PATCH /api/research-playbooks/{playbook_id}", http.HandlerFunc(h.patch), write...)
	r.Handle("DELETE /api/research-playbooks/{playbook_id}", http.HandlerFunc(h.remove), write...)
}

// fail never logs request data; only the error type.
func (h playbookRoutes) fail(w http.ResponseWriter, op string, err error) {
	h.d.logger().Error("playbooks: request failed", "op", op, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// storeError maps the store's sentinels to Python's statuses and details; false when err is not one.
func (h playbookRoutes) storeError(w http.ResponseWriter, err error) bool {
	for _, m := range []struct {
		err    error
		status int
		detail string
	}{
		{playbooks.ErrNotFound, http.StatusNotFound, "Playbook not found"},
		{playbooks.ErrAudienceNotFound, http.StatusNotFound, "Scheduled audience not found"},
		{playbooks.ErrNameTaken, http.StatusConflict, "A playbook with this name already exists"},
		{playbooks.ErrActiveRun, http.StatusConflict, "Cannot delete a playbook with an active run"},
		{playbooks.ErrBadOutputFormat, http.StatusUnprocessableEntity, "output_format must be text or json"},
	} {
		if errors.Is(err, m.err) {
			httpx.Detail(w, m.status, m.detail)
			return true
		}
	}
	return false
}

func (h playbookRoutes) ctx(w http.ResponseWriter, r *http.Request) (identity.WorkspaceCtx, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
	return c, ok
}

func (h playbookRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, ok := wbcrud.ReadBody(r.Body)
	if !ok {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
	}
	return raw, ok
}

func (h playbookRoutes) list(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var rows []playbooks.Playbook
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		rows, err = playbooks.List(r.Context(), tx, c.WorkspaceID)
		return err
	}); err != nil {
		h.fail(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
}

func (h playbookRoutes) create(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, errs := playbooks.ParseCreate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var out playbooks.Playbook
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		out, err = playbooks.Create(r.Context(), tx, c.WorkspaceID, in)
		return err
	})
	if err != nil {
		if !h.storeError(w, err) {
			h.fail(w, "create", err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// patch applies the body and recomputes the schedule (mirror, next_run_at, pending job) in one transaction.
func (h playbookRoutes) patch(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, errs := playbooks.ParsePatch(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var out playbooks.Playbook
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		out, err = playbooks.Patch(r.Context(), tx, c.WorkspaceID, r.PathValue("playbook_id"), in)
		return err
	})
	if err != nil {
		if !h.storeError(w, err) {
			h.fail(w, "patch", err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// remove deletes the playbook with its schedule mirror and pending occurrence (playbooks.Delete does all three).
func (h playbookRoutes) remove(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		return playbooks.Delete(r.Context(), tx, c.WorkspaceID, r.PathValue("playbook_id"))
	})
	if err != nil {
		if !h.storeError(w, err) {
			h.fail(w, "delete", err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json") // FastAPI keeps it on a 204
	w.WriteHeader(http.StatusNoContent)
}
