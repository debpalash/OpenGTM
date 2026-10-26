package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// tuneMigrateService lets tests adjust the migrate service (page size, row cap, lead reader).
var tuneMigrateService = func(*wbcrud.Service) {}

// registerWorkbookMigrate registers POST /api/workbooks/{id}/migrate
// (workbooks.py migrate_workbook_to_v2, require_editor): snapshot a v1 workbook's
// leads into workbook_rows in one transaction. Bounded per request: at most
// wbcrud.DefaultMaxMigrateRows leads and wbcrud.MaxMigrateColumns columns (413).
func registerWorkbookMigrate(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil || !d.workbooksEnabled("workbook migrate") {
		return
	}
	svc := &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}, Now: time.Now}
	tuneMigrateService(svc)
	h := migrateRoutes{d: d, svc: svc}
	r.Handle("POST /api/workbooks/{workbook_id}/migrate", http.HandlerFunc(h.migrate),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"))
}

type migrateRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

func (h migrateRoutes) migrate(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	var res wbcrud.MigrateResult
	err := h.svc.WithTx(r.Context(), wc.WorkspaceID, func(tx pgx.Tx) error {
		wb, err := h.svc.Owned(r.Context(), tx, wc.WorkspaceID, r.PathValue("workbook_id"), true)
		if err != nil {
			return err
		}
		res, err = h.svc.Migrate(r.Context(), tx, wc.WorkspaceID, wb)
		return err
	})
	var g *wbcrud.GuardError
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, res)
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	case errors.As(err, &g):
		httpx.Detail(w, g.Status, g.Detail)
	default:
		l := h.d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("workbook migrate failed", "err_type", fmt.Sprintf("%T", err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}
