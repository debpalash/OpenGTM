package api

import (
	"bufio"
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
	"sync/atomic"
	"time"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// POST /api/lead/{lead_id}/enrich?action=... (routers/leads.py enrich_lead): streams SSE progress for one lead.
//
// Actions and where their work comes from:
//   - web_research: the chat provider streamed over the egress client (here, WP-LLMROUTES).
//   - find_address: Nominatim over the egress client (here); the web-search fallback goes through Searcher.
//   - find_emails / scrape_website: providers by registry name (providerLookup). WP-PROVIDERS registers
//     ddg_email_provider and website_scraper_provider; until it lands the lookup misses and the action answers a
//     terminal SSE error step. The Python path (flag off, nginx) is unchanged meanwhile.
//   - find_phone / the find_address fallback: Searcher (WP-WEB's search over the browser service). Unset today.

// SearchHit is one web search result.
type SearchHit struct{ Title, Href, Body string }

// Searcher is the web search seam (WP-WEB). Search returns at most max hits in engine order.
type Searcher interface {
	Search(ctx context.Context, query string, max int) ([]SearchHit, error)
}

// providerLookup finds an enrichment provider by registry name.
type providerLookup interface {
	Get(name string) (providers.Provider, bool)
}

// registryProviders reads the process-wide providers registry at request time, so providers registered later
// (WP-PROVIDERS) are picked up without a code change here.
type registryProviders struct{}

func (registryProviders) Get(name string) (providers.Provider, bool) { return providers.Get(name) }

const (
	webResearchSystem = "You are a business analyst who researches companies for sales intelligence. Be concise and specific."
	enrichStreamIdle  = 60 * time.Second
	nominatimTimeout  = 10 * time.Second
	enrichBodyCap     = 1 << 20
)

// The web_research stream is bounded overall, not only by idleness: a provider (or a hostile base URL) that keeps
// the connection busy must not hold the request, memory or the SSE response open for ever. Variables so tests can
// lower them.
var (
	enrichStreamTotal    = 5 * time.Minute
	enrichStreamMaxBytes = 4 << 20 // raw bytes read from the provider, SSE framing included
	enrichTextMax        = 256 << 10
)

type enrichCtxKey struct{}

type enrichTarget struct {
	ID     int64
	Action string
}

// leadEnrich validates the path and query like FastAPI (a bad lead_id is a 422 that does not count against the
// rate limit), then runs the limited stream.
func (h *llmRoutes) leadEnrich(w http.ResponseWriter, r *http.Request) {
	id, none, ok := pathLeadID(w, r, nil)
	if !ok {
		return
	}
	action := "web_research"
	if q := r.URL.Query()["action"]; len(q) > 0 {
		action = q[len(q)-1]
	}
	t := enrichTarget{ID: id, Action: action}
	if none {
		t.ID = 0
	}
	h.limitedEnrich().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), enrichCtxKey{}, t)))
}

func (h *llmRoutes) limitedEnrich() http.Handler {
	h.enrichOnce.Do(func() {
		var inner http.Handler = http.HandlerFunc(h.enrichStream)
		if h.d.Limiter != nil {
			inner = h.d.Limiter.Limit("30/minute")(inner)
		}
		h.enrichLimited = inner
	})
	return h.enrichLimited
}

type sseOut struct {
	w http.ResponseWriter
	f http.Flusher
}

// event writes one `data: <json.dumps(dict)>` frame: Python's separators, ensure_ascii, keys in the given order.
func (s sseOut) event(kv ...any) {
	var b strings.Builder
	b.WriteString("data: {")
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteString(", ")
		}
		pyDumpString(&b, kv[i].(string))
		b.WriteString(": ")
		switch v := kv[i+1].(type) {
		case string:
			pyDumpString(&b, v)
		case []string:
			b.WriteByte('[')
			for j, e := range v {
				if j > 0 {
					b.WriteString(", ")
				}
				pyDumpString(&b, e)
			}
			b.WriteByte(']')
		}
	}
	b.WriteString("}\n\n")
	_, _ = io.WriteString(s.w, b.String())
	if s.f != nil {
		s.f.Flush()
	}
}

// pyDumpString writes a JSON string literal as json.dumps does with ensure_ascii.
func pyDumpString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	b.WriteByte('"')
}

func (h *llmRoutes) enrichStream(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(enrichCtxKey{}).(enrichTarget)
	ws := h.wsID(r)
	lead, err := h.store.Load(r.Context(), ws, t.ID)
	if err != nil {
		h.internal(w, "enrich load", err)
		return
	}
	if lead == nil {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	out := sseOut{w: w, f: f}
	run := &enrichRun{h: h, ctx: r.Context(), ws: ws, lead: lead, out: out}
	run.do(t.Action)
}

type enrichRun struct {
	h    *llmRoutes
	ctx  context.Context
	ws   string
	lead *leadsread.Lead
	out  sseOut
}

func (e *enrichRun) fail(what string, err error) {
	e.h.log.Error("lead enrich: "+what+" failed", "err_type", errTypeName(err))
	e.out.event("step", "error", "message", "Enrichment failed: "+what)
}

func (e *enrichRun) do(action string) {
	switch action {
	case "web_research":
		e.webResearch()
	case "find_emails":
		e.findEmails()
	case "scrape_website":
		e.scrapeWebsite()
	case "find_phone":
		e.findPhone()
	case "find_address":
		e.findAddress()
	default:
		e.out.event("step", "error", "message", "Unknown action: "+action)
	}
}

// saveFields is update_lead_fields; last_enriched_at is an ISO timestamp (Python stored the literal "now").
func (e *enrichRun) saveFields(fields map[string]any) error {
	if _, ok := fields["last_enriched_at"]; ok {
		fields["last_enriched_at"] = time.Now().UTC().Format("2006-01-02T15:04:05.000000") + "+00:00"
	}
	return e.h.store.Save(e.ctx, e.ws, int64(e.lead.ID), fields)
}

func orUnknown(p *string) string {
	if v := orStr(p); v != "" {
		return v
	}
	return "Unknown"
}

func orNone(p *string) string {
	if v := orStr(p); v != "" {
		return v
	}
	return "None"
}

func (e *enrichRun) researchPrompt() string {
	l := e.lead
	return "Research this company and provide a comprehensive analysis:\n\n" +
		"Company: " + l.Company + "\n" +
		"City: " + orUnknown(l.City) + "\n" +
		"Website: " + orUnknown(l.Website) + "\n" +
		"Specialization: " + orUnknown(l.Specialization) + "\n" +
		"Current description: " + orNone(l.Description) + "\n\n" +
		`Provide:
1. **Company Overview** (2-3 sentences about what they do)
2. **Key Services/Products** they offer
3. **Company Size** estimate if possible
4. **Industry/Niche** they operate in
5. **Potential Needs** — what problems they likely face
6. **Outreach Angle** — best way to approach them as a lead

Format as clean text with section headers. Be specific and actionable.`
}

// chatProvider is the provider web_research streams from: the head of the chat chain. Python sent every provider
// to /chat/completions; a native one (Anthropic) does not serve it, so the first OpenAI-compatible provider is
// used instead (deviation).
func chatProvider(ctx context.Context, c chatSource) llm.ChatProvider {
	p := c.ActiveChatProvider(ctx)
	if p.OpenAICompatible {
		return p
	}
	for _, q := range c.ChatChain(ctx) {
		if q.OpenAICompatible {
			return q
		}
	}
	return p
}

func (e *enrichRun) webResearch() {
	l := e.lead
	e.out.event("step", "start", "action", "web_research", "message", "Researching "+l.Company+"...")
	b := e.h.newLLM(e.ctx, e.ws)
	prov := chatProvider(e.ctx, b.Chat)
	if prov.APIKey == "" {
		e.out.event("step", "error", "message", "No AI provider configured. Go to Settings.")
		return
	}
	body, _ := json.Marshal(map[string]any{
		"model": prov.Model,
		"messages": []map[string]string{
			{"role": "system", "content": webResearchSystem},
			{"role": "user", "content": e.researchPrompt()},
		},
		"stream":      true,
		"temperature": 0.5,
	})
	rctx, cancel := context.WithTimeout(e.ctx, enrichStreamTotal)
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(enrichStreamIdle, func() { timedOut.Store(true); cancel() })
	defer timer.Stop()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(prov.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		e.fail("request", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.h.httpc.Do(req)
	if err != nil {
		e.streamErr(err, timedOut.Load() || rctx.Err() == context.DeadlineExceeded)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, enrichBodyCap))
		e.out.event("step", "error", "message", "LLM error: "+runeHead(strings.ToValidUTF8(string(raw), "�"), 200))
		return
	}
	var full strings.Builder
	limited := &io.LimitedReader{R: resp.Body, N: int64(enrichStreamMaxBytes) + 1}
	br := bufio.NewReaderSize(limited, 64<<10)
	for {
		line, rerr := br.ReadString('\n')
		if limited.N <= 0 {
			e.out.event("step", "error", "message", "LLM response too large")
			return
		}
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok {
			// Only a data frame is progress: comments and blank keep-alives do not reset the idle timer.
			timer.Reset(enrichStreamIdle)
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
				if tok := chunk.Choices[0].Delta.Content; tok != "" {
					if full.Len()+len(tok) > enrichTextMax {
						e.out.event("step", "error", "message", "LLM response too large")
						return
					}
					full.WriteString(tok)
					e.out.event("step", "token", "content", tok)
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				e.streamErr(rerr, timedOut.Load() || rctx.Err() == context.DeadlineExceeded)
				return
			}
			break
		}
	}
	if full.Len() > 0 {
		if err := e.saveFields(map[string]any{"description": runeHead(full.String(), 2000), "last_enriched_at": "now"}); err != nil {
			e.fail("saving the research", err)
			return
		}
		e.out.event("step", "saved", "message", "Research saved to lead")
	}
	e.out.event("step", "done", "action", "web_research")
}

func (e *enrichRun) streamErr(err error, timedOut bool) {
	if e.ctx.Err() != nil {
		return // the client went away
	}
	msg := "LLM request failed"
	if timedOut {
		msg = "LLM request timed out"
	}
	e.h.log.Warn("lead enrich: LLM stream failed", "err_type", errTypeName(err))
	e.out.event("step", "error", "message", msg)
}

// leadMap is the lead dict a provider receives.
func (e *enrichRun) leadMap() map[string]any {
	m := map[string]any{"company": e.lead.Company}
	for _, k := range []string{"website", "email", "phone", "contact_person", "city", "state", "linkedin_url", "specialization", "description"} {
		if v, ok := e.lead.Get(k); ok {
			if s, isPtr := v.(*string); isPtr {
				if s == nil {
					m[k] = ""
				} else {
					m[k] = *s
				}
			} else if v != nil {
				m[k] = v
			}
		}
	}
	return m
}

func noKey() (string, error) { return "", nil }

// Lookup names per action: the Python provider name first, then the Go registry name.
var (
	emailProviderNames   = []string{"ddg_email_provider", "ddg_email"}
	scraperProviderNames = []string{"website_scraper_provider", "website_scraper"}
)

func (e *enrichRun) provider(names ...string) (providers.Provider, bool) {
	lk := e.h.providerLookup
	if lk == nil {
		return nil, false
	}
	for _, n := range names {
		if p, ok := lk.Get(n); ok {
			return p, true
		}
	}
	return nil, false
}

func (e *enrichRun) findEmails() {
	e.out.event("step", "start", "action", "find_emails", "message", "Finding emails...")
	p, ok := e.provider(emailProviderNames...)
	if !ok {
		e.out.event("step", "error", "message", "Email finder is not available")
		return
	}
	out, err := p.Enrich(e.ctx, e.h.userHTTP, noKey, providers.Input{Lead: e.leadMap(), Field: "email"})
	if err != nil {
		if e.ctx.Err() == nil {
			e.fail("the email search", err)
		}
		return
	}
	email, _ := out.Value.(string)
	if out.Error == "" && email != "" {
		if err := e.saveFields(map[string]any{"email": email, "last_enriched_at": "now"}); err != nil {
			e.fail("saving the email", err)
			return
		}
		e.out.event("step", "result", "email", email)
	} else {
		e.out.event("step", "result", "message", "No email found")
	}
	e.out.event("step", "done", "action", "find_emails")
}

func (e *enrichRun) scrapeWebsite() {
	l := e.lead
	site := orStr(l.Website)
	if site == "" {
		e.out.event("step", "error", "message", "No website URL on this lead")
		return
	}
	e.out.event("step", "start", "action", "scrape_website", "message", "Scraping "+site+"...")
	// The site is user data: refuse private, loopback and metadata targets before any provider dials it.
	if err := egress.CheckURLFor(e.h.appEnv, e.h.allowHost, site, true); err != nil {
		e.out.event("step", "error", "message", "Website URL is not allowed")
		return
	}
	p, ok := e.provider(scraperProviderNames...)
	if !ok {
		e.out.event("step", "error", "message", "Website scraper is not available")
		return
	}
	out, err := p.Enrich(e.ctx, e.h.userHTTP, noKey, providers.Input{Lead: e.leadMap(), Field: "email"})
	if err != nil {
		if e.ctx.Err() == nil {
			e.fail("the website scrape", err)
		}
		return
	}
	// Python compared the scraper's result with the lead it had just mutated in place, so it never saved
	// anything; here the result is compared with the lead as loaded (deviation: the fix).
	fields := map[string]any{}
	var order []string
	for _, c := range []struct {
		key string
		old *string
	}{{"email", l.Email}, {"phone", l.Phone}, {"description", l.Description}, {"contact_person", l.ContactPerson}} {
		v, present := out.Extra.Get(c.key)
		s, _ := v.(string)
		if present && s != "" && s != orStr(c.old) {
			fields[c.key] = s
			order = append(order, c.key)
		}
	}
	if len(fields) > 0 {
		fields["last_enriched_at"] = "now"
		order = append(order, "last_enriched_at")
		if err := e.saveFields(fields); err != nil {
			e.fail("saving the scraped fields", err)
			return
		}
		e.out.event("step", "result", "fields_updated", order)
	} else {
		e.out.event("step", "result", "message", "No new data found")
	}
	e.out.event("step", "done", "action", "scrape_website")
}

var phonePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\+?91[\-\s]?[0-9]{5}[\-\s]?[0-9]{5}`),
	regexp.MustCompile(`\+?91[\-\s]?[0-9]{10}`),
	regexp.MustCompile(`1800[\-\s]?[0-9]{2,3}[\-\s]?[0-9]{4,6}`),
	regexp.MustCompile(`0[0-9]{2,4}[\-\s]?[0-9]{6,8}`),
	regexp.MustCompile(`\b[0-9]{10}\b`),
}

// extractPhone is search_enricher._extract_phone: the first pattern (in list order) that matches wins.
// (Python's \d also matches non-ASCII digits; here only 0-9.)
func extractPhone(text string) string {
	for _, p := range phonePatterns {
		if m := p.FindString(text); m != "" {
			return pyStrip(m)
		}
	}
	return ""
}

func pyIsSpaceRune(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }
func pyStrip(s string) string   { return strings.TrimFunc(s, pyIsSpaceRune) }
func pyRstrip(s string) string  { return strings.TrimRightFunc(s, pyIsSpaceRune) }

func (e *enrichRun) findPhone() {
	l := e.lead
	e.out.event("step", "start", "action", "find_phone", "message", "Searching phone for "+l.Company+"...")
	s := e.h.searcher
	if s == nil {
		e.out.event("step", "error", "message", "Web search is not available")
		return
	}
	city := orStr(l.City)
	found := ""
	for _, q := range []string{
		l.Company + " " + city + " contact number phone",
		l.Company + " " + city + " office phone number",
	} {
		hits, err := s.Search(e.ctx, pyStrip(q), 5)
		if err != nil {
			if e.ctx.Err() != nil {
				return
			}
			continue
		}
		for _, hit := range hits {
			if p := extractPhone(hit.Title + " " + hit.Body); p != "" {
				found = p
				break
			}
		}
		if found != "" {
			break
		}
	}
	if found != "" {
		if err := e.saveFields(map[string]any{"phone": found, "last_enriched_at": "now"}); err != nil {
			e.fail("saving the phone", err)
			return
		}
		e.out.event("step", "result", "message", "Phone found: "+found, "phone", found)
	} else {
		e.out.event("step", "result", "message", "No phone number found")
	}
	e.out.event("step", "done", "action", "find_phone")
}

var addrRE = regexp.MustCompile(`(?i)(?:address|located|office)[:\s]+([^.]+)`)

var indianStates = []string{"Maharashtra", "Karnataka", "Tamil Nadu", "Delhi", "Telangana",
	"Gujarat", "Rajasthan", "Uttar Pradesh", "West Bengal", "Kerala",
	"Madhya Pradesh", "Haryana", "Punjab", "Andhra Pradesh", "Bihar"}

type geoResult struct {
	Display string
	Lat     string
	Lon     string
}

// jsonText is str(v) for a JSON scalar (Nominatim sends strings; numbers keep their text).
func jsonText(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	t := strings.TrimSpace(string(v))
	if t == "null" {
		return "None"
	}
	return t
}

// nominatim returns the first result, ok=false for none or a non-200 answer.
func (e *enrichRun) nominatim(q string) (geoResult, bool, error) {
	u, err := url.Parse(e.h.nominatimURL)
	if err != nil {
		return geoResult{}, false, err
	}
	v := url.Values{"q": {q}, "format": {"json"}, "limit": {"3"}, "addressdetails": {"1"}}
	u.RawQuery = v.Encode()
	ctx, cancel := context.WithTimeout(e.ctx, nominatimTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return geoResult{}, false, err
	}
	req.Header.Set("User-Agent", "LeadEngine/1.0")
	resp, err := e.h.httpc.Do(req)
	if err != nil {
		return geoResult{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return geoResult{}, false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, enrichBodyCap))
	if err != nil {
		return geoResult{}, false, err
	}
	var results []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &results); err != nil {
		return geoResult{}, false, errors.New("invalid geocoder response")
	}
	if len(results) == 0 {
		return geoResult{}, false, nil
	}
	best := results[0]
	g := geoResult{}
	if v, ok := best["display_name"]; ok {
		g.Display = jsonText(v)
	}
	if v, ok := best["lat"]; ok {
		g.Lat = jsonText(v)
	}
	if v, ok := best["lon"]; ok {
		g.Lon = jsonText(v)
	}
	return g, true, nil
}

func (e *enrichRun) findAddress() {
	l := e.lead
	e.out.event("step", "start", "action", "find_address", "message", "Looking up address for "+l.Company+"...")
	city := orStr(l.City)
	if city == "" {
		city = "India"
	}
	address, lat, lon := "", "", ""
	g, ok, err := e.nominatim(l.Company + ", " + city)
	switch {
	case err != nil:
		if e.ctx.Err() != nil {
			return
		}
		reason := err.Error()
		var ue *url.Error
		if errors.As(err, &ue) {
			reason = ue.Err.Error() // without the request URL
		}
		e.out.event("step", "result", "message", "OSM lookup failed: "+runeHead(reason, 80))
	case ok:
		address, lat, lon = g.Display, g.Lat, g.Lon
		e.out.event("step", "result", "message", "OSM: "+runeHead(address, 100), "source", "openstreetmap")
	}
	if address == "" && e.h.searcher != nil {
		q := pyStrip(l.Company + " " + orStr(l.City) + " office address location")
		if hits, err := e.h.searcher.Search(e.ctx, q, 5); err == nil {
			for _, hit := range hits {
				if m := addrRE.FindStringSubmatch(hit.Body); m != nil {
					address = runeHead(pyStrip(m[1]), 200)
					e.out.event("step", "result", "message", "Search: "+runeHead(address, 100), "source", "web_search")
					break
				}
			}
		} else if e.ctx.Err() != nil {
			return
		}
	}
	if address == "" {
		e.out.event("step", "result", "message", "No address found")
		e.out.event("step", "done", "action", "find_address")
		return
	}
	note := "\n\n📍 Address: " + runeHead(address, 300)
	if lat != "" && lon != "" {
		note += "\n🗺️ Map: https://www.openstreetmap.org/?mlat=" + lat + "&mlon=" + lon + "#map=16/" + lat + "/" + lon
	}
	fields := map[string]any{"last_enriched_at": "now", "notes": pyRstrip(orStr(l.Notes)) + note}
	if orStr(l.State) == "" {
		low := strings.ToLower(address)
		for _, st := range indianStates {
			if strings.Contains(low, strings.ToLower(st)) {
				fields["state"] = st
				break
			}
		}
	}
	if err := e.saveFields(fields); err != nil {
		e.fail("saving the address", err)
		return
	}
	e.out.event("step", "saved", "message", "Address saved")
	e.out.event("step", "done", "action", "find_address")
}
