package api

import (
	"errors"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/nlcolumn"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5"
)

// POST /api/v2/workbooks/{workbook_id}/generate-column: turns an instruction into a validated column config; it does
// not add the column. The response is GenerateColumnResponse, so the column carries every ColumnConfig field (null
// when unset), in model order.

var columnConfigOrder = []string{
	"id", "name", "type", "width", "reactive", "lead_field", "provider", "waterfall", "target_field", "verify",
	"icp", "channels", "target_rows", "prompt", "input_columns", "max_steps", "cell_budget_usd", "output_format",
	"max_tokens", "goal", "tools", "policy", "condition", "destination", "destination_config", "run_once",
	"http_url", "http_method", "http_headers", "http_body", "http_extract", "formula",
}

// fullColumn is ColumnConfig dumped without exclude_none.
func fullColumn(c wbcrud.Column) *wbcrud.Obj {
	o := wbcrud.NewObj()
	for _, k := range columnConfigOrder {
		o.Set(k, c.Get(k))
	}
	return o
}

func (h *llmRoutes) generateColumn(w http.ResponseWriter, r *http.Request) {
	o, ok := readLLMBody(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	instruction := strField(o, "instruction", true, "", 3, 2000, &errs)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws := h.wsID(r)
	var cols []wbcrud.Column
	svc := &wbcrud.Service{Pool: h.d.Pool}
	err := svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := svc.Owned(r.Context(), tx, ws, r.PathValue("workbook_id"), false)
		cols = wb.Columns
		return err
	})
	if errors.Is(err, wbcrud.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, "Workbook not found")
		return
	}
	if err != nil {
		h.internal(w, "generate-column load", err)
		return
	}
	res, err := nlcolumn.Generate(r.Context(), h.newLLM(r.Context(), ws).Extract, instruction, cols)
	var nlErr *nlcolumn.Error
	if errors.As(err, &nlErr) {
		httpx.Detail(w, http.StatusBadGateway, "Column generation failed: "+nlErr.Msg)
		return
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.internal(w, "generate-column", err)
		return
	}
	writePy(w, columnResponse(res))
}

// columnResponse is GenerateColumnResponse. A generated http column that failed validation is dropped: column is
// null and "warnings" says why (a field Python's response never had, present only then).
func columnResponse(res nlcolumn.Result) *wbcrud.Obj {
	resp := wbcrud.NewObj()
	if res.Dropped {
		resp.Set("column", nil)
	} else {
		resp.Set("column", fullColumn(res.Column))
	}
	resp.Set("kind", res.Kind)
	resp.Set("explanation", res.Explanation)
	if len(res.Warnings) > 0 {
		resp.Set("warnings", res.Warnings)
	}
	return resp
}
