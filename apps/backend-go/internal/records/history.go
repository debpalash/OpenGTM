package records

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

type Observation struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"record_type"`
	Field      string          `json:"field"`
	Value      json.RawMessage `json:"value"`
	Source     string          `json:"source"`
	ObservedAt string          `json:"observed_at"`
	Accepted   bool            `json:"accepted"`
	Reason     string          `json:"reason"`
}

func (s Store) History(ctx context.Context, ws string, k Kind, id string, before int64) ([]Observation, error) {
	out := []Observation{}
	e := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if _, e := get(ctx, tx, ws, k, id, false); e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT id,record_type,field_name,value,source,observed_at::text,accepted,reason FROM record_observations WHERE workspace_id=$1 AND ((record_type=$2 AND record_id=$3) OR ($2='person' AND record_type='employment' AND record_id IN (SELECT id::text FROM person_employments WHERE workspace_id=$1 AND person_id=$3))) AND ($4::bigint=0 OR id<$4) ORDER BY id DESC LIMIT 200`, ws, k, id, before)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var o Observation
			if e = rows.Scan(&o.ID, &o.Kind, &o.Field, &o.Value, &o.Source, &o.ObservedAt, &o.Accepted, &o.Reason); e != nil {
				return e
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if errors.Is(e, pgx.ErrNoRows) {
		e = ErrNotFound
	}
	return out, e
}
