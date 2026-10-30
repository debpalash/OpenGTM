package destinbound

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// reconcileRecords is the canonical adapter of the inbound receipt contract.
// Identity resolution never creates a record from an unmatched callback.
func reconcileRecords(ctx context.Context, tx pgx.Tx, d Destination, body InboundBody, event, external string) (Receipt, bool, bool, error) {
	ws := d.WorkspaceID
	cid, pid := "", ""
	explicit := false
	var legacy pgtype.Int4
	preIgnored := []string{}
	for key, dst := range map[string]*string{"company_id": &cid, "person_id": &pid} {
		if v, ok := body.m[key]; ok && v != nil {
			value, ok := v.(string)
			if !ok || len(value) > 128 {
				return Receipt{}, false, true, ErrInvalidBody
			}
			*dst = value
			explicit = true
		}
	}
	if !explicit && body.m["lead_id"] != nil {
		return Receipt{}, false, false, nil
	}
	if !explicit && external != "" {
		var c, p pgtype.Text
		e := tx.QueryRow(ctx, `SELECT company_entity_id,person_entity_id,lead_id FROM destination_deliveries WHERE workspace_id=$1 AND destination_id=$2 AND external_id=$3 AND status='success' ORDER BY delivered_at DESC,id DESC LIMIT 1`, ws, d.ID, external).Scan(&c, &p, &legacy)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return Receipt{}, false, true, e
		}
		if legacy.Valid {
			return Receipt{}, false, false, nil
		}
		cid, pid = c.String, p.String
	}
	if !explicit && cid == "" && pid == "" {
		if lead, ok := body.m["lead_id"].(int64); ok {
			legacy = pgtype.Int4{Int32: int32(lead), Valid: lead > 0 && lead <= 2147483647}
			var c, p pgtype.Text
			e := tx.QueryRow(ctx, `SELECT company_entity_id,person_entity_id FROM leads WHERE workspace_id=$1 AND id=$2`, ws, lead).Scan(&c, &p)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return Receipt{}, false, true, e
			}
			cid, pid = c.String, p.String
		}
	}
	fields, _ := fieldsOf(body.m["fields"])
	rev := reverseMap(d.FieldMap)
	mapped := map[string]any{}
	for _, f := range fields {
		key := f.Key
		_, configured := rev[key]
		if target, ok := rev[key]; ok {
			key = target
		}
		if key == "id" || key == "workspace_id" || key == "company_id" || key == "person_id" || key == "record_type" || key == "external_id" || strings.HasPrefix(key, "_") {
			continue
		}
		if !AllowedFields[key] && !configured && !strings.HasPrefix(key, "company.") && !strings.HasPrefix(key, "person.") && !strings.HasPrefix(key, "employment.") {
			preIgnored = append(preIgnored, key)
			continue
		}
		mapped[key] = f.Value
	}
	if !explicit && cid == "" && pid == "" {
		if email, ok := mapped["email"].(string); ok && strings.TrimSpace(email) != "" {
			var legacyMatch bool
			if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM leads WHERE workspace_id=$1 AND lower(email)=lower(trim($2)))`, ws, email).Scan(&legacyMatch); e != nil {
				return Receipt{}, false, true, e
			}
			if legacyMatch {
				return Receipt{}, false, false, nil
			}
			rows, e := tx.Query(ctx, `SELECT p.id,p.company_entity_id FROM person_entities p JOIN person_identifiers i ON i.person_id=p.id AND i.workspace_id=p.workspace_id WHERE p.workspace_id=$1 AND i.kind='email' AND i.value=lower(trim($2)) LIMIT 2`, ws, email)
			if e != nil {
				return Receipt{}, false, true, e
			}
			var candidates []struct {
				id      string
				company pgtype.Text
			}
			for rows.Next() {
				var v struct {
					id      string
					company pgtype.Text
				}
				if e = rows.Scan(&v.id, &v.company); e != nil {
					rows.Close()
					return Receipt{}, false, true, e
				}
				candidates = append(candidates, v)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return Receipt{}, false, true, e
			}
			if len(candidates) == 1 {
				pid = candidates[0].id
				cid = candidates[0].company.String
			}
		}
	}
	if !explicit && cid == "" && pid == "" {
		return Receipt{}, false, false, nil
	}
	// Serialise each callback event before checking its receipt, including replays
	// that arrive concurrently with the first request.
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+"|inbound|"+d.ID+"|"+event); e != nil {
		return Receipt{}, false, true, e
	}
	if prior, e := priorReceipt(ctx, gen.New(tx), ws, d.ID, event); e != nil || prior != nil {
		if e != nil {
			return Receipt{}, false, true, e
		}
		return *prior, true, true, nil
	}
	kind := records.Company
	if pid != "" {
		kind = records.Person
	}
	matched := true
	if pid != "" {
		var current pgtype.Text
		e := tx.QueryRow(ctx, `SELECT company_entity_id FROM person_entities WHERE workspace_id=$1 AND id=$2`, ws, pid).Scan(&current)
		if errors.Is(e, pgx.ErrNoRows) {
			matched = false
		} else if e != nil {
			return Receipt{}, false, true, e
		}
		if cid != "" && cid != current.String {
			return Receipt{}, false, true, ErrInvalidBody
		}
		cid = current.String
	}
	if cid != "" {
		var exists string
		e := tx.QueryRow(ctx, `SELECT id FROM company_entities WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, ws, cid).Scan(&exists)
		if errors.Is(e, pgx.ErrNoRows) {
			matched = false
		} else if e != nil {
			return Receipt{}, false, true, e
		}
	}
	if pid != "" {
		var current pgtype.Text
		e := tx.QueryRow(ctx, `SELECT company_entity_id FROM person_entities WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, ws, pid).Scan(&current)
		if errors.Is(e, pgx.ErrNoRows) {
			matched = false
		} else if e != nil {
			return Receipt{}, false, true, e
		}
		if cid != current.String {
			return Receipt{}, false, true, records.ErrConflict
		}
	}
	policy := conflictPolicy(d.Config)
	status := "unmatched"
	applied, ignored := []string{}, preIgnored
	var errText pgtype.Text
	if matched {
		readFacts := func() (map[string]any, error) {
			var raw []byte
			e := tx.QueryRow(ctx, `SELECT jsonb_build_object('company',coalesce((SELECT accepted_fields FROM company_entities WHERE workspace_id=$1 AND id=$2),'{}'),'person',coalesce((SELECT accepted_fields FROM person_entities WHERE workspace_id=$1 AND id=$3),'{}'),'employment',coalesce((SELECT accepted_fields FROM person_employments WHERE workspace_id=$1 AND person_id=$3 AND is_current<>0 ORDER BY id DESC LIMIT 1),'{}'))`, ws, cid, pid).Scan(&raw)
			var out map[string]any
			if e == nil {
				e = json.Unmarshal(raw, &out)
			}
			return out, e
		}
		before, e := readFacts()
		if e != nil {
			return Receipt{}, false, true, e
		}
		_, mapping, e := records.FlatWrite(kind, mapped, cid, pid)
		if e != nil {
			return Receipt{}, false, true, e
		}
		accepted := map[string]any{}
		lookup := func(facts map[string]any, target string) any {
			parts := strings.SplitN(target, ".", 2)
			if len(parts) != 2 {
				return nil
			}
			owner, _ := facts[parts[0]].(map[string]any)
			return owner[parts[1]]
		}
		for key, value := range mapped {
			cur := lookup(before, mapping[key])
			if value == nil || value == "" || (policy == "fill_missing" && cur != nil && cur != "" && cur != "N/A") {
				ignored = append(ignored, key)
				continue
			}
			if cid == "" && strings.HasPrefix(mapping[key], "company.") {
				ignored = append(ignored, key)
				continue
			}
			accepted[key] = value
		}
		write, _, e := records.FlatWrite(kind, accepted, cid, pid)
		if e != nil {
			return Receipt{}, false, true, e
		}
		if len(accepted) > 0 {
			if _, e = records.Apply(ctx, tx, ws, kind, write, "connector:"+d.ID, "", policy == "crm_wins"); e != nil {
				return Receipt{}, false, true, e
			}
		}
		after, e := readFacts()
		if e != nil {
			return Receipt{}, false, true, e
		}
		for key := range accepted {
			if !reflect.DeepEqual(lookup(before, mapping[key]), lookup(after, mapping[key])) {
				applied = append(applied, key)
			} else if !reflect.DeepEqual(lookup(before, mapping[key]), mapped[key]) {
				ignored = append(ignored, key)
			}
		}
		status = "applied"
		if len(applied) == 0 {
			status = "no_change"
		}
	} else {
		errText = pgtype.Text{String: UnmatchedError, Valid: true}
		cid, pid = "", ""
	}
	sort.Strings(applied)
	sort.Strings(ignored)
	a, _ := json.Marshal(applied)
	i, _ := json.Marshal(ignored)
	fp, e := fingerprint(body)
	if e != nil {
		return Receipt{}, false, true, e
	}
	id := newID()
	_, e = tx.Exec(ctx, `INSERT INTO destination_inbound_receipts(id,workspace_id,destination_id,provider,external_event_id,external_record_id,lead_id,company_entity_id,person_entity_id,status,conflict_policy,applied_fields,ignored_fields,payload_fingerprint,error) VALUES($1,$2,$3,$4,$5,nullif($6,''),$15,nullif($7,''),nullif($8,''),$9,$10,$11,$12,$13,$14)`, id, ws, d.ID, d.DestinationType, event, external, cid, pid, status, policy, a, i, fp, errText, legacy)
	if e != nil {
		return Receipt{}, false, true, e
	}
	r, e := priorReceipt(ctx, gen.New(tx), ws, d.ID, event)
	if e != nil {
		return Receipt{}, false, true, e
	}
	return *r, false, true, nil
}
