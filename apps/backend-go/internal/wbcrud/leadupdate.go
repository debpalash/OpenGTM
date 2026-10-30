package wbcrud

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var leadColumnName = regexp.MustCompile(`^[a-z][a-z_]*$`)

// leadISO is datetime.now(timezone.utc).isoformat().
func leadISO(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// leadValue converts a JSON value for the leads column col. Python hands the
// raw value to the database driver, which coerces many shapes and answers 500
// for the rest; those rest are a 400 here.
func leadValue(col string, v any) (any, bool) {
	if col == "score" {
		switch t := v.(type) {
		case nil:
			return nil, true
		case bool:
			if t {
				return int32(1), true
			}
			return int32(0), true
		case json.Number:
			f, err := strconv.ParseFloat(string(t), 64)
			if err != nil || math.IsInf(f, 0) {
				return nil, false
			}
			f = math.RoundToEven(f)
			if f < math.MinInt32 || f > math.MaxInt32 {
				return nil, false
			}
			return int32(f), true
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 32)
			if err != nil {
				return nil, false
			}
			return int32(n), true
		}
		return nil, false
	}
	switch t := v.(type) {
	case nil:
		return nil, col != "company" // company is NOT NULL
	case string:
		return t, true
	case json.Number:
		return string(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return nil, false
}

// UpdateLead is update_lead_field: LEAD_FIELD_MAP keys of the body are written
// to the workspace's lead (400 when none are valid, 404 when the lead is not in
// this workspace). It returns the written keys in body order.
func (s *Service) UpdateLead(ctx context.Context, tx pgx.Tx, ws string, leadID int64, body *Obj) ([]string, *GuardError, error) {
	known := map[string]struct{}{}
	for _, f := range s.leadFieldList() {
		known[f] = struct{}{}
	}
	var keys []string
	for _, k := range body.Keys() {
		if _, ok := known[k]; ok && leadColumnName.MatchString(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, &GuardError{Status: 400, Detail: "No valid lead fields to update"}, nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leads WHERE workspace_id = $1 AND id = $2)`, ws, leadID).Scan(&exists); err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, &GuardError{Status: 404, Detail: "Lead not found"}, nil
	}
	sets := make([]string, 0, len(keys)+1)
	args := make([]any, 0, len(keys)+3)
	for _, k := range keys {
		raw, _ := body.Get(k)
		val, ok := leadValue(k, raw)
		if !ok {
			return nil, &GuardError{Status: 400, Detail: fmt.Sprintf("Invalid value for lead field %s", k)}, nil
		}
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", k, len(args)))
	}
	args = append(args, leadISO(time.Now()), ws, leadID)
	sql := fmt.Sprintf("UPDATE leads SET %s, updated_at = $%d WHERE workspace_id = $%d AND id = $%d",
		strings.Join(sets, ", "), len(keys)+1, len(keys)+2, len(keys)+3)
	var prior string
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('opengtm.record_source',true),'')`).Scan(&prior); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('opengtm.record_source','manual',true)`); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('opengtm.record_source',$1,true)`, prior); err != nil {
		return nil, nil, err
	}
	return keys, nil, nil
}
