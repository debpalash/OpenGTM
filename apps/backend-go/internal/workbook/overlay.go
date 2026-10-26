package workbook

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
	q := gen.New(tx)
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
			return err
		}
	}
	if err := q.UpsertEnrichment(ctx, gen.UpsertEnrichmentParams{
		WorkbookID: w.WorkbookID, WorkspaceID: w.WorkspaceID, LeadID: w.LeadID, ColumnID: w.ColumnID,
		Value: textOf(w.Value), Status: w.Status, Provider: textOf(w.Provider), Error: textOf(w.Error),
		CellMetadata: meta,
	}); err != nil {
		return err
	}
	cell, err := json.Marshal(w.overlayCell())
	if err != nil {
		return err
	}
	// Zero rows affected (row deleted mid-run) is silent, like Python's
	// `if wr is not None`.
	_, err = q.MergeRowOverlay(ctx, gen.MergeRowOverlayParams{
		ColumnID: w.ColumnID, Cell: cell, RowID: w.RowID, WorkbookID: w.WorkbookID, WorkspaceID: w.WorkspaceID,
	})
	return err
}
