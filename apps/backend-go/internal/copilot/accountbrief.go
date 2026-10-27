package copilot

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports services/leadgen/account_discovery.py build_account_discovery_brief and
// extract_account_discovery_request: the deterministic parse of a bounded market request ("find 50 IT
// staffing companies in Pune"). Python's regexes use IGNORECASE (which also folds U+0130/U+0131), Unicode
// \b and \d, lazy groups and lookaheads; RE2 has no lookahead, so the lazy groups are scanned by hand.
//
// Two Python defects are fixed, not mirrored: a company type captured as "of 30 logistics" (the "list"
// keyword of "build a list of 30 ..." won over the count) loses its "of <count>" prefix, and a purely numeric
// company type ("find 15 companies") counts as missing. Geographies may contain any letter (Python's
// [a-z] dropped "São Paulo").

// isPyWordRune is Python's Unicode \w.
func isPyWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// isPySpaceRune is str.isspace / Python's Unicode \s.
func isPySpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// pyClean is re.sub(r"\s+", " ", str(v or "").strip()).
func pyClean(s string) string {
	s = strings.TrimFunc(s, isPySpaceRune)
	var b strings.Builder
	in := false
	for _, r := range s {
		if isPySpaceRune(r) {
			if !in {
				b.WriteByte(' ')
			}
			in = true
			continue
		}
		in = false
		b.WriteRune(r)
	}
	return b.String()
}

// asciiFold reports whether r matches the ASCII letter or digit c under Python's IGNORECASE.
func asciiFold(r rune, c byte) bool {
	if r < 0x80 {
		return unicode.ToLower(r) == rune(c) || r == rune(c)
	}
	switch c {
	case 'i':
		return r == 0x130 || r == 0x131
	case 's':
		return r == 0x17f
	case 'k':
		return r == 0x212a
	}
	return false
}

// isAZ is [a-z] under IGNORECASE.
func isAZ(r rune) bool {
	return r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') || r == 0x130 || r == 0x131 || r == 0x17f || r == 0x212a
}

func is09(r rune) bool { return r >= '0' && r <= '9' }

// hasWord reports whether rs[i:] starts with the (lowercase ASCII) literal w under IGNORECASE; it returns the
// end index.
func hasWord(rs []rune, i int, w string) (int, bool) {
	if len(rs)-i < len(w) {
		return 0, false
	}
	for k := 0; k < len(w); k++ {
		c := w[k]
		if c == ' ' {
			if rs[i+k] != ' ' {
				return 0, false
			}
			continue
		}
		if !asciiFold(rs[i+k], c) {
			return 0, false
		}
	}
	return i + len(w), true
}

// boundaryAt is Python's \b at rune index i.
func boundaryAt(rs []rune, i int) bool {
	before := i > 0 && isPyWordRune(rs[i-1])
	after := i < len(rs) && isPyWordRune(rs[i])
	return before != after
}

// spaces consumes \s+ (after pyClean, single spaces) and returns the new index.
func spaces(rs []rune, i int) (int, bool) {
	j := i
	for j < len(rs) && isPySpaceRune(rs[j]) {
		j++
	}
	return j, j > i
}

// digitValue is the value of a Unicode decimal digit.
func digitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	start := r
	for unicode.IsDigit(start-1) && r-start < 64 {
		start--
	}
	return int(r-start) % 10
}

// countAfter matches \d{1,4}\b at i (greedy; a shorter run never ends at a boundary).
func countAfter(rs []rune, i int) (int, int, bool) {
	j := i
	for j < len(rs) && j-i < 4 && unicode.IsDigit(rs[j]) {
		j++
	}
	if j == i || !boundaryAt(rs, j) {
		return 0, 0, false
	}
	n := 0
	for _, r := range rs[i:j] {
		n = n*10 + digitValue(r)
	}
	return n, j, true
}

// matchCount is _COUNT_RE.search: the requested count, or 0.
func matchCount(rs []rune) int {
	for p := 0; p < len(rs); p++ {
		if !boundaryAt(rs, p) {
			continue
		}
		// alternatives in Python's order; each is a literal prefix ending where \s+ must follow
		var heads []int
		for _, w := range []string{"find", "source", "list"} {
			if e, ok := hasWord(rs, p, w); ok {
				heads = append(heads, e)
			}
		}
		if e, ok := hasWord(rs, p, "build"); ok {
			// build(?:\s+(?:me\s+)?)?(?:a\s+list\s+of)? in backtracking order
			var g1 []int
			if s1, ok := spaces(rs, e); ok {
				if m, ok := hasWord(rs, s1, "me"); ok {
					if s2, ok := spaces(rs, m); ok {
						g1 = append(g1, s2)
					}
				}
				g1 = append(g1, s1)
			}
			g1 = append(g1, e)
			for _, g := range g1 {
				if l, ok := hasWord(rs, g, "a list of"); ok {
					heads = append(heads, l)
				}
				heads = append(heads, g)
			}
		}
		if e, ok := hasWord(rs, p, "show"); ok {
			heads = append(heads, e)
		}
		for _, h := range heads {
			if s, ok := spaces(rs, h); ok {
				if n, _, ok := countAfter(rs, s); ok {
					return n
				}
			}
		}
	}
	return 0
}

const (
	reWordPrev = `(?:^|[^\p{L}\p{N}_])`
	reWordNext = `(?:$|[^\p{L}\p{N}_])`
)

// companyTypeRE is _COMPANY_TYPE_RE with \b as consumed non-word neighbours and \d as \p{Nd}.
var companyTypeRE = regexp.MustCompile(`(?i)` + reWordPrev +
	`(?:\p{Nd}{1,4} +|(?:f[i\x{130}\x{131}]nd|source|l[i\x{130}\x{131}]st|show) +(?:\p{Nd}{1,4} +)?)` +
	`(.+?) +(?:compan[i\x{130}\x{131}]es|bus[i\x{130}\x{131}]nesses|accounts)` + reWordNext)

var leadingOfCount = regexp.MustCompile(`(?i)^of +\p{Nd}{1,4} +`)

// lazyGroup scans the lazy group [first][rest]{min,max}? starting at i and returns the first end at which
// look holds, or -1.
func lazyGroup(rs []rune, i int, first, rest func(rune) bool, minRest, maxRest int, look func(int) bool) int {
	if i >= len(rs) || !first(rs[i]) {
		return -1
	}
	for n := 0; n <= maxRest; n++ {
		end := i + 1 + n
		if n > 0 {
			if end > len(rs) || !rest(rs[end-1]) {
				return -1
			}
		}
		if n >= minRest && look(end) {
			return end
		}
	}
	return -1
}

func isPunctEnd(rs []rune, i int) bool {
	return i == len(rs) || strings.ContainsRune(",.!?", rs[i])
}

// spaceThenWord is (?=\s+(?:w1|w2|...)\b ...) for single words; "and are"/"and that" carry their space.
func spaceThenWord(rs []rune, i int, words ...string) bool {
	s, ok := spaces(rs, i)
	if !ok {
		return false
	}
	for _, w := range words {
		if strings.HasPrefix(w, "and ") {
			e, ok := hasWord(rs, s, "and")
			if !ok {
				continue
			}
			s2, ok := spaces(rs, e)
			if !ok {
				continue
			}
			if e2, ok := hasWord(rs, s2, w[4:]); ok && boundaryAt(rs, e2) {
				return true
			}
			continue
		}
		if e, ok := hasWord(rs, s, w); ok && boundaryAt(rs, e) {
			return true
		}
	}
	return false
}

// matchGeography is _GEOGRAPHY_RE (letters widened to Unicode).
func matchGeography(rs []rune) string {
	letter := func(r rune) bool { return isAZ(r) || unicode.IsLetter(r) }
	rest := func(r rune) bool { return letter(r) || r == ' ' || r == '.' || r == '\'' || r == '-' }
	look := func(i int) bool {
		return spaceThenWord(rs, i, "that", "which", "who", "using", "with", "and are", "and that") || isPunctEnd(rs, i)
	}
	for p := 0; p < len(rs); p++ {
		if !boundaryAt(rs, p) {
			continue
		}
		e, ok := hasWord(rs, p, "in")
		if !ok {
			continue
		}
		s, ok := spaces(rs, e)
		if !ok {
			continue
		}
		if end := lazyGroup(rs, s, letter, rest, 1, 60, look); end >= 0 {
			return string(rs[s:end])
		}
	}
	return ""
}

// matchTechnology is _TECHNOLOGY_RE.
func matchTechnology(rs []rune) string {
	first := func(r rune) bool { return isAZ(r) || is09(r) }
	rest := func(r rune) bool { return first(r) || strings.ContainsRune(" .+_-", r) }
	look := func(i int) bool {
		return spaceThenWord(rs, i, "and", "that", "which", "who", "while") || isPunctEnd(rs, i)
	}
	for p := 0; p < len(rs); p++ {
		if !boundaryAt(rs, p) {
			continue
		}
		var heads []int
		for _, w := range []string{"use", "uses", "using"} {
			if e, ok := hasWord(rs, p, w); ok {
				heads = append(heads, e)
			}
		}
		if e, ok := hasWord(rs, p, "powered"); ok {
			if s, ok := spaces(rs, e); ok {
				if b, ok := hasWord(rs, s, "by"); ok {
					heads = append(heads, b)
				}
			}
		}
		for _, h := range heads {
			s, ok := spaces(rs, h)
			if !ok {
				continue
			}
			if end := lazyGroup(rs, s, first, rest, 0, 60, look); end >= 0 {
				return string(rs[s:end])
			}
		}
	}
	return ""
}

// matchHiring is _HIRING_RE.
func matchHiring(rs []rune) string {
	first := func(r rune) bool { return isAZ(r) || is09(r) }
	rest := func(r rune) bool { return first(r) || strings.ContainsRune(" &/+_-", r) }
	// (?:\s+roles?|\s+positions?|\s+jobs?)?(?=[,.!?]|$) in backtracking order
	tail := func(i int) bool {
		if s, ok := spaces(rs, i); ok {
			for _, w := range []string{"role", "position", "job"} {
				if e, ok := hasWord(rs, s, w); ok {
					if e < len(rs) && asciiFold(rs[e], 's') && isPunctEnd(rs, e+1) {
						return true
					}
					if isPunctEnd(rs, e) {
						return true
					}
				}
			}
		}
		return isPunctEnd(rs, i)
	}
	for p := 0; p < len(rs); p++ {
		if !boundaryAt(rs, p) {
			continue
		}
		var starts []int
		if e, ok := hasWord(rs, p, "are"); ok {
			if s, ok := spaces(rs, e); ok {
				starts = append(starts, s)
			}
		}
		starts = append(starts, p)
		for _, st := range starts {
			h, ok := hasWord(rs, st, "hiring")
			if !ok {
				continue
			}
			var heads []int
			if s, ok := spaces(rs, h); ok {
				if f, ok := hasWord(rs, s, "for"); ok {
					heads = append(heads, f)
				}
			}
			heads = append(heads, h)
			for _, hd := range heads {
				s, ok := spaces(rs, hd)
				if !ok {
					continue
				}
				if end := lazyGroup(rs, s, first, rest, 0, 80, tail); end >= 0 {
					return string(rs[s:end])
				}
			}
		}
	}
	return ""
}

// wordAt reports \b(?:w1|w2|...)\b at i and returns the end of the first alternative that fits.
func wordAt(rs []rune, i int, words ...string) (int, bool) {
	if !boundaryAt(rs, i) {
		return 0, false
	}
	for _, w := range words {
		if e, ok := hasWord(rs, i, w); ok && boundaryAt(rs, e) {
			return e, true
		}
	}
	return 0, false
}

// isMarketRequest is _MARKET_REQUEST_RE.search: \b(find|source|list|build|show)\b.*\b\d{1,4}\b.*\b(companies|
// businesses|accounts)\b, in that order (the string has no newlines after pyClean).
func isMarketRequest(rs []rune) bool {
	kw := -1
	for i := 0; i < len(rs) && kw < 0; i++ {
		if e, ok := wordAt(rs, i, "find", "source", "list", "build", "show"); ok {
			kw = e
		}
	}
	if kw < 0 {
		return false
	}
	num := -1
	for i := kw; i < len(rs) && num < 0; i++ {
		if !boundaryAt(rs, i) || !unicode.IsDigit(rs[i]) {
			continue
		}
		for n := 1; n <= 4 && i+n <= len(rs) && unicode.IsDigit(rs[i+n-1]); n++ {
			if boundaryAt(rs, i+n) {
				num = i + n
				break
			}
		}
	}
	if num < 0 {
		return false
	}
	for i := num; i < len(rs); i++ {
		if _, ok := wordAt(rs, i, "companies", "businesses", "accounts"); ok {
			return true
		}
	}
	return false
}

// one is _one: [v] when the cleaned value stripped of " ,." is non-empty.
func one(v string) []any {
	v = strings.Trim(pyClean(v), " ,.")
	if v == "" {
		return []any{}
	}
	return []any{v}
}

func runePrefixStr(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// AccountBriefID is account_discovery_brief_id.
func AccountBriefID(requested int64, companyTypes, geos, techs, roles, exclusions []any) string {
	payload, err := canonjson.Marshal(map[string]any{
		"requested_count": requested, "company_types": companyTypes, "geographies": geos,
		"technologies": techs, "hiring_roles": roles, "exclusions": exclusions,
	}, true)
	if err != nil {
		payload = nil
	}
	sum := sha256.Sum256(payload)
	return "accounts_" + hex.EncodeToString(sum[:])[:24]
}

// BuildAccountBrief is build_account_discovery_brief (Python key order).
func BuildAccountBrief(query string) *wbcrud.Obj {
	query = runePrefixStr(pyClean(query), 500)
	rs := []rune(query)
	count := min(max(matchCount(rs), 0), 500)
	var companyTypes []any
	if m := companyTypeRE.FindStringSubmatch(query); m != nil {
		ct := leadingOfCount.ReplaceAllString(m[1], "")
		if strings.TrimFunc(ct, func(r rune) bool { return unicode.IsDigit(r) || r == ' ' }) != "" {
			companyTypes = one(ct)
		}
	}
	if companyTypes == nil {
		companyTypes = []any{}
	}
	geos, techs, roles := one(matchGeography(rs)), one(matchTechnology(rs)), one(matchHiring(rs))
	missing := []any{}
	if count == 0 {
		missing = append(missing, "requested_count")
	}
	if len(companyTypes) == 0 {
		missing = append(missing, "company_type")
	}
	o := wbcrud.NewObj()
	o.Set("schema_version", "1.0")
	o.Set("original_query", query)
	o.Set("requested_count", int64(count))
	o.Set("company_types", companyTypes)
	o.Set("geographies", geos)
	o.Set("technologies", techs)
	o.Set("hiring_roles", roles)
	o.Set("exclusions", []any{})
	o.Set("evidence_requirements", []any{"canonical_company_domain", "criterion_evidence", "evidence_url", "retrieved_at", "field_confidence"})
	o.Set("missing_fields", missing)
	o.Set("complete", len(missing) == 0)
	o.Set("brief_id", AccountBriefID(int64(count), companyTypes, geos, techs, roles, []any{}))
	return o
}

// ExtractAccountDiscovery is extract_account_discovery_request: a complete brief for an explicit list
// request, else nil.
func ExtractAccountDiscovery(query string) *wbcrud.Obj {
	if !isMarketRequest([]rune(pyClean(query))) {
		return nil
	}
	b := BuildAccountBrief(query)
	if c, _ := b.Get("complete"); c != true {
		return nil
	}
	return b
}
