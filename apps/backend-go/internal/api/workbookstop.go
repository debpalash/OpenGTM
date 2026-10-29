package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// stop is stop_workbook: the workbook becomes `paused` and its pending or
// processing run_workbook jobs are cancelled, in one transaction. Cancelled is
// final: the worker's fenced updates never overwrite it.
func (h estimateRoutes) stop(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, id := wc.WorkspaceID, r.PathValue("workbook_id")
	err := h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		if _, err := h.svc.Owned(r.Context(), tx, ws, id, true); err != nil {
			return err
		}
		q := gen.New(tx)
		if _, err := q.SetWorkbookPaused(r.Context(), gen.SetWorkbookPausedParams{WorkbookID: id, WorkspaceID: ws}); err != nil {
			return err
		}
		_, err := q.StopCancelRunJobs(r.Context(), gen.StopCancelRunJobsParams{WorkspaceID: ws, WorkbookID: id})
		return err
	})
	if err != nil {
		h.fail(w, "stop", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{"paused"})
}
