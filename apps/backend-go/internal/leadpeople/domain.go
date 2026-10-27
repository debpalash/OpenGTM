package leadpeople

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// multiPartSuffixes is _MULTI_PART_SUFFIXES of leadgen/dedup.py.
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

// stripDiacritics is strip_diacritics: NFKD, then drop combining characters.
func stripDiacritics(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		if norm.NFD.PropertiesString(string(r)).CCC() != 0 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeDomain is leadgen/dedup.py normalize_domain: the registrable domain
// (eTLD+1) of a URL or host, with the common two-part public suffixes
// (acme.co.uk stays acme.co.uk). It differs from leaddedup.NormalizeDomain and
// entities.NormalizeDomain, which port other modules and keep subdomains.
func NormalizeDomain(website string) string {
	if website == "" {
		return ""
	}
	d := strings.ToLower(providers.PyStrip(stripDiacritics(website)))
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
