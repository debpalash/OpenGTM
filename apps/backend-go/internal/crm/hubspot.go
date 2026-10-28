package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// DefaultBaseURL is the HubSpot API host (hubspot.py BASE_URL).
const DefaultBaseURL = "https://api.hubapi.com"

// Per-call timeouts, as hubspot.py (httpx.AsyncClient(timeout=...)).
const (
	statusTimeout = 10 * time.Second
	callTimeout   = 15 * time.Second
	fetchTimeout  = 20 * time.Second
	batchSpacing  = 150 * time.Millisecond // HubSpot allows 100/10s for private apps
)

// FieldPair is one entry of an ordered field map. Order is significant: it
// fixes the property order of the request body (Python dict insertion order).
type FieldPair struct{ Key, Value string }

// FieldMap is an ordered field map. For pushes Key is the lead field and Value
// the HubSpot property; for imports Key is the HubSpot property and Value the
// lead field.
type FieldMap []FieldPair

// DefaultFieldMap maps Yupcha lead fields to HubSpot contact properties.
var DefaultFieldMap = FieldMap{
	{"company", "company"},
	{"email", "email"},
	{"phone", "phone"},
	{"website", "website"},
	{"city", "city"},
	{"contact_person", "firstname"}, // HubSpot splits first/last
	{"contact_title", "jobtitle"},
	{"linkedin_url", "hs_linkedinid"},
	{"score", "hs_lead_status"},
}

// ImportDefaultProperties are pulled when no import field map is given.
var ImportDefaultProperties = []string{"email", "firstname", "lastname", "company", "phone", "website", "jobtitle"}

// Lead is what a push reads: leadsread.Lead satisfies it. Get returns the
// field by its Python name; ok is false for unknown names (getattr default).
type Lead interface {
	Get(key string) (any, bool)
}

// HubSpot is the HubSpot CRM v3 client. Tokens are resolved on every call from
// the workspace (then global, then env) and never stored on the client.
type HubSpot struct {
	HTTP    *http.Client      // nil: the SSRF-guarded egress client
	Secrets *secrets.Resolver // nil: HUBSPOT_TOKEN from the environment only
	BaseURL string            // "" = DefaultBaseURL
	// Sleep waits between 429 retries and between batch pushes; nil honours ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

var defaultClient = sync.OnceValue(func() *http.Client { return egress.NewClient(nil) })

func (h *HubSpot) base() string {
	if h.BaseURL != "" {
		return strings.TrimRight(h.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (h *HubSpot) sleep() func(context.Context, time.Duration) error {
	if h.Sleep != nil {
		return h.Sleep
	}
	return ctxSleep
}

// token is _get_token: workspace secret, then global settings, then env (the
// resolver walks that chain itself). A resolver error is not "not found": it
// fails closed (no token, no request) instead of falling back to the process
// environment as the Python except branch does (security deviation).
func (h *HubSpot) token(ctx context.Context, ws string) string {
	if h.Secrets == nil {
		return os.Getenv("HUBSPOT_TOKEN")
	}
	v, _, err := h.Secrets.Get(ctx, ws, "HUBSPOT_TOKEN")
	if err != nil {
		return ""
	}
	return v
}

// IsConnected reports whether a token is configured for ws.
func (h *HubSpot) IsConnected(ctx context.Context, ws string) bool {
	return h.token(ctx, ws) != ""
}

// ── transport ────────────────────────────────────────────────────────────

type callError struct{ msg string }

func (e *callError) Error() string { return e.msg }

type call struct {
	method, path string
	query        [][2]string
	body         []byte
	token        string
	timeout      time.Duration
	contentType  bool
	retry        bool
}

type reply struct {
	status int
	body   []byte
	token  string
}

// text is resp.text (token scrubbed in case a vendor echoes it).
func (r reply) text() string {
	return providers.Scrub(strings.ToValidUTF8(string(r.body), "�"), r.token)
}

// failText is "HTTP <status>: <text[:n]>".
func (r reply) failText(n int) string {
	return fmt.Sprintf("HTTP %d: %s", r.status, providers.TruncRunes(r.text(), n))
}

func (h *HubSpot) client() *http.Client {
	c := h.HTTP
	if c == nil {
		c = defaultClient()
	}
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

func encodeQuery(q [][2]string) string {
	var b strings.Builder
	for i, kv := range q {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(strings.ReplaceAll(url.QueryEscape(kv[0]), "+", "%20"))
		b.WriteByte('=')
		b.WriteString(strings.ReplaceAll(url.QueryEscape(kv[1]), "+", "%20"))
	}
	return b.String()
}

func (h *HubSpot) send(ctx context.Context, c call) (reply, error) {
	u := h.base() + c.path
	if len(c.query) > 0 {
		u += "?" + encodeQuery(c.query)
	}
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
		req, err := http.NewRequestWithContext(cctx, c.method, u, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if c.contentType {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}
	cl := h.client()
	var resp *http.Response
	var err error
	if c.retry {
		resp, err = RequestWithRetry(ctx, cl, build, DefaultMaxRetries, h.Sleep)
	} else {
		var req *http.Request
		if req, err = build(); err == nil {
			resp, err = cl.Do(req)
		}
	}
	if err != nil {
		return reply{}, h.scrubErr(ctx, err, c.token)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, providers.MaxBody+1))
	if err != nil {
		return reply{}, h.scrubErr(ctx, err, c.token)
	}
	if len(b) > providers.MaxBody {
		return reply{}, &callError{msg: fmt.Sprintf("response body exceeds %d bytes", providers.MaxBody)}
	}
	return reply{status: resp.StatusCode, body: b, token: c.token}, nil
}

// scrubErr turns a transport error into one that can never carry the token. A
// cancelled caller context stays recognisable through errors.Is.
func (h *HubSpot) scrubErr(ctx context.Context, err error, token string) error {
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &callError{msg: providers.Scrub(err.Error(), token)}
}

// parseJSON is resp.json() with CPython's "property name" message for an
// object whose first key is not a string.
func parseJSON(body []byte) (any, error) {
	v, err := providers.ParseJSON(body)
	if err == nil {
		return v, nil
	}
	t := bytes.TrimLeft(body, " \t\r\n")
	if len(t) > 0 && bytes.ContainsRune([]byte("tfn"), rune(t[0])) && !hasLiteralPrefix(t) {
		return nil, &providers.PyException{Msg: fmt.Sprintf("Expecting value: line 1 column %d (char %d)",
			len(body)-len(t)+1, len(body)-len(t))}
	}
	if len(t) > 0 && t[0] == '{' {
		rest := bytes.TrimLeft(t[1:], " \t\r\n")
		if len(rest) > 0 && rest[0] != '"' && rest[0] != '}' {
			pos := len(body) - len(rest)
			prefix := body[:pos]
			chars := len([]rune(string(prefix)))
			line := bytes.Count(prefix, []byte("\n")) + 1
			col := chars + 1
			if i := bytes.LastIndexByte(prefix, '\n'); i >= 0 {
				col = len([]rune(string(prefix[i+1:]))) + 1
			}
			return nil, &providers.PyException{Msg: fmt.Sprintf(
				"Expecting property name enclosed in double quotes: line %d column %d (char %d)", line, col, chars)}
		}
	}
	return nil, err
}

func hasLiteralPrefix(t []byte) bool {
	for _, lit := range []string{"true", "false", "null"} {
		if bytes.HasPrefix(t, []byte(lit)) {
			return true
		}
	}
	return false
}

// ── ordered JSON bodies (Python json.dumps, compact, ensure_ascii=False) ──

func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

type propList struct {
	keys []string
	m    map[string]string
}

func (p *propList) set(k, v string) {
	if p.m == nil {
		p.m = map[string]string{}
	}
	if _, ok := p.m[k]; !ok {
		p.keys = append(p.keys, k)
	}
	p.m[k] = v
}

func (p *propList) propertiesBody() []byte {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i, k := range p.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pyJSONString(k) + ":" + pyJSONString(p.m[k]))
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// ── status ───────────────────────────────────────────────────────────────

// TestConnection fetches account info. The map marshals (sorted keys) as
// connected, portal_id, time_zone or connected, error.
func (h *HubSpot) TestConnection(ctx context.Context, ws string) map[string]any {
	tok := h.token(ctx, ws)
	if tok == "" {
		return map[string]any{"connected": false, "error": "No HubSpot token configured"}
	}
	r, err := h.send(ctx, call{method: "GET", path: "/account-info/v3/details", token: tok, timeout: statusTimeout})
	if err != nil {
		return map[string]any{"connected": false, "error": err.Error()}
	}
	if r.status != 200 {
		return map[string]any{"connected": false, "error": r.failText(200)}
	}
	data, err := parseJSON(r.body)
	if err == nil {
		var pid, tz any
		if pid, err = providers.PyGet(data, "portalId", nil); err == nil {
			tz, err = providers.PyGet(data, "timeZone", nil)
		}
		if err == nil {
			return map[string]any{"connected": true, "portal_id": pid, "time_zone": tz}
		}
	}
	return map[string]any{"connected": false, "error": err.Error()}
}

// ── push ─────────────────────────────────────────────────────────────────

// PushResult is the dict push_lead_as_contact returns. It marshals as
// {success, error} when Error is set, else {success, action, hubspot_id}.
type PushResult struct {
	Success   bool
	Action    string // created | updated
	HubspotID any
	Error     string
}

func (r PushResult) MarshalJSON() ([]byte, error) {
	if r.Error != "" {
		return []byte(`{"success":` + strconv.FormatBool(r.Success) + `,"error":` + pyJSONString(r.Error) + `}`), nil
	}
	id, err := marshalAny(r.HubspotID)
	if err != nil {
		return nil, err
	}
	return []byte(`{"success":` + strconv.FormatBool(r.Success) + `,"action":` + pyJSONString(r.Action) + `,"hubspot_id":` + id + `}`), nil
}

func marshalAny(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "null", nil
	case string:
		return pyJSONString(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return providers.Str(t), nil
	}
	return "", fmt.Errorf("unsupported id type %T", v)
}

const noEmailMsg = "Lead has no email — required for HubSpot contact"

// PushLeadAsContact searches HubSpot for the lead's email, then updates the
// match or creates a contact. A nil/empty fieldMap means DefaultFieldMap.
func (h *HubSpot) PushLeadAsContact(ctx context.Context, ws string, lead Lead, fieldMap FieldMap) PushResult {
	tok := h.token(ctx, ws)
	if tok == "" {
		return PushResult{Error: "No HubSpot token"}
	}
	fmap := fieldMap
	if len(fmap) == 0 {
		fmap = DefaultFieldMap
	}
	var props propList
	for _, fp := range fmap {
		v, ok := lead.Get(fp.Key)
		if !ok || !providers.Truthy(v) {
			continue
		}
		s := providers.Str(v)
		if first, last, found := strings.Cut(s, " "); fp.Key == "contact_person" && found {
			props.set("firstname", first)
			props.set("lastname", last)
		} else {
			props.set(fp.Value, s)
		}
	}
	email := props.m["email"]
	if email == "" {
		return PushResult{Error: noEmailMsg}
	}
	fail := func(err error) PushResult { return PushResult{Error: err.Error()} }

	search := `{"filterGroups":[{"filters":[{"propertyName":"email","operator":"EQ","value":` +
		pyJSONString(email) + `}]}],"limit":1}`
	sr, err := h.send(ctx, call{method: "POST", path: "/crm/v3/objects/contacts/search", body: []byte(search),
		token: tok, timeout: callTimeout, contentType: true})
	if err != nil {
		return fail(err)
	}
	if sr.status != 200 {
		// A failed search is not "not found": creating now would duplicate the contact.
		return PushResult{Error: "HubSpot contact search failed (retryable): " + sr.failText(200)}
	}
	{
		data, err := parseJSON(sr.body)
		if err != nil {
			return fail(err)
		}
		results, err := providers.PyGet(data, "results", []any{})
		if err != nil {
			return fail(err)
		}
		if providers.Truthy(results) {
			list, ok := results.([]any)
			if !ok { // a non-list truthy value: results[0] raises KeyError(0) for dicts
				return PushResult{Error: "0"}
			}
			first, ok := list[0].(map[string]any)
			id, has := first["id"]
			if !ok || !has {
				return PushResult{Error: "'id'"}
			}
			ur, err := h.send(ctx, call{method: "PATCH", path: "/crm/v3/objects/contacts/" + url.PathEscape(providers.Str(id)),
				body: props.propertiesBody(), token: tok, timeout: callTimeout, contentType: true})
			if err != nil {
				return fail(err)
			}
			return PushResult{Success: ur.status == 200, Action: "updated", HubspotID: id}
		}
	}
	cr, err := h.send(ctx, call{method: "POST", path: "/crm/v3/objects/contacts", body: props.propertiesBody(),
		token: tok, timeout: callTimeout, contentType: true})
	if err != nil {
		return fail(err)
	}
	if cr.status != 201 {
		return PushResult{Error: cr.failText(200)}
	}
	data, err := parseJSON(cr.body)
	if err != nil {
		return fail(err)
	}
	id, err := providers.PyGet(data, "id", nil)
	if err != nil {
		return fail(err)
	}
	return PushResult{Success: true, Action: "created", HubspotID: id}
}

// SyncResult is the batch outcome (hubspot.py SyncResult).
type SyncResult struct {
	Created int      `json:"created"`
	Updated int      `json:"updated"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors"`
}

// PushLeadsBatch pushes the leads one by one with 150 ms spacing. It stops
// when ctx is cancelled (the counts so far are returned; check ctx.Err()).
func (h *HubSpot) PushLeadsBatch(ctx context.Context, ws string, leads []Lead, fieldMap FieldMap) SyncResult {
	res := SyncResult{Errors: []string{}}
	for _, lead := range leads {
		if ctx.Err() != nil {
			break
		}
		pr := h.PushLeadAsContact(ctx, ws, lead, fieldMap)
		if pr.Success {
			if pr.Action == "created" {
				res.Created++
			} else {
				res.Updated++
			}
		} else {
			res.Failed++
			msg := pr.Error
			if msg == "" {
				msg = "Unknown error"
			}
			id, _ := lead.Get("id")
			res.Errors = append(res.Errors, fmt.Sprintf("Lead %s: %s", providers.Str(id), msg))
		}
		if h.sleep()(ctx, batchSpacing) != nil {
			break
		}
	}
	return res
}

// UpdateResult is the dict update_contact_by_id returns: {success, action,
// hubspot_id}, {success, not_found, error} or {success, error}.
type UpdateResult struct {
	Success   bool
	NotFound  bool
	Error     string
	Action    string
	HubspotID string
}

func (r UpdateResult) MarshalJSON() ([]byte, error) {
	switch {
	case r.Success:
		return []byte(`{"success":true,"action":` + pyJSONString(r.Action) + `,"hubspot_id":` + pyJSONString(r.HubspotID) + `}`), nil
	case r.NotFound:
		return []byte(`{"success":false,"not_found":true,"error":` + pyJSONString(r.Error) + `}`), nil
	}
	return []byte(`{"success":false,"error":` + pyJSONString(r.Error) + `}`), nil
}

// UpdateContactByID PATCHes an existing contact (CRM write-back). A 404 sets
// NotFound so the caller can fall back to create. 429 is retried (bounded).
// properties is ordered (Key = HubSpot property, Value = its value) so the body
// keeps the caller's property order, like a Python dict.
func (h *HubSpot) UpdateContactByID(ctx context.Context, ws, id string, properties FieldMap) UpdateResult {
	tok := h.token(ctx, ws)
	if tok == "" {
		return UpdateResult{Error: "No HubSpot token"}
	}
	if len(properties) == 0 {
		return UpdateResult{Error: "no mapped fields to update"}
	}
	var props propList
	for _, p := range properties {
		props.set(p.Key, p.Value)
	}
	r, err := h.send(ctx, call{method: "PATCH", path: "/crm/v3/objects/contacts/" + url.PathEscape(id),
		body: props.propertiesBody(), token: tok, timeout: callTimeout, contentType: true, retry: true})
	if err != nil {
		return UpdateResult{Error: providers.TruncRunes(err.Error(), 200)}
	}
	switch r.status {
	case 200:
		return UpdateResult{Success: true, Action: "updated", HubspotID: id}
	case 404:
		return UpdateResult{NotFound: true, Error: fmt.Sprintf("contact %s not found in HubSpot", id)}
	}
	return UpdateResult{Error: r.failText(160)}
}

// ── import (pull contacts) ───────────────────────────────────────────────

// MapImportProperties maps a HubSpot contact's properties to lead fields.
// fieldMap is {hubspot_property: lead_field}; without one the default
// properties map to lead fields and firstname+lastname merge into
// contact_person.
func MapImportProperties(props map[string]any, fieldMap FieldMap) map[string]any {
	out := map[string]any{}
	has := func(k string) bool {
		v, ok := props[k]
		if !ok || v == nil {
			return false
		}
		s, isStr := v.(string)
		return !(isStr && s == "")
	}
	if len(fieldMap) > 0 {
		for _, fp := range fieldMap {
			if has(fp.Key) {
				out[fp.Value] = props[fp.Key]
			}
		}
		return out
	}
	for _, m := range [][2]string{{"email", "email"}, {"company", "company"}, {"phone", "phone"},
		{"website", "website"}, {"jobtitle", "contact_title"}} {
		if has(m[0]) {
			out[m[1]] = props[m[0]]
		}
	}
	part := func(k string) string {
		if v := props[k]; providers.Truthy(v) {
			return providers.PyStrip(providers.Str(v))
		}
		return ""
	}
	if name := providers.PyStrip(part("firstname") + " " + part("lastname")); name != "" {
		out["contact_person"] = name
	}
	return out
}

// FetchResult is the dict fetch_contacts returns.
type FetchResult struct {
	Success  bool
	Contacts []map[string]any
	Error    string
}

func (r FetchResult) MarshalJSON() ([]byte, error) {
	if !r.Success {
		return []byte(`{"success":false,"error":` + pyJSONString(r.Error) + `}`), nil
	}
	cs := r.Contacts
	if cs == nil {
		cs = []map[string]any{}
	}
	b, err := jsonMarshalNoEscape(cs)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(`{"success":true,"contacts":`), b...), '}'), nil
}

// FetchContacts pulls up to limit contacts (cursor paging, 429 retried). A
// non-empty listID restricts the pull to that HubSpot list (memberships, then
// batch read). fieldMap is {hubspot_property: lead_field}; nil uses
// ImportDefaultProperties. Each contact carries crm_external_id, crm_type and
// crm_object. HTTP failures come back as FetchResult{Success: false}.
func (h *HubSpot) FetchContacts(ctx context.Context, ws string, limit int, listID string, fieldMap FieldMap) FetchResult {
	tok := h.token(ctx, ws)
	if tok == "" {
		return FetchResult{Error: "No HubSpot token configured"}
	}
	props := append([]string(nil), ImportDefaultProperties...)
	if len(fieldMap) > 0 {
		props = props[:0]
		for _, fp := range fieldMap {
			props = append(props, fp.Key)
		}
	}
	get := func(path string, q [][2]string) (reply, error) {
		return h.send(ctx, call{method: "GET", path: path, query: q, token: tok, timeout: fetchTimeout, contentType: true, retry: true})
	}
	fail := func(err error) FetchResult {
		return FetchResult{Error: providers.TruncRunes(err.Error(), 200)}
	}
	row := func(obj any) (map[string]any, error) {
		p, err := providers.PyGet(obj, "properties", nil)
		if err != nil {
			return nil, err
		}
		var pm map[string]any
		if providers.Truthy(p) {
			var ok bool
			if pm, ok = p.(map[string]any); !ok {
				return nil, &providers.PyException{Msg: fmt.Sprintf("'%s' object has no attribute 'get'", providers.TypeName(p))}
			}
		}
		out := MapImportProperties(pm, fieldMap)
		id, _ := providers.PyGet(obj, "id", nil)
		out["crm_external_id"] = providers.Str(id)
		out["crm_type"] = "hubspot"
		out["crm_object"] = "contact"
		return out, nil
	}
	// checkPage maps the shared status handling of both page loops.
	checkPage := func(r reply) (any, *FetchResult) {
		if r.status == 401 || r.status == 403 {
			return nil, &FetchResult{Error: fmt.Sprintf("HubSpot token invalid or expired (HTTP %d)", r.status)}
		}
		if r.status != 200 {
			return nil, &FetchResult{Error: r.failText(160)}
		}
		data, err := parseJSON(r.body)
		if err != nil {
			f := fail(err)
			return nil, &f
		}
		return data, nil
	}
	nextAfter := func(data any) (string, error) {
		paging, err := providers.PyGet(data, "paging", nil)
		if err != nil {
			return "", err
		}
		next, err := orEmptyGet(paging, "next")
		if err != nil {
			return "", err
		}
		after, err := orEmptyGet(next, "after")
		if err != nil {
			return "", err
		}
		if !providers.Truthy(after) {
			return "", nil
		}
		return providers.Str(after), nil
	}

	var contacts []map[string]any
	if listID != "" {
		var ids []string
		after := ""
		for len(ids) < limit {
			q := [][2]string{{"limit", strconv.Itoa(min(250, limit-len(ids)))}}
			if after != "" {
				q = append(q, [2]string{"after", after})
			}
			r, err := get("/crm/v3/lists/"+url.PathEscape(listID)+"/memberships", q)
			if err != nil {
				return fail(err)
			}
			data, bad := checkPage(r)
			if bad != nil {
				return *bad
			}
			results, err := providers.PyGet(data, "results", []any{})
			if err != nil {
				return fail(err)
			}
			list, _ := results.([]any)
			for _, rec := range list {
				rid, err := providers.PyGet(rec, "recordId", nil)
				if err != nil {
					return fail(err)
				}
				ids = append(ids, providers.Str(rid))
			}
			if after, err = nextAfter(data); err != nil {
				return fail(err)
			}
			if after == "" {
				break
			}
		}
		if len(ids) > limit {
			ids = ids[:max(limit, 0)]
		}
		for i := 0; i < len(ids); i += 100 {
			chunk := ids[i:min(i+100, len(ids))]
			var b strings.Builder
			b.WriteString(`{"properties":[`)
			for j, p := range props {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteString(pyJSONString(p))
			}
			b.WriteString(`],"inputs":[`)
			for j, id := range chunk {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`{"id":` + pyJSONString(id) + `}`)
			}
			b.WriteString(`]}`)
			r, err := h.send(ctx, call{method: "POST", path: "/crm/v3/objects/contacts/batch/read", body: []byte(b.String()),
				token: tok, timeout: fetchTimeout, contentType: true, retry: true})
			if err != nil {
				return fail(err)
			}
			if r.status != 200 && r.status != 207 {
				return FetchResult{Error: r.failText(160)}
			}
			data, err := parseJSON(r.body)
			if err != nil {
				return fail(err)
			}
			results, err := providers.PyGet(data, "results", []any{})
			if err != nil {
				return fail(err)
			}
			list, _ := results.([]any)
			for _, o := range list {
				m, err := row(o)
				if err != nil {
					return fail(err)
				}
				contacts = append(contacts, m)
			}
		}
	} else {
		after := ""
		for len(contacts) < limit {
			q := [][2]string{{"limit", strconv.Itoa(min(100, limit-len(contacts)))}, {"properties", strings.Join(props, ",")}}
			if after != "" {
				q = append(q, [2]string{"after", after})
			}
			r, err := get("/crm/v3/objects/contacts", q)
			if err != nil {
				return fail(err)
			}
			data, bad := checkPage(r)
			if bad != nil {
				return *bad
			}
			results, err := providers.PyGet(data, "results", []any{})
			if err != nil {
				return fail(err)
			}
			list, _ := results.([]any)
			for _, o := range list {
				m, err := row(o)
				if err != nil {
					return fail(err)
				}
				contacts = append(contacts, m)
			}
			if after, err = nextAfter(data); err != nil {
				return fail(err)
			}
			if after == "" {
				break
			}
		}
	}
	if len(contacts) > limit {
		contacts = contacts[:max(limit, 0)]
	}
	return FetchResult{Success: true, Contacts: contacts}
}

// orEmptyGet is `(v or {}).get(key)`.
func orEmptyGet(v any, key string) (any, error) {
	if !providers.Truthy(v) {
		return nil, nil
	}
	return providers.PyGet(v, key, nil)
}

// GetContacts fetches recent contacts. As Python, any HTTP or decoding failure
// (and a missing token) yields an empty list; results are raw JSON elements; only a cancelled ctx is an error.
func (h *HubSpot) GetContacts(ctx context.Context, ws string, limit int) ([]json.RawMessage, error) {
	empty := []json.RawMessage{}
	tok := h.token(ctx, ws)
	if tok == "" {
		return empty, nil
	}
	r, err := h.send(ctx, call{method: "GET", path: "/crm/v3/objects/contacts", token: tok, timeout: callTimeout,
		query: [][2]string{{"limit", strconv.Itoa(limit)}, {"properties", "email,firstname,lastname,company,phone,jobtitle"}}})
	if err != nil {
		if ce := ctx.Err(); ce != nil {
			return empty, ce
		}
		return empty, nil
	}
	if r.status != 200 {
		return empty, nil
	}
	data, err := parseJSON(r.body)
	if err != nil {
		return empty, nil
	}
	results, err := providers.PyGet(data, "results", []any{})
	if err != nil {
		return empty, nil
	}
	if _, isList := results.([]any); !isList {
		return empty, nil
	}
	// Results are passed through as HubSpot sent them (key order, non-object elements).
	var raw struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(r.body, &raw); err != nil || raw.Results == nil {
		return empty, nil
	}
	return raw.Results, nil
}

func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
