package watchsvc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// HTTPError is the shared REST rejection type (autosvc.HTTPError).
type HTTPError = autosvc.HTTPError

func herr(status int, format string, a ...any) *HTTPError {
	return &HTTPError{Status: status, Detail: fmt.Sprintf(format, a...)}
}

// Config is the slice of INTENT_POLLER_* / AUTOMATIONS settings the service reads.
type Config struct {
	DefaultInterval       string // INTENT_POLLER_DEFAULT_INTERVAL
	MaxWatchesPerWS       int    // INTENT_POLLER_MAX_WATCHES_PER_WS
	JobChangeMaxContacts  int    // INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS
	PollNowMinIntervalSec int    // INTENT_POLLER_POLL_NOW_MIN_INTERVAL_SEC
	PollNowDailyQuota     int    // INTENT_POLLER_POLL_NOW_DAILY_QUOTA
	AutomationsEnabled    bool   // AUTOMATIONS_ENABLED (webhook rule creation)
	WebhookAllowlist      []string
}

// RequireEnabled is _require_enabled: 404 when the poller is off, then 409
// when the Postgres lead store is off. Nil when the routes may serve.
func RequireEnabled(pollerEnabled, pgLeadStore bool) *HTTPError {
	if !pollerEnabled {
		return herr(404, "intent poller disabled")
	}
	if !pgLeadStore {
		return herr(409, "intent_poller_requires_pg_lead_store")
	}
	return nil
}

// KindSignalTypes is _KIND_SIGNAL_TYPES.
var kindSignalTypes = map[string][]string{
	"funding":       {"company_funded", "executive_hired"},
	"hiring":        {"hiring_surge", "new_tech_adopted"},
	"feed":          {"news"},
	"company":       {"company_funded", "executive_hired", "hiring_surge", "new_tech_adopted"},
	"job_change":    {"job_change"},
	"account_group": {"partnership_hiring", "leadership_change", "funding", "pricing_page_change"},
}

var watchIntervals = map[string]bool{"hourly": true, "daily": true, "weekly": true}

func allowedSignal(kind string) map[string]bool {
	m := map[string]bool{}
	for _, s := range kindSignalTypes[kind] {
		m[s] = true
	}
	return m
}

// pyIntStr is Python int(str) (see autosvc): whitespace, sign, digits with
// single underscores.
func pyIntStr(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// pyIntOf is int(v) for a decoded JSON value: ok is false where Python raises
// TypeError/ValueError. Floats truncate toward zero.
func pyIntOf(v any) (int64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		f, err := t.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= 1<<63 {
			return 0, false
		}
		return int64(f), true
	case string:
		return pyIntStr(t)
	}
	return 0, false
}

// stripField is `(entry.get(k) or "").strip()`: a falsy value is "", a truthy
// non-string is a Python crash (ok false).
func stripField(o *wbcrud.Obj, k string) (string, bool) {
	v, _ := o.Get(k)
	if !truthyStr(v) {
		return "", true
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return colgraph.PyStrip(s), true
}

// ValidateJobChangeConfig is _validate_job_change_config: the normalised
// roster. config is nil, an object, or (invalid) another JSON value.
// Python 500s on a non-object config or a non-string name/company/url; those
// answer 422 (D4).
func ValidateJobChangeConfig(config any, maxContacts int) (*wbcrud.Obj, error) {
	cfg, ok := config.(*wbcrud.Obj)
	if !ok {
		if truthyStr(config) {
			return nil, herr(422, "config must be an object")
		}
		cfg = wbcrud.NewObj()
	}
	cv, _ := cfg.Get("contacts")
	contacts, isList := cv.([]any)
	if !isList || len(contacts) == 0 {
		return nil, herr(422, "job_change watch requires config.contacts (non-empty list)")
	}
	if len(contacts) > maxContacts {
		return nil, herr(422, "job_change watch supports at most %d contacts", maxContacts)
	}
	norm := make([]any, 0, len(contacts))
	for i, c := range contacts {
		entry, ok := c.(*wbcrud.Obj)
		if !ok {
			return nil, herr(422, "contacts[%d] must be an object", i)
		}
		var name, company, link string
		for _, f := range []struct {
			dst *string
			key string
		}{{&name, "name"}, {&company, "company"}, {&link, "linkedin_url"}} {
			s, ok := stripField(entry, f.key)
			if !ok {
				return nil, herr(422, "contacts[%d].%s must be a string", i, f.key)
			}
			*f.dst = s
		}
		item := wbcrud.NewObj()
		if lv, _ := entry.Get("lead_id"); lv != nil {
			n, ok := pyIntOf(lv)
			if !ok {
				return nil, herr(422, "contacts[%d].lead_id must be an int", i)
			}
			item.Set("lead_id", json.Number(strconv.FormatInt(n, 10)))
		} else {
			if name == "" {
				return nil, herr(422, "contacts[%d] requires lead_id or a name", i)
			}
			if link != "" && !strings.Contains(strings.ToLower(link), "linkedin.com/in") {
				return nil, herr(422, "contacts[%d].linkedin_url must be a linkedin.com/in profile URL", i)
			}
		}
		item.Set("name", name)
		item.Set("company", company)
		item.Set("linkedin_url", link)
		norm = append(norm, item)
	}
	out := wbcrud.NewObj()
	out.Set("contacts", norm)
	if mv, _ := cfg.Get("max_contacts_per_poll"); mv != nil {
		n, ok := pyIntOf(mv)
		if !ok {
			return nil, herr(422, "max_contacts_per_poll must be an int")
		}
		if n < 1 {
			return nil, herr(422, "max_contacts_per_poll must be >= 1")
		}
		out.Set("max_contacts_per_poll", json.Number(strconv.FormatInt(n, 10)))
	}
	return out, nil
}

// CreateInput is POST /watches.
type CreateInput struct {
	Kind              string
	Target            string
	LeadID            *int32
	SignalTypes       []string
	Interval          *string
	Config            any // nil, *wbcrud.Obj, or another JSON value
	CreateWebhookRule bool
	WebhookURL        *string
	WebhookSecretRef  *string
}

// Validated is the normalised create request.
type Validated struct {
	Kind, Target, Interval string
	SignalTypes            []string
	Config                 *wbcrud.Obj // job_change roster, nil otherwise
}

// ValidateCreate applies Python's create checks in Python's order, short of
// the DB-backed ones (max watches, webhook rule, lead pin).
func ValidateCreate(in CreateInput, cfg Config) (Validated, error) {
	v := Validated{Kind: in.Kind}
	if _, ok := kindSignalTypes[in.Kind]; !ok {
		return v, herr(422, "invalid kind '%s'", in.Kind)
	}
	if in.Kind == "account_group" {
		return v, herr(422, "account_group watches require an exact persisted workbook selection")
	}
	iv := ""
	if in.Interval != nil {
		iv = *in.Interval
	}
	if in.Kind == "job_change" {
		if iv == "" {
			iv = "weekly"
		}
		if iv != "weekly" {
			return v, herr(422, "job_change watches poll at most weekly (interval='weekly')")
		}
		c, err := ValidateJobChangeConfig(in.Config, cfg.JobChangeMaxContacts)
		if err != nil {
			return v, err
		}
		v.Config = c
	} else if iv == "" {
		iv = cfg.DefaultInterval
	}
	if !watchIntervals[iv] {
		return v, herr(422, "invalid interval '%s'", iv)
	}
	v.Interval = iv
	allowed := allowedSignal(in.Kind)
	st := in.SignalTypes
	if len(st) == 0 {
		st = append([]string(nil), kindSignalTypes[in.Kind]...)
		sort.Strings(st)
	}
	if bad := badSignals(st, allowed); len(bad) > 0 {
		return v, herr(422, "signal_types %s not valid for kind '%s'", reprList(bad), in.Kind)
	}
	v.SignalTypes = st
	v.Target = colgraph.PyStrip(in.Target)
	return v, nil
}

func badSignals(st []string, allowed map[string]bool) []string {
	var bad []string
	for _, s := range st {
		if !allowed[s] {
			bad = append(bad, s)
		}
	}
	return bad
}

func reprList(ss []string) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = colgraph.PyStrRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// PatchInput is PATCH /watches/{id}; nil fields are unchanged.
type PatchInput struct {
	Enabled     *bool
	Interval    *string
	SignalTypes *[]string
	LeadID      *int32
	Config      any // nil = unchanged
}

// PatchChanges are the validated field changes ValidatePatch returns.
type PatchChanges struct {
	Interval    *string
	Config      *wbcrud.Obj
	SignalTypes *[]string
}

// ValidatePatch applies the patch checks for a watch of the given kind in
// Python's order: interval, config, signal_types.
func ValidatePatch(kind string, in PatchInput, cfg Config) (PatchChanges, error) {
	var ch PatchChanges
	if in.Interval != nil {
		if !watchIntervals[*in.Interval] {
			return ch, herr(422, "invalid interval '%s'", *in.Interval)
		}
		if kind == "job_change" && *in.Interval != "weekly" {
			return ch, herr(422, "job_change watches poll at most weekly (interval='weekly')")
		}
		ch.Interval = in.Interval
	}
	if in.Config != nil {
		if kind != "job_change" {
			return ch, herr(422, "config not supported for kind '%s'", kind)
		}
		c, err := ValidateJobChangeConfig(in.Config, cfg.JobChangeMaxContacts)
		if err != nil {
			return ch, err
		}
		ch.Config = c
	}
	if in.SignalTypes != nil {
		if bad := badSignals(*in.SignalTypes, allowedSignal(kind)); len(bad) > 0 {
			return ch, herr(422, "signal_types %s not valid for kind '%s'", reprList(bad), kind)
		}
		ch.SignalTypes = in.SignalTypes
	}
	return ch, nil
}

// webhookRuleName is f"watch webhook ({kind}:{target})"[:200] (code points).
func webhookRuleName(kind, target string) string {
	s := "watch webhook (" + kind + ":" + target + ")"
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

// ---- cursor ---------------------------------------------------------------

var errBadCursor = &HTTPError{Status: 422, Detail: "invalid watch cursor"}

// EncodeCursor is _encode_watch_cursor: urlsafe base64 without padding of the
// compact JSON [created_at.isoformat(), id]. createdAt is the isoformat text
// ("" stands for a missing created_at: the Unix epoch).
func EncodeCursor(createdAt, id string) string {
	if createdAt == "" {
		createdAt = "1970-01-01T00:00:00+00:00"
	}
	b, _ := canonjson.Marshal([]any{createdAt, id}, false)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cursorOf(w Watch) string {
	if w.CreatedAt == nil {
		return EncodeCursor("", w.ID)
	}
	return EncodeCursor(autosvc.Iso(*w.CreatedAt), w.ID)
}

// DecodeCursor is _decode_watch_cursor: the instant (a naive timestamp is
// UTC) and the id. Any malformation is 422 "invalid watch cursor".
func DecodeCursor(cursor string) (time.Time, string, error) {
	var clean []byte
	for i := 0; i < len(cursor); i++ {
		c := cursor[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			clean = append(clean, c)
		}
	}
	if len(clean)%4 == 1 {
		return time.Time{}, "", errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(clean))
	if err != nil || !utf8.Valid(raw) {
		return time.Time{}, "", errBadCursor
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil || len(elems) != 2 {
		return time.Time{}, "", errBadCursor
	}
	var ts, id string
	if json.Unmarshal(elems[1], &id) != nil || !strings.HasPrefix(strings.TrimSpace(string(elems[1])), `"`) {
		return time.Time{}, "", errBadCursor
	}
	if json.Unmarshal(elems[0], &ts) != nil || !strings.HasPrefix(strings.TrimSpace(string(elems[0])), `"`) {
		return time.Time{}, "", errBadCursor
	}
	if strings.ContainsRune(id, 0) || strings.ContainsRune(ts, 0) {
		return time.Time{}, "", errBadCursor
	}
	t, ok := parseISO(ts)
	if !ok {
		return time.Time{}, "", errBadCursor
	}
	return t, id, nil
}

// parseISO is datetime.fromisoformat (Python 3.13) for calendar dates: date as
// YYYY-MM-DD or YYYYMMDD, an optional time after any one separator character
// (HH, HH:MM, HH:MM:SS[.f] or the compact forms) and an optional Z or numeric
// offset. A naive value is UTC. ISO week dates are not supported (rejected).
func parseISO(s string) (time.Time, bool) {
	r := []rune(s)
	atoi := func(rs []rune) (int, bool) {
		if len(rs) == 0 {
			return 0, false
		}
		n := 0
		for _, c := range rs {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	if len(r) < 8 {
		return time.Time{}, false
	}
	var y, mo, d int
	var ok1, ok2, ok3 bool
	dateEnd := 0
	if len(r) >= 10 && r[4] == '-' {
		if r[7] != '-' {
			return time.Time{}, false
		}
		y, ok1 = atoi(r[0:4])
		mo, ok2 = atoi(r[5:7])
		d, ok3 = atoi(r[8:10])
		dateEnd = 10
	} else {
		y, ok1 = atoi(r[0:4])
		mo, ok2 = atoi(r[4:6])
		d, ok3 = atoi(r[6:8])
		dateEnd = 8
	}
	if !ok1 || !ok2 || !ok3 || y < 1 || mo < 1 || mo > 12 || d < 1 {
		return time.Time{}, false
	}
	if d > time.Date(y, time.Month(mo)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return time.Time{}, false
	}
	h, mi, sec, ns := 0, 0, 0, 0
	loc := time.UTC
	if len(r) > dateEnd {
		rest := r[dateEnd+1:] // any single separator character
		if len(r) == dateEnd+1 {
			return time.Time{}, false
		}
		tzPos := len(rest)
		for i, c := range rest {
			if c == '+' || c == '-' || c == 'Z' {
				tzPos = i
				break
			}
		}
		tm, tz := rest[:tzPos], rest[tzPos:]
		var ok bool
		if h, mi, sec, ns, ok = parseClock(tm, atoi); !ok {
			return time.Time{}, false
		}
		if len(tz) > 0 {
			off, ok := parseOffset(tz, atoi)
			if !ok {
				return time.Time{}, false
			}
			loc = time.FixedZone("", off)
		}
	}
	return time.Date(y, time.Month(mo), d, h, mi, sec, ns, loc).UTC(), true
}

func parseClock(tm []rune, atoi func([]rune) (int, bool)) (h, mi, sec, ns int, ok bool) {
	if len(tm) < 2 {
		return
	}
	var err bool
	frac := []rune(nil)
	if i := indexRune(tm, '.'); i >= 0 {
		tm, frac = tm[:i], tm[i+1:]
		if len(frac) == 0 {
			return
		}
	}
	colon := indexRune(tm, ':') >= 0
	var parts [][]rune
	if colon {
		for _, p := range strings.Split(string(tm), ":") {
			parts = append(parts, []rune(p))
		}
		if len(parts) > 3 {
			return
		}
		for _, p := range parts {
			if len(p) != 2 {
				return
			}
		}
	} else {
		switch len(tm) {
		case 2:
			parts = [][]rune{tm}
		case 4:
			parts = [][]rune{tm[:2], tm[2:]}
		case 6:
			parts = [][]rune{tm[:2], tm[2:4], tm[4:]}
		default:
			return
		}
	}
	if frac != nil && len(parts) != 3 {
		return
	}
	vals := [3]int{}
	for i, p := range parts {
		n, good := atoi(p)
		if !good {
			err = true
		}
		vals[i] = n
	}
	if err || vals[0] > 23 || vals[1] > 59 || vals[2] > 59 {
		return
	}
	if frac != nil {
		for _, c := range frac {
			if c < '0' || c > '9' {
				return
			}
		}
		f := string(frac)
		if len(f) > 6 {
			f = f[:6]
		}
		for len(f) < 6 {
			f += "0"
		}
		us, _ := strconv.Atoi(f)
		ns = us * 1000
	}
	return vals[0], vals[1], vals[2], ns, true
}

func parseOffset(tz []rune, atoi func([]rune) (int, bool)) (int, bool) {
	if len(tz) == 1 && tz[0] == 'Z' {
		return 0, true
	}
	if tz[0] != '+' && tz[0] != '-' {
		return 0, false
	}
	sign := 1
	if tz[0] == '-' {
		sign = -1
	}
	h, mi, sec, _, ok := parseClock(tz[1:], atoi)
	if !ok || h > 23 {
		return 0, false
	}
	return sign * (h*3600 + mi*60 + sec), true
}

func indexRune(rs []rune, c rune) int {
	for i, r := range rs {
		if r == c {
			return i
		}
	}
	return -1
}
