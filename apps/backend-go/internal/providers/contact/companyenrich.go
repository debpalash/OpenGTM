package contact

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const companyEnrichBase = "https://api.companyenrich.com"

// Separate provider names make every paid action an explicit workbook choice.
// A company lookup never implicitly searches people or expands workforce data.
type companyEnrich struct{ action string }

func (p companyEnrich) Name() string   { return "companyenrich_" + p.action }
func (companyEnrich) KeyEnv() []string { return []string{"COMPANYENRICH_API_KEY"} }
func (companyEnrich) CacheByInput()    {}

func (p companyEnrich) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	if in.Field != "" && !p.supports(in.Field) {
		return fail("Unsupported CompanyEnrich target field"), nil
	}
	req, err := p.request(in)
	if err != nil {
		return fail(err.Error()), nil
	}
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No CompanyEnrich API key configured"), nil
	}
	req.Header = append(req.Header, [2]string{"Authorization", "Bearer " + k}, [2]string{"Accept", "application/json"})
	// A fresh HTTP/1 connection prevents Go from transparently replaying paid
	// GETs on reused connections. Clone the transport to retain its guarded dialer.
	client := *c
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if tr, ok := transport.(*http.Transport); ok {
		tr = tr.Clone()
		tr.DisableKeepAlives = true
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetHTTP1(true)
		client.Transport = tr
	}
	resp, err := providers.Do(ctx, &client, req, k)
	if err != nil {
		return finish(ctx, providers.Output{}, err)
	}
	if resp.Status != http.StatusOK {
		// Never expose the vendor body (which may contain input or credentials).
		return fail(fmt.Sprintf("CompanyEnrich HTTP %d; request was not retried", resp.Status)), nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return fail("Invalid CompanyEnrich JSON response"), nil
	}
	obj, ok := data.(map[string]any)
	if !ok || obj == nil {
		return fail("Invalid CompanyEnrich response: expected an object"), nil
	}
	f := newFields()
	switch p.action {
	case "company":
		if _, ok := obj["id"].(string); !ok {
			return fail("Invalid CompanyEnrich company response"), nil
		}
		for from, to := range map[string]string{"employees": "company_size", "description": "description", "industries": "industry_tags", "technologies": "technologies", "founded_year": "founding_year", "revenue": "revenue_estimate"} {
			ceCopy(f, to, obj[from])
		}
		if v, ok := obj["employees"].(string); ok {
			bands := map[string]string{"501-1K": "501-1000", "1K-5K": "1001-5000", "5K-10K": "5001-10000", "over-10K": "10001+"}
			if band, exists := bands[v]; exists {
				f.Set("company_size", band)
			}
		}
		if _, exists := f.Get("industry_tags"); !exists {
			ceCopy(f, "industry_tags", obj["industry"])
		}
		socials, _ := obj["socials"].(map[string]any)
		for _, name := range []string{"linkedin_url", "twitter_url", "facebook_url"} {
			ceCopy(f, name, socials[name])
		}
		loc, _ := obj["location"].(map[string]any)
		for _, name := range []string{"city", "state"} {
			item, _ := loc[name].(map[string]any)
			ceCopy(f, name, item["name"])
		}
		ceCopy(f, "address", loc["address"])
		financial, _ := obj["financial"].(map[string]any)
		ceCopy(f, "funding_stage", financial["funding_stage"])
	case "person":
		if _, ok := obj["id"].(int64); !ok {
			return fail("Invalid CompanyEnrich person response"), nil
		}
		ceCopy(f, "contact_person", obj["name"])
		ceCopy(f, "contact_title", obj["position"])
		socials, _ := obj["socials"].(map[string]any)
		ceCopy(f, "linkedin_url", socials["linkedin_url"])
	case "workforce":
		if _, ok := obj["company_id"].(string); !ok {
			return fail("Invalid CompanyEnrich workforce response"), nil
		}
	case "people", "search", "similar":
		if _, ok := obj["items"].([]any); !ok {
			return fail("Invalid CompanyEnrich search response"), nil
		}
	}
	// Keep the entire response as JSON text, including page metadata, nested fields,
	// zero counts, similarity scores and exact int64 person IDs. The workbook cache
	// stores values as text, so this shape stays identical before/after a cache hit.
	raw, err := json.Marshal(obj)
	if err != nil {
		return fail("Invalid CompanyEnrich response"), nil
	}
	f.Set(p.Name(), string(raw))
	return success(in, f, 0.7), nil // unmeasured prior, not an accuracy claim
}

func ceCopy(f *fields, key string, value any) {
	if value == nil || value == "" {
		return
	}
	f.Set(key, value)
}

func ceDomain(in providers.Input) (string, error) {
	raw := strings.TrimSpace(lead(in, "website"))
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Hostname() == "" || len(u.Hostname()) > 253 || strings.ContainsAny(u.Hostname(), " \t\r\n") {
		return "", fmt.Errorf("CompanyEnrich requires a valid company website")
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), nil
}

func (p companyEnrich) request(in providers.Input) (providers.Req, error) {
	domain, err := ceDomain(in)
	if err != nil && p.action != "person" && p.action != "search" {
		return providers.Req{}, err
	}
	post := func(path string, body jo) (providers.Req, error) { return jsonReq(companyEnrichBase+path, body), nil }
	get := func(path string, query ...[2]string) (providers.Req, error) {
		return providers.Req{Method: http.MethodGet, URL: companyEnrichBase + path, Query: query}, nil
	}
	switch p.action {
	case "company":
		if domain != "" {
			return get("/companies/enrich", [2]string{"domain", domain}, [2]string{"waitForEnrichment", "false"})
		}
		name := strings.TrimSpace(lead(in, "company"))
		if name == "" || utf8.RuneCountInString(name) > 1024 {
			return providers.Req{}, fmt.Errorf("CompanyEnrich company lookup requires website or company name (up to 1024 characters)")
		}
		return post("/companies/enrich", jo{{"name", name}})
	case "person":
		email := strings.TrimSpace(lead(in, "email"))
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email || len(email) > 254 {
			return providers.Req{}, fmt.Errorf("CompanyEnrich person lookup requires an existing email address")
		}
		return post("/people/lookup", jo{{"email", email}})
	case "workforce", "people", "similar":
		if domain == "" {
			return providers.Req{}, fmt.Errorf("CompanyEnrich %s requires a company website", p.action)
		}
		if p.action == "workforce" {
			return get("/companies/workforce", [2]string{"domain", domain})
		}
		body := jo{{"domains", []string{domain}}, {"page", 1}, {"pageSize", 10}}
		if p.action == "people" {
			return post("/people/search", body)
		}
		req := jsonReq(companyEnrichBase+"/companies/similar", body)
		req.Query = [][2]string{{"waitForEnrichment", "false"}}
		return req, nil
	case "search":
		query := strings.TrimSpace(lead(in, "description"))
		if query == "" || utf8.RuneCountInString(query) > 500 {
			return providers.Req{}, fmt.Errorf("CompanyEnrich search requires a target description (1–500 characters) in the description field")
		}
		return post("/companies/search", jo{{"semanticQuery", query}, {"page", 1}, {"pageSize", 10}})
	default:
		return providers.Req{}, fmt.Errorf("Unknown CompanyEnrich action")
	}
}

func (p companyEnrich) supports(field string) bool {
	if field == p.Name() {
		return true
	}
	var allowed []string
	switch p.action {
	case "company":
		allowed = []string{"company_size", "description", "industry_tags", "technologies", "founding_year", "revenue_estimate", "funding_stage", "linkedin_url", "twitter_url", "facebook_url", "city", "state", "address"}
	case "person":
		allowed = []string{"contact_person", "contact_title", "linkedin_url"}
	}
	for _, name := range allowed {
		if field == name {
			return true
		}
	}
	return false
}
