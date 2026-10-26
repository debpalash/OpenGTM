package leadtimeline

import (
	"math/big"
	"strings"
	"time"
	"unicode"
)

// ParseTimestamp ports _timeline_timestamp for string values: datetime.fromisoformat
// (Python 3.11+ grammar, "Z" rewritten to "+00:00") with naive values read as UTC;
// anything unparseable is 0.0.
//
// Documented deviations: ISO week dates ("2026-W34-1") answer 0.0 here, and
// naive strings are UTC (Python's str path uses the server's local zone, which is
// UTC in every deployment).
func ParseTimestamp(s string) float64 {
	if s == "" {
		return 0
	}
	s = strings.ReplaceAll(s, "Z", "+00:00")
	micros, ok := parseISO(s)
	if !ok {
		return 0
	}
	return microsToFloat(micros)
}

// microsToFloat is timedelta.total_seconds: integer microseconds / 10**6, correctly rounded.
func microsToFloat(micros int64) float64 {
	if micros > -(1<<53) && micros < 1<<53 {
		return float64(micros) / 1e6
	}
	f, _ := new(big.Rat).SetFrac64(micros, 1_000_000).Float64()
	return f
}

func digits(s string, n int) (int, bool) {
	if len(s) < n {
		return 0, false
	}
	v := 0
	for i := 0; i < n; i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v = v*10 + int(s[i]-'0')
	}
	return v, true
}

func parseISO(s string) (int64, bool) {
	var y, mo, d int
	var ok bool
	var rest string
	switch {
	case len(s) >= 10 && s[4] == '-':
		if s[7] != '-' {
			return 0, false
		}
		if y, ok = digits(s, 4); !ok {
			return 0, false
		}
		if mo, ok = digits(s[5:], 2); !ok {
			return 0, false
		}
		if d, ok = digits(s[8:], 2); !ok {
			return 0, false
		}
		rest = s[10:]
	case len(s) >= 8:
		if y, ok = digits(s, 4); !ok {
			return 0, false
		}
		if mo, ok = digits(s[4:], 2); !ok {
			return 0, false
		}
		if d, ok = digits(s[6:], 2); !ok {
			return 0, false
		}
		rest = s[8:]
	default:
		return 0, false
	}
	if y < 1 || mo < 1 || mo > 12 || d < 1 {
		return 0, false
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d {
		return 0, false
	}
	var hh, mm, ss, us int
	var off int64 // seconds east of UTC, microsecond part folded below
	var offUS int64
	if rest != "" {
		// one separator character of any kind, then the time
		sep := []rune(rest)[0]
		ts := rest[len(string(sep)):]
		if sep == unicode.ReplacementChar && !strings.HasPrefix(rest, "�") {
			return 0, false
		}
		tz := ""
		if i := strings.IndexByte(ts, '-'); i >= 0 {
			ts, tz = ts[:i], ts[i:]
		} else if i := strings.IndexByte(ts, '+'); i >= 0 {
			ts, tz = ts[:i], ts[i:]
		}
		var tok bool
		hh, mm, ss, us, tok = parseTime(ts)
		if !tok {
			return 0, false
		}
		if tz != "" {
			sign := int64(1)
			if tz[0] == '-' {
				sign = -1
			}
			th, tm, tsec, tus, ok2 := parseTime(tz[1:])
			if !ok2 || th > 23 || tm > 59 || tsec > 59 {
				return 0, false
			}
			off = sign * int64(th*3600+tm*60+tsec)
			offUS = sign * int64(tus)
		}
	}
	if hh > 23 || mm > 59 || ss > 59 {
		return 0, false
	}
	secs := t.Unix() + int64(hh*3600+mm*60+ss) - off
	return secs*1_000_000 + int64(us) - offUS, true
}

// parseTime parses HH[:MM[:SS[.f]]] or compact HH[MM[SS[.f]]]; the empty string is invalid.
func parseTime(s string) (hh, mm, ss, us int, ok bool) {
	if s == "" {
		return
	}
	var h bool
	if hh, h = digits(s, 2); !h {
		return
	}
	s = s[2:]
	sepd := false
	for i := 0; i < 2 && s != ""; i++ {
		if i == 0 && s[0] == ':' {
			sepd = true
		}
		if sepd {
			if s[0] != ':' {
				break
			}
			s = s[1:]
		} else if s[0] == '.' || s[0] == ',' {
			break
		}
		v, g := digits(s, 2)
		if !g {
			return 0, 0, 0, 0, false
		}
		if i == 0 {
			mm = v
		} else {
			ss = v
		}
		s = s[2:]
	}
	if s != "" {
		if s[0] != '.' && s[0] != ',' {
			return 0, 0, 0, 0, false
		}
		frac := s[1:]
		if frac == "" {
			return 0, 0, 0, 0, false
		}
		for j := 0; j < len(frac); j++ {
			if frac[j] < '0' || frac[j] > '9' {
				return 0, 0, 0, 0, false
			}
		}
		if len(frac) > 6 {
			frac = frac[:6]
		}
		frac += strings.Repeat("0", 6-len(frac))
		us, _ = digits(frac, 6)
	}
	return hh, mm, ss, us, true
}

// pyTitle is str.title(): a cased letter after a non-cased character is
// title-cased, every other cased letter lower-cased.
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		cased := unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
		switch {
		case !cased:
			b.WriteRune(r)
		case prevCased:
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(unicode.ToTitle(r))
		}
		prevCased = cased
	}
	return b.String()
}

// lessThan is Python's (a, aid) < (b, bid) for (float, str) tuples (NaN never orders).
func lessThan(a float64, aid string, b float64, bid string) bool {
	if a != b {
		return a < b
	}
	return aid < bid
}
