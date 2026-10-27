package copilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports the people tools of services/copilot/tools.py (plan 15 WP-COPILOT-TOOLS):
// find_people_at_company, verify_people_at_company, enrich_people_contacts and create_people_workbook.
// Research, verification, contact enrichment and the durable contact spend boundary are internal/leadpeople
// (WP-PEOPLE) behind PeopleTools; the person identity is leadpeople.PersonEntityID / PeopleResultSetID.

// PeopleTools is what the people tools need from internal/leadpeople. LeadPeople is the production
// implementation; tests use fakes.
type PeopleTools interface {
	Research(ctx context.Context, company, function string, o leadpeople.ResearchOptions) (map[string]any, error)
	Verify(ctx context.Context, company, function string, people []map[string]any) (map[string]any, error)
	EnrichContacts(ctx context.Context, company, function string, people []map[string]any, o leadpeople.ContactOptions) (map[string]any, error)
	ExecuteContactOnce(ctx context.Context, ws, actionID string, contract any, op func(context.Context) (map[string]any, error)) (map[string]any, error)
}

// LeadPeople adapts *leadpeople.Service and *leadpeople.ContactExecutor to PeopleTools.
type LeadPeople struct {
	Service  *leadpeople.Service
	Executor *leadpeople.ContactExecutor
}

// ContactExecutionTimeout is execute_contact_once's default claim timeout.
const ContactExecutionTimeout = 900 * time.Second

func (l LeadPeople) Research(ctx context.Context, company, function string, o leadpeople.ResearchOptions) (map[string]any, error) {
	return l.Service.ResearchPeopleAtCompany(ctx, company, function, o)
}

func (l LeadPeople) Verify(ctx context.Context, company, function string, people []map[string]any) (map[string]any, error) {
	return l.Service.VerifyPeopleAtCompany(ctx, company, function, people, 15)
}

func (l LeadPeople) EnrichContacts(ctx context.Context, company, function string, people []map[string]any, o leadpeople.ContactOptions) (map[string]any, error) {
	return l.Service.EnrichPeopleContacts(ctx, company, function, people, o)
}

func (l LeadPeople) ExecuteContactOnce(ctx context.Context, ws, actionID string, contract any, op func(context.Context) (map[string]any, error)) (map[string]any, error) {
	return l.Executor.ExecuteOnce(ctx, ws, actionID, contract, op, ContactExecutionTimeout)
}

// PersonObservation is entities.resolve_person's input for one saved person.
type PersonObservation struct {
	Name, Company, Source, Title, CompanyDomain, LinkedInURL, Email, EvidenceURL string
	LegacyIDs                                                                    []string
}

// PersonResolver is entities.resolve_person inside the caller's transaction; it returns the canonical
// person id.
type PersonResolver func(ctx context.Context, tx pgx.Tx, ws string, o PersonObservation) (string, error)

// ---- value helpers ----

// goTree converts a Go value from leadpeople (maps, slices, Go numbers) to a tree wbcrud renders like
// json.dumps: maps become key-sorted objects (Python's insertion order is not available), floats keep
// their Python repr ("1.0").
func goTree(v any) any {
	switch x := v.(type) {
	case nil, string, bool, int64, float64, json.Number:
		return x
	case *wbcrud.Obj:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return float64(x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := wbcrud.NewObj()
		for _, k := range keys {
			o.Set(k, goTree(x[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = goTree(e)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = goTree(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	o, err := wbcrud.ParseObjectCanon([]byte(`{"v": ` + string(b) + `}`))
	if err != nil {
		return nil
	}
	r, _ := o.Get("v")
	return r
}

// pyDumps is json.dumps of a tree (ordered objects keep their order).
func pyDumps(v any) (string, error) {
	w := wbcrud.NewObj()
	w.Set("v", goTree(v))
	b, err := w.MarshalJSON()
	if err != nil {
		return "", err
	}
	r, err := ssestream.Reformat(b[len(`{"v":`) : len(b)-1])
	return string(r), err
}

// toPlain converts an ordered tree to canonjson shapes (map[string]any, int64, float64) for leadpeople.
func toPlain(v any) any {
	w := wbcrud.NewObj()
	w.Set("v", v)
	b, err := w.MarshalJSON()
	if err != nil {
		return nil
	}
	d, err := canonjson.Decode(b)
	if err != nil {
		return nil
	}
	return d.(map[string]any)["v"]
}

func plainMap(o *wbcrud.Obj) map[string]any {
	m, _ := toPlain(o).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func oget(o *wbcrud.Obj, k string) any {
	v, _ := o.Get(k)
	return v
}

// pyStripStr is str(v).strip() for a value that is not None.
func pyStripStr(v any) string { return strings.TrimFunc(pyStr(v), isPySpaceRune) }

// orStr is str(v or dflt).strip().
func stripOr(v any, dflt string) string {
	if pyTruthy(v) {
		return pyStripStr(v)
	}
	return strings.TrimFunc(dflt, isPySpaceRune)
}

// pyTitle is str.title().
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			if prevCased {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToTitle(r))
			}
			prevCased = true
		default:
			b.WriteRune(r)
			prevCased = false
		}
	}
	return b.String()
}

// dictPeople is [dict(p) for p in people if isinstance(p, dict)] over an ordered list.
func dictPeople(v any) []*wbcrud.Obj {
	l, _ := v.([]any)
	var out []*wbcrud.Obj
	for _, p := range l {
		if o, ok := p.(*wbcrud.Obj); ok {
			c := wbcrud.NewObj()
			for _, k := range o.Keys() {
				c.Set(k, oget(o, k))
			}
			out = append(out, c)
		}
	}
	return out
}

// personID is leadpeople.PersonEntityID over an ordered person.
func personID(company string, p *wbcrud.Obj) string {
	return leadpeople.PersonEntityID(company, plainMap(p))
}

func resultSetID(company, function string, people []*wbcrud.Obj) string {
	ps := make([]map[string]any, len(people))
	for i, p := range people {
		ps[i] = plainMap(p)
	}
	return leadpeople.PeopleResultSetID(company, function, ps)
}

// requestedIDs is list(dict.fromkeys(str(x).strip() for x in ids if str(x).strip())).
func requestedIDs(v any) []string {
	l, _ := v.([]any)
	var out []string
	seen := map[string]bool{}
	for _, x := range l {
		s := pyStripStr(x)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func digest16(ids []string) string {
	s := append([]string(nil), ids...)
	sort.Strings(s)
	sum := sha256.Sum256([]byte(strings.Join(s, "|")))
	return hex.EncodeToString(sum[:])[:16]
}

// ---- ordered conversation lookups ----

// orderedResult re-reads a qualifying tool row's result with its key order.
func orderedResult(m chathistory.Message) *wbcrud.Obj {
	o, err := wbcrud.ParseObjectCanon([]byte(*m.ToolData))
	if err != nil {
		return nil
	}
	r, _ := oget(o, "result").(*wbcrud.Obj)
	return r
}

// latestOrdered is latest_conversation_tool_result with the result's key order kept.
func latestOrdered(msgs []chathistory.Message, names ...string) (string, *wbcrud.Obj, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		p, good := parseToolRow(msgs[i])
		if !good {
			continue
		}
		s, isStr := p.name.(string)
		if !isStr {
			continue
		}
		for _, n := range names {
			if s == n {
				if r := orderedResult(msgs[i]); r != nil {
					return s, r, true
				}
			}
		}
	}
	return "", nil, false
}

// byActionOrdered is conversation_tool_result_by_action_id with the result's key order kept.
func byActionOrdered(msgs []chathistory.Message, name, actionID string) (*wbcrud.Obj, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		p, good := parseToolRow(msgs[i])
		if !good {
			continue
		}
		if s, isStr := p.name.(string); !isStr || s != name {
			continue
		}
		if a, isStr := p.result["action_id"].(string); isStr && a == actionID {
			if r := orderedResult(msgs[i]); r != nil {
				return r, true
			}
		}
	}
	return nil, false
}

// conversation loads an owned conversation's messages; found=false when it is not this user's.
func (t *Turn) conversation(ctx context.Context, convID string) (bool, []chathistory.Message, error) {
	var found bool
	var msgs []chathistory.Message
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		c, err := chathistory.Get(ctx, tx, convID, t.WS, t.UID)
		if err != nil || c == nil {
			return err
		}
		found = true
		msgs, err = chathistory.Messages(ctx, tx, convID, t.WS, t.UID)
		return err
	})
	return found, msgs, err
}

func objErr(pairs ...any) (string, bool, error) {
	o := wbcrud.NewObj()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	s, err := objJSON(o)
	return s, err == nil, err
}

// ---- tools ----

func titleList(v any) []string {
	l, _ := v.([]any)
	out := []string{}
	for _, x := range l {
		out = append(out, pyStr(x))
	}
	return out
}

func (t *Turn) findPeople(ctx context.Context, a map[string]any) (string, bool, error) {
	people := t.Engine.People
	limit := int64(8)
	if v, present := a["limit"]; present {
		if !pyTruthy(v) {
			v = int64(8)
		}
		n, msg := autopilot.IntOf(v)
		if msg != "" {
			return toolErr("find_people_at_company", msg), true, nil
		}
		limit = n
	}
	if limit == 0 {
		limit = 8
	}
	limit = max(1, min(limit, 15))
	res, err := people.Research(ctx, stripOr(a["company"], ""), stripOr(a["function"], ""), leadpeople.ResearchOptions{
		Titles: titleList(a["titles"]), Location: stripOr(a["location"], ""), Limit: int(limit)})
	if err != nil {
		return "", false, err
	}
	s, err := pyDumps(res)
	return s, err == nil, err
}

func plainPeople(v any) []map[string]any {
	l, _ := v.([]any)
	out := []map[string]any{}
	for _, p := range l {
		switch x := p.(type) {
		case map[string]any:
			out = append(out, x)
		case *wbcrud.Obj:
			out = append(out, plainMap(x))
		}
	}
	return out
}

func (t *Turn) verifyPeople(ctx context.Context, a map[string]any) (string, bool, error) {
	res, err := t.Engine.People.Verify(ctx, stripOr(a["company"], ""), stripOr(a["function"], ""), plainPeople(a["people"]))
	if err != nil {
		return "", false, err
	}
	s, err := pyDumps(res)
	return s, err == nil, err
}

func (t *Turn) enrichContacts(ctx context.Context, a map[string]any) (string, bool, error) {
	convID := stripOr(a["conversation_id"], "")
	found := false
	var msgs []chathistory.Message
	if convID != "" {
		var err error
		if found, msgs, err = t.conversation(ctx, convID); err != nil {
			return "", false, err
		}
	}
	if !found {
		return errObj("Conversation not found"), true, nil
	}
	_, research, ok := latestOrdered(msgs, "verify_people_at_company", "find_people_at_company")
	if !ok {
		return errObj("No people research result found in this conversation"), true, nil
	}
	company := stripOr(oget(research, "company"), "")
	function := stripOr(oget(research, "function"), "")
	people := dictPeople(oget(research, "people"))
	if len(people) > 100 {
		people = people[:100]
	}
	for _, p := range people {
		p.Set("person_id", personID(company, p))
	}
	if len(people) == 0 {
		return errObj("The latest people research result has no rows"), true, nil
	}
	requested := requestedIDs(a["person_ids"])
	known := map[string]bool{}
	for _, p := range people {
		known[oget(p, "person_id").(string)] = true
	}
	var unknown []string
	for _, id := range requested {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return objErr("error", "Unknown people selection", "unknown_person_ids", anyStrings(unknown))
	}
	selected := requested
	if len(selected) == 0 {
		for _, p := range people {
			selected = append(selected, oget(p, "person_id").(string))
		}
	}
	setID := stripOr(oget(research, "result_set_id"), "")
	if setID == "" {
		setID = resultSetID(company, function, people)
	}
	supplied := stripOr(a["idempotency_key"], "")
	if utf8.RuneCountInString(supplied) > 255 {
		return errObj("Idempotency key exceeds 255 characters"), true, nil
	}
	actionID := supplied
	if actionID == "" {
		actionID = "chat-contacts:" + convID + ":" + setID + ":" + digest16(selected)
	}
	previous, hasPrev := byActionOrdered(msgs, "enrich_people_contacts", actionID)
	prevOK := hasPrev && pyTruthy(oget(previous, "ok"))
	if prevOK {
		var prevSel []string
		if l, ok := oget(previous, "selected_person_ids").([]any); ok {
			for _, x := range l {
				prevSel = append(prevSel, pyStripStr(x))
			}
		}
		a, b := append([]string(nil), prevSel...), append([]string(nil), selected...)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, "\x00") != strings.Join(b, "\x00") || len(a) != len(b) {
			return objErr("error", "Idempotency key conflicts with a different people selection", "action_id", actionID)
		}
	}
	resolution, _ := oget(research, "company_resolution").(*wbcrud.Obj)
	if resolution == nil {
		resolution = wbcrud.NewObj()
	}
	sel := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
	}
	var contractPeople []*wbcrud.Obj
	for _, p := range people {
		if sel[oget(p, "person_id").(string)] {
			contractPeople = append(contractPeople, p)
		}
	}
	sort.SliceStable(contractPeople, func(i, j int) bool {
		return oget(contractPeople[i], "person_id").(string) < oget(contractPeople[j], "person_id").(string)
	})
	cp := make([]any, len(contractPeople))
	for i, p := range contractPeople {
		cp[i] = plainMap(p)
	}
	contract := map[string]any{"conversation_id": convID, "company": company, "function": function,
		"company_resolution": plainMap(resolution), "people": cp}
	plainPeopleList := make([]map[string]any, len(people))
	for i, p := range people {
		plainPeopleList[i] = plainMap(p)
	}
	op := func(ctx context.Context) (map[string]any, error) {
		if prevOK {
			if oget(previous, "company") != company || oget(previous, "function") != function {
				return map[string]any{"ok": false, "error": "Idempotency key conflicts with a different contact contract",
					"action_id": actionID}, nil
			}
			m := plainMap(previous)
			m["reused"] = true
			return m, nil
		}
		return t.Engine.People.EnrichContacts(ctx, company, function, plainPeopleList, leadpeople.ContactOptions{
			CompanyResolution: plainMap(resolution), PersonIDs: selected, WorkspaceID: t.WS, ActionID: actionID})
	}
	res, err := t.Engine.People.ExecuteContactOnce(ctx, t.WS, actionID, contract, op)
	if err != nil {
		return "", false, err
	}
	s, err := pyDumps(res)
	return s, err == nil, err
}

// peopleColumns is create_people_workbook's columns_config.
const peopleColumns = `[{"id": "full_name", "name": "Person", "type": "lead_field", "lead_field": "full_name", "width": 200}, ` +
	`{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 180}, ` +
	`{"id": "title", "name": "Title", "type": "lead_field", "lead_field": "title", "width": 220}, ` +
	`{"id": "function", "name": "Function", "type": "lead_field", "lead_field": "function", "width": 160}, ` +
	`{"id": "location", "name": "Location", "type": "lead_field", "lead_field": "location", "width": 150}, ` +
	`{"id": "linkedin_url", "name": "LinkedIn", "type": "lead_field", "lead_field": "linkedin_url", "width": 240}, ` +
	`{"id": "verification_status", "name": "Verification", "type": "lead_field", "lead_field": "verification_status", "width": 190}, ` +
	`{"id": "confidence", "name": "Confidence", "type": "lead_field", "lead_field": "confidence", "width": 110}, ` +
	`{"id": "evidence_url", "name": "Original Evidence", "type": "lead_field", "lead_field": "evidence_url", "width": 240}, ` +
	`{"id": "verification_evidence_url", "name": "Fresh Evidence", "type": "lead_field", "lead_field": "verification_evidence_url", "width": 240}, ` +
	`{"id": "checked_at", "name": "Checked", "type": "lead_field", "lead_field": "checked_at", "width": 120}, ` +
	`{"id": "email", "name": "Email", "type": "lead_field", "lead_field": "email", "width": 210}, ` +
	`{"id": "email_status", "name": "Email Status", "type": "lead_field", "lead_field": "email_status", "width": 140}, ` +
	`{"id": "email_finder", "name": "Email Finder", "type": "lead_field", "lead_field": "email_finder", "width": 140}, ` +
	`{"id": "email_verifier", "name": "Email Verifier", "type": "lead_field", "lead_field": "email_verifier", "width": 140}]`

// peopleWorkbookName is f"{company} — {function.title()} People".
func peopleWorkbookName(company, function string) string {
	return company + " — " + pyTitle(function) + " People"
}

func (t *Turn) createPeopleWorkbook(ctx context.Context, a map[string]any) (string, bool, error) {
	convID := stripOr(a["conversation_id"], "")
	found := false
	var msgs []chathistory.Message
	if convID != "" {
		var err error
		if found, msgs, err = t.conversation(ctx, convID); err != nil {
			return "", false, err
		}
	}
	if !found {
		return errObj("Conversation not found"), true, nil
	}
	priorName, research, ok := latestOrdered(msgs, "enrich_people_contacts", "verify_people_at_company", "find_people_at_company")
	if !ok {
		return errObj("No people research result found in this conversation"), true, nil
	}
	if !pyTruthy(oget(research, "people")) {
		return errObj("The latest people research result has no rows"), true, nil
	}
	company := stripOr(oget(research, "company"), "Target Company")
	function := stripOr(oget(research, "function"), "Team")
	resolution, _ := oget(research, "company_resolution").(*wbcrud.Obj)
	if resolution == nil {
		resolution = wbcrud.NewObj()
	}
	canonDomain := strings.ToLower(stripOr(oget(resolution, "canonical_domain"), ""))
	raw, isList := oget(research, "people").([]any)
	if !isList {
		return "", false, errors.New("copilot: people result is not a list")
	}
	if len(raw) > 100 {
		raw = raw[:100]
	}
	var people []*wbcrud.Obj
	byID := map[string]*wbcrud.Obj{}
	for _, p := range dictPeople(raw) {
		id := personID(company, p)
		p.Set("person_id", id)
		if byID[id] == nil {
			people = append(people, p)
			byID[id] = p
		}
	}
	if len(people) == 0 {
		return errObj("The latest people research result has no valid rows"), true, nil
	}
	requested := requestedIDs(a["person_ids"])
	var unknown []string
	for _, id := range requested {
		if byID[id] == nil {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return objErr("error", "Unknown people selection", "unknown_person_ids", anyStrings(unknown))
	}
	selected := people
	if len(requested) > 0 {
		selected = nil
		for _, id := range requested {
			selected = append(selected, byID[id])
		}
	}
	var selectedIDs []string
	for _, p := range selected {
		selectedIDs = append(selectedIDs, oget(p, "person_id").(string))
	}
	setID := stripOr(oget(research, "result_set_id"), "")
	if setID == "" {
		setID = resultSetID(company, function, people)
	}
	supplied := stripOr(a["idempotency_key"], "")
	if utf8.RuneCountInString(supplied) > 255 {
		return errObj("Idempotency key exceeds 255 characters"), true, nil
	}
	key := supplied
	if key == "" {
		key = "chat-people:" + convID + ":" + setID + ":" + digest16(selectedIDs)
	}
	wbName := runePrefix(stripOr(a["name"], peopleWorkbookName(company, function)), 255)

	var rows []*wbcrud.Obj
	seen := map[string]bool{}
	for _, p := range selected {
		full := stripOr(or2(oget(p, "name"), oget(p, "full_name")), "")
		linkedin := stripOr(oget(p, "linkedin_url"), "")
		id := oget(p, "person_id").(string)
		if full == "" || seen[id] {
			continue
		}
		seen[id] = true
		freshURL := ""
		if srcs, ok := oget(p, "verification_sources").([]any); ok {
			for _, s := range srcs {
				if so, ok := s.(*wbcrud.Obj); ok && pyTruthy(oget(so, "url")) {
					freshURL = pyStr(oget(so, "url"))
					break
				}
			}
		}
		confidence := oget(p, "verification_confidence")
		if confidence == nil {
			confidence = oget(p, "confidence")
		}
		contact, _ := oget(p, "contactability").(*wbcrud.Obj)
		if contact == nil {
			contact = wbcrud.NewObj()
		}
		emailStatus := "unavailable"
		if v := oget(contact, "status"); pyTruthy(v) {
			emailStatus = pyStr(v)
		}
		var email any
		if emailStatus == "verified" || emailStatus == "risky" || emailStatus == "catch_all" {
			email = oget(contact, "email")
		}
		attempts := []any{}
		if l, ok := oget(contact, "attempts").([]any); ok {
			attempts = l
		}
		evidence := strOr(oget(p, "evidence_url"), linkedin)
		r := wbcrud.NewObj()
		r.Set("person_id", id)
		r.Set("full_name", full)
		r.Set("contact_person", full)
		r.Set("company", strOr(oget(p, "company"), company))
		r.Set("canonical_company_domain", strings.ToLower(strings.TrimFunc(strOr(oget(p, "canonical_company_domain"), canonDomain), isPySpaceRune)))
		r.Set("title", strOr(oget(p, "title"), ""))
		r.Set("contact_title", strOr(oget(p, "title"), ""))
		r.Set("function", strOr(oget(p, "function"), function))
		r.Set("location", strOr(oget(p, "location"), ""))
		r.Set("linkedin_url", linkedin)
		r.Set("verification_status", strOr(oget(p, "verification_status"), "profile_evidence"))
		r.Set("confidence", confidence)
		r.Set("evidence_url", evidence)
		r.Set("verification_evidence_url", freshURL)
		r.Set("checked_at", strOr(or2(oget(p, "checked_at"), oget(p, "retrieved_at")), ""))
		r.Set("email", email)
		r.Set("email_status", emailStatus)
		r.Set("email_finder", strOr(oget(contact, "finder_provider"), ""))
		r.Set("email_verifier", strOr(oget(contact, "verifier_provider"), ""))
		r.Set("contact_observed_at", strOr(oget(contact, "observed_at"), ""))
		r.Set("contact_provider_attempts", attempts)
		r.Set("source", priorName)
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return errObj("No valid people rows to save"), true, nil
	}
	if len(rows) != len(selectedIDs) {
		return objErr("error", "Selected people are missing required identity data",
			"selected_count", int64(len(selectedIDs)), "valid_count", int64(len(rows)))
	}
	cfg := wbcrud.NewObj()
	cfg.Set("conversation_id", convID)
	cfg.Set("tool", priorName)
	cfg.Set("company", company)
	cfg.Set("company_resolution", resolution)
	cfg.Set("canonical_company_domain", canonDomain)
	cfg.Set("function", function)
	cfg.Set("result_set_id", setID)
	cfg.Set("selected_person_ids", anyStrings(selectedIDs))
	cfgJSON, err := pyJSONText(cfg)
	if err != nil {
		return "", false, err
	}
	desc := "People research snapshot for " + company + " — " + function + ". Created from a trusted Chat tool result."
	var out string
	err = t.inTx(ctx, func(tx pgx.Tx) error {
		if wbID, err := peopleWorkbookByKey(ctx, tx, t.WS, key); err != nil || wbID != "" {
			if err == nil {
				out, err = peopleReceipt(ctx, tx, t.WS, wbID, true)
			}
			return err
		}
		wbID := uuid.NewString()
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sp.Exec(ctx, `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
			filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd,
			refresh_policy, action_idempotency_key, created_at, updated_at)
			VALUES ($1, $2, $3, 'draft', $4, 'people_research', $5::json, '{}'::json, $6::json, $7, 0, false, 0.0, 0.0,
			        '{}'::json, $8, (now() AT TIME ZONE 'utc'), (now() AT TIME ZONE 'utc'))`,
			wbID, wbName, desc, t.WS, cfgJSON, peopleColumns, len(rows), key)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if err := sp.Rollback(ctx); err != nil {
				return err
			}
			existing, err := peopleWorkbookByKey(ctx, tx, t.WS, key)
			if err != nil {
				return err
			}
			if existing == "" {
				out = errObj("Workbook persistence failed")
				return nil
			}
			out, err = peopleReceipt(ctx, tx, t.WS, existing, true)
			return err
		}
		if err != nil {
			_ = sp.Rollback(ctx)
			return err
		}
		for _, r := range rows {
			email := ""
			if oget(r, "email_status") == "verified" {
				email = pyStr(oget(r, "email"))
			}
			canon, err := t.Engine.ResolvePerson(ctx, sp, t.WS, PersonObservation{
				Name: oget(r, "full_name").(string), Company: oget(r, "company").(string),
				CompanyDomain: oget(r, "canonical_company_domain").(string), Title: oget(r, "title").(string),
				LinkedInURL: oget(r, "linkedin_url").(string), Email: email, EvidenceURL: oget(r, "evidence_url").(string),
				Source: "chat_people_research", LegacyIDs: []string{oget(r, "person_id").(string)}})
			if err != nil {
				_ = sp.Rollback(ctx)
				return err
			}
			r.Set("canonical_person_id", canon)
		}
		for pos, r := range rows {
			data, err := pyJSONText(r)
			if err != nil {
				_ = sp.Rollback(ctx)
				return err
			}
			corroboration := 1
			if oget(r, "verification_status") == "independent_role_evidence" {
				corroboration = 2
			}
			if _, err := sp.Exec(ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments,
				source_provider, source_record_id, corroboration_count)
				VALUES ($1, $2, $3, $4::json, '{}'::json, 'chat_people_research', $5, $6)`,
				wbID, t.WS, pos, data, oget(r, "person_id"), corroboration); err != nil {
				_ = sp.Rollback(ctx)
				return err
			}
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		out, err = peopleReceipt(ctx, tx, t.WS, wbID, false)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// or2 is `a or b`.
func or2(a, b any) any {
	if pyTruthy(a) {
		return a
	}
	return b
}

// strOr is str(v or dflt) (no strip).
func strOr(v any, dflt string) string {
	if pyTruthy(v) {
		return pyStr(v)
	}
	return dflt
}

func peopleWorkbookByKey(ctx context.Context, tx pgx.Tx, ws, key string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM workbooks WHERE workspace_id = $1 AND action_idempotency_key = $2 LIMIT 1`, ws, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// peopleReceipt is create_people_workbook's _receipt.
func peopleReceipt(ctx context.Context, tx pgx.Tx, ws, id string, reused bool) (string, error) {
	var name string
	var key, cfg *string
	var total *int64
	var persisted int64
	if err := tx.QueryRow(ctx, `SELECT w.name, w.action_idempotency_key, w.source_config::text, w.total_rows,
		(SELECT count(*) FROM workbook_rows r WHERE r.workbook_id = w.id)
		FROM workbooks w WHERE w.id = $1 AND w.workspace_id = $2`, id, ws).Scan(&name, &key, &cfg, &total, &persisted); err != nil {
		return "", err
	}
	config := wbcrud.NewObj()
	if cfg != nil {
		if o, err := wbcrud.ParseObjectCanon([]byte(*cfg)); err == nil {
			config = o
		}
	}
	saved, _ := oget(config, "selected_person_ids").([]any)
	if saved == nil {
		saved = []any{}
	}
	var keyV any
	if key != nil {
		keyV = *key
	}
	t := int64(0)
	if total != nil {
		t = *total
	}
	msg := "Created '" + name + "' with " + pyStr(persisted) + " people."
	if reused {
		msg = "Reused '" + name + "' with " + pyStr(persisted) + " people."
	}
	o := wbcrud.NewObj()
	o.Set("ok", true)
	o.Set("persisted", persisted == t)
	o.Set("reused", reused)
	o.Set("action_id", keyV)
	o.Set("workbook_id", id)
	o.Set("name", name)
	o.Set("total_rows", persisted)
	o.Set("skipped_count", int64(0))
	o.Set("selected_person_ids", saved)
	o.Set("result_set_id", oget(config, "result_set_id"))
	o.Set("url", "/workbooks/"+id)
	o.Set("source", "people_research")
	o.Set("message", msg)
	return objJSON(o)
}
