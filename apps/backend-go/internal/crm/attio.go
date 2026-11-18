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
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Attio push client: one person record per lead, upserted by email, linked to its company record when the lead has
// a domain. Push only; nothing is read back from Attio.
//
// Official API docs (confirmed 2026-11):
//   - https://docs.attio.com/rest-api/guides/authentication.md : "Authorization: Bearer <access_token>".
//   - https://docs.attio.com/rest-api/guides/rate-limiting.md : 25 requests per second for writes; over the limit the
//     status is 429, the request is not processed and Retry-After is an HTTP-date (usually the next second). A
//     company created through target_record_reference with if_unmatched=create counts as an extra write.
//   - https://docs.attio.com/rest-api/endpoint-reference/records/upsert-a-record.md : PUT /v2/objects/{object}/records
//     ?matching_attribute=<slug>, body {"data":{"values":{...}}}, scopes record_permission:read-write and
//     object_configuration:read, 200 returns data.id.record_id; errors are {status_code, type, code, message}
//     (400 validation_type and multiple_match_results, 403 unauthorized, 404 not_found, 409 uniqueness_conflict).
//   - Attribute types: https://docs.attio.com/rest-api/attribute-types/attribute-types-personal-name.md (object with
//     first_name, last_name and full_name, or a string), ...-email-address.md and ...-domain.md (strings in an
//     array), ...-phone-number.md (E.164 only, strings in an array), ...-text.md (a string),
//     ...-record-reference.md (target_record_reference with match, if_unmatched, create).
//   - https://docs.attio.com/docs/standard-objects/standard-objects-people : slugs name, email_addresses,
//     phone_numbers, job_title, linkedin, company; .../standard-objects-companies : name, domains.
//
// The token is scrubbed from every error text, and so is every lead value sent (an Attio validation message may
// echo the rejected value, which is PII). Nothing here logs.

// AttioBaseURL is the Attio REST host.
const AttioBaseURL = "https://api.attio.com"

const attioNotConnected = "Attio not connected (set ATTIO_ACCESS_TOKEN)"

// Attio write targets a field map may name. Anything else (custom attributes) is ignored.
const (
	AttioEmail       = "email_addresses"
	AttioName        = "name"
	AttioPhone       = "phone_numbers"
	AttioJobTitle    = "job_title"
	AttioLinkedIn    = "linkedin"
	AttioCompanyDom  = "company_domain" // links the person to the company with this domain
	AttioCompanyName = "company_name"   // names the company when it has to be created
)

// AttioDefaultFieldMap maps lead fields to Attio targets (ordered: it fixes the request body order).
var AttioDefaultFieldMap = FieldMap{
	{"email", AttioEmail},
	{"contact_person", AttioName},
	{"phone", AttioPhone},
	{"contact_title", AttioJobTitle},
	{"linkedin_url", AttioLinkedIn},
	{"website", AttioCompanyDom},
	{"company", AttioCompanyName},
}

// Attio is the Attio REST client. The token is resolved on every call from the workspace (then global, then env,
// both only with ALLOW_GLOBAL_CREDENTIAL_FALLBACK) and never stored on the client.
type Attio struct {
	HTTP    *http.Client      // nil: the SSRF-guarded egress client
	Secrets *secrets.Resolver // nil: ATTIO_ACCESS_TOKEN from the environment, only with ALLOW_GLOBAL_CREDENTIAL_FALLBACK
	BaseURL string            // "" = AttioBaseURL (tests)
	// Sleep waits between 429 retries; nil honours ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (a *Attio) token(ctx context.Context) func(ws string) string {
	return func(ws string) string {
		if a.Secrets == nil {
			return secrets.EnvCredential("ATTIO_ACCESS_TOKEN")
		}
		v, err := a.Secrets.Credential(ctx, ws, "ATTIO_ACCESS_TOKEN")
		if err != nil { // fail closed: no request, no fallback
			return ""
		}
		return v
	}
}

// IsConnected reports whether a token is configured for ws.
func (a *Attio) IsConnected(ctx context.Context, ws string) bool { return a.token(ctx)(ws) != "" }

// AttioPushResult is the outcome of one push. Action is "upserted": the upsert answer does not say whether the
// record was created or updated.
type AttioPushResult struct {
	Success bool   `json:"success"`
	Action  string `json:"action,omitempty"`
	AttioID string `json:"attio_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

const attioNoEmailMsg = "Lead has no email — required for Attio person"

var (
	attioPhoneRE  = regexp.MustCompile(`^\+[0-9 ()\-.]+$`)
	attioDigitsRE = regexp.MustCompile(`[0-9]`)
)

// orderedBody builds a JSON object with a fixed key order.
type orderedBody struct {
	keys []string
	vals map[string]any
}

func (o *orderedBody) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *orderedBody) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := jsonMarshalNoEscape(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(bytesTrimNL(vb))
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func bytesTrimNL(b []byte) []byte { return []byte(strings.TrimRight(string(b), "\n")) }

// attioName is the personal-name value: the object form (all three properties are required by Attio) when the text
// has a first and a last name, the string form (read as a first name) for a single word.
func attioName(text string) any {
	words := strings.Fields(strings.ReplaceAll(text, ",", " "))
	switch len(words) {
	case 0:
		return nil
	case 1:
		return words[0]
	}
	n := &orderedBody{}
	n.set("first_name", words[0])
	n.set("last_name", strings.Join(words[1:], " "))
	n.set("full_name", strings.Join(words, " "))
	return n
}

// attioPhone returns the number when Attio will accept it (E.164 with a leading +), else "".
func attioPhone(s string) string {
	s = strings.TrimSpace(s)
	if !attioPhoneRE.MatchString(s) || len(attioDigitsRE.FindAllString(s, -1)) < 7 {
		return ""
	}
	return s
}

// attioDomain reduces a website or domain to the host Attio matches companies by ("" when it is not one).
func attioDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	h := strings.TrimPrefix(u.Hostname(), "www.")
	if !strings.Contains(h, ".") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return ""
	}
	return h
}

// attioValues maps the lead onto an Attio person's values per fieldMap (nil: AttioDefaultFieldMap). A missing or
// empty optional field is omitted. sent lists every lead value placed in the body (for scrubbing error text).
func attioValues(lead Lead, fieldMap FieldMap) (vals *orderedBody, email string, sent []string) {
	fmap := fieldMap
	if len(fmap) == 0 {
		fmap = AttioDefaultFieldMap
	}
	got := map[string]string{}
	for _, fp := range fmap {
		v, ok := lead.Get(fp.Key)
		if !ok || !providers.Truthy(v) {
			continue
		}
		if s := strings.TrimSpace(providers.Str(v)); s != "" {
			got[fp.Value] = s
		}
	}
	email = got[AttioEmail]
	if !strings.Contains(email, "@") {
		return nil, "", nil
	}
	vals = &orderedBody{}
	vals.set(AttioEmail, []any{email})
	sent = append(sent, email)
	if nv := attioName(got[AttioName]); nv != nil {
		vals.set(AttioName, nv)
		sent = append(sent, strings.Fields(got[AttioName])...)
		sent = append(sent, got[AttioName])
	}
	if p := attioPhone(got[AttioPhone]); p != "" {
		vals.set(AttioPhone, []any{p})
		sent = append(sent, p)
	}
	if t := got[AttioJobTitle]; t != "" {
		vals.set(AttioJobTitle, t)
		sent = append(sent, t)
	}
	if l := got[AttioLinkedIn]; l != "" {
		vals.set(AttioLinkedIn, l)
		sent = append(sent, l)
	}
	if d := attioDomain(got[AttioCompanyDom]); d != "" {
		ref := &orderedBody{}
		ref.set("target_object", "companies")
		ref.set("match", &orderedBody{keys: []string{"domains"}, vals: map[string]any{"domains": []any{d}}})
		ref.set("if_unmatched", "create")
		if cn := got[AttioCompanyName]; cn != "" {
			ref.set("create", &orderedBody{keys: []string{"name"}, vals: map[string]any{"name": cn}})
			sent = append(sent, cn)
		}
		vals.set("company", []any{ref})
		sent = append(sent, d)
	}
	return vals, email, sent
}

// attioError is the {status_code, type, code, message} error body.
type attioError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PushLeadAsPerson upserts the lead as an Attio person (matched by email) and, when the lead has a domain, links
// or creates its company. Re-delivery updates the same record.
func (a *Attio) PushLeadAsPerson(ctx context.Context, ws string, lead Lead, fieldMap FieldMap) AttioPushResult {
	tok := a.token(ctx)(ws)
	if tok == "" {
		return AttioPushResult{Error: attioNotConnected}
	}
	vals, _, sent := attioValues(lead, fieldMap)
	if vals == nil {
		return AttioPushResult{Error: attioNoEmailMsg}
	}
	data := &orderedBody{}
	data.set("values", vals)
	body := &orderedBody{}
	body.set("data", data)
	b, err := body.MarshalJSON()
	if err != nil {
		return AttioPushResult{Error: "Attio request could not be encoded"}
	}
	r, err := a.send(ctx, call{method: "PUT", path: "/v2/objects/people/records",
		query: [][2]string{{"matching_attribute", AttioEmail}}, body: b, token: tok, timeout: callTimeout,
		contentType: true, retry: true})
	if err != nil {
		return AttioPushResult{Error: scrubValues(err.Error(), sent)}
	}
	switch {
	case r.status == 200:
		var out struct {
			Data struct {
				ID struct {
					RecordID string `json:"record_id"`
				} `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(r.body, &out) != nil || out.Data.ID.RecordID == "" {
			return AttioPushResult{Error: "Attio answered without a record id"}
		}
		return AttioPushResult{Success: true, Action: "upserted", AttioID: out.Data.ID.RecordID}
	case r.status == 401:
		return AttioPushResult{Error: "Attio auth failed (invalid ATTIO_ACCESS_TOKEN)"}
	case r.status == 403:
		return AttioPushResult{Error: "Attio token lacks permission (needs record_permission:read-write and object_configuration:read)"}
	case r.status == 429:
		return AttioPushResult{Error: "HTTP 429: Attio rate limit (25 writes per second)"}
	}
	var e attioError
	_ = json.Unmarshal(r.body, &e)
	msg := scrubValues(providers.TruncRunes(strings.TrimSpace(e.Code+": "+e.Message), 160), sent)
	if e.Code == "" && e.Message == "" {
		msg = scrubValues(providers.TruncRunes(r.text(), 160), sent)
	}
	return AttioPushResult{Error: fmt.Sprintf("HTTP %d: %s", r.status, providers.Scrub(msg, tok))}
}

// scrubValues removes every lead value sent from s.
func scrubValues(s string, sent []string) string {
	for _, v := range sent {
		if len(v) >= 3 {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

func (a *Attio) base() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return AttioBaseURL
}

func (a *Attio) client() *http.Client {
	c := a.HTTP
	if c == nil {
		c = defaultClient()
	}
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

// send is one Attio call: Bearer auth, redirects refused, bounded body, a 429 retried with Retry-After (an
// HTTP-date) or exponential backoff.
func (a *Attio) send(ctx context.Context, c call) (reply, error) {
	u := a.base() + c.path
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
		cctx, cancel := context.WithTimeout(ctx, c.timeout)
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
	resp, err := RequestWithRetry(ctx, a.client(), build, DefaultMaxRetries, a.Sleep)
	if err != nil {
		return reply{}, a.scrubErr(ctx, err, c.token)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, providers.MaxBody+1))
	if err != nil {
		return reply{}, a.scrubErr(ctx, err, c.token)
	}
	if len(b) > providers.MaxBody {
		return reply{}, &callError{msg: fmt.Sprintf("response body exceeds %d bytes", providers.MaxBody)}
	}
	return reply{status: resp.StatusCode, body: b, token: c.token}, nil
}

// scrubErr turns a transport error into one that can never carry the token.
func (a *Attio) scrubErr(ctx context.Context, err error, token string) error {
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &callError{msg: providers.Scrub(err.Error(), token)}
}
