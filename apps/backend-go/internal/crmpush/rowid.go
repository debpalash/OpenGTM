package crmpush

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// StoreRowCRMID is _store_row_crm_id: after a 404 -> create fallback, stamp the
// fresh CRM id onto the workbook row the cell ran for. crm_import rows have a
// NULL lead_id (enrichment passes the row id as lead_id), so candidates are the
// rows with lead_id = leadID or id = leadID. The first candidate that still
// carries the stale key (crm_external_id == oldExtID, crm_type equal ignoring
// case) is updated; false means no row was identified. Runs inside
// tenancy.InWorkspace and is additionally scoped to the workspace.
func StoreRowCRMID(ctx context.Context, pool *pgxpool.Pool, ws, workbookID string, leadID int64,
	crmType, oldExtID, newID, newObject string) (bool, error) {
	if pool == nil {
		return false, errors.New("crmpush: no database pool")
	}
	stored := false
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, data::text FROM record_workbook_rows
			 WHERE workspace_id = $1 AND workbook_id = $2 AND (lead_id = $3 OR id = $3)
			 ORDER BY id
			 FOR UPDATE`, ws, workbookID, leadID)
		if err != nil {
			return err
		}
		type cand struct {
			id   int64
			data string
		}
		var cands []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.data); err != nil {
				rows.Close()
				return err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range cands {
			doc, err := rowtmpl.DecodeOrdered([]byte(c.data))
			if err != nil {
				continue // Python: (r.data or {}).get raises -> the whole stamp fails; skip a corrupt row instead
			}
			data, isObj := doc.(colgraph.Col)
			if !isObj {
				continue
			}
			if idText(data.Vals["crm_external_id"]) != oldExtID ||
				strings.ToLower(idText(data.Vals["crm_type"])) != crmType {
				continue
			}
			data.Set("crm_external_id", newID)
			data.Set("crm_type", crmType)
			data.Set("crm_object", newObject)
			b, err := rowtmpl.MarshalOrdered(data)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE workbook_rows SET data = $1::json, updated_at = (now() AT TIME ZONE 'utc')
				 WHERE id = $2 AND workspace_id = $3 AND workbook_id = $4`, string(b), c.id, ws, workbookID); err != nil {
				return err
			}
			stored = true
			return nil
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return stored, nil
}

// idText is str(x or "") for the stale-key comparison: nil and falsy values
// are "".
func idText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if !t {
			return ""
		}
	case string:
		return t
	}
	s := rowtmpl.PyStr(v)
	if s == "0" || s == "0.0" || s == "[]" || s == "{}" {
		return ""
	}
	return s
}
