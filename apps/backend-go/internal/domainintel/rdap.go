package domainintel

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is an insertion-ordered JSON object.
type Obj = wbcrud.Obj

// NewObj returns an empty ordered object.
func NewObj() *Obj { return wbcrud.NewObj() }

const (
	rdapBase     = "https://rdap.org/domain/"
	rdapMaxBytes = 2 << 20
)

type rdapInfo struct {
	Registrar, RegDate, ExpDate any
	AgeDays                     int
	Nameservers, Status         any
}

func newRDAP() rdapInfo {
	return rdapInfo{Registrar: "", RegDate: "", ExpDate: "", Nameservers: []any{}, Status: []any{}}
}

func (r rdapInfo) obj() *Obj {
	o := NewObj()
	o.Set("registrar", r.Registrar)
	o.Set("registration_date", r.RegDate)
	o.Set("expiration_date", r.ExpDate)
	o.Set("domain_age_days", r.AgeDays)
	o.Set("nameservers", r.Nameservers)
	o.Set("status", r.Status)
	return o
}

func (a *analyzer) rdap(ctx context.Context, domain string) rdapInfo {
	res := newRDAP()
	status, body, err := a.get(ctx, rdapBase+domain, map[string]string{"Accept": "application/rdap+json, application/json"}, rdapTimeout, rdapMaxBytes, anyHTTPS)
	if err != nil || status != 200 {
		return res
	}
	doc, ok := decodeJSON(body)
	if !ok {
		return res
	}
	parseRDAP(&res, doc, a.now())
	return res
}

// pyTruthy is Python truthiness of a decoded JSON value.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return err != nil || f != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case *Obj:
		return t.Len() > 0
	}
	return true
}

func asObj(v any) (*Obj, bool) { o, ok := v.(*Obj); return o, ok }

func getOr(o *Obj, key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}

// iterItems is `for x in v`: list elements, dict keys, string characters; ok=false where Python raises TypeError.
func iterItems(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case *Obj:
		ks := t.Keys()
		out := make([]any, len(ks))
		for i, k := range ks {
			out[i] = k
		}
		return out, true
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, true
	}
	return nil, false
}

// contains is `needle in v` for a string needle; ok=false where Python raises TypeError.
func contains(v any, needle string) (found, ok bool) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if s, isStr := e.(string); isStr && s == needle {
				return true, true
			}
		}
		return false, true
	case string:
		return strings.Contains(t, needle), true
	case *Obj:
		_, has := t.Get(needle)
		return has, true
	}
	return false, false
}

// slice10 is date_str[:10] for a string (by code point) or list; ok=false where Python raises.
func slice10(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		n := 0
		for i := range t {
			if n == 10 {
				return t[:i], true
			}
			n++
		}
		return t, true
	case []any:
		if len(t) > 10 {
			return t[:10], true
		}
		return t, true
	}
	return nil, false
}

// parseRDAP is the body of get_rdap_info after the fetch. Python wraps it in one try/except: the first exception
// aborts the parse and keeps what was already stored, which is reproduced by returning at the same points.
func parseRDAP(res *rdapInfo, doc any, now time.Time) {
	data, ok := asObj(doc)
	if !ok {
		return
	}
	entities, ok := iterItems(getOr(data, "entities", []any{}))
	if !ok {
		return
	}
	for _, e := range entities {
		entity, ok := asObj(e)
		if !ok {
			return
		}
		isReg, ok := contains(getOr(entity, "roles", []any{}), "registrar")
		if !ok {
			return
		}
		if !isReg {
			continue
		}
		va := getOr(entity, "vcardArray", nil)
		if !pyTruthy(va) {
			continue
		}
		var vcard any
		switch t := va.(type) {
		case []any:
			if len(t) < 2 {
				return
			}
			vcard = t[1]
		case string:
			rs := []rune(t)
			if len(rs) < 2 {
				return
			}
			vcard = string(rs[1])
		default:
			return
		}
		items, ok := iterItems(vcard)
		if !ok {
			return
		}
		for _, it := range items {
			switch t := it.(type) {
			case []any:
				if len(t) == 0 {
					return
				}
				if first, _ := t[0].(string); first == "fn" {
					if len(t) < 4 {
						return
					}
					res.Registrar = t[3]
				} else {
					continue
				}
			case string:
				if t == "" {
					return
				}
				continue // item[0] is one character, never "fn"
			default:
				return
			}
			break
		}
	}
	events, ok := iterItems(getOr(data, "events", []any{}))
	if !ok {
		return
	}
	for _, e := range events {
		event, ok := asObj(e)
		if !ok {
			return
		}
		action, _ := getOr(event, "eventAction", "").(string)
		dateStr := getOr(event, "eventDate", "")
		switch action {
		case "registration":
			sl, ok := slice10(dateStr)
			if !ok {
				return
			}
			res.RegDate = sl
			if s, isStr := sl.(string); isStr {
				if age, ok := ageDays(s, now); ok {
					res.AgeDays = age
				}
			}
		case "expiration":
			sl, ok := slice10(dateStr)
			if !ok {
				return
			}
			res.ExpDate = sl
		}
	}
	nsItems, ok := iterItems(getOr(data, "nameservers", []any{}))
	if !ok {
		return
	}
	names := []any{}
	for _, n := range nsItems {
		ns, ok := asObj(n)
		if !ok {
			return
		}
		if v, _ := ns.Get("ldhName"); pyTruthy(v) {
			names = append(names, v)
		}
	}
	res.Nameservers = names
	res.Status = getOr(data, "status", []any{})
}

// ageDays is (datetime.now() - datetime.fromisoformat(s[:10])).days with now a naive wall-clock time; ok=false when
// fromisoformat raises (Python 3.13 accepted forms: YYYY-MM-DD, YYYYMMDD, ISO week dates, and the 10-character
// "YYYYWwwTHH" date-time).
func ageDays(s string, now time.Time) (int, bool) {
	sl, _ := slice10(s)
	d, ok := parseISO(sl.(string))
	if !ok {
		return 0, false
	}
	n := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.UTC)
	diff := n.Unix() - d.Unix()
	days := diff / 86400
	if diff%86400 < 0 {
		days--
	}
	return int(days), true
}

func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func parseISO(s string) (time.Time, bool) {
	if !isASCII(s) || utf8.RuneCountInString(s) != len(s) {
		return time.Time{}, false
	}
	hour := 0
	if len(s) == 10 && s[4] == 'W' && s[7] != '-' { // YYYYWww + separator + HH
		if _, isDate := digits(s[0:4] + s[5:7]); isDate {
			h, ok := digits(s[8:10])
			if !ok || h > 23 {
				return time.Time{}, false
			}
			hour = h
			s = s[:7]
		}
	}
	var y, m, d, w, wd int
	var ok1, ok2, ok3 bool
	week := false
	switch len(s) {
	case 10:
		if s[4] == '-' && s[7] == '-' {
			y, ok1 = digits(s[0:4])
			m, ok2 = digits(s[5:7])
			d, ok3 = digits(s[8:10])
		} else if s[4] == '-' && s[5] == 'W' && s[8] == '-' {
			week = true
			y, ok1 = digits(s[0:4])
			w, ok2 = digits(s[6:8])
			wd, ok3 = digits(s[9:10])
		}
	case 8:
		switch {
		case s[4] == '-' && s[5] == 'W':
			week = true
			y, ok1 = digits(s[0:4])
			w, ok2 = digits(s[6:8])
			wd, ok3 = 1, true
		case s[4] == 'W':
			week = true
			y, ok1 = digits(s[0:4])
			w, ok2 = digits(s[5:7])
			wd, ok3 = digits(s[7:8])
		default:
			y, ok1 = digits(s[0:4])
			m, ok2 = digits(s[4:6])
			d, ok3 = digits(s[6:8])
		}
	case 7:
		if s[4] == 'W' {
			week = true
			y, ok1 = digits(s[0:4])
			w, ok2 = digits(s[5:7])
			wd, ok3 = 1, true
		}
	}
	if !ok1 || !ok2 || !ok3 || y < 1 {
		return time.Time{}, false
	}
	if week {
		if wd < 1 || wd > 7 || w < 1 || w > 53 {
			return time.Time{}, false
		}
		jan1 := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Weekday()
		leap := y%4 == 0 && (y%100 != 0 || y%400 == 0)
		if w == 53 && !(jan1 == time.Thursday || (jan1 == time.Wednesday && leap)) {
			return time.Time{}, false
		}
		jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
		off := (int(jan4.Weekday()) + 6) % 7
		t := jan4.AddDate(0, 0, -off+(w-1)*7+(wd-1))
		if t.Year() < 1 || t.Year() > 9999 {
			return time.Time{}, false
		}
		return t.Add(time.Duration(hour) * time.Hour), true
	}
	if m < 1 || m > 12 || d < 1 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Month() != time.Month(m) || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}
