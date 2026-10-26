// Package collectintent ports services/leadgen/collection_intent.py: the
// deterministic, pure routing decision for lead-collection requests.
package collectintent

import (
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

var specialized = map[string]bool{
	"company_research": true, "people_at_company": true,
	"technology_users": true, "signal_monitor": true,
}

var knownIntents = map[string]bool{
	"market_search": true, "company_research": true, "people_at_company": true,
	"technology_users": true, "signal_monitor": true,
}

// Decision mirrors CollectionDecision.
type Decision struct {
	Query                 string
	Intent                string
	Domain                string
	Entity                string
	ClarificationKind     string
	ClarificationRequired bool
	Reason                string
	Options               []IntentOption
}

// CanCollect mirrors CollectionDecision.can_collect.
func (d Decision) CanCollect() bool {
	return d.Intent == "market_search" && !d.ClarificationRequired
}

// ToDict mirrors CollectionDecision.to_dict (Python key order).
func (d Decision) ToDict() *Obj {
	o := wbcrud.NewObj()
	o.Set("query", d.Query)
	o.Set("intent", d.Intent)
	o.Set("domain", d.Domain)
	o.Set("entity", d.Entity)
	o.Set("clarification_kind", d.ClarificationKind)
	o.Set("clarification_required", d.ClarificationRequired)
	o.Set("reason", d.Reason)
	opts := make([]any, 0, len(d.Options))
	for _, op := range d.Options {
		opts = append(opts, op.ToDict())
	}
	o.Set("options", opts)
	return o
}

// domainLabelChar is [a-z0-9-] under Python IGNORECASE: ASCII alphanumerics and
// hyphen plus U+0130, U+0131 (i variants), U+017F and U+212A (s and k folds).
func domainAlnum(r rune) bool {
	return r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') ||
		r == 0x130 || r == 0x131 || r == 0x17f || r == 0x212a
}

// parseLabel consumes [a-z0-9][a-z0-9-]{0,62} from s[i:] (greedy) and returns
// the end offset, or -1.
func parseLabel(s string, i int) int {
	r, size := utf8.DecodeRuneInString(s[i:])
	if i >= len(s) || !domainAlnum(r) {
		return -1
	}
	i += size
	for n := 0; n < 62 && i < len(s); n++ {
		r, size = utf8.DecodeRuneInString(s[i:])
		if !domainAlnum(r) && r != '-' {
			break
		}
		i += size
	}
	return i
}

// parseDomain consumes label(\.label)+ greedily from s[i:] and returns the
// end offset, or -1.
func parseDomain(s string, i int) int {
	j := parseLabel(s, i)
	if j < 0 {
		return -1
	}
	reps := 0
	for j < len(s) && s[j] == '.' {
		k := parseLabel(s, j+1)
		if k < 0 {
			break
		}
		j = k
		reps++
	}
	if reps == 0 {
		return -1
	}
	return j
}

func hasFoldPrefix(s string, i int, p string) bool {
	return len(s)-i >= len(p) && strings.EqualFold(s[i:i+len(p)], p) && utf8.ValidString(s[i:i+len(p)])
}

// prefixStarts lists the offsets at which the domain may begin, in the order a
// backtracking engine tries them for (?:https?://)?(?:www\.)?.
func prefixStarts(s string, i int) []int {
	var schemes []int
	switch {
	case hasFoldPrefix(s, i, "https://"):
		schemes = []int{i + 8}
	case hasFoldPrefix(s, i, "http://"):
		schemes = []int{i + 7}
	}
	schemes = append(schemes, i)
	var out []int
	for _, st := range schemes {
		if hasFoldPrefix(s, st, "www.") {
			out = append(out, st+4)
		}
		out = append(out, st)
	}
	return out
}

// NormalizeDomain mirrors normalize_domain (fullmatch of _DOMAIN_RE).
func NormalizeDomain(value string) string {
	raw := pyStrip(value)
	body := strings.TrimSuffix(raw, "/")
	for _, st := range prefixStarts(body, 0) {
		if end := parseDomain(body, st); end == len(body) {
			// urlparse(...).hostname lowercases (already lower here).
			return strings.TrimRight(pyLower(body[st:end]), ".")
		}
	}
	return ""
}

// DomainInQuery mirrors domain_in_query. _DOMAIN_IN_TEXT_RE starts with the
// lookbehind (?<![@\w.-]) which RE2 lacks, so the search is hand-written.
func DomainInQuery(query string) string {
	if bare := NormalizeDomain(query); bare != "" {
		return bare
	}
	text := pyStrip(query)
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if i == 0 || !blocksDomainStart(lastRune(text[:i])) {
			if domainAlnum(r) {
				for _, st := range prefixStarts(text, i) {
					if end := parseDomain(text, st); end >= 0 {
						return strings.TrimRight(pyLower(text[st:end]), ".")
					}
				}
			}
		}
		i += size
	}
	return ""
}

func blocksDomainStart(p rune) bool {
	return p == '@' || p == '.' || p == '-' || isPyWord(p)
}

func brandOf(domain string) string {
	label := strings.Split(domain, ".")[0]
	label = pyStrip(strings.ReplaceAll(label, "-", " "))
	if t := pyTitle(label); t != "" {
		return t
	}
	return domain
}

// Decide mirrors decide_collection; intent nil means requested_intent=None.
func Decide(query string, intent *string) Decision {
	clean := collapseWS(pyStrip(query))
	domain := DomainInQuery(clean)
	bareDomain := NormalizeDomain(clean) != ""
	team := ExtractCompanyTeamRequest(clean)

	if team != nil {
		return Decision{
			Query:                 clean,
			Intent:                "people_at_company",
			Domain:                domain,
			Entity:                team.Entity,
			ClarificationKind:     "company_team",
			ClarificationRequired: true,
			Reason: "“" + clean + "” can mean people on the team, partner companies, or " +
				"an overview of the organization.",
			Options: CompanyTeamOptions(*team),
		}
	}

	inferred := Infer(clean)
	var cur string
	if intent != nil && knownIntents[*intent] {
		cur = *intent
	} else {
		cur = inferred
	}
	if cur == "market_search" && specialized[inferred] {
		cur = inferred
	}

	if bareDomain && intent == nil {
		return Decision{
			Query:                 clean,
			Intent:                "company_research",
			Domain:                domain,
			Entity:                brandOf(domain),
			ClarificationKind:     "bare_domain",
			ClarificationRequired: true,
			Reason:                "A domain is a company target, not a market-search specification.",
			Options:               ClarificationOptions(domain),
		}
	}

	if specialized[cur] {
		var opts []IntentOption
		if domain != "" {
			for _, o := range ClarificationOptions(domain) {
				if o.Intent == cur {
					opts = append(opts, o)
				}
			}
		} else {
			opts = specializedRouteOption(cur, clean)
		}
		return Decision{
			Query:                 clean,
			Intent:                cur,
			Domain:                domain,
			ClarificationRequired: true,
			Reason: "This request needs a specialized GTM workflow and cannot safely " +
				"run through the broad lead collector.",
			Options: opts,
		}
	}

	if bareDomain {
		return Decision{
			Query:                 clean,
			Intent:                "market_search",
			Domain:                domain,
			ClarificationRequired: true,
			Reason:                "A bare domain cannot be forced into market-search mode.",
			Options:               ClarificationOptions(domain),
		}
	}
	return Decision{Query: clean, Intent: "market_search", Domain: domain}
}
