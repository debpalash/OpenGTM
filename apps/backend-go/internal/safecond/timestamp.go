package safecond

import (
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// parseTimestamp is _parse_timestamp: epoch seconds, epoch milliseconds (a
// float above 1e12) or an ISO-8601 string as accepted by Python 3.13
// datetime.fromisoformat after "Z" -> "+00:00". ok is false for None.
func parseTimestamp(v string) (float64, bool) {
	v = colgraph.PyStrip(v)
	if v == "" {
		return 0, false
	}
	if f, ok := colgraph.PyParseFloat(v); ok {
		if f > 1e12 {
			return f / 1000.0, true
		}
		return f, true
	}
	return parseISO(strings.ReplaceAll(v, "Z", "+00:00"))
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// digits reads exactly n ASCII digits at s[p:].
func digits(s string, p, n int) (val, next int, ok bool) {
	if p+n > len(s) {
		return 0, p, false
	}
	for i := 0; i < n; i++ {
		if !isDigit(s[p+i]) {
			return 0, p, false
		}
		val = val*10 + int(s[p+i]-'0')
	}
	return val, p + n, true
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func daysIn(y, m int) int {
	switch m {
	case 4, 6, 9, 11:
		return 30
	case 2:
		if isLeap(y) {
			return 29
		}
		return 28
	}
	return 31
}

// parseDate returns the calendar date and the length of the date part (the
// separator, when there is a time, is the next byte). Week-date separator
// detection mirrors CPython 3.13 _find_isoformat_datetime_separator.
func parseDate(s string) (y, m, d, n int, ok bool) {
	if len(s) < 7 {
		return
	}
	y, _, ok = digits(s, 0, 4)
	if !ok {
		return 0, 0, 0, 0, false
	}
	week := func(w, day, n int) (int, int, int, int, bool) {
		cy, cm, cd, ok := isoWeekToDate(y, w, day)
		return cy, cm, cd, n, ok
	}
	switch {
	case s[4] == '-' && s[5] == 'W': // YYYY-Www[-d]
		w, _, ok := digits(s, 6, 2)
		if !ok {
			return 0, 0, 0, 0, false
		}
		if len(s) == 8 || (len(s) > 8 && s[8] != '-') {
			return week(w, 1, 8)
		}
		if len(s) == 9 {
			return 0, 0, 0, 0, false
		}
		if len(s) > 10 && isDigit(s[10]) {
			return week(w, 1, 8)
		}
		day, _, ok := digits(s, 9, 1)
		if !ok {
			return 0, 0, 0, 0, false
		}
		return week(w, day, 10)
	case s[4] == '-': // YYYY-MM-DD
		if len(s) < 10 || s[7] != '-' {
			return 0, 0, 0, 0, false
		}
		mo, _, ok1 := digits(s, 5, 2)
		dd, _, ok2 := digits(s, 8, 2)
		if !ok1 || !ok2 || !validYMD(y, mo, dd) {
			return 0, 0, 0, 0, false
		}
		return y, mo, dd, 10, true
	case s[4] == 'W': // YYYYWww[d]
		w, _, ok := digits(s, 5, 2)
		if !ok {
			return 0, 0, 0, 0, false
		}
		// Python picks the day-present form (separator at 8) when the run of
		// digits after the week is even, or exactly one; observed on 3.13.
		k := 0
		for 7+k < len(s) && isDigit(s[7+k]) {
			k++
		}
		if k == 0 || (k%2 == 1 && k > 1) {
			return week(w, 1, 7)
		}
		return week(w, int(s[7]-'0'), 8)
	}
	mo, _, ok1 := digits(s, 4, 2) // YYYYMMDD
	dd, _, ok2 := digits(s, 6, 2)
	if len(s) < 8 || !ok1 || !ok2 || !validYMD(y, mo, dd) {
		return 0, 0, 0, 0, false
	}
	return y, mo, dd, 8, true
}

func validYMD(y, m, d int) bool { return y >= 1 && m >= 1 && m <= 12 && d >= 1 && d <= daysIn(y, m) }

func isoWeekToDate(y, w, d int) (int, int, int, bool) {
	if y < 1 || w < 1 || w > 53 || d < 1 || d > 7 {
		return 0, 0, 0, false
	}
	if w == 53 {
		if _, wk := time.Date(y, 12, 28, 0, 0, 0, 0, time.UTC).ISOWeek(); wk != 53 {
			return 0, 0, 0, false
		}
	}
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7
	}
	t := jan4.AddDate(0, 0, -(wd-1)+(w-1)*7+(d-1))
	if t.Year() < 1 || t.Year() > 9999 {
		return 0, 0, 0, false
	}
	return t.Year(), int(t.Month()), t.Day(), true
}

// byteAt returns s[i], or 0 past the end (the C parser reads a NUL terminator).
func byteAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// parseHMS follows CPython parse_hh_mm_ss_ff over s[p:end] (the C parser reads
// the byte at end, and a NUL past the string). rv is 0 when the field ends
// cleanly, 1 when one more byte follows (a timezone marker when there is one)
// and negative on error. The byte before the marker is not validated, which is
// why Python accepts "10:30:15 +05:30".
func parseHMS(s string, p, end int) (h, mi, sec, us, rv int) {
	vals := [3]*int{&h, &mi, &sec}
	sep := true
	for i := 0; i < 3; i++ {
		v, np, ok := digits(s, p, 2)
		if !ok {
			return 0, 0, 0, 0, -3
		}
		*vals[i] = v
		p = np
		c := byteAt(s, p)
		p++
		if i == 0 {
			sep = c == ':'
		}
		if p >= end {
			if c != 0 {
				return h, mi, sec, 0, 1
			}
			return h, mi, sec, 0, 0
		}
		if sep && c == ':' {
			continue
		}
		if c == '.' || c == ',' {
			break
		}
		if !sep {
			p--
		} else {
			return 0, 0, 0, 0, -4
		}
	}
	remain := end - p
	toParse := remain
	if remain >= 6 {
		toParse = 6
	}
	if toParse < 1 {
		return 0, 0, 0, 0, -3
	}
	f, np, ok := digits(s, p, toParse)
	if !ok {
		return 0, 0, 0, 0, -3
	}
	for i := toParse; i < 6; i++ {
		f *= 10
	}
	us = f
	p = np
	for p < len(s) && isDigit(s[p]) {
		p++
	}
	if byteAt(s, p) != 0 {
		return h, mi, sec, us, 1
	}
	return h, mi, sec, us, 0
}

func parseISO(s string) (float64, bool) {
	y, m, d, n, ok := parseDate(s)
	if !ok {
		return 0, false
	}
	var hh, mm, ss, us int
	offset, aware := 0.0, false
	if n < len(s) {
		start := n + 1
		if start >= len(s) {
			return 0, false
		}
		tzp := len(s)
		for i := start; i < len(s); i++ {
			if s[i] == 'Z' || s[i] == '+' || s[i] == '-' {
				tzp = i
				break
			}
		}
		var rv int
		hh, mm, ss, us, rv = parseHMS(s, start, tzp)
		if rv < 0 || (rv == 1 && tzp == len(s)) {
			return 0, false
		}
		if tzp < len(s) {
			sign := 1
			if s[tzp] == '-' {
				sign = -1
			}
			th, tm, ts, tus, rv := parseHMS(s, tzp+1, len(s))
			if rv != 0 {
				return 0, false
			}
			if (th*3600+tm*60+ts)*1000000+tus >= 86400*1000000 {
				return 0, false
			}
			if whole := th*3600 + tm*60 + ts; whole != 0 { // a zero whole-second offset is UTC: Python drops its fraction
				offset = float64(sign) * (float64(whole) + float64(tus)/1e6)
			}
			aware = true
		}
	}
	if hh > 23 || mm > 59 || ss > 59 {
		return 0, false
	}
	// Naive timestamps are UTC (audit F2); Python read them in the process zone.
	loc := time.UTC
	if !aware && y == 1 && m == 1 && d == 1 {
		// Python: naive datetime.timestamp() of 0001-01-01 overflows (year 0).
		return 0, false
	}
	t := time.Date(y, time.Month(m), d, hh, mm, ss, 0, loc)
	secs := float64(t.Unix())
	if aware {
		secs -= offset
	}
	return secs + float64(us)/1e6, true
}
