package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5"
)

type Workflow struct {
	Name    string           `json:"name"`
	Columns []map[string]any `json:"columns"`
}

func (s Store) Workflow(ctx context.Context, ws string, k Kind) (Workflow, error) {
	out := Workflow{Name: "Default enrichment", Columns: []map[string]any{}}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		var b []byte
		e := tx.QueryRow(ctx, `SELECT name,columns_config FROM record_workflows WHERE workspace_id=$1 AND record_type=$2`, ws, k).Scan(&out.Name, &b)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		return json.Unmarshal(b, &out.Columns)
	})
	return out, err
}
func (s Store) SaveWorkflow(ctx context.Context, ws string, k Kind, w Workflow) error {
	if !k.Valid() || w.Name == "" || len(w.Name) > 200 || len(w.Columns) > 50 {
		return errors.New("workflow needs a name and up to 50 columns")
	}
	// Reuse the workbook parser: provider configuration, dependencies and column
	// validation have one contract for saved workflows and ordinary workbooks.
	b, _ := json.Marshal(map[string]any{"name": w.Name, "columns": w.Columns})
	_, errs := wbcrud.ParseCreateBody(b)
	if len(errs) > 0 {
		return fmt.Errorf("invalid workflow columns: %s", errs[0].Msg)
	}
	b, e := json.Marshal(w.Columns)
	if e != nil {
		return e
	}
	return tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO record_workflows(workspace_id,record_type,name,columns_config) VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id,record_type) DO UPDATE SET name=excluded.name,columns_config=excluded.columns_config,updated_at=now()`, ws, k, w.Name, b)
		return e
	})
}

// Workbook references records directly. No synthetic legacy lead is created.
// The returned workbook runs through the existing durable queue and spend ledger.
func (s Store) Workbook(ctx context.Context, ws string, k Kind, ids []string, name string) (string, error) {
	if !k.Valid() || len(ids) == 0 || len(ids) > 10000 {
		return "", errors.New("select 1 to 10000 company or person IDs")
	}
	if len(name) > 200 {
		return "", errors.New("workbook name is too long")
	}
	if name == "" {
		name = "Enrich " + string(k) + " records"
	}
	w, e := s.Workflow(ctx, ws, k)
	if e != nil {
		return "", e
	}
	columns := w.Columns
	if len(columns) == 0 {
		columns = []map[string]any{{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "target_field": "company"}, {"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website", "target_field": "website"}}
		if k == Person {
			columns = append(columns, map[string]any{"id": "email", "name": "Email", "type": "lead_field", "lead_field": "email", "target_field": "email"})
		}
	}
	raw, _ := json.Marshal(columns)
	id := wbcrud.NewID()
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO workbooks(id,workspace_id,name,description,status,source_type,source_config,columns_config,total_rows,completed_rows,sync_to_leads,budget_spent_usd,record_type) VALUES($1,$2,$3,'Shared Company/Person records','draft','records','{"row_storage_version":2}',$4,0,0,false,0,$5)`, id, ws, name, raw, k)
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		unique := make([]string, 0, len(ids))
		for _, rid := range ids {
			if !seen[rid] {
				seen[rid] = true
				unique = append(unique, rid)
			}
		}
		table, cid, pid := "company_entities", "r.id", "NULL::varchar"
		if k == Person {
			table, cid, pid = "person_entities", "r.company_entity_id", "r.id"
		}
		query := `INSERT INTO workbook_rows(workbook_id,workspace_id,position,data,enrichments,corroboration_count,company_entity_id,person_entity_id) SELECT $1,$2,(i.position-1)::int,'{}'::json,'{}'::json,1,` + cid + `,` + pid + ` FROM unnest($3::text[]) WITH ORDINALITY i(id,position) JOIN ` + table + ` r ON r.id=i.id AND r.workspace_id=$2 ORDER BY i.position RETURNING id`
		rows, e := tx.Query(ctx, query, id, ws, unique)
		if e != nil {
			return e
		}
		rowIDs := make([]int64, 0, len(unique))
		for rows.Next() {
			var rid int64
			if e = rows.Scan(&rid); e != nil {
				rows.Close()
				return e
			}
			rowIDs = append(rowIDs, rid)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(rowIDs) != len(unique) {
			return ErrNotFound
		}
		if s.Hook != nil {
			_, _ = s.Hook.RowsAdded(ctx, tx, ws, id, rowIDs)
		}
		_, e = tx.Exec(ctx, `UPDATE workbooks SET total_rows=$1 WHERE id=$2 AND workspace_id=$3`, len(rowIDs), id, ws)
		return e
	})
	return id, e
}
