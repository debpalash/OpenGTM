package destvalidate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EncodeHistoryCursor is Python's _encode_history_cursor: the naive UTC
// isoformat of the (DB, timezone-less) created_at plus the row id, as compact
// JSON, URL-safe base64 without padding.
func EncodeHistoryCursor(createdAt time.Time, id string) string {
	t := createdAt.UTC()
	iso := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		iso += fmt.Sprintf(".%06d", us)
	}
	payload, _ := json.Marshal([]string{iso, id})
	// Python's json.dumps escapes non-ASCII; ids are ASCII uuids in practice,
	// and the decoder accepts either form.
	return base64.RawURLEncoding.EncodeToString(payload)
}

// DecodeHistoryCursor parses a cursor; any failure is the 422 detail
// "invalid <label> cursor". A naive timestamp is taken as UTC.
func DecodeHistoryCursor(cursor, label string) (time.Time, string, error) {
	bad := fmt.Errorf("invalid %s cursor", label)
	raw, ok := pyURLSafeB64Decode(cursor)
	if !ok {
		return time.Time{}, "", bad
	}
	var v []any
	if err := json.Unmarshal(raw, &v); err != nil || len(v) != 2 {
		return time.Time{}, "", bad
	}
	ts, ok1 := v[0].(string)
	id, ok2 := v[1].(string)
	if !ok1 || !ok2 {
		return time.Time{}, "", bad
	}
	t, err := parseISO(ts)
	if err != nil {
		return time.Time{}, "", bad
	}
	return t, id, nil
}

// pyURLSafeB64Decode follows base64.urlsafe_b64decode: characters outside the
// alphabet are discarded, and the data must be a valid length.
func pyURLSafeB64Decode(s string) ([]byte, bool) {
	var clean strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			clean.WriteByte(c)
		}
	}
	if clean.Len()%4 == 1 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(clean.String()) // trailing bits ignored, as Python does
	return b, err == nil
}

// parseISO accepts datetime.fromisoformat's common extended forms:
// YYYY-MM-DD[(T| )HH[:MM[:SS[.f]]][Z|+HH[:MM[:SS]]]].
func parseISO(s string) (time.Time, error) {
	bad := errors.New("bad iso timestamp")
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return time.Time{}, bad
	}
	num := func(p string, lo, hi int) (int, bool) {
		if p == "" || len(p) > 9 {
			return 0, false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return 0, false
			}
		}
		n, _ := strconv.Atoi(p)
		return n, n >= lo && n <= hi
	}
	y, ok1 := num(s[:4], 1, 9999)
	mo, ok2 := num(s[5:7], 1, 12)
	d, ok3 := num(s[8:10], 1, 31)
	if !ok1 || !ok2 || !ok3 {
		return time.Time{}, bad
	}
	hh, mm, ss, ns := 0, 0, 0, 0
	loc := time.UTC
	rest := s[10:]
	if rest != "" {
		if rest[0] != 'T' && rest[0] != 't' && rest[0] != ' ' {
			return time.Time{}, bad
		}
		rest = rest[1:]
		tz := ""
		if i := strings.IndexAny(rest, "Zz+-"); i >= 0 {
			rest, tz = rest[:i], rest[i:]
		}
		clock, frac, hasFrac := strings.Cut(rest, ".")
		fields := strings.Split(clock, ":")
		if len(fields) > 3 || len(fields[0]) != 2 {
			return time.Time{}, bad
		}
		var ok bool
		if hh, ok = num(fields[0], 0, 23); !ok {
			return time.Time{}, bad
		}
		if len(fields) > 1 {
			if len(fields[1]) != 2 {
				return time.Time{}, bad
			}
			if mm, ok = num(fields[1], 0, 59); !ok {
				return time.Time{}, bad
			}
		}
		if len(fields) > 2 {
			if len(fields[2]) != 2 {
				return time.Time{}, bad
			}
			if ss, ok = num(fields[2], 0, 59); !ok {
				return time.Time{}, bad
			}
		}
		if hasFrac {
			if len(fields) < 3 || frac == "" || len(frac) > 9 {
				return time.Time{}, bad
			}
			if _, ok = num(frac, 0, 999999999); !ok {
				return time.Time{}, bad
			}
			if len(frac) > 6 {
				frac = frac[:6]
			}
			n, _ := strconv.Atoi((frac + "000000")[:6])
			ns = n * 1000
		}
		switch {
		case tz == "":
		case tz == "Z" || tz == "z":
		default:
			sign := 1
			if tz[0] == '-' {
				sign = -1
			}
			parts := strings.Split(tz[1:], ":")
			if len(parts) > 3 || len(parts[0]) != 2 {
				return time.Time{}, bad
			}
			secs := 0
			for i, p := range parts {
				if len(p) != 2 {
					return time.Time{}, bad
				}
				n, ok := num(p, 0, 59)
				if !ok || (i == 0 && n > 23) {
					return time.Time{}, bad
				}
				secs += n * []int{3600, 60, 1}[i]
			}
			loc = time.FixedZone("", sign*secs)
		}
	}
	t := time.Date(y, time.Month(mo), d, hh, mm, ss, ns, loc)
	if t.Day() != d { // e.g. Feb 30
		return time.Time{}, bad
	}
	return t.UTC(), nil
}
