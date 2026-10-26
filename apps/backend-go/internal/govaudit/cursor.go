// Package govaudit lists governance audit events (keyset pages), exports them
// as CSV, and owns the cursor codec shared with the retention-run and
// playbook-run history endpoints. Behaviour mirrors
// legacy/python/apps/api/routers/governance.py.
package govaudit

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CursorError is a malformed history cursor; HTTP answer is 422 with Detail.
type CursorError struct{ Detail string }

func (e *CursorError) Error() string { return e.Detail }

// Status is the HTTP status the router answers with.
func (e *CursorError) Status() int { return 422 }

// isoformat renders like Python datetime.isoformat(): a time located in
// time.UTC is a naive column value (no suffix); any other location is an aware
// value and renders its offset. Microseconds only when non-zero.
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if t.Location() == time.UTC {
		return s
	}
	return s + t.Format("-07:00")
}

// EncodeCursor is urlsafe base64 (no padding) of compact JSON [iso, id]. t is
// rendered naive when located in time.UTC (DB naive columns), else with its
// offset, like Python's isoformat().
func EncodeCursor(t time.Time, id string) string {
	// json.Marshal of []string matches Python json.dumps(ensure_ascii=True) except
	// for non-ASCII and HTML escapes, so encode by hand.
	payload := `["` + isoformat(t) + `",` + pyJSONString(id, true) + `]`
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// DecodeCursor reverses EncodeCursor. label is "audit event", "retention run"
// or "playbook run". Naive times become UTC. Any malformed input yields a 422
// CursorError, never a panic.
func DecodeCursor(s, label string) (time.Time, string, *CursorError) {
	bad := &CursorError{Detail: "invalid " + label + " cursor"}
	if len(s) > 1024 {
		return time.Time{}, "", bad
	}
	// Python: b64decode(cursor + "=" * (-len % 4)) (urlsafe, non-strict padding).
	raw, err := base64.URLEncoding.DecodeString(s + strings.Repeat("=", (4-len(s)%4)%4))
	if err != nil {
		return time.Time{}, "", bad
	}
	var v []json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil || len(v) != 2 {
		return time.Time{}, "", bad
	}
	var id, iso string
	if json.Unmarshal(v[1], &id) != nil || json.Unmarshal(v[0], &iso) != nil {
		return time.Time{}, "", bad
	}
	t, err := parseISO(iso)
	if err != nil {
		return time.Time{}, "", bad
	}
	return t, id, nil
}

var isoRe = regexp.MustCompile(`^(\d{4})-?(\d{2})-?(\d{2})(?:[^\d](\d{2})(?::?(\d{2})(?::?(\d{2})(?:[.,](\d+))?)?)?(Z|[+-]\d{2}(?::?\d{2}(?::?\d{2})?)?)?)?$`)

// parseISO accepts the datetime.fromisoformat grammar subset a cursor can hold
// (date, date+time, fractions, Z or numeric offset). Naive results are
// UTC-located; aware ones carry a fixed zone (so isoformat prints the offset).
func parseISO(s string) (time.Time, error) {
	m := isoRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid isoformat %q", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	year, mon, day := n(1), n(2), n(3)
	hh, mm, ss := n(4), n(5), n(6)
	us := 0
	if f := m[7]; f != "" {
		if len(f) > 6 {
			f = f[:6]
		}
		us, _ = strconv.Atoi(f + strings.Repeat("0", 6-len(f)))
	}
	if year < 1 || mon < 1 || mon > 12 || day < 1 || hh > 23 || mm > 59 || ss > 59 {
		return time.Time{}, fmt.Errorf("out of range %q", s)
	}
	t := time.Date(year, time.Month(mon), day, hh, mm, ss, us*1000, time.UTC)
	if t.Day() != day {
		return time.Time{}, fmt.Errorf("day out of range %q", s)
	}
	z := m[8]
	if z == "" {
		return t, nil
	}
	off := 0
	if z != "Z" {
		d := strings.NewReplacer(":", "").Replace(z[1:])
		oh, _ := strconv.Atoi(d[:2])
		om, os := 0, 0
		if len(d) >= 4 {
			om, _ = strconv.Atoi(d[2:4])
		}
		if len(d) >= 6 {
			os, _ = strconv.Atoi(d[4:6])
		}
		if oh > 23 || om > 59 || os > 59 {
			return time.Time{}, fmt.Errorf("bad offset %q", s)
		}
		off = oh*3600 + om*60 + os
		if z[0] == '-' {
			off = -off
		}
	}
	loc := time.FixedZone("", off)
	return time.Date(year, time.Month(mon), day, hh, mm, ss, us*1000, loc), nil
}
