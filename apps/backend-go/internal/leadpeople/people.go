// Package leadpeople is the Go port of the people-discovery half of the lead
// pipeline (plan 15 WP-PEOPLE): evidence-gated targeted people research for one
// named company (targeted_people.py), exact-person work-email discovery and
// verification (people_contacts.py), the durable spend boundary for contact
// enrichment (contact_execution.py) and the deterministic account-discovery
// brief and fit gate (account_discovery.py).
//
// It is a library: no flags, no routes. Search goes through leadweb, the
// crosslinked people finder comes from the provider registry (or is injected),
// company identity resolves through Wikidata, email discovery runs through the
// provider runner and verification through emailverify. Every database write
// runs inside tenancy.InWorkspace.
//
// Deviations from Python (Python bugs fixed, not mirrored):
//   - non-numeric "confidence" values count as 0 instead of raising;
//   - the account brief parser no longer returns "of 30 B2B software" as the
//     company type of "build a list of 30 B2B software companies";
//   - \b in the account-discovery regexes is ASCII word boundary (Python's is
//     Unicode); the people-evidence matchers use Unicode boundaries.
package leadpeople

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

var functionQueryOrder = []string{
	"partnership", "alliance", "business development", "biz dev", "channel", "sales", "marketing", "revenue",
	"growth", "product", "engineering", "customer success", "leadership", "executive",
}

var functionQueries = map[string][]string{
	"partnership":          {"partnerships", "strategic partnerships", "alliances", "business development", "partner ecosystem"},
	"alliance":             {"alliances", "strategic alliances", "partnerships"},
	"business development": {"business development", "partnerships"},
	"biz dev":              {"business development", "partnerships"},
	"channel":              {"channel partnerships", "channel sales", "alliances"},
	"sales":                {"head of sales", "sales director", "vice president sales"},
	"marketing":            {"head of marketing", "marketing director", "CMO"},
	"revenue":              {"chief revenue officer", "revenue operations", "VP revenue"},
	"growth":               {"head of growth", "growth director", "VP growth"},
	"product":              {"head of product", "product director", "VP product"},
	"engineering":          {"head of engineering", "engineering director", "CTO"},
	"customer success":     {"head of customer success", "customer success director"},
	"leadership":           {"CEO", "founder", "vice president", "director"},
	"executive":            {"CEO", "founder", "vice president", "director"},
}

var functionEvidenceOrder = []string{"partnership", "alliance", "business development", "biz dev", "channel", "leadership", "executive"}

var functionEvidence = map[string][]string{
	"partnership":          {"partnership", "partner development", "partner ecosystem", "alliance", "business development", "channel partner"},
	"alliance":             {"alliance", "partnership"},
	"business development": {"business development", "partnership"},
	"biz dev":              {"business development", "partnership"},
	"channel":              {"channel", "partnership", "alliance"},
	"leadership":           {"chief executive", "ceo", "founder", "president", "vice president", "director", "head of"},
	"executive":            {"chief ", "ceo", "cto", "cfo", "coo", "cmo", "president", "vice president", "director"},
}

var (
	legalSuffixRe = regexp.MustCompile(`(?i)[` + pyS + `]+(?:incorporated|inc|corp(?:oration)?|llc|ltd|limited|plc|pvt)\.?$`)
	domainRe      = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?([a-z0-9-]+)(?:\.[a-z0-9-]+)+(?:/.*)?$`)
	nonAlnumRe    = regexp.MustCompile(`[^a-z0-9]+`)
	personIDRe    = regexp.MustCompile(`^person_[a-zA-Z0-9_-]{1,56}$`)
	titleStripRe  = regexp.MustCompile(`(?i)\b(?:head of|vice president|vp|director of|director|manager|lead)\b`)
)

// companyAliases is _company_aliases.
func companyAliases(company string) []string {
	cl := clean(company)
	if m := domainRe.FindStringSubmatch(cl); m != nil {
		cl = strings.ReplaceAll(m[1], "-", " ")
	}
	aliases := []string{cl}
	if ws := providers.PyStrip(legalSuffixRe.ReplaceAllString(cl, "")); ws != "" && !strings.EqualFold(ws, cl) {
		aliases = append(aliases, ws)
	}
	out := aliases[:0:0]
	for _, a := range aliases {
		if len([]rune(a)) >= 2 {
			out = append(out, a)
		}
	}
	return out
}

// identityText is _identity_text.
func identityText(v string) string {
	return providers.PyStrip(nonAlnumRe.ReplaceAllString(strings.ToLower(v), " "))
}

func sha24(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:24]
}

func companyKey(company string) string {
	al := companyAliases(company)
	if len(al) > 0 {
		return identityText(al[len(al)-1])
	}
	return identityText(company)
}

// PersonEntityID is person_entity_id: a stable ID for a person scoped to the
// canonical target company.
func PersonEntityID(company string, person map[string]any) string {
	existing := providers.PyStrip(getStr(person, "person_id"))
	if personIDRe.MatchString(existing) {
		return existing
	}
	profileURL := providers.PyStrip(firstStr(person, "linkedin_url", "linkedin", "evidence_url"))
	profileKey := ""
	if host, path := pyURL(profileURL); host != "" && path != "" {
		profileKey = host + strings.ToLower(strings.TrimRight(path, "/"))
	}
	nameKey := identityText(firstStr(person, "name", "full_name"))
	identity := profileKey
	if identity == "" {
		identity = nameKey
	}
	return "person_" + sha24(companyKey(company)+"|"+identity)
}

// PeopleResultSetID is people_result_set_id: it identifies the exact
// order-independent people set returned to Chat.
func PeopleResultSetID(company, function string, people []map[string]any) string {
	ids := make([]string, 0, len(people))
	for _, p := range people {
		ids = append(ids, PersonEntityID(company, p))
	}
	sort.Strings(ids)
	parts := append([]string{companyKey(company), identityText(function)}, ids...)
	return "people_" + sha24(strings.Join(parts, "|"))
}

const sepChars = `|·,;:.()\[\]–—-`

var relCache sync.Map // alias -> *regexp.Regexp

func relationshipRe(alias string) *regexp.Regexp {
	if re, ok := relCache.Load(alias); ok {
		return re.(*regexp.Regexp)
	}
	a := regexp.QuoteMeta(alias)
	tail := a + `[` + pyS + `]*(?:[` + sepChars + `]|$)`
	pre := `(?:^|[^` + pyW + `])`
	pat := `(?i)(?:` +
		pre + `(?:at|for|with)[` + pyS + `]+` + tail + `|` +
		`[` + pyW + `]@[` + pyS + `]+` + tail + `|` +
		pre + `experience[` + pyS + `]*:[` + pyS + `]*` + tail + `|` +
		`(?:^|[|·;:–—-])[` + pyS + `]*` + tail + `)`
	re := regexp.MustCompile(pat)
	relCache.Store(alias, re)
	return re
}

// ExactCompanyRelationship is _exact_company_relationship: the company name in
// an employment-like context, followed by a separator ("Stripe" does not match
// "Stripe Theory").
func ExactCompanyRelationship(text, company string) bool {
	h := collapse(text)
	for _, alias := range companyAliases(company) {
		if relationshipRe(alias).MatchString(h) {
			return true
		}
	}
	return false
}

// functionTerms is _function_terms.
func functionTerms(function string, titles []string) []string {
	normalized := strings.ToLower(clean(function))
	for _, stem := range functionEvidenceOrder {
		if strings.Contains(normalized, stem) {
			return append([]string(nil), functionEvidence[stem]...)
		}
	}
	var terms []string
	for _, t := range titles {
		c := strings.ToLower(clean(t))
		c = strings.Trim(titleStripRe.ReplaceAllString(c, ""), " -")
		if len([]rune(c)) >= 3 {
			terms = append(terms, c)
		}
	}
	if normalized != "" {
		terms = append(terms, normalized)
	}
	return dedupStrings(terms)
}

var respRe = regexp.MustCompile(`(?i)i[` + pyS + `]+(?:currently[` + pyS + `]+)?(?:lead|head|manage|run|build|own|oversee|am[` + pyS + `]+(?:directly[` + pyS + `]+)?responsible[` + pyS + `]+for)`)

// responsibilityClause is the first `\bi\s+...\b.{0,220}` match of _function_evidence_strength.
func responsibilityClause(s string) (string, bool) {
	from := 0
	for from <= len(s) {
		loc := respRe.FindStringIndex(s[from:])
		if loc == nil {
			return "", false
		}
		st, en := from+loc[0], from+loc[1]
		if boundary(s, st) && boundary(s, en) {
			end := en
			for n := 0; n < 220 && end < len(s) && s[end] != '\n'; n++ {
				_, w := utf8.DecodeRuneInString(s[end:])
				end += w
			}
			return s[st:end], true
		}
		from = st + 1
	}
	return "", false
}

// FunctionEvidenceStrength is _function_evidence_strength: functional remit
// (profile headline, summary before an "Experience: <company>" marker, or a
// first-person responsibility statement), not a coincidental mention.
func FunctionEvidenceStrength(evidenceTitle, evidenceSnippet string, terms []string, company string) (status string, conf float64, ok bool) {
	title := strings.ToLower(evidenceTitle)
	snippet := strings.ToLower(evidenceSnippet)
	if anySubstr(title, terms) {
		return "profile_headline", 0.78, true
	}
	if i := strings.Index(snippet, "experience:"); i >= 0 {
		if anySubstr(snippet[:i], terms) && ExactCompanyRelationship(snippet[i:], company) {
			return "profile_summary_plus_experience", 0.72, true
		}
	}
	if clause, found := responsibilityClause(snippet); found && anySubstr(strings.ToLower(clause), terms) {
		return "first_person_responsibility", 0.74, true
	}
	return "", 0, false
}

// queryTitles is _query_titles.
func queryTitles(function string, titles []string) []string {
	var supplied []string
	for _, t := range titles {
		if s := providers.PyStrip(t); s != "" {
			supplied = append(supplied, s)
		}
	}
	if len(supplied) > 0 {
		if len(supplied) > 5 {
			supplied = supplied[:5]
		}
		return supplied
	}
	normalized := strings.ToLower(clean(function))
	for _, stem := range functionQueryOrder {
		if strings.Contains(normalized, stem) {
			q := functionQueries[stem]
			if len(q) > 5 {
				q = q[:5]
			}
			return append([]string(nil), q...)
		}
	}
	if f := providers.PyStrip(function); f != "" {
		return []string{f}
	}
	return []string{"leadership"}
}

// exactNameMatch is _exact_name_match.
func exactNameMatch(text, name string) bool {
	ct := collapse(strings.ToLower(text))
	cn := strings.ToLower(clean(name))
	if cn == "" {
		return false
	}
	return len(occurrences(litRe(cn), ct, 0)) > 0
}

var (
	companyThenPersonKw = anchored(`appointed|named|hired|promoted|welcomed|selected`)
	personThenCompanyKw = anchored(`joined|joins|leads?|heads?|runs?|works?[` + pyS + `]+(?:at|for)`)
	atForRe             = func(alias string) *regexp.Regexp {
		return regexp.MustCompile(`(?i)^(?:at|for)[` + pyS + `]+` + regexp.QuoteMeta(alias))
	}
)

// chain reports `\bA\b.{0,g1}\b(?:kw)\b.{0,g2}\bB\b`.
func chain(text string, a, kw, b *regexp.Regexp, g1, g2 int) bool {
	// a is an unanchored literal; kw and b are anchored.
	for _, oa := range occurrences(a, text, 0) {
		for _, p := range gapStarts(text, oa[1], g1) {
			ke, ok := boundedAt(kw, text, p)
			if !ok {
				continue
			}
			for _, q := range gapStarts(text, ke, g2) {
				if _, ok := boundedAt(b, text, q); ok {
					return true
				}
			}
		}
	}
	return false
}

// verificationCompanyMatch is _verification_company_match.
func verificationCompanyMatch(text, company, name string) bool {
	if ExactCompanyRelationship(text, company) {
		return true
	}
	compact := collapse(text)
	nameRe, nameAt := litRe(name), anchoredLit(name)
	for _, alias := range companyAliases(company) {
		aliasRe, aliasAt := litRe(alias), anchoredLit(alias)
		if chain(compact, aliasRe, companyThenPersonKw, nameAt, 100, 100) {
			return true
		}
		if chain(compact, nameRe, personThenCompanyKw, aliasAt, 100, 80) {
			return true
		}
	}
	return false
}

// verificationClauses is the findall of the two clause patterns in
// _verification_function_match for one alias.
func verificationClauses(compact, name string, alias string) []string {
	nameRe, aliasRe, nameAt := litRe(name), litRe(alias), anchoredLit(name)
	atFor := atForRe(alias)
	var clauses []string
	// \bNAME\b.{0,180}?\b(?:at|for)\s+ALIAS\b
	pos := 0
	for pos <= len(compact) {
		matched := false
		for _, on := range occurrences(nameRe, compact, pos) {
			found := false
			for _, p := range gapStarts(compact, on[1], 180) {
				if !boundary(compact, p) {
					continue
				}
				if m, ok := matchAt(atFor, compact, p); ok && boundary(compact, m) {
					clauses = append(clauses, compact[on[0]:m])
					pos = m
					found = true
					break
				}
			}
			if found {
				matched = true
				break
			}
		}
		if !matched {
			break
		}
	}
	// \bALIAS\b.{0,180}?\bNAME\b
	pos = 0
	for pos <= len(compact) {
		matched := false
		for _, oa := range occurrences(aliasRe, compact, pos) {
			found := false
			for _, p := range gapStarts(compact, oa[1], 180) {
				if m, ok := boundedAt(nameAt, compact, p); ok {
					clauses = append(clauses, compact[oa[0]:m])
					pos = m
					found = true
					break
				}
			}
			if found {
				matched = true
				break
			}
		}
		if !matched {
			break
		}
	}
	return clauses
}

// verificationFunctionMatch is _verification_function_match: the function must
// sit inside a headline or an employment clause.
func verificationFunctionMatch(hitTitle, snippet, company, name string, terms []string) bool {
	if anySubstr(strings.ToLower(hitTitle), terms) {
		return true
	}
	compact := collapse(snippet)
	for _, alias := range companyAliases(company) {
		for _, clause := range verificationClauses(compact, name, alias) {
			if anySubstr(strings.ToLower(clause), terms) {
				return true
			}
		}
	}
	_, _, ok := FunctionEvidenceStrength(hitTitle, snippet, terms, company)
	return ok
}
