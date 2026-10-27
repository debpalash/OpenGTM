package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcewb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	// peopleSearchEnv is settings.PEOPLE_SEARCH_SOURCE_ENABLED (default off).
	peopleSearchEnv = "PEOPLE_SEARCH_SOURCE_ENABLED"
	// peopleSearchDisabledDetail is Python's 403 detail, verbatim.
	peopleSearchDisabledDetail = "people_search source is disabled (PEOPLE_SEARCH_SOURCE_ENABLED)"
	// maxSourceBody bounds the add-source request body (hardening; Python reads it all).
	maxSourceBody = 1 << 20
)

// registerWorkbookSources registers POST .../sources and POST .../sources/{col_id}/run
// (routers/workbooks.py add_source_column and run_source_column), both require_editor,
// and, with GO_SOURCE_PREVIEW_ENABLED, GET .../sources/{col_id}/preview
// (preview_source_column, any workspace member). The run route enqueues a source_workbook
// job; its executor is 'go' only for the GO_EXECUTOR_SOURCE_WORKBOOK cohort (plan 15).
func registerWorkbookSources(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := sourceRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")
	r.Handle("POST /api/workbooks/{workbook_id}/sources", http.HandlerFunc(h.add), authn, workspace, editor)
	run := []httpx.Middleware{authn, workspace, editor}
	if d.Limiter != nil {
		run = append(run, d.Limiter.Limit("20/minute")) // @limiter.limit("20/minute"): after the auth dependencies
	}
	r.Handle("POST /api/workbooks/{workbook_id}/sources/{col_id}/run", http.HandlerFunc(h.run), run...)
	if config.SourceWbFrom(d.getenv()).PreviewEnabled {
		r.Handle("GET /api/workbooks/{workbook_id}/sources/{col_id}/preview", http.HandlerFunc(h.preview), authn, workspace)
	}
}

type sourceRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

func (h sourceRoutes) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h sourceRoutes) fail(w http.ResponseWriter, what string, err error) {
	var g *wbcrud.GuardError
	switch {
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	case errors.As(err, &g):
		httpx.Detail(w, g.Status, g.Detail)
	default:
		h.log().Error("workbook sources: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}

// peopleSearchEnabled reads PEOPLE_SEARCH_SOURCE_ENABLED the way pydantic parses a bool.
func (h sourceRoutes) peopleSearchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(h.d.getenv()(peopleSearchEnv))) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	}
	return false
}

func newSourceColumnID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "src_" + hex.EncodeToString(b[:]), nil
}

// add is add_source_column: validate the body (422 precedes the lookup, as FastAPI
// resolves the body before the endpoint runs), lock the workbook FOR UPDATE (404),
// check the people_search flag (403) and config (422), append the source column.
func (h sourceRoutes) add(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSourceBody+1))
	if err != nil || len(raw) > maxSourceBody {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	body, errs := parseSourceColumnBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, wbID := wc.WorkspaceID, r.PathValue("workbook_id")

	var col wbcrud.Column
	err = h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := h.svc.Owned(r.Context(), tx, ws, wbID, true)
		if err != nil {
			return err
		}
		id, err := newSourceColumnID()
		if err != nil {
			return err
		}
		if body.Kind == "people_search" {
			if !h.peopleSearchEnabled() {
				return &wbcrud.GuardError{Status: http.StatusForbidden, Detail: peopleSearchDisabledDetail}
			}
			ps, verr := runplan.ValidatePeopleSearchConfig(map[string]any{
				"companies": stringsToAny(body.Companies), "from_column": body.FromColumn,
				"titles": stringsToAny(body.Titles), "seniority": body.Seniority, "geo": body.Geo,
				"max_per_company": body.MaxPerCompany, "max_searches": body.MaxSearches,
			})
			if verr != nil {
				if msg, ok := runplan.IsPeopleSearchError(verr); ok {
					return &wbcrud.GuardError{Status: http.StatusUnprocessableEntity, Detail: string(msg)}
				}
				return verr
			}
			col = wbcrud.MakeColumn("id", id, "name", body.Name, "type", "source", "kind", "people_search",
				"companies", ps.Companies, "from_column", ps.FromColumn, "titles", ps.Titles,
				"seniority", ps.Seniority, "geo", ps.Geo,
				"max_per_company", ps.MaxPerCompany, "max_searches", ps.MaxSearches)
		} else {
			col = wbcrud.MakeColumn("id", id, "name", body.Name, "type", "source",
				"icp", body.ICP, "channels", body.Channels, "target_rows", body.TargetRows)
			if body.Source.Len() > 0 { // `if body.source:`
				col.Set("source", body.Source)
			}
		}
		cols := append(append([]wbcrud.Column{}, wb.Columns...), col)
		_, _, err = h.svc.UpdateIfChanged(r.Context(), tx, ws, wb, wbcrud.UpdatePatch{Columns: &cols})
		return err
	})
	if err != nil {
		h.fail(w, "add", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Column wbcrud.Column `json:"column"`
	}{col})
}

func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// sourceColumn finds the workbook's `source` column (404 "Source column not found").
func sourceColumn(wb wbcrud.Workbook, colID string) (wbcrud.Column, error) {
	for _, c := range wb.Columns {
		if id, _ := c.Get("id").(string); id == colID {
			if ty, _ := c.Get("type").(string); ty == "source" {
				return c, nil
			}
		}
	}
	return wbcrud.Column{}, &wbcrud.GuardError{Status: http.StatusNotFound, Detail: "Source column not found"}
}

// preview is preview_source_column: the query that would run and the sources it would
// hit (people_search: the companies/searches a run would use). No write, no network.
func (h sourceRoutes) preview(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, wbID, colID := wc.WorkspaceID, r.PathValue("workbook_id"), r.PathValue("col_id")
	var col wbcrud.Column
	err := h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := h.svc.Owned(r.Context(), tx, ws, wbID, false)
		if err != nil {
			return err
		}
		col, err = sourceColumn(wb, colID)
		return err
	})
	if err != nil {
		h.fail(w, "preview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sourcewb.Preview(col))
}

// run is run_source_column: find the `source` column and enqueue source_workbook (no
// debit and no status change, as in Python); the executor follows the cohort flags.
func (h sourceRoutes) run(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	ws, wbID, colID := wc.WorkspaceID, r.PathValue("workbook_id"), r.PathValue("col_id")
	err := h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := h.svc.Owned(r.Context(), tx, ws, wbID, false)
		if err != nil {
			return err
		}
		if _, err := sourceColumn(wb, colID); err != nil {
			return err
		}
		_, err = sourcewb.Enqueue(r.Context(), tx, ws, wbID, colID, config.SourceWorkbookExecutor(h.d.getenv(), ws))
		return err
	})
	if err != nil {
		h.fail(w, "run", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status   string `json:"status"`
		ColumnID string `json:"column_id"`
		Message  string `json:"message"`
	}{"started", colID, "Sourcing started"})
}
