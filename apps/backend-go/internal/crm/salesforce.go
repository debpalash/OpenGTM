package crm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// APIVersion is the Salesforce REST API version (salesforce.py API_VERSION).
const APIVersion = "v59.0"

// Per-call timeouts, as salesforce.py (httpx.AsyncClient(timeout=...)).
const (
	sfFetchTimeout = 20 * time.Second
	sfCallTimeout  = 15 * time.Second
	// sfMaxPages bounds nextRecordsUrl paging (D4 hardening; Python loops until done).
	sfMaxPages = 1000
)

const sfNotConnected = "Salesforce not connected (set SALESFORCE_INSTANCE_URL + SALESFORCE_ACCESS_TOKEN)"

// sfAllowedSuffixes are the domains a Salesforce instance URL may live on
// (documented deviation D3: Python accepts any URL). The suffixes cover
// *.my.salesforce.com, *.salesforce.com (login, na/eu/cs instances),
// *.force.com (Lightning, sites, visual.force.com) and *.cloudforce.com.
var sfAllowedSuffixes = []string{".salesforce.com", ".force.com", ".cloudforce.com", ".my.salesforce.com"}

// SalesforceDefaultFieldMap maps Yupcha lead fields to Salesforce Lead fields
// (salesforce.py DEFAULT_FIELD_MAP). Ordered: it fixes the request body order.
var SalesforceDefaultFieldMap = FieldMap{
	{"company", "Company"},
	{"email", "Email"},
	{"phone", "Phone"},
	{"website", "Website"},
	{"city", "City"},
	{"contact_title", "Title"},
}

// ImportDefaultFields maps a Salesforce Contact field to a lead field (the
// import default; FirstName/LastName merge into contact_person).
var ImportDefaultFields = FieldMap{
	{"Email", "email"},
	{"Account.Name", "company"},
	{"Phone", "phone"},
	{"Title", "contact_title"},
}

// ContactUpdateFieldMap is the update write-back default (Contact sObject):
// lead field to Salesforce field. Company is deliberately absent.
var ContactUpdateFieldMap = FieldMap{
	{"email", "Email"},
	{"phone", "Phone"},
	{"contact_title", "Title"},
}

// Salesforce is the Salesforce REST client. The instance URL and token are
// resolved on every call from the workspace (then global, then env) and never
// stored on the client. The instance URL is user-typed: it is validated on
// every call (https, allowed Salesforce domain, public host) and the default
// HTTP client re-checks the resolved address at dial time.
type Salesforce struct {
	HTTP    *http.Client      // nil: the SSRF-guarded egress client
	Secrets *secrets.Resolver // nil: environment only
	// Sleep waits between 429 retries; nil honours ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// SOQLEscape escapes a value for interpolation into a SOQL string literal:
// backslash, single and double quote are backslash-escaped, newline, carriage
// return and tab are dropped.
func SOQLEscape(value string) string {
	var b strings.Builder
	for _, ch := range value {
		switch ch {
		case '\\', '\'', '"':
			b.WriteByte('\\')
			b.WriteRune(ch)
		case '\n', '\r', '\t':
		default:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func (s *Salesforce) secret(ctx context.Context, ws, key string) string {
	if s.Secrets == nil {
		return os.Getenv(key)
	}
	// The resolver itself walks workspace -> global -> operator env and reports
	// not-found as ("", false, nil). Any error (DB outage, decrypt failure) fails
	// closed: no process-env fallback, so no cross-tenant credential use.
	v, _, err := s.Secrets.Get(ctx, ws, key)
	if err != nil {
		return ""
	}
	return v
}

// creds is _creds: (instance_url without trailing slashes, access token).
func (s *Salesforce) creds(ctx context.Context, ws string) (string, string) {
	return strings.TrimRight(s.secret(ctx, ws, "SALESFORCE_INSTANCE_URL"), "/"),
		s.secret(ctx, ws, "SALESFORCE_ACCESS_TOKEN")
}

// IsConnected reports whether an instance URL and token are configured.
func (s *Salesforce) IsConnected(ctx context.Context, ws string) bool {
	inst, tok := s.creds(ctx, ws)
	return inst != "" && tok != ""
}

// checkInstanceURL vets the user-typed instance URL: https, no userinfo, an
// allowed Salesforce domain (never the bare apex), default port, no path, query
// or fragment, and not a private/blocked address (egress.CheckURL).
func checkInstanceURL(raw string) error {
	if raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "\t\r\n ") {
		return fmt.Errorf("blocked url: invalid instance url")
	}
	if err := egress.CheckURL(raw, false); err != nil {
		return fmt.Errorf("blocked url: %s", reasonOf(err))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("blocked url: invalid instance url")
	}
	if u.User != nil {
		return fmt.Errorf("blocked url: credentials in url are not allowed")
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("blocked url: port %s is not allowed", p)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return fmt.Errorf("blocked url: instance url must be a bare origin")
	}
	host := strings.ToLower(u.Hostname())
	for _, suf := range sfAllowedSuffixes {
		if strings.HasSuffix(host, suf) && len(host) > len(suf) && !strings.HasPrefix(host[:len(host)-len(suf)], ".") {
			return nil
		}
	}
	return fmt.Errorf("blocked url: host is not an allowed Salesforce domain")
}

func reasonOf(err error) string {
	var be *egress.BlockedError
	if errors.As(err, &be) {
		return be.Reason
	}
	return err.Error()
}

// begin returns the validated instance URL and token, or the not-connected /
// blocked-url message.
func (s *Salesforce) begin(ctx context.Context, ws string) (inst, tok, errMsg string) {
	inst, tok = s.creds(ctx, ws)
	if inst == "" || tok == "" {
		return "", "", sfNotConnected
	}
	if err := checkInstanceURL(inst); err != nil {
		return "", "", err.Error()
	}
	return inst, tok, ""
}

// ── transport ────────────────────────────────────────────────────────────

type sfCall struct {
	method, url string
	body        []byte
	token       string
	timeout     time.Duration
	contentType bool
	retry       bool
}

func (s *Salesforce) client() *http.Client {
	c := s.HTTP
	if c == nil {
		c = defaultClient()
	}
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

func (s *Salesforce) send(ctx context.Context, c sfCall) (reply, error) {
	var cancels []context.CancelFunc
	defer func() {
		for _, f := range cancels {
			f()
		}
	}()
	build := func() (*http.Request, error) {
		cctx, cancel := context.WithTimeout(ctx, c.timeout) // per attempt, like httpx
		cancels = append(cancels, cancel)
		var body io.Reader
		if c.body != nil {
			body = bytes.NewReader(c.body)
		}
		req, err := http.NewRequestWithContext(cctx, c.method, c.url, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if c.contentType {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}
	cl := s.client()
	var resp *http.Response
	var err error
	if c.retry {
		resp, err = RequestWithRetry(ctx, cl, build, DefaultMaxRetries, s.Sleep)
	} else {
		var req *http.Request
		if req, err = build(); err == nil {
			resp, err = cl.Do(req)
		}
	}
	if err != nil {
		return reply{}, s.scrubErr(ctx, err, c.token)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, providers.MaxBody+1))
	if err != nil {
		return reply{}, s.scrubErr(ctx, err, c.token)
	}
	if len(b) > providers.MaxBody {
		return reply{}, &callError{msg: fmt.Sprintf("response body exceeds %d bytes", providers.MaxBody)}
	}
	return reply{status: resp.StatusCode, body: b, token: c.token}, nil
}

// scrubErr turns a transport error into one that can never carry the token. A
// dial-time egress refusal becomes "blocked url: <reason>"; a cancelled caller
// context stays recognisable through errors.Is.
func (s *Salesforce) scrubErr(ctx context.Context, err error, token string) error {
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	if errors.Is(err, egress.ErrBlocked) {
		return &callError{msg: providers.Scrub("blocked url: "+reasonOf(unwrapBlocked(err)), token)}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &callError{msg: providers.Scrub(err.Error(), token)}
}

func unwrapBlocked(err error) error {
	var be *egress.BlockedError
	if errors.As(err, &be) {
		return be
	}
	return err
}

// ── ordered JSON object body (Python json.dumps, compact, ensure_ascii=False) ──

func (p *propList) objectBody() []byte {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range p.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pyJSONString(k) + ":" + pyJSONString(p.m[k]))
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// sfQuery is httpx params={"q": soql}: space as '+', everything else
// percent-encoded except the unreserved set.
func sfQuery(soql string) string { return "q=" + url.QueryEscape(soql) }

// ── import (pull contacts) ───────────────────────────────────────────────

// recordValue resolves a (possibly dotted) SOQL field path against a record.
func recordValue(record map[string]any, path string) any {
	var cur any = record
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func sfEmpty(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// mapImportRecord maps a Salesforce Contact record to lead fields. fieldMap is
// {salesforce_field: lead_field}; without one the import defaults apply and
// FirstName/LastName merge into contact_person.
func mapImportRecord(record map[string]any, fieldMap FieldMap) map[string]any {
	out := map[string]any{}
	if len(fieldMap) > 0 {
		for _, fp := range fieldMap {
			if v := recordValue(record, fp.Key); !sfEmpty(v) {
				out[fp.Value] = v
			}
		}
		return out
	}
	for _, fp := range ImportDefaultFields {
		if v := recordValue(record, fp.Key); !sfEmpty(v) {
			out[fp.Value] = v
		}
	}
	part := func(k string) string {
		if v := record[k]; providers.Truthy(v) {
			return providers.PyStrip(providers.Str(v))
		}
		return ""
	}
	if name := providers.PyStrip(part("FirstName") + " " + part("LastName")); name != "" {
		out["contact_person"] = name
	}
	return out
}

// FetchContacts pulls up to limit Contacts (REST query, nextRecordsUrl paging,
// 429 retried). where is a raw SOQL WHERE fragment from the workbook owner (it
// runs under their own token). fieldMap is {salesforce_field: lead_field}; nil
// uses the import defaults. Each contact carries crm_external_id, crm_type and
// crm_object. HTTP failures come back as FetchResult{Success: false}.
func (s *Salesforce) FetchContacts(ctx context.Context, ws string, limit int, where string, fieldMap FieldMap) FetchResult {
	inst, tok, msg := s.begin(ctx, ws)
	if msg != "" {
		return FetchResult{Error: msg}
	}
	selectFields := []string{"Email", "FirstName", "LastName", "Account.Name", "Phone", "Title"}
	if len(fieldMap) > 0 {
		selectFields = selectFields[:0]
		for _, fp := range fieldMap {
			if strings.ToLower(fp.Key) != "id" {
				selectFields = append(selectFields, fp.Key)
			}
		}
	}
	soql := "SELECT Id, " + strings.Join(selectFields, ", ") + " FROM Contact"
	if w := providers.PyStrip(where); w != "" {
		soql += " WHERE " + w
	}
	soql += " LIMIT " + strconv.Itoa(limit)

	fail := func(err error) FetchResult { return FetchResult{Error: providers.TruncRunes(err.Error(), 200)} }
	get := func(u string) (reply, error) {
		return s.send(ctx, sfCall{method: "GET", url: u, token: tok, timeout: sfFetchTimeout, retry: true})
	}
	base := inst + "/services/data/" + APIVersion
	r, err := get(base + "/query?" + sfQuery(soql))
	var contacts []map[string]any
	for page := 0; ; page++ {
		if err != nil {
			return fail(err)
		}
		if r.status == 401 {
			return FetchResult{Error: "Salesforce token invalid or expired (HTTP 401)"}
		}
		if r.status != 200 {
			return FetchResult{Error: r.failText(160)}
		}
		data, perr := parseJSON(r.body)
		if perr != nil {
			return fail(perr)
		}
		recs, perr := providers.PyGet(data, "records", []any{})
		if perr != nil {
			return fail(perr)
		}
		list, isList := recs.([]any)
		if !isList && providers.Truthy(recs) {
			return fail(&providers.PyException{Msg: fmt.Sprintf("'%s' object is not iterable", providers.TypeName(recs))})
		}
		for _, o := range list {
			rec, ok := o.(map[string]any)
			if !ok {
				return fail(&providers.PyException{Msg: fmt.Sprintf("'%s' object has no attribute 'get'", providers.TypeName(o))})
			}
			row := mapImportRecord(rec, fieldMap)
			row["crm_external_id"] = providers.Str(rec["Id"])
			row["crm_type"] = "salesforce"
			row["crm_object"] = "contact"
			contacts = append(contacts, row)
			if len(contacts) >= limit {
				break
			}
		}
		next, perr := providers.PyGet(data, "nextRecordsUrl", nil)
		if perr != nil {
			return fail(perr)
		}
		done, _ := providers.PyGet(data, "done", nil)
		if providers.Truthy(done) || !providers.Truthy(next) || len(contacts) >= limit {
			break
		}
		nu, ok := next.(string)
		if !ok || !strings.HasPrefix(nu, "/services/data/") || strings.Contains(nu, "//") ||
			strings.ContainsAny(nu, "\\@ \t\r\n#") {
			return FetchResult{Error: "invalid nextRecordsUrl from Salesforce"}
		}
		if page+1 >= sfMaxPages {
			return FetchResult{Error: "too many result pages from Salesforce"}
		}
		r, err = get(inst + nu)
	}
	if len(contacts) > limit {
		contacts = contacts[:max(limit, 0)]
	}
	return FetchResult{Success: true, Contacts: contacts}
}

// ── update ───────────────────────────────────────────────────────────────

// SalesforceUpdateResult is the dict update_record returns: {success, action,
// salesforce_id}, {success, not_found, error} or {success, error}.
type SalesforceUpdateResult struct {
	Success      bool
	NotFound     bool
	Error        string
	Action       string
	SalesforceID string
}

func (r SalesforceUpdateResult) MarshalJSON() ([]byte, error) {
	switch {
	case r.Success:
		return []byte(`{"success":true,"action":` + pyJSONString(r.Action) + `,"salesforce_id":` + pyJSONString(r.SalesforceID) + `}`), nil
	case r.NotFound:
		return []byte(`{"success":false,"not_found":true,"error":` + pyJSONString(r.Error) + `}`), nil
	}
	return []byte(`{"success":false,"error":` + pyJSONString(r.Error) + `}`), nil
}

// UpdateRecord PATCHes an existing record by Id (CRM-sync write-back). A 404
// sets NotFound so the caller can fall back to create. 429 is retried
// (bounded). fields is ordered (Key = Salesforce field, Value = its value) so
// the body keeps the caller's order, like a Python dict.
func (s *Salesforce) UpdateRecord(ctx context.Context, ws, sobject, id string, fields FieldMap) SalesforceUpdateResult {
	inst, tok, msg := s.begin(ctx, ws)
	if msg != "" {
		return SalesforceUpdateResult{Error: msg}
	}
	if len(fields) == 0 {
		return SalesforceUpdateResult{Error: "no mapped fields to update"}
	}
	var props propList
	for _, f := range fields {
		props.set(f.Key, f.Value)
	}
	r, err := s.send(ctx, sfCall{method: "PATCH",
		url:  inst + "/services/data/" + APIVersion + "/sobjects/" + url.PathEscape(sobject) + "/" + url.PathEscape(id),
		body: props.objectBody(), token: tok, timeout: sfCallTimeout, contentType: true, retry: true})
	if err != nil {
		return SalesforceUpdateResult{Error: providers.TruncRunes(err.Error(), 200)}
	}
	switch r.status {
	case 200, 204:
		return SalesforceUpdateResult{Success: true, Action: "updated", SalesforceID: id}
	case 404:
		return SalesforceUpdateResult{NotFound: true, Error: fmt.Sprintf("%s %s not found in Salesforce", sobject, id)}
	}
	return SalesforceUpdateResult{Error: r.failText(160)}
}

// ── push ─────────────────────────────────────────────────────────────────

// SalesforcePushResult is the dict push_lead_as_contact returns. Updated
// results carry {success, action, salesforce_id, error|null}, created results
// {success, action, salesforce_id}, failures {success, error}.
type SalesforcePushResult struct {
	Success      bool
	Action       string // "", created | updated
	SalesforceID any
	Error        string
}

func (r SalesforcePushResult) MarshalJSON() ([]byte, error) {
	if r.Action == "" {
		return []byte(`{"success":` + strconv.FormatBool(r.Success) + `,"error":` + pyJSONString(r.Error) + `}`), nil
	}
	id, err := marshalAny(r.SalesforceID)
	if err != nil {
		return nil, err
	}
	out := `{"success":` + strconv.FormatBool(r.Success) + `,"action":` + pyJSONString(r.Action) + `,"salesforce_id":` + id
	if r.Action == "updated" {
		if r.Error == "" {
			out += `,"error":null`
		} else {
			out += `,"error":` + pyJSONString(r.Error)
		}
	}
	return []byte(out + `}`), nil
}

// PushLeadAsContact pushes a lead as a Salesforce Lead record: it searches by
// email (escaped SOQL), PATCHes the match or creates a new Lead. A nil/empty
// fieldMap means SalesforceDefaultFieldMap.
func (s *Salesforce) PushLeadAsContact(ctx context.Context, ws string, lead Lead, fieldMap FieldMap) SalesforcePushResult {
	inst, tok, msg := s.begin(ctx, ws)
	if msg != "" {
		return SalesforcePushResult{Error: msg}
	}
	fmap := fieldMap
	if len(fmap) == 0 {
		fmap = SalesforceDefaultFieldMap
	}
	var fields propList
	for _, fp := range fmap {
		if v, ok := lead.Get(fp.Key); ok && providers.Truthy(v) {
			fields.set(fp.Value, providers.Str(v))
		}
	}
	// Salesforce Lead requires LastName + Company.
	var contact string
	if v, ok := lead.Get("contact_person"); ok && providers.Truthy(v) {
		contact = providers.Str(v)
	}
	if first, last, found := strings.Cut(contact, " "); found {
		fields.set("FirstName", first)
		fields.set("LastName", last)
	} else {
		switch {
		case contact != "":
			fields.set("LastName", contact)
		case fields.m["Company"] != "":
			fields.set("LastName", fields.m["Company"])
		default:
			fields.set("LastName", "Unknown")
		}
	}
	if fields.m["Company"] == "" {
		return SalesforcePushResult{Error: "Lead has no company — required for Salesforce Lead"}
	}
	fail := func(err error) SalesforcePushResult { return SalesforcePushResult{Error: err.Error()} }
	base := inst + "/services/data/" + APIVersion
	body := fields.objectBody()

	if email := fields.m["Email"]; email != "" {
		soql := "SELECT Id FROM Lead WHERE Email = '" + SOQLEscape(email) + "' LIMIT 1"
		sr, err := s.send(ctx, sfCall{method: "GET", url: base + "/query?" + sfQuery(soql), token: tok,
			timeout: sfCallTimeout, contentType: true})
		if err != nil {
			return fail(err)
		}
		if sr.status == 200 {
			data, err := parseJSON(sr.body)
			if err != nil {
				return fail(err)
			}
			recs, err := providers.PyGet(data, "records", nil)
			if err != nil {
				return fail(err)
			}
			if providers.Truthy(recs) {
				list, ok := recs.([]any)
				if !ok {
					return SalesforcePushResult{Error: "0"}
				}
				first, ok := list[0].(map[string]any)
				sid, has := first["Id"]
				if !ok || !has {
					return SalesforcePushResult{Error: "'Id'"}
				}
				ur, err := s.send(ctx, sfCall{method: "PATCH", url: base + "/sobjects/Lead/" + url.PathEscape(providers.Str(sid)),
					body: body, token: tok, timeout: sfCallTimeout, contentType: true})
				if err != nil {
					return fail(err)
				}
				ok = ur.status == 200 || ur.status == 204
				res := SalesforcePushResult{Success: ok, Action: "updated", SalesforceID: sid}
				if !ok {
					res.Error = ur.failText(160)
				}
				return res
			}
		}
	}
	cr, err := s.send(ctx, sfCall{method: "POST", url: base + "/sobjects/Lead", body: body, token: tok,
		timeout: sfCallTimeout, contentType: true})
	if err != nil {
		return fail(err)
	}
	if cr.status != 200 && cr.status != 201 {
		return SalesforcePushResult{Error: cr.failText(160)}
	}
	data, err := parseJSON(cr.body)
	if err != nil {
		return fail(err)
	}
	id, err := providers.PyGet(data, "id", nil)
	if err != nil {
		return fail(err)
	}
	return SalesforcePushResult{Success: true, Action: "created", SalesforceID: id}
}
