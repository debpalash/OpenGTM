package leadpeople

import (
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leaddedup"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Account discovery (account_discovery.py): a bounded market request is parsed
// into a structured brief without an LLM, and rows are accepted only with
// positive evidence for every requested criterion.
//
// The Python lookaheads are consumed here (only the capture group is read, so
// the match is the same); \b is ASCII.
var (
	countRe  = regexp.MustCompile(`(?i)\b(?:find|source|list|build(?:\s+(?:me\s+)?)?(?:a\s+list\s+of)?|show)\s+(?P<count>[0-9]{1,4})\b`)
	typeRe   = regexp.MustCompile(`(?i)(?:\b[0-9]{1,4}\s+|\b(?:find|source|list|show)\s+(?:[0-9]{1,4}\s+)?)(?P<company_type>.+?)\s+(?:companies|businesses|accounts)\b`)
	geoRe    = regexp.MustCompile(`(?i)\bin\s+(?P<geography>[a-z][a-z .'-]{1,60}?)(?:\s+(?:that|which|who|using|with|and\s+(?:are|that))\b|[,.!?]|$)`)
	techRe   = regexp.MustCompile(`(?i)\b(?:use|uses|using|powered\s+by)\s+(?P<technology>[a-z0-9][a-z0-9 .+_-]{0,60}?)(?:\s+(?:and|that|which|who|while)\b|[,.!?]|$)`)
	hireRe   = regexp.MustCompile(`(?i)\b(?:are\s+)?hiring(?:\s+for)?\s+(?P<role>[a-z0-9][a-z0-9 &/+_-]{0,80}?)(?:\s+roles?|\s+positions?|\s+jobs?)?(?:[,.!?]|$)`)
	marketRe = regexp.MustCompile(`(?i)\b(?:find|source|list|build|show)\b.*\b[0-9]{1,4}\b.*\b(?:companies|businesses|accounts)\b`)
	tokenRe  = regexp.MustCompile(`[a-z0-9]+`)
	// listOfRe strips the "of 30 " of "build a list of 30 B2B software
	// companies" that typeRe leaves at the front of the company type (a Python bug).
	listOfRe = regexp.MustCompile(`(?i)^of\s+[0-9]{1,4}\s+`)
)

var stopwords = map[string]bool{"a": true, "an": true, "and": true, "business": true, "businesses": true, "company": true, "companies": true, "the": true}

func one(v string) []any {
	v = strings.Trim(clean(v), " ,.")
	if v == "" {
		return []any{}
	}
	return []any{v}
}

func group(re *regexp.Regexp, name, s string) (string, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[re.SubexpIndex(name)], true
}

// AccountDiscoveryBriefID is account_discovery_brief_id: a stable hash of the
// brief's canonical criteria.
func AccountDiscoveryBriefID(brief map[string]any) string {
	payload := map[string]any{
		"requested_count": toInt(brief["requested_count"]),
		"company_types":   anyList(brief["company_types"]),
		"geographies":     anyList(brief["geographies"]),
		"technologies":    anyList(brief["technologies"]),
		"hiring_roles":    anyList(brief["hiring_roles"]),
		"exclusions":      anyList(brief["exclusions"]),
	}
	b, err := canonjson.Marshal(payload, true)
	if err != nil {
		return ""
	}
	return "accounts_" + sha24(string(b))
}

// BuildAccountDiscoveryBrief is build_account_discovery_brief: it parses a
// bounded market request ("find 50 SaaS companies in Austin that use
// Salesforce") into a brief with its missing fields.
func BuildAccountDiscoveryBrief(query string) map[string]any {
	query = trunc(clean(query), 500)
	var count int64
	if c, ok := group(countRe, "count", query); ok {
		count = toInt(c)
	}
	count = max(0, min(count, 500))
	companyTypes, geographies, technologies, hiringRoles := []any{}, []any{}, []any{}, []any{}
	if t, ok := group(typeRe, "company_type", query); ok {
		companyTypes = one(listOfRe.ReplaceAllString(t, ""))
	}
	if g, ok := group(geoRe, "geography", query); ok {
		geographies = one(g)
	}
	if g, ok := group(techRe, "technology", query); ok {
		technologies = one(g)
	}
	if g, ok := group(hireRe, "role", query); ok {
		hiringRoles = one(g)
	}
	missing := []any{}
	if count == 0 {
		missing = append(missing, "requested_count")
	}
	if len(companyTypes) == 0 {
		missing = append(missing, "company_type")
	}
	brief := map[string]any{
		"schema_version": "1.0", "original_query": query, "requested_count": count,
		"company_types": companyTypes, "geographies": geographies, "technologies": technologies,
		"hiring_roles": hiringRoles, "exclusions": []any{},
		"evidence_requirements": []any{"canonical_company_domain", "criterion_evidence", "evidence_url", "retrieved_at", "field_confidence"},
		"missing_fields":        missing, "complete": len(missing) == 0,
	}
	brief["brief_id"] = AccountDiscoveryBriefID(brief)
	return brief
}

// ExtractAccountDiscoveryRequest is extract_account_discovery_request: the
// brief of a complete explicit list request that can be routed
// deterministically, else nil.
func ExtractAccountDiscoveryRequest(query string) map[string]any {
	if !marketRe.MatchString(clean(query)) {
		return nil
	}
	if b := BuildAccountDiscoveryBrief(query); b["complete"] == true {
		return b
	}
	return nil
}

func tokens(v any) map[string]bool {
	out := map[string]bool{}
	for _, tok := range tokenRe.FindAllString(strings.ToLower(clean(valStr(v))), -1) {
		if stopwords[tok] {
			continue
		}
		if len(tok) > 4 && strings.HasSuffix(tok, "s") && !strings.HasSuffix(tok, "ss") && tok != "saas" {
			tok = tok[:len(tok)-1]
		}
		out[tok] = true
	}
	return out
}

// flatten is _flatten: containers as sorted-key JSON, scalars as cleaned text.
func flatten(v any) string {
	switch v.(type) {
	case map[string]any, []any, []string, []map[string]any, map[string]string:
		b, err := canonjson.Marshal(canonical(v), true)
		if err != nil {
			return ""
		}
		return string(b)
	}
	return clean(valStr(v))
}

// canonical converts common Go shapes to the ones canonjson accepts.
func canonical(v any) any {
	switch t := v.(type) {
	case []string, []map[string]any:
		l := anyList(t)
		for i := range l {
			l[i] = canonical(l[i])
		}
		return l
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = canonical(t[i])
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = x
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = canonical(x)
		}
		return out
	case int:
		return int64(t)
	case float32:
		return float64(t)
	}
	return v
}

// httpURL is _http_url: the cleaned URL when it has an http(s) scheme and a host.
func httpURL(v any) string {
	raw := clean(valStr(v))
	i := strings.IndexByte(raw, ':')
	if i <= 0 {
		return ""
	}
	scheme := strings.ToLower(raw[:i])
	if scheme != "http" && scheme != "https" {
		return ""
	}
	rest := raw[i+1:]
	if !strings.HasPrefix(rest, "//") {
		return ""
	}
	rest = rest[2:]
	end := len(rest)
	for j := 0; j < len(rest); j++ {
		if rest[j] == '/' || rest[j] == '?' || rest[j] == '#' {
			end = j
			break
		}
	}
	netloc := rest[:end]
	if netloc == "" || strings.Count(netloc, "[") != strings.Count(netloc, "]") {
		return ""
	}
	return raw
}

func isNumberNotBool(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

func subset(required, have map[string]bool) bool {
	if len(required) == 0 {
		return false
	}
	for t := range required {
		if !have[t] {
			return false
		}
	}
	return true
}

func joinFields(lead map[string]any, fields ...string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = flatten(lead[f])
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func strList(v any) []string {
	var out []string
	for _, x := range anyList(v) {
		out = append(out, valStr(x))
	}
	return out
}

func srcFields(f ...string) []any {
	out := make([]any, len(f))
	for i, s := range f {
		out[i] = s
	}
	return out
}

// EvaluateAccountFit is evaluate_account_fit: positive evidence is required
// for every requested criterion, plus a canonical domain, an evidence URL and a
// retrieval time.
func EvaluateAccountFit(lead map[string]any, brief map[string]any, now time.Time) map[string]any {
	company := clean(valStr(lead["company"]))
	website := clean(valStr(lead["website"]))
	canonicalDomain := leaddedup.NormalizeDomain(website)
	sourceURL := httpURL(lead["source_url"])
	websiteURL := ""
	if website != "" {
		if strings.HasPrefix(website, "http://") || strings.HasPrefix(website, "https://") {
			websiteURL = httpURL(website)
		} else {
			websiteURL = httpURL("https://" + website)
		}
	}
	retrievedAt := clean(firstValStr(lead, "updated_at", "created_at"))

	reasons := []any{}
	criteria := map[string]any{}
	rejections := []string{}
	evidence := func(primary string, fallback string) string {
		if primary != "" {
			return primary
		}
		return fallback
	}

	typeText := joinFields(lead, "specialization", "industry_tags", "description")
	for _, ct := range strList(brief["company_types"]) {
		matched := subset(tokens(ct), tokens(typeText))
		criteria["company_type:"+ct] = map[string]any{"matched": matched,
			"source_fields": srcFields("specialization", "industry_tags", "description"),
			"evidence_url":  evidence(sourceURL, websiteURL)}
		if matched {
			reasons = append(reasons, "Company type evidence matches "+ct)
		} else {
			rejections = append(rejections, "company_type_missing")
		}
	}
	geoText := joinFields(lead, "city", "state", "address")
	for _, g := range strList(brief["geographies"]) {
		matched := subset(tokens(g), tokens(geoText))
		criteria["geography:"+g] = map[string]any{"matched": matched,
			"source_fields": srcFields("city", "state", "address"), "evidence_url": evidence(sourceURL, websiteURL)}
		if matched {
			reasons = append(reasons, "Geography evidence matches "+g)
		} else {
			rejections = append(rejections, "geography_missing")
		}
	}
	techText := joinFields(lead, "technologies", "technographics")
	for _, tech := range strList(brief["technologies"]) {
		matched := subset(tokens(tech), tokens(techText))
		criteria["technology:"+tech] = map[string]any{"matched": matched,
			"source_fields": srcFields("technologies", "technographics"), "evidence_url": sourceURL}
		if matched && sourceURL != "" {
			reasons = append(reasons, "Technology evidence matches "+tech)
		} else {
			rejections = append(rejections, "technology_evidence_missing")
		}
	}
	hiringText := strings.ToLower(flatten(lead["hiring_signals"]))
	for _, role := range strList(brief["hiring_roles"]) {
		matched := subset(tokens(role), tokens(hiringText))
		criteria["hiring:"+role] = map[string]any{"matched": matched,
			"source_fields": srcFields("hiring_signals"), "evidence_url": sourceURL}
		if matched && sourceURL != "" {
			reasons = append(reasons, "Hiring evidence matches "+role)
		} else {
			rejections = append(rejections, "hiring_evidence_missing")
		}
	}
	if company == "" {
		rejections = append(rejections, "company_name_missing")
	}
	if canonicalDomain == "" {
		rejections = append(rejections, "canonical_domain_missing")
	}
	if sourceURL == "" {
		rejections = append(rejections, "evidence_url_missing")
	}
	if retrievedAt == "" {
		rejections = append(rejections, "retrieval_time_missing")
	}
	confidence := 0.0
	if score, ok := isNumberNotBool(lead["score"]); ok && !math.IsNaN(score) {
		confidence = round4(math.Max(0, math.Min(score/100, 1)))
	}
	urls := []any{}
	for _, u := range dedupStrings([]string{sourceURL, websiteURL}) {
		if u != "" {
			urls = append(urls, u)
		}
	}
	rej := []any{}
	for _, r := range dedupStrings(rejections) {
		rej = append(rej, r)
	}
	return map[string]any{
		"accepted": len(rejections) == 0, "company": company, "canonical_domain": canonicalDomain,
		"fit_reasons": reasons, "criteria_evidence": criteria, "evidence_urls": urls,
		"retrieved_at": retrievedAt, "field_confidence": confidence, "rejection_reasons": rej,
		"evaluated_at": isoFull(now),
	}
}

func firstValStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if providers.Truthy(m[k]) {
			return providers.Str(m[k])
		}
	}
	return ""
}
