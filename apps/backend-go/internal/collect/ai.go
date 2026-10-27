package collect

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// The LLM stages of the pipeline (ai_stages.py). Prompts are product behaviour: the text is the
// legacy f-strings verbatim (vectors in testdata/ai.json).

// LLM is the part of *llm.Client the stages use. ExtractJSON's error is legacy extract_json's
// empty result ({}): every caller treats a failure like an answer without the expected keys.
type LLM interface {
	ExtractJSON(ctx context.Context, prompt, system string, maxTokens int) (map[string]any, error)
	Usage() llm.TokenUsage
}

func extract(ctx context.Context, c LLM, prompt, system string, maxTokens int) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	m, err := c.ExtractJSON(ctx, prompt, system, maxTokens)
	if err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// ── clean_page_text ────────────────────────────────────────────────────────

var droppedTags = map[string]bool{"script": true, "style": true, "nav": true, "footer": true, "header": true,
	"noscript": true, "iframe": true}

var wsRunRe = mustRx(`\s+`)

// CleanPageText is clean_page_text: the page's text without script, style, nav, footer, header,
// noscript and iframe elements (BeautifulSoup get_text(" ", strip=True)), whitespace collapsed,
// at most maxChars characters.
func CleanPageText(src string, maxChars int) string {
	doc, err := html.Parse(strings.NewReader(src))
	var parts []string
	if err == nil {
		var walk func(n *html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && droppedTags[n.Data] {
				return
			}
			if n.Type == html.TextNode {
				if s := pyStrip(n.Data); s != "" {
					parts = append(parts, s)
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
	}
	text := pyStrip(wsRunRe.Sub(strings.Join(parts, " "), " "))
	return truncRunes(text, maxChars)
}

// ── ai_expand_query ────────────────────────────────────────────────────────

const expandSystem = "You are a B2B lead generation expert. Generate search queries that find company websites directly."

func expandPrompt(query string) string {
	return `Generate exactly 4 DuckDuckGo search queries to find real companies for: "` + query + `"

Rules:
- Each query should find actual company WEBSITES, not directories or listicles
- Include the company type and location from the original query
- Vary the keywords: use "company", "pvt ltd", "agency", "services" etc.
- One query should target the company's about/contact page
- Do NOT include queries for "top 10" or "best" lists

Return JSON: {"queries": ["query1", "query2", "query3", "query4"]}`
}

// AIExpandQuery is ai_expand_query: up to four LLM search queries when the answer has at least
// two, else [query]. Non-string entries are rendered like Python str() would not be needed for:
// they are kept out (legacy passed them on to the search call, which failed for them).
func AIExpandQuery(ctx context.Context, c LLM, query string) []string {
	data := extract(ctx, c, expandPrompt(query), expandSystem, 200)
	qs, _ := data["queries"].([]any)
	if len(qs) >= 2 {
		if len(qs) > 4 {
			qs = qs[:4]
		}
		out := make([]string, 0, len(qs))
		for _, q := range qs {
			out = append(out, pyStr(q))
		}
		return out
	}
	return []string{query}
}

// ── ai_extract_company ─────────────────────────────────────────────────────

const extractSystem = "You are a data extraction expert. Extract only factual information visible on the page. Never guess or hallucinate. For fields not found on the page, use null."

func extractPrompt(url, clean string) string {
	return `Extract business information from this company webpage.

URL: ` + url + `
Page content:
` + clean + `

Return JSON with these fields (use null for fields you're not confident about):
{
  "company_name": "Official registered company name (not a tagline or slogan)",
  "description": "What the company does in 1-2 sentences",
  "email": "Primary business email address (not personal gmail/yahoo)",
  "phone": "Primary phone number with country code",
  "city": "City where the company is headquartered",
  "state": "State or region",
  "address": "Full registered office address if mentioned",
  "specialization": "Primary service type (e.g. IT Staffing, HR Consulting, RPO)",
  "industry_tags": "Comma-separated industry tags (e.g. IT Staffing, Payroll, Contract Staffing)",
  "employee_count": "Approximate employee count or range like '50-200'",
  "employee_count_exact": null,
  "revenue_range": "Annual revenue if mentioned (e.g. ₹10-50 Cr, $5M-10M)",
  "founded_year": "Year the company was founded/established",
  "technologies": "Key technologies, platforms, or tools mentioned (e.g. SAP, Workday, Oracle HCM)",
  "funding_stage": "Funding stage if mentioned (Bootstrapped, Seed, Series A, etc.)",
  "contact_person": "Name and title of a key contact if mentioned (e.g. 'Rahul Sharma, CEO')",
  "secondary_emails": "Any additional email addresses found, pipe-separated",
  "secondary_phones": "Any additional phone numbers found, pipe-separated",
  "glassdoor_rating": "Company rating if mentioned on the page",
  "is_real_company": true
}

If this is NOT a real company page (it's a directory, article, blog, or listing site), return:
{"is_real_company": false}`
}

// AIExtractCompany is ai_extract_company: nil when the cleaned page is shorter than 50 characters
// or the model says it is not a real company page, else the model's object.
func AIExtractCompany(ctx context.Context, c LLM, page, url string) map[string]any {
	clean := CleanPageText(page, 3000)
	if runeLen(clean) < 50 {
		return nil
	}
	data := extract(ctx, c, extractPrompt(url, clean), extractSystem, 600)
	if len(data) == 0 || !pyTruthy(data["is_real_company"]) {
		return nil
	}
	return data
}

// ── ai_score_leads ─────────────────────────────────────────────────────────

const scoreSystem = "You are a B2B sales intelligence analyst. Score leads objectively based on ICP fit."

//go:embed icp.json
var icpJSON []byte

var icp = func() map[string]any {
	var d struct {
		ICP map[string]any `json:"ICP"`
	}
	if err := json.Unmarshal(icpJSON, &d); err != nil {
		panic("collect: icp.json: " + err.Error())
	}
	return d.ICP
}()

func icpText() string {
	list := func(k string) string {
		xs, _ := icp[k].([]any)
		if len(xs) > 5 {
			xs = xs[:5]
		}
		s := make([]string, len(xs))
		for i, x := range xs {
			s[i] = pyStr(x)
		}
		return strings.Join(s, ", ")
	}
	return "We sell: " + pyStr(icp["value_proposition"]) + "\n" +
		"Target industries: " + list("target_industries") + "\n" +
		"Target cities: " + list("target_cities") + "\n" +
		"Preferred size: " + pyStr(icp["min_company_size"]) + "+ employees"
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func scorePrompt(batch []*Lead) string {
	lines := make([]string, len(batch))
	for j, l := range batch {
		lines[j] = fmt.Sprintf("%d. %s — %s | City: %s | Specialization: %s | Size: %s | Has email: %s | Has phone: %s",
			j+1, l.Company, or(l.Description, "no description"), or(l.City, "unknown"), or(l.Specialization, "unknown"),
			or(l.CompanySize, "unknown"), pyBool(l.HasEmail()), pyBool(l.HasPhone()))
	}
	return `Score each lead 0-100 based on how well they match our Ideal Customer Profile (ICP).

OUR ICP:
` + icpText() + `

LEADS:
` + strings.Join(lines, "\n") + `

Scoring guide:
- 70-100 (Hot): Perfect ICP match — right industry, right city, right size, has contact info
- 50-69 (Warm): Good match — most criteria met, some gaps
- 30-49 (Cold): Partial match — related industry or city, missing key data
- 0-29 (Unqualified): Poor match — wrong industry, no useful data

Return JSON: {"scores": [
  {"id": 1, "score": 75, "tier": "hot", "reason": "IT staffing in Bangalore, 100+ employees, has email"},
  {"id": 2, "score": 35, "tier": "cold", "reason": "Related industry but no contact info"}
]}`
}

// AIScoreLeads is ai_score_leads: batches of five, score clamped to 0..100 and the tier derived
// from it, the model's reason kept as the description of a lead that has none. Returns how many
// leads got an AI score. Fix: a JSON boolean score is not a number (legacy scored true as 1).
func AIScoreLeads(ctx context.Context, c LLM, leads []*Lead) int {
	applied := 0
	for i := 0; i < len(leads); i += 5 {
		batch := leads[i:min(i+5, len(leads))]
		data := extract(ctx, c, scorePrompt(batch), scoreSystem, 600)
		scores, _ := data["scores"].([]any)
		for j, l := range batch {
			var hit map[string]any
			for _, s := range scores {
				m, ok := s.(map[string]any)
				if ok && numEq(m["id"], j+1) {
					hit = m
					break
				}
			}
			if hit == nil {
				continue
			}
			raw, ok := hit["score"].(float64)
			if !ok {
				if n, isNum := hit["score"].(json.Number); isNum {
					raw, ok = numberFloat(n)
				}
			}
			if !ok || math.IsNaN(raw) {
				continue
			}
			l.Score = clampScore(raw)
			l.ScoreTier = leadscore.Tier(l.Score)
			applied++
			if reason, _ := hit["reason"].(string); reason != "" && l.Description == "" {
				l.Description = reason
			}
		}
	}
	return applied
}

func numberFloat(n json.Number) (float64, bool) {
	f, err := n.Float64()
	return f, err == nil
}

func clampScore(f float64) int {
	t := math.Trunc(f) // int() truncates toward zero
	switch {
	case t < 0:
		return 0
	case t > 100:
		return 100
	}
	return int(t)
}

func numEq(v any, want int) bool {
	switch x := v.(type) {
	case float64:
		return x == float64(want)
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == float64(want)
	case int:
		return x == want
	}
	return false
}

// ── Python value helpers ────────────────────────────────────────────────────

// pyTruthy is Python truthiness of a decoded JSON value.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr is str() of a decoded JSON value (strings as is, integral floats without ".0" when they
// came from a JSON integer).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		return pyBool(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// orStr is `value or default` for a text field: falsy values give the default, others their
// str() (legacy stored the raw value; the leads columns are text).
func orStr(v any, def string) string {
	if !pyTruthy(v) {
		return def
	}
	return pyStr(v)
}

// orInt is `value or 0` for an integer field (strings holding an integer are parsed; anything
// else is 0).
func orInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, err := strconv.Atoi(pyStrip(x))
		if err == nil {
			return n
		}
	}
	return 0
}
