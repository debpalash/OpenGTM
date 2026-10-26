package workbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// cellWrite is the argument list of enrichment._set_enrichment for the
// waterfall branch (no research/verify/provenance metadata).
type cellWrite struct {
	WorkspaceID, WorkbookID string
	LeadID                  int32
	RowID                   int32
	ColumnID                string
	Value                   *string
	Status                  string
	Provider                *string
	Error                   *string
	Skipped                 []skippedProvider
	// Raw is the non-string Python value kept in the row overlay (the table
	// column holds its json.dumps text); Meta is cell_metadata (research).
	Raw    any
	HasRaw bool
	Meta   map[string]any
}

func textOf(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func anyOf(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func (w cellWrite) skippedJSON() []map[string]string {
	out := make([]map[string]string, 0, len(w.Skipped))
	for _, s := range w.Skipped {
		out = append(out, map[string]string{"provider": s.Provider, "reason": s.Reason})
	}
	return out
}

// overlayCell is the dict mirrored into workbook_rows.enrichments[column_id]:
// {"value","status","provider","error"} plus skipped_providers when present.
func (w cellWrite) overlayCell() map[string]any {
	cell := map[string]any{
		"value": anyOf(w.Value), "status": w.Status,
		"provider": anyOf(w.Provider), "error": anyOf(w.Error),
	}
	if w.HasRaw {
		cell["value"] = w.Raw
	}
	if r, ok := w.Meta["research"].(map[string]any); ok {
		cell["research"] = r
	}
	if len(w.Skipped) > 0 {
		cell["skipped_providers"] = w.skippedJSON()
	}
	return cell
}

// setEnrichment ports _set_enrichment: upsert the (workbook, lead, column)
// overlay row and mirror the cell into the workbook row's enrichments JSON.
// The caller owns the transaction (and the lease re-check that precedes it).
func setEnrichment(ctx context.Context, tx pgx.Tx, w cellWrite) error {
	_, err := setEnrichmentChange(ctx, tx, w)
	return err
}

// rowChange is one on_row_changed delta: a complete cell whose prior overlay value differs.
type rowChange struct {
	rowID    int64
	field    string
	old, new any
}

// decodeNumber decodes JSON with UseNumber so integers beyond 2^53 and the int/float
// distinction survive (Python's json keeps them).
func decodeNumber(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// priorCellValue is `_prev.get("value") if isinstance(_prev, dict) else _prev`.
func priorCellValue(ctx context.Context, q *gen.Queries, w cellWrite) (any, error) {
	cell, err := q.GetRowOverlayCell(ctx, gen.GetRowOverlayCellParams{
		ColumnID: w.ColumnID, RowID: w.RowID, WorkbookID: w.WorkbookID, WorkspaceID: w.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prev, err := decodeNumber([]byte(cell))
	if err != nil {
		return nil, nil // unreadable prior cell: treated as absent, like Python's broad except
	}
	if m, ok := prev.(map[string]any); ok {
		return m["value"], nil
	}
	return prev, nil
}

// setEnrichmentChange is setEnrichment that also reports the on_row_changed delta (status
// complete and str(old) != str(new)); nil when there is none or the row no longer exists.
func setEnrichmentChange(ctx context.Context, tx pgx.Tx, w cellWrite) (*rowChange, error) {
	q := gen.New(tx)
	var old any
	if w.Status == "complete" {
		var err error
		if old, err = priorCellValue(ctx, q, w); err != nil {
			return nil, err
		}
	}
	var meta []byte
	if len(w.Skipped) > 0 || len(w.Meta) > 0 {
		m := make(map[string]any, len(w.Meta)+1)
		for k, v := range w.Meta {
			m[k] = v
		}
		if len(w.Skipped) > 0 {
			m["skipped_providers"] = w.skippedJSON()
		}
		var err error
		if meta, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	if err := q.UpsertEnrichment(ctx, gen.UpsertEnrichmentParams{
		WorkbookID: w.WorkbookID, WorkspaceID: w.WorkspaceID, LeadID: w.LeadID, ColumnID: w.ColumnID,
		Value: textOf(w.Value), Status: w.Status, Provider: textOf(w.Provider), Error: textOf(w.Error),
		CellMetadata: meta,
	}); err != nil {
		return nil, err
	}
	cell, err := json.Marshal(w.overlayCell())
	if err != nil {
		return nil, err
	}
	// Zero rows affected (row deleted mid-run) is silent, like Python's
	// `if wr is not None`.
	n, err := q.MergeRowOverlay(ctx, gen.MergeRowOverlayParams{
		ColumnID: w.ColumnID, Cell: cell, RowID: w.RowID, WorkbookID: w.WorkbookID, WorkspaceID: w.WorkspaceID,
	})
	if err != nil || n == 0 || w.Status != "complete" {
		return nil, err
	}
	var nv any
	if w.HasRaw {
		nv = w.Raw
	} else if w.Value != nil {
		nv = *w.Value
	}
	if automationhook.PyStr(old) == automationhook.PyStr(nv) {
		return nil, nil
	}
	return &rowChange{rowID: int64(w.RowID), field: w.ColumnID, old: old, new: nv}, nil
}
