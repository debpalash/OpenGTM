// Package outreachdraft ports services/outreach/drafting.py (plan 15 WP-COPILOT-TOOLS): evidence-grounded,
// draft-only outreach for one exact saved contact. A draft is a row in outreach_drafts (state "draft", no
// sent_at); nothing here can enqueue, schedule or send mail, and there is no draft-to-send path.
//
// Deliberate differences from Python (safety, documented in the WP report): an address on the workspace's
// suppression list is not eligible (reason "suppressed_address"); line breaks and other control characters
// in the company name never reach the subject (a later send must not see a split header); values longer
// than the outreach_drafts columns are refused with a DraftingError instead of a database error.
package outreachdraft

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Error is DraftingError: a refusal the copilot reports to the model with its per-person flags.
type Error struct {
	Msg   string
	Flags []any // []*wbcrud.Obj
}

func (e *Error) Error() string { return e.Msg }

func fail(msg string, flags []any) *Error {
	if flags == nil {
		flags = []any{}
	}
	return &Error{Msg: msg, Flags: flags}
}

var emailRE = regexp.MustCompile(`^[^\s\p{Z}\x{1c}-\x{1f}\x{85}@]+@[^\s\p{Z}\x{1c}-\x{1f}\x{85}@]+\.[^\s\p{Z}\x{1c}-\x{1f}\x{85}@]+$`)

var functionTerms = []string{"partnership", "alliances", "alliance", "partner development", "partner ecosystem",
	"channel partner", "business development"}

var genericLocalParts = []string{"admin", "contact", "hello", "info", "office", "partners", "partnerships", "sales",
	"support", "team"}

var seniority = []struct {
	term   string
	points int64
}{{"chief", 5}, {"vice president", 5}, {"vp", 5}, {"head", 4}, {"director", 3}, {"lead", 2}, {"manager", 1}}

// ---- Python value helpers over ordered JSON trees ----

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func pyFloat(f float64) string {
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return s
}

// pyStr is str() of a JSON value (containers render like Python's repr).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyFloat(x)
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return pyFloat(f)
			}
		}
		return s
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch x := v.(type) {
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		s := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(x)
		if q == "'" {
			s = strings.ReplaceAll(s, "'", `\'`)
		}
		return q + s + q
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *wbcrud.Obj:
		keys := x.Keys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			e, _ := x.Get(k)
			parts[i] = pyRepr(k) + ": " + pyRepr(e)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStr(v)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case *wbcrud.Obj:
		return x.Len() > 0
	}
	return true
}

// text is _text: str(value).strip(), "" for None.
func text(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimFunc(pyStr(v), isPySpace)
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func dict(v any) *wbcrud.Obj {
	if o, ok := v.(*wbcrud.Obj); ok && o != nil {
		return o
	}
	return wbcrud.NewObj()
}

func at(o *wbcrud.Obj, k string) any {
	v, _ := o.Get(k)
	return v
}

// or is Python's `a or b` over raw values.
func or(vals ...any) any {
	for _, v := range vals[:len(vals)-1] {
		if truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

// number is isinstance(v, (int, float)) (bool included) as a float.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		return f, err == nil
	}
	return 0, false
}

// isHTTPURL is _is_http_url (urlsplit: scheme http/https and a non-empty netloc).
func isHTTPURL(v any) bool {
	s := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, text(v))
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return false
	}
	scheme := strings.ToLower(s[:i])
	if scheme != "http" && scheme != "https" {
		return false
	}
	rest := s[i+1:]
	if !strings.HasPrefix(rest, "//") {
		return false
	}
	rest = rest[2:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return rest != ""
}

func genericEmail(email string) bool {
	local := strings.ToLower(email)
	if i := strings.LastIndexByte(local, '@'); i >= 0 {
		local = local[:i]
	}
	for _, g := range genericLocalParts {
		if local == g || strings.HasPrefix(local, g+"+") {
			return true
		}
	}
	return false
}

func hasValidVerifier(contact *wbcrud.Obj) bool {
	for _, a := range list(at(contact, "attempts")) {
		o, ok := a.(*wbcrud.Obj)
		if ok && text(at(o, "stage")) == "verification" && strings.ToLower(text(at(o, "status"))) == "valid" {
			return true
		}
	}
	return false
}

// round4 is round(x, 4).
func round4(f float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 4, 64), 64)
	return r
}

func evidenceSources(person *wbcrud.Obj) []any {
	observed := text(or(at(person, "checked_at"), at(person, "retrieved_at"), at(dict(at(person, "contactability")), "observed_at")))
	confidence, ok := number(at(person, "verification_confidence"))
	if !ok {
		if confidence, ok = number(at(person, "confidence")); !ok {
			confidence = 0
		}
	}
	type cand struct{ url, label string }
	var cands []cand
	for _, s := range list(at(person, "verification_sources")) {
		if o, ok := s.(*wbcrud.Obj); ok {
			cands = append(cands, cand{text(at(o, "url")), text(or(at(o, "label"), at(o, "title")))})
		}
	}
	cands = append(cands, cand{text(at(person, "evidence_url")), "Saved role evidence"},
		cand{text(at(person, "linkedin_url")), "Saved public profile"})
	out := []any{}
	if observed == "" {
		return out
	}
	seen := map[string]bool{}
	for _, c := range cands {
		if !isHTTPURL(c.url) || seen[c.url] {
			continue
		}
		seen[c.url] = true
		label := c.label
		if label == "" {
			label = "Saved public evidence"
		}
		e := wbcrud.NewObj()
		e.Set("source_url", c.url)
		e.Set("label", label)
		e.Set("observed_at", observed)
		e.Set("confidence", round4(math.Max(0, math.Min(confidence, 1))))
		out = append(out, e)
	}
	return out
}

// Candidate is one eligible saved contact.
type Candidate struct {
	Person                                         *wbcrud.Obj
	PersonID, Name, Title, Function, Email, Status string
	GenericInbox, RoleAddress                      bool
	Evidence                                       []any
	Score                                          int64
}

// Obj is the selected candidate without the person (the Python dict minus "person").
func (c *Candidate) Obj() *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("person_id", c.PersonID)
	o.Set("name", c.Name)
	o.Set("title", c.Title)
	o.Set("function", c.Function)
	o.Set("email", c.Email)
	o.Set("contact_status", c.Status)
	o.Set("generic_inbox", c.GenericInbox)
	o.Set("is_role_address", c.RoleAddress)
	o.Set("evidence", c.Evidence)
	o.Set("score", c.Score)
	return o
}

func candidate(person *wbcrud.Obj, allowRisky bool, suppressed func(string) bool) (*Candidate, *wbcrud.Obj) {
	personID := text(at(person, "person_id"))
	name := text(or(at(person, "name"), at(person, "full_name")))
	title := text(or(at(person, "title"), at(person, "contact_title")))
	function := text(at(person, "function"))
	contact := dict(at(person, "contactability"))
	email := strings.ToLower(text(or(at(contact, "email"), at(person, "email"))))
	status := strings.ToLower(text(at(contact, "status")))
	role := truthy(at(contact, "is_role"))
	generic := email != "" && genericEmail(email)
	flag := wbcrud.NewObj()
	flag.Set("person_id", personID)
	flag.Set("person_name", name)
	if email != "" {
		flag.Set("email", email)
	} else {
		flag.Set("email", nil)
	}
	if status != "" {
		flag.Set("contact_status", status)
	} else {
		flag.Set("contact_status", "unavailable")
	}
	flag.Set("generic_inbox", generic)
	flag.Set("is_role_address", role)
	flag.Set("reason", "")
	reject := func(reason string) (*Candidate, *wbcrud.Obj) {
		flag.Set("reason", reason)
		return nil, flag
	}
	low := strings.ToLower(title + " " + function)
	switch {
	case personID == "" || name == "":
		return reject("missing_person_identity")
	case !emailRE.MatchString(email):
		return reject("missing_valid_email")
	case generic || role:
		return reject("generic_or_role_address")
	case status == "verified" && !hasValidVerifier(contact):
		return reject("verified_without_valid_verifier")
	case status != "verified" && status != "risky":
		return reject("contact_not_eligible")
	case status == "risky" && !allowRisky:
		return reject("risky_address_requires_approval")
	case !containsAny(low, functionTerms):
		return reject("partnership_function_not_supported")
	}
	evidence := evidenceSources(person)
	if len(evidence) == 0 {
		return reject("personalization_evidence_missing")
	}
	if suppressed != nil && suppressed(email) {
		return reject("suppressed_address") // Go only: a suppressed address is never drafted to
	}
	var points int64
	lt := strings.ToLower(title)
	for _, s := range seniority {
		if strings.Contains(lt, s.term) {
			points = s.points
			break
		}
	}
	vc, ok := number(at(contact, "verification_confidence"))
	if !ok {
		vc = 0
	}
	base := int64(50)
	if status == "verified" {
		base = 100
	}
	score := base + points*5 + int64(math.RoundToEven(vc*10)) + int64(min(len(evidence), 3))
	if function == "" {
		function = "partnerships"
	}
	return &Candidate{Person: person, PersonID: personID, Name: name, Title: title, Function: function, Email: email,
		Status: status, GenericInbox: generic, RoleAddress: role, Evidence: evidence, Score: score}, flag
}

func containsAny(s string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// Selection is select_best_contact's result.
type Selection struct {
	Selected *Candidate
	Flags    []any // one flag object per considered person
}

// Emails lists every lower-cased address of the saved people (for the caller's suppression lookup).
func Emails(source *wbcrud.Obj) []string {
	var out []string
	for _, p := range list(at(source, "people")) {
		if o, ok := p.(*wbcrud.Obj); ok {
			if e := strings.ToLower(text(or(at(dict(at(o, "contactability")), "email"), at(o, "email")))); e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}

// SelectBestContact is select_best_contact: the best eligible person (or the requested one) and a flag
// per person. suppressed (nil: none) reports addresses on the workspace suppression list.
func SelectBestContact(source *wbcrud.Obj, requested string, allowRisky bool, suppressed func(string) bool) (*Selection, *Error) {
	var people []*wbcrud.Obj
	for _, p := range list(at(source, "people")) {
		if o, ok := p.(*wbcrud.Obj); ok {
			people = append(people, o)
		}
	}
	if req := text(requested); req != "" {
		var keep []*wbcrud.Obj
		for _, p := range people {
			if text(at(p, "person_id")) == req {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			return nil, fail("Selected person is not in the saved contact result", nil)
		}
		people = keep
	}
	var eligible []*Candidate
	flags := []any{}
	for _, p := range people {
		c, flag := candidate(p, allowRisky, suppressed)
		flags = append(flags, flag)
		if c != nil {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil, fail("No saved contact has an eligible evidence-backed address for drafting", flags)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Score != eligible[j].Score {
			return eligible[i].Score > eligible[j].Score
		}
		return eligible[i].PersonID < eligible[j].PersonID
	})
	return &Selection{Selected: eligible[0], Flags: flags}, nil
}

func claimID(personID, claim string) string {
	sum := sha256.Sum256([]byte(personID + "|" + claim))
	return "claim_" + hex.EncodeToString(sum[:])[:16]
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// ContractHash is _contract_hash.
func ContractHash(sourceActionID string, c *Candidate, allowRisky bool) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{sourceActionID, c.PersonID, c.Email, c.Status, pyBool(allowRisky)}, "|")))
	return hex.EncodeToString(sum[:])
}
