package wbcrud

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Ports of services/leadgen/dedup.py normalize_domain / normalize_company and
// the identity closure of routers/workbooks.add_rows. (entities.NormalizeDomain
// ports a different module, services/dedup.py.)

var multiPartSuffixes = map[string]bool{}

func init() {
	for _, s := range strings.Fields(`co.uk org.uk ac.uk gov.uk me.uk ltd.uk plc.uk net.uk
 com.au net.au org.au edu.au gov.au id.au
 co.in net.in org.in gen.in firm.in ind.in ac.in gov.in edu.in
 co.nz net.nz org.nz govt.nz ac.nz co.za org.za web.za net.za com.br net.br org.br gov.br
 com.sg edu.sg gov.sg org.sg net.sg com.cn net.cn org.cn gov.cn co.jp or.jp ne.jp go.jp ac.jp
 com.mx com.tr com.tw com.hk com.my com.ph co.id co.kr co.th com.ar com.co co.il com.sa
 com.ua com.pk com.ng com.eg com.bd com.vn`) {
		multiPartSuffixes[s] = true
	}
}

// legalSuffixes in regex alternation order (holdings? / labs? expanded).
var legalSuffixes = []string{"inc", "incorporated", "llc", "l.l.c", "ltd", "limited", "corp", "corporation", "co",
	"company", "gmbh", "ag", "sarl", "sas", "sa", "bv", "plc", "pvt", "pte", "llp", "group", "holdings", "holding",
	"technologies", "technology", "solutions", "services", "systems", "labs", "lab"}

// pyTrim is str.strip().
func pyTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// pyWordRune is regex \w for str patterns.
func pyWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// stripDiacritics: NFKD, then drop characters with a canonical combining class.
func stripDiacritics(s string) string {
	d := norm.NFKD.String(s)
	var b strings.Builder
	for _, r := range d {
		if norm.NFD.PropertiesString(string(r)).CCC() != 0 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func normalizeLeadCompany(name string) string {
	if name == "" {
		return ""
	}
	rs := []rune(strings.ToLower(stripDiacritics(name)))
	var out strings.Builder
	for i := 0; i < len(rs); {
		if i == 0 || !pyWordRune(rs[i-1]) {
			if n := legalSuffixAt(rs, i); n > 0 {
				i += n
				continue
			}
		}
		if (rs[i] >= 'a' && rs[i] <= 'z') || (rs[i] >= '0' && rs[i] <= '9') {
			out.WriteRune(rs[i])
		}
		i++
	}
	return out.String()
}

// legalSuffixAt returns the length consumed by `\b(alt)\b\.?` at rs[i:], or 0.
func legalSuffixAt(rs []rune, i int) int {
next:
	for _, w := range legalSuffixes {
		wr := []rune(w)
		if i+len(wr) > len(rs) {
			continue
		}
		for k, c := range wr {
			if rs[i+k] != c {
				continue next
			}
		}
		end := i + len(wr)
		if end < len(rs) && pyWordRune(rs[end]) {
			continue
		}
		if end < len(rs) && rs[end] == '.' {
			end++
		}
		return end - i
	}
	return 0
}

func normalizeLeadDomain(website string) string {
	if website == "" {
		return ""
	}
	d := strings.ToLower(pyTrim(stripDiacritics(website)))
	for _, p := range []string{"https://", "http://", "www."} {
		d = strings.TrimPrefix(d, p)
	}
	for _, sep := range []string{"/", "?", "#"} {
		if i := strings.Index(d, sep); i >= 0 {
			d = d[:i]
		}
	}
	if i := strings.LastIndex(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[:i]
	}
	var parts []string
	for _, p := range strings.Split(d, ".") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	n := len(parts)
	if n >= 3 && multiPartSuffixes[parts[n-2]+"."+parts[n-1]] {
		return strings.Join(parts[n-3:], ".")
	}
	if n >= 2 {
		return strings.Join(parts[n-2:], ".")
	}
	return d
}

// identityOf is add_rows._identity over already-extracted string values:
// website or domain (first non-empty), else the normalised company.
func identityOf(website, domain, company string) string {
	site := website
	if site == "" {
		site = domain
	}
	if dom := normalizeLeadDomain(site); dom != "" {
		return "d:" + dom
	}
	if comp := normalizeLeadCompany(company); comp != "" {
		return "n:" + comp
	}
	return ""
}

func strField(o *Obj, k string) string {
	v, _ := o.Get(k)
	s, _ := v.(string) // a non-string value is treated as absent (Python raises a 500 here)
	return s
}

// RowIdentity is the dedupe identity of a row dict ("" = no identity).
func RowIdentity(o *Obj) string {
	return identityOf(strField(o, "website"), strField(o, "domain"), strField(o, "company"))
}

// TrimPy is Python's str.strip().
func TrimPy(s string) string { return pyTrim(s) }
