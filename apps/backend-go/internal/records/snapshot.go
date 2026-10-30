package records

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

// Snapshot is the lossless record interchange format. CSV is the Clay table
// interchange; snapshots additionally retain history, source identities and
// field ownership. Restore remaps local primary keys to support workspaces on
// the same database and reports the mapping rather than silently merging.
type Snapshot struct {
	Format        string                      `json:"format"`
	SchemaVersion int                         `json:"schema_version"`
	Tables        map[string][]map[string]any `json:"tables"`
}

var snapshotTables = []string{"company_entities", "person_entities", "person_employments", "company_identifiers", "person_identifiers", "record_external_identities", "record_field_owners", "record_observations", "record_workflows"}

func (s Store) Snapshot(ctx context.Context, ws string) (Snapshot, error) {
	out := Snapshot{Format: "opengtm.records.v1", SchemaVersion: 5, Tables: map[string][]map[string]any{}}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		// One statement gives every table the same MVCC snapshot.
		query := `SELECT jsonb_build_object(`
		for i, t := range snapshotTables {
			if i > 0 {
				query += ","
			}
			query += "'" + t + "',(SELECT coalesce(jsonb_agg(to_jsonb(r)-'workspace_id' ORDER BY to_jsonb(r)::text),'[]') FROM " + t + " r WHERE workspace_id=$1)"
		}
		query += ")"
		var raw []byte
		if e := tx.QueryRow(ctx, query, ws).Scan(&raw); e != nil {
			return e
		}
		return decodeJSON(raw, &out.Tables)
	})
	return out, err
}

type RestoreRequest struct {
	Snapshot       Snapshot `json:"snapshot"`
	IdempotencyKey string   `json:"idempotency_key"`
}
type RestoreResult struct {
	Replayed  bool              `json:"replayed"`
	Companies int               `json:"companies"`
	People    int               `json:"people"`
	IDs       map[string]string `json:"id_mapping"`
}

func (s Store) Restore(ctx context.Context, ws string, req RestoreRequest, preview bool) (RestoreResult, error) {
	out := RestoreResult{IDs: map[string]string{}}
	if req.Snapshot.Format != "opengtm.records.v1" || req.Snapshot.SchemaVersion != 5 {
		return out, errors.New("unsupported record snapshot version")
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200 {
		return out, errors.New("idempotency_key is required")
	}
	allowed := map[string]bool{}
	for _, t := range snapshotTables {
		allowed[t] = true
	}
	count := 0
	for table, rows := range req.Snapshot.Tables {
		if !allowed[table] {
			return out, fmt.Errorf("unsupported snapshot table %q", table)
		}
		count += len(rows)
	}
	if count > 200000 || len(req.Snapshot.Tables["company_entities"])+len(req.Snapshot.Tables["person_entities"]) > MaxImportRows {
		return out, errors.New("snapshot exceeds the restore row limit")
	}
	raw, e := json.Marshal(req.Snapshot)
	if e != nil {
		return out, e
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+"|restore"); e != nil {
			return e
		}
		var oldHash string
		var oldResult []byte
		e := tx.QueryRow(ctx, `SELECT payload_hash,result FROM record_imports WHERE workspace_id=$1 AND idempotency_key=$2`, ws, req.IdempotencyKey).Scan(&oldHash, &oldResult)
		if e == nil {
			if oldHash != hash {
				return ErrConflict
			}
			if e = decodeJSON(oldResult, &out); e != nil {
				return e
			}
			out.Replayed = true
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		var n int
		if e = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM company_entities WHERE workspace_id=$1)+(SELECT count(*) FROM person_entities WHERE workspace_id=$1)`, ws).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			return errors.New("restore a snapshot into a new, empty workspace; use table import to update existing records")
		}
		if _, e = tx.Exec(ctx, `SELECT set_config('opengtm.record_restoring','true',true)`); e != nil {
			return e
		}
		maps := map[string]map[string]any{"company": {}, "person": {}, "employment": {}}
		for _, item := range []struct{ table, kind string }{{"company_entities", "company"}, {"person_entities", "person"}, {"person_employments", "employment"}} {
			for _, row := range req.Snapshot.Tables[item.table] {
				old := textValue(row["id"])
				if old == "" || maps[item.kind][old] != nil {
					return errors.New("missing or repeated record ID in snapshot")
				}
				var id any
				if item.kind == "employment" {
					var i int64
					if e = tx.QueryRow(ctx, `SELECT nextval('person_employments_id_seq')`).Scan(&i); e != nil {
						return e
					}
					id = i
				} else {
					var v string
					if e = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&v); e != nil {
						return e
					}
					id = v
				}
				maps[item.kind][old] = id
				out.IDs[item.kind+":"+old] = fmt.Sprint(id)
			}
		}
		remap := func(row map[string]any, key, kind string) error {
			if row[key] == nil || row[key] == "" {
				return nil
			}
			v := maps[kind][textValue(row[key])]
			if v == nil {
				return fmt.Errorf("snapshot has a missing %s relationship", kind)
			}
			row[key] = v
			return nil
		}
		for _, table := range snapshotTables {
			for _, original := range req.Snapshot.Tables[table] {
				row := map[string]any{}
				for k, v := range original {
					row[k] = v
				}
				row["workspace_id"] = ws
				switch table {
				case "company_entities":
					row["id"] = maps["company"][textValue(row["id"])]
					out.Companies++
				case "person_entities":
					row["id"] = maps["person"][textValue(row["id"])]
					if e = remap(row, "company_entity_id", "company"); e != nil {
						return e
					}
					out.People++
				case "person_employments":
					row["id"] = maps["employment"][textValue(row["id"])]
					if e = remap(row, "person_id", "person"); e != nil {
						return e
					}
					if e = remap(row, "company_entity_id", "company"); e != nil {
						return e
					}
					if row["company_entity_id"] != nil {
						row["company_key"] = "record:" + fmt.Sprint(row["company_entity_id"])
					}
				case "company_identifiers", "person_identifiers":
					key, kind := "entity_id", "company"
					if table == "person_identifiers" {
						key, kind = "person_id", "person"
					}
					if e = remap(row, key, kind); e != nil {
						return e
					}
					var id int64
					if e = tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence($1,'id'))`, table).Scan(&id); e != nil {
						return e
					}
					row["id"] = id
				case "record_external_identities", "record_field_owners", "record_observations":
					kind, _ := row["record_type"].(string)
					if maps[kind] == nil {
						return errors.New("invalid snapshot record type")
					}
					if e = remap(row, "record_id", kind); e != nil {
						return e
					}
					row["record_id"] = fmt.Sprint(row["record_id"])
					if table == "record_observations" {
						var id int64
						if e = tx.QueryRow(ctx, `SELECT nextval('record_observations_id_seq')`).Scan(&id); e != nil {
							return e
						}
						row["id"] = id
					}
				}
				data, e := json.Marshal(row)
				if e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `INSERT INTO `+table+` SELECT (jsonb_populate_record(NULL::`+table+`,$1::jsonb)).*`, data); e != nil {
					return e
				}
			}
		}
		if preview {
			return previewRollback
		}
		result, _ := json.Marshal(out)
		_, e = tx.Exec(ctx, `INSERT INTO record_imports(workspace_id,idempotency_key,payload_hash,source,result) VALUES($1,$2,$3,'opengtm:snapshot',$4)`, ws, req.IdempotencyKey, hash, result)
		return e
	})
	if errors.Is(e, previewRollback) {
		e = nil
	}
	return out, e
}
