package audiencerefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"time"
)

// Record audiences use canonical subjects. Legacy audiences retain their existing
// filters and numeric IDs until explicitly converted; historical member/event IDs
// and activation receipts remain intact.
func applyRecordDiff(ctx context.Context, tx pgx.Tx, ws, audienceID, refreshID string, now time.Time, filters map[string]any) (counts, error) {
	var c counts
	kind, _ := filters["_record_type"].(string)
	if kind != "company" && kind != "person" {
		return c, errors.New("invalid audience record type")
	}
	var ids []string
	if raw, ok := filters["record_ids"].([]any); ok {
		for _, v := range raw {
			id, ok := v.(string)
			if !ok {
				return c, errors.New("record_ids must contain strings")
			}
			ids = append(ids, id)
		}
	}
	if len(ids) > 10000 {
		return c, errors.New("audience accepts up to 10000 explicit record IDs")
	}
	where := map[string]any{}
	if v, ok := filters["where"].(map[string]any); ok {
		where = v
	}
	match, e := json.Marshal(where)
	if e != nil {
		return c, e
	}
	type member struct {
		id              int32
		lead            pgtype.Int4
		company, person pgtype.Text
		snapshot        []byte
		seen            bool
	}
	existing := map[string]*member{}
	var old []*member
	rows, e := tx.Query(ctx, `SELECT id,lead_id,company_entity_id,person_entity_id,snapshot FROM audience_members WHERE workspace_id=$1 AND audience_id=$2 ORDER BY id`, ws, audienceID)
	if e != nil {
		return c, e
	}
	for rows.Next() {
		m := &member{}
		if e = rows.Scan(&m.id, &m.lead, &m.company, &m.person, &m.snapshot); e != nil {
			rows.Close()
			return c, e
		}
		key := m.company.String
		if kind == "person" {
			key = m.person.String
		}
		if key != "" && existing[key] == nil {
			existing[key] = m
		}
		old = append(old, m)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return c, e
	}
	table := "company_entities"
	cid := "r.id"
	pid := "NULL::text"
	if kind == "person" {
		table = "person_entities"
		cid = "r.company_entity_id"
		pid = "r.id"
	}
	query := `SELECT r.id,` + cid + `,` + pid + `,f.fields||jsonb_build_object('id',r.id,'record_type',$5::text) FROM ` + table + ` r CROSS JOIN LATERAL (SELECT opengtm_record_flat($1,` + cid + `,` + pid + `) fields OFFSET 0) f WHERE r.workspace_id=$1 AND (NOT $2::bool OR r.id=ANY($3::text[])) AND f.fields @> $4::jsonb ORDER BY r.id`
	rows, e = tx.Query(ctx, query, ws, filters["record_ids"] != nil, ids, match, kind)
	if e != nil {
		return c, e
	}
	type subject struct {
		id              string
		company, person pgtype.Text
		snapshot        []byte
	}
	var subjects []subject
	for rows.Next() {
		var s subject
		if e = rows.Scan(&s.id, &s.company, &s.person, &s.snapshot); e != nil {
			rows.Close()
			return c, e
		}
		subjects = append(subjects, s)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return c, e
	}
	event := func(m *member, typ string, snapshot []byte) error {
		_, e := tx.Exec(ctx, `INSERT INTO audience_membership_events(workspace_id,audience_id,lead_id,company_entity_id,person_entity_id,event_type,snapshot,refresh_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, ws, audienceID, m.lead, m.company, m.person, typ, snapshot, refreshID)
		return e
	}
	for _, s := range subjects {
		m := existing[s.id]
		if m == nil {
			m = &member{company: s.company, person: s.person, snapshot: s.snapshot, seen: true}
			if e = tx.QueryRow(ctx, `INSERT INTO audience_members(workspace_id,audience_id,company_entity_id,person_entity_id,snapshot,refresh_token,joined_at,last_seen_at) VALUES($1,$2,$3,$4,$5,$6,$7 AT TIME ZONE 'UTC',$7 AT TIME ZONE 'UTC') RETURNING id`, ws, audienceID, s.company, s.person, s.snapshot, refreshID, now).Scan(&m.id); e != nil {
				return c, e
			}
			if e = event(m, "entered", s.snapshot); e != nil {
				return c, e
			}
			c.entered++
		} else {
			m.seen = true
			var a, b any
			if e = json.Unmarshal(m.snapshot, &a); e != nil {
				return c, e
			}
			if e = json.Unmarshal(s.snapshot, &b); e != nil {
				return c, e
			}
			if !pyEqual(a, b) {
				c.changed++
			}
			_, e = tx.Exec(ctx, `UPDATE audience_members SET snapshot=$1,company_entity_id=$2,person_entity_id=$3,refresh_token=$4,last_seen_at=$5 AT TIME ZONE 'UTC' WHERE workspace_id=$6 AND id=$7`, s.snapshot, s.company, s.person, refreshID, now, ws, m.id)
			if e != nil {
				return c, e
			}
		}
		c.matched++
	}
	for _, m := range old {
		if m.seen {
			continue
		}
		if e = event(m, "exited", m.snapshot); e != nil {
			return c, e
		}
		if _, e = tx.Exec(ctx, `DELETE FROM audience_members WHERE id=$1 AND workspace_id=$2`, m.id, ws); e != nil {
			return c, e
		}
		c.exited++
	}
	_, e = tx.Exec(ctx, `UPDATE audiences SET member_count=$1,record_type=$2,refreshed_at=$3 AT TIME ZONE 'UTC',updated_at=now() AT TIME ZONE 'UTC' WHERE id=$4 AND workspace_id=$5`, c.matched, kind, now, audienceID, ws)
	if e != nil {
		return c, fmt.Errorf("finish record audience: %w", e)
	}
	return c, nil
}
