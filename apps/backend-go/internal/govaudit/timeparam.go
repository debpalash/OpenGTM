package govaudit

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimeParamError is a datetime query parameter pydantic v2 would reject; the
// router answers 422 with Type/Msg in the standard validation body.
type TimeParamError struct{ Type, Msg string }

func (e *TimeParamError) Error() string { return e.Type + ": " + e.Msg }

var (
	tpDateTime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[Tt _](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?(Z|z|[+-]\d{2}:?\d{2})?)?$`)
	tpNumeric  = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)
)

const tpPrefix = "Input should be a valid datetime or date, "

func tpErr(reason string) error {
	return &TimeParamError{Type: "datetime_from_date_parsing", Msg: tpPrefix + reason}
}

// ParseTimeParam parses a `since`/`before` query value the way pydantic v2
// does for the recorded grammar: YYYY-MM-DD (midnight), date + T/t/space/_ +
// HH:MM[:SS[.f1+]] with optional Z/z/+HH:MM/+HHMM, or a unix timestamp
// (seconds, or milliseconds above 2e10). Naive inputs stay UTC-located (naive);
// aware inputs carry a fixed zone. Anything else is a *TimeParamError.
func ParseTimeParam(s string) (time.Time, error) {
	if tpNumeric.MatchString(s) {
		return parseUnix(s)
	}
	m := tpDateTime.FindStringSubmatch(s)
	if m == nil {
		switch {
		case len(s) < 10:
			return time.Time{}, tpErr("input is too short")
		case s[0] < '0' || s[0] > '9':
			return time.Time{}, tpErr("invalid character in year")
		}
		return time.Time{}, tpErr("unexpected extra characters at the end of the input")
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	year, mon, day := n(1), n(2), n(3)
	if mon < 1 || mon > 12 {
		return time.Time{}, tpErr("month value is outside expected range of 1-12")
	}
	if d := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC); day < 1 || d.Day() != day {
		return time.Time{}, tpErr("day value is outside expected range")
	}
	hh, mm, ss := n(4), n(5), n(6)
	if hh > 23 || mm > 59 || ss > 59 {
		return time.Time{}, tpErr("unexpected extra characters at the end of the input")
	}
	us := 0
	if f := m[7]; f != "" {
		if len(f) > 6 {
			f = f[:6]
		}
		us, _ = strconv.Atoi(f + strings.Repeat("0", 6-len(f)))
	}
	z := m[8]
	if z == "" {
		return time.Date(year, time.Month(mon), day, hh, mm, ss, us*1000, time.UTC), nil
	}
	off := 0
	if z != "Z" && z != "z" {
		d := strings.ReplaceAll(z[1:], ":", "")
		oh, _ := strconv.Atoi(d[:2])
		om, _ := strconv.Atoi(d[2:])
		if oh > 23 || om > 59 {
			return time.Time{}, tpErr("unexpected extra characters at the end of the input")
		}
		off = oh*3600 + om*60
		if z[0] == '-' {
			off = -off
		}
	}
	return time.Date(year, time.Month(mon), day, hh, mm, ss, us*1000, time.FixedZone("", off)), nil
}

func parseUnix(s string) (time.Time, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	ip, fp, _ := strings.Cut(s, ".")
	if len(ip) > 15 {
		return time.Time{}, tpErr("dates before 1600 and after 9999 are not supported as unix timestamps")
	}
	iv, _ := strconv.ParseInt(ip, 10, 64)
	scale, fdigits := int64(1_000_000), 6
	if iv > 20_000_000_000 {
		scale, fdigits = 1_000, 3
	}
	if len(fp) > fdigits {
		fp = fp[:fdigits]
	}
	fv, _ := strconv.ParseInt(fp+strings.Repeat("0", fdigits-len(fp)), 10, 64)
	total := iv*scale + fv // microseconds in both modes
	if neg {
		total = -total
	}
	t := time.UnixMicro(total).In(time.FixedZone("", 0))
	if y := t.Year(); y < 1 || y > 9999 {
		return time.Time{}, tpErr("dates before 1600 and after 9999 are not supported as unix timestamps")
	}
	return t, nil
}
