package wbcrud

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectorRunsParams are the validated query parameters of GET /{id}/connector-runs.
type ConnectorRunsParams struct {
	Limit, Offset int
	Cursor        string // empty: none
}

// isoAware is datetime.isoformat() of a UTC-aware value: "+00:00" and a
// microsecond fraction only when non-zero.
func isoAware(t *time.Time) any {
	if t == nil {
		return nil
	}
	u := t.UTC()
	layout := "2006-01-02T15:04:05+00:00"
	if u.Nanosecond()/1000 != 0 {
		layout = "2006-01-02T15:04:05.000000+00:00"
	}
	return u.Format(layout)
}

// orEmptyObj is `value or {}` for a JSON column (empty/zero values read as {}).
func orEmptyObj(raw []byte) any {
	if len(raw) == 0 {
		return NewObj()
	}
	v, err := decodeDocument(raw, true)
	if err != nil || !truthy(v) {
		return NewObj()
	}
	if n, ok := v.(json.Number); ok && (string(n) == "0" || string(n) == "0.0") {
		return NewObj()
	}
	return v
}

// ConnectorRuns lists the durable connector executions of a workbook, newest
// first, by offset or (preferred) cursor.
func (s *Service) ConnectorRuns(ctx context.Context, ws, workbookID string, p ConnectorRunsParams) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.Owned(ctx, tx, ws, workbookID, false); err != nil {
			return err
		}
		sql := `SELECT id, workspace_id, workbook_id, connector, status, query, cursor, requested_count, fetched_count, added_count,
			updated_count, skipped_count, pages_fetched, source_total, target_met, exhausted, error, started_at, completed_at,
			created_at, updated_at FROM connector_runs WHERE workspace_id = $1 AND workbook_id = $2`
		args := []any{ws, workbookID}
		if p.Cursor != "" {
			createdAt, runID, g := DecodeConnectorRunCursor(p.Cursor)
			if g != nil {
				return g
			}
			sql += ` AND (created_at < $3 OR (created_at = $3 AND id < $4))`
			args = append(args, createdAt, runID)
		}
		sql += ` ORDER BY created_at DESC, id DESC LIMIT $` + itoa(len(args)+1)
		args = append(args, int64(p.Limit)+1)
		if p.Offset > 0 && p.Cursor == "" {
			sql += ` OFFSET $` + itoa(len(args)+1)
			args = append(args, int64(p.Offset))
		}
		rs, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		type run struct {
			obj       *Obj
			createdAt time.Time
			id        string
		}
		var runs []run
		for rs.Next() {
			var (
				id, wsID, wbID, connector, status                  string
				query, cursor                                      []byte
				requested, fetched, added, updated, skipped, pages int32
				sourceTotal                                        *int32
				targetMet, exhausted                               bool
				errText                                            *string
				startedAt, completedAt                             *time.Time
				createdAt                                          time.Time
				updatedAt                                          *time.Time
			)
			if err := rs.Scan(&id, &wsID, &wbID, &connector, &status, &query, &cursor, &requested, &fetched, &added, &updated, &skipped,
				&pages, &sourceTotal, &targetMet, &exhausted, &errText, &startedAt, &completedAt, &createdAt, &updatedAt); err != nil {
				return err
			}
			o := NewObj()
			o.Set("id", id)
			o.Set("workspace_id", wsID)
			o.Set("workbook_id", wbID)
			o.Set("connector", connector)
			o.Set("status", status)
			o.Set("query", orEmptyObj(query))
			o.Set("cursor", orEmptyObj(cursor))
			o.Set("requested_count", int64(requested))
			o.Set("fetched_count", int64(fetched))
			o.Set("added_count", int64(added))
			o.Set("updated_count", int64(updated))
			o.Set("skipped_count", int64(skipped))
			o.Set("pages_fetched", int64(pages))
			o.Set("source_total", nullInt(sourceTotal))
			o.Set("target_met", targetMet)
			o.Set("exhausted", exhausted)
			o.Set("error", nullStr(errText))
			o.Set("started_at", isoAware(startedAt))
			o.Set("completed_at", isoAware(completedAt))
			o.Set("created_at", isoAware(&createdAt))
			o.Set("updated_at", isoAware(updatedAt))
			runs = append(runs, run{o, createdAt, id})
		}
		if err := rs.Err(); err != nil {
			return err
		}
		hasMore := len(runs) > p.Limit
		if hasMore {
			runs = runs[:p.Limit]
		}
		list := make([]any, len(runs))
		for i, r := range runs {
			list[i] = r.obj
		}
		var offset, next any
		if p.Cursor == "" {
			offset = int64(p.Offset)
		}
		if hasMore {
			last := runs[len(runs)-1]
			next = EncodeConnectorRunCursor(last.createdAt.UTC(), last.id)
		}
		out = NewObj()
		out.Set("runs", list)
		out.Set("limit", int64(p.Limit))
		out.Set("offset", offset)
		out.Set("has_more", hasMore)
		out.Set("next_cursor", next)
		return nil
	})
	return out, err
}

func itoa(n int) string { return strconv.Itoa(n) }
