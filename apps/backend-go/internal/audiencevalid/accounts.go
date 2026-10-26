package audiencevalid

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// ErrTooLarge is returned when an audience has more members than maxMembers
// (documented deviation D3: Python rolled up an unbounded audience).
var ErrTooLarge = errors.New("audience too large for accounts rollup")

// Member is one audience member row: the lead id and its snapshot JSON.
type Member struct {
	LeadID   int
	Snapshot map[string]any
}

// Signal is the signal row count and weight sum of one lead.
type Signal struct{ Count, Weight int }

// PyFloat marshals like Python's json.dumps of a float (40.0, 7.5).
type PyFloat float64

func (f PyFloat) MarshalJSON() ([]byte, error) {
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return nil, errors.New("non-finite float in accounts rollup")
	}
	return []byte(pyFloatRepr(float64(f))), nil
}

// Profile is one of the first 10 members of an account.
type Profile struct {
	LeadID int `json:"lead_id"`
	Name   any `json:"name"`
	Title  any `json:"title"`
	Email  any `json:"email"`
}

// Account keeps Python's key order.
type Account struct {
	Key              string    `json:"key"`
	Company          string    `json:"company"`
	Website          any       `json:"website"`
	Contacts         int       `json:"contacts"`
	WithEmail        int       `json:"with_email"`
	WithPhone        int       `json:"with_phone"`
	DecisionMakers   int       `json:"decision_makers"`
	SignalCount      int       `json:"signal_count"`
	SignalWeight     int       `json:"signal_weight"`
	Profiles         []Profile `json:"profiles"`
	AvgScore         PyFloat   `json:"avg_score"`
	EmailCoveragePct int       `json:"email_coverage_pct"`
	PhoneCoveragePct int       `json:"phone_coverage_pct"`

	scoreTotal float64
}

type Summary struct {
	AccountCount               int `json:"account_count"`
	ContactCount               int `json:"contact_count"`
	AccountsWithSignals        int `json:"accounts_with_signals"`
	AccountsWithDecisionMakers int `json:"accounts_with_decision_makers"`
}

type Pagination struct {
	Limit      int  `json:"limit"`
	Offset     int  `json:"offset"`
	NextOffset *int `json:"next_offset"`
	Total      int  `json:"total"`
}

// Result is the accounts response body.
type Result struct {
	Accounts   []Account  `json:"accounts"`
	Summary    Summary    `json:"summary"`
	Pagination Pagination `json:"pagination"`
}

// hostname returns urlparse(url).hostname for the website string, mirroring
// CPython 3.13 urlsplit (control-char stripping, scheme split, netloc, bracket
// validation) without the NFKC netloc check. ok is false where Python raises
// ValueError or returns no hostname.
func hostname(website string) (string, bool) {
	u := website
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	u = strings.TrimLeft(u, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	u = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
	if i := strings.IndexByte(u, ':'); i > 0 && isASCIIAlpha(u[0]) {
		valid := true
		for j := 0; j < i; j++ {
			c := u[j]
			if !(isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
				valid = false
				break
			}
		}
		if valid {
			u = u[i+1:]
		}
	}
	if !strings.HasPrefix(u, "//") {
		return "", false
	}
	netloc := u[2:]
	if i := strings.IndexAny(netloc, "/?#"); i >= 0 {
		netloc = netloc[:i]
	}
	hasOpen, hasClose := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
	if hasOpen != hasClose {
		return "", false
	}
	if hasOpen {
		_, rest, _ := strings.Cut(netloc, "[")
		bracketed, _, _ := strings.Cut(rest, "]")
		if !validBracketedHost(bracketed) {
			return "", false
		}
	}
	info := netloc[strings.LastIndexByte(netloc, '@')+1:]
	var host string
	if _, rest, found := strings.Cut(info, "["); found {
		host, _, _ = strings.Cut(rest, "]")
	} else {
		host, _, _ = strings.Cut(info, ":")
	}
	if host == "" {
		return "", false
	}
	h, zone, hasZone := strings.Cut(host, "%")
	h = pyLower(h)
	if hasZone {
		h += "%" + zone
	}
	return h, true
}

func isASCIIAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// validBracketedHost is urllib's _check_bracketed_host: an IPv6 literal or an
// IPvFuture ("v" hex+ "." anything).
func validBracketedHost(h string) bool {
	if strings.HasPrefix(h, "v") {
		dot := strings.IndexByte(h, '.')
		if dot < 2 || dot == len(h)-1 {
			return false
		}
		for _, c := range h[1:dot] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && !a.Is4()
}

// AccountKey is _account_key: the lower-cased website's hostname without a
// leading "www.", else "company:" plus the whitespace-collapsed lower-cased
// company. An unparseable URL (where Python's urlparse raises ValueError and
// the request 500s) falls back to the company key instead (deviation D3).
func AccountKey(snapshot map[string]any) (key, company string) {
	var site string
	if w := snapshot["website"]; truthy(w) {
		site = pyLower(pyStrip(pyStr(w)))
	}
	if site != "" {
		if h, ok := hostname(site); ok {
			if domain := strings.TrimPrefix(h, "www."); domain != "" {
				company = domain
				if c := snapshot["company"]; truthy(c) {
					company = pyStr(c)
				}
				return domain, company
			}
		}
	}
	company = "Unknown account"
	if c := snapshot["company"]; truthy(c) {
		company = pyStr(c)
	}
	return "company:" + pyCollapse(pyLower(company)), company
}

var decisionMakerTerms = []string{"chief", "ceo", "cto", "cmo", "vp", "vice president", "head", "director", "owner", "founder"}

func isDecisionMaker(title string) bool {
	t := pyLower(title)
	for _, term := range decisionMakerTerms {
		if strings.Contains(t, term) {
			return true
		}
	}
	return false
}

// pyFloat is float(v) for a snapshot score (callers guard with truthiness).
func pyFloat(v any) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		return x, nil
	case json.Number:
		return strconv.ParseFloat(string(x), 64)
	case string:
		return strconv.ParseFloat(pyStrip(x), 64)
	}
	if b, ok := toBig(v); ok {
		f, _ := strconv.ParseFloat(b.String(), 64)
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not %T", v)
}

// Rollup is list_audience_accounts after the queries: members (id order) are
// grouped by AccountKey, signals are the per-lead counts and weights, and the
// accounts sort by (-signal_weight, -avg_score, -contacts, company.lower())
// with first-seen order breaking ties. More than maxMembers members yield
// ErrTooLarge; an iterator error is returned as is.
func Rollup(members iter.Seq2[Member, error], signals map[int]Signal, limit, offset, maxMembers int) (Result, error) {
	byKey := map[string]*Account{}
	var order []*Account
	contactCount := 0
	for m, err := range members {
		if err != nil {
			return Result{}, err
		}
		contactCount++
		if contactCount > maxMembers {
			return Result{}, ErrTooLarge
		}
		snap := m.Snapshot
		key, company := AccountKey(snap)
		a := byKey[key]
		if a == nil {
			website := snap["website"]
			if !truthy(website) {
				website = ""
			}
			a = &Account{Key: key, Company: company, Website: website, Profiles: []Profile{}}
			byKey[key] = a
			order = append(order, a)
		}
		a.Contacts++
		if truthy(snap["email"]) {
			a.WithEmail++
		}
		if truthy(snap["phone"]) {
			a.WithPhone++
		}
		if truthy(snap["contact_title"]) && isDecisionMaker(pyStr(snap["contact_title"])) {
			a.DecisionMakers++
		}
		if truthy(snap["score"]) {
			f, err := pyFloat(snap["score"])
			if err != nil {
				return Result{}, fmt.Errorf("lead %d score: %w", m.LeadID, err)
			}
			a.scoreTotal += f
		}
		s := signals[m.LeadID]
		a.SignalCount += s.Count
		a.SignalWeight += s.Weight
		if len(a.Profiles) < 10 {
			or := func(v any, def any) any {
				if truthy(v) {
					return v
				}
				return def
			}
			a.Profiles = append(a.Profiles, Profile{
				LeadID: m.LeadID,
				Name:   or(snap["contact_person"], fmt.Sprintf("Lead %d", m.LeadID)),
				Title:  or(snap["contact_title"], ""),
				Email:  or(snap["email"], ""),
			})
		}
	}
	rows := make([]Account, len(order))
	for i, a := range order {
		c := float64(a.Contacts)
		a.AvgScore = PyFloat(PyRound(a.scoreTotal/c, 1))
		a.EmailCoveragePct = int(PyRound(float64(a.WithEmail)/c*100, 0))
		a.PhoneCoveragePct = int(PyRound(float64(a.WithPhone)/c*100, 0))
		rows[i] = *a
	}
	lower := make([]string, len(rows))
	for i := range rows {
		lower[i] = pyLower(rows[i].Company)
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		a, b := &rows[idx[x]], &rows[idx[y]]
		switch {
		case a.SignalWeight != b.SignalWeight:
			return a.SignalWeight > b.SignalWeight
		case a.AvgScore != b.AvgScore:
			return a.AvgScore > b.AvgScore
		case a.Contacts != b.Contacts:
			return a.Contacts > b.Contacts
		}
		return lower[idx[x]] < lower[idx[y]]
	})
	sorted := make([]Account, len(rows))
	sum := Summary{AccountCount: len(rows), ContactCount: contactCount}
	for i, j := range idx {
		sorted[i] = rows[j]
		if rows[j].SignalCount > 0 {
			sum.AccountsWithSignals++
		}
		if rows[j].DecisionMakers > 0 {
			sum.AccountsWithDecisionMakers++
		}
	}
	total := len(sorted)
	lo := min(max(offset, 0), total)
	hi := min(lo+max(limit, 0), total)
	page := append([]Account{}, sorted[lo:hi]...)
	pg := Pagination{Limit: limit, Offset: offset, Total: total}
	if offset+len(page) < total {
		n := offset + len(page)
		pg.NextOffset = &n
	}
	return Result{Accounts: page, Summary: sum, Pagination: pg}, nil
}

// MarshalJSON encodes without HTML escaping, like Python's json.dumps.
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(plain(r)); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
