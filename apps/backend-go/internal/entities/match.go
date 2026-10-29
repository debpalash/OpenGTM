package entities

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// Ports of services/dedup.py (compare_leads and its normalisers) and
// services/entities/graph.py::identity_domain, kept byte-for-byte compatible with the
// Python behaviour the merge log score and recompute depend on.

// Lead is the lead-shaped view CompanyEntity.repr_dict() feeds to compare_leads.
type Lead struct{ Company, Website, Phone, Email, City string }

var companySuffixes = []string{"pvt", "private", "ltd", "limited", "llp", "llc", "inc", "incorporated", "corp",
	"corporation", "co", "company", "group", "enterprises", "solutions", "technologies", "tech", "services",
	"consulting", "consultants", "india", "global", "international", "intl"}

const noiseChars = ".-,&()'\"!@#$%^*+="

// pySpace mirrors Python's str.isspace / regex \s.
func pySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// pyWord mirrors Python's regex \w for str patterns.
func pyWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func pyStrip(s string) string { return strings.TrimFunc(s, pySpace) }

// NormalizeCompany is dedup.normalize_company.
func NormalizeCompany(name string) string {
	if name == "" {
		return ""
	}
	name = pyStrip(strings.ToLower(name))
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(noiseChars, r) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	rs := []rune(b.String())
	var out []rune
	for i := 0; i < len(rs); {
		if i == 0 || !pyWord(rs[i-1]) { // \b before a word character
			if n := suffixAt(rs, i); n > 0 {
				i += n
				continue
			}
		}
		out = append(out, rs[i])
		i++
	}
	return strings.Join(strings.FieldsFunc(string(out), pySpace), " ")
}

// suffixAt returns the length of the first company suffix (alternation order) at rs[i:] that ends on a
// word boundary, or 0.
func suffixAt(rs []rune, i int) int {
	for _, w := range companySuffixes {
		wr := []rune(w)
		if i+len(wr) > len(rs) {
			continue
		}
		match := true
		for k, c := range wr {
			if rs[i+k] != c {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if end := i + len(wr); end == len(rs) || !pyWord(rs[end]) {
			return len(wr)
		}
	}
	return 0
}

// NormalizeDomain is dedup.normalize_domain (urlparse(...).hostname without "www.").
func NormalizeDomain(url string) string {
	if url == "" {
		return ""
	}
	url = pyStrip(strings.ToLower(url))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	host, ok := urlHostname(url)
	if !ok {
		return strings.Trim(strings.ReplaceAll(strings.ToLower(url), "www.", ""), "/")
	}
	return strings.TrimPrefix(host, "www.")
}

// urlHostname extracts urlparse(url).hostname for an "http(s)://..." string; ok=false when
// urlparse raises ValueError.
func urlHostname(url string) (string, bool) {
	url = strings.TrimLeftFunc(url, func(r rune) bool { return r <= 0x20 })
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)
	rest := url[strings.Index(url, ":")+1:] // scheme is http or https
	netloc := ""
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := len(rest)
		for _, c := range "/?#" {
			if i := strings.IndexRune(rest, c); i >= 0 && i < end {
				end = i
			}
		}
		netloc = rest[:end]
	}
	if (strings.Contains(netloc, "[") && !strings.Contains(netloc, "]")) ||
		(strings.Contains(netloc, "]") && !strings.Contains(netloc, "[")) {
		return "", false
	}
	hostinfo := netloc
	if i := strings.LastIndex(netloc, "@"); i >= 0 {
		hostinfo = netloc[i+1:]
	}
	var host string
	if strings.Contains(hostinfo, "[") {
		bracketed := hostinfo[strings.Index(hostinfo, "[")+1:]
		host = bracketed
		if i := strings.Index(bracketed, "]"); i >= 0 {
			host = bracketed[:i]
		}
		if !strings.HasPrefix(host, "v") {
			a, err := netip.ParseAddr(host)
			if err != nil || !a.Is6() {
				return "", false
			}
		}
	} else {
		host = hostinfo
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		}
	}
	return strings.ToLower(host), true
}

// NormalizePhone is dedup.normalize_phone: the last 10 decimal digits.
func NormalizePhone(phone string) string {
	var ds []rune
	for _, r := range phone {
		if unicode.IsDigit(r) {
			ds = append(ds, r)
		}
	}
	if len(ds) > 10 {
		ds = ds[len(ds)-10:]
	}
	return string(ds)
}

func normalizeEmail(e string) string { return pyStrip(strings.ToLower(e)) }

// sharedHosts are hosts whose path (not hostname) identifies a business.
var sharedHosts = map[string]bool{}

func init() {
	for _, h := range strings.Fields(`facebook.com fb.com fb.me instagram.com linkedin.com twitter.com x.com
		youtube.com youtu.be tiktok.com pinterest.com threads.net wa.me whatsapp.com t.me linktr.ee linkin.bio
		beacons.ai carrd.co bio.link google.com goo.gl g.page bit.ly tinyurl.com ow.ly yelp.com tripadvisor.com
		justdial.com indiamart.com zomato.com swiggy.com amazon.com etsy.com ebay.com github.com medium.com
		substack.com notion.site crunchbase.com glassdoor.com indeed.com clutch.co g2.com wikipedia.org
		yellowpages.com angi.com houzz.com`) {
		sharedHosts[h] = true
	}
}

// IdentityDomain is graph.identity_domain: a company's own domain, or "" for shared/profile hosts.
func IdentityDomain(url string) string {
	d := NormalizeDomain(url)
	if d == "" || !strings.Contains(d, ".") {
		return ""
	}
	if sharedHosts[d] {
		return ""
	}
	for h := range sharedHosts {
		if strings.HasSuffix(d, "."+h) {
			return ""
		}
	}
	return d
}

func jaro(s1, s2 []rune) float64 {
	if string(s1) == string(s2) {
		return 1
	}
	if len(s1) == 0 || len(s2) == 0 {
		return 0
	}
	l1, l2 := len(s1), len(s2)
	md := max(l1, l2)/2 - 1
	if md < 0 {
		md = 0
	}
	m1, m2 := make([]bool, l1), make([]bool, l2)
	matches, trans := 0, 0
	for i := 0; i < l1; i++ {
		for j := max(0, i-md); j < min(i+md+1, l2); j++ {
			if m2[j] || s1[i] != s2[j] {
				continue
			}
			m1[i], m2[j] = true, true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}
	k := 0
	for i := 0; i < l1; i++ {
		if !m1[i] {
			continue
		}
		for !m2[k] {
			k++
		}
		if s1[i] != s2[k] {
			trans++
		}
		k++
	}
	m := float64(matches)
	return (m/float64(l1) + m/float64(l2) + (m-float64(trans)/2)/m) / 3
}

func jaroWinkler(a, b string) float64 {
	s1, s2 := []rune(a), []rune(b)
	j := jaro(s1, s2)
	prefix := 0
	for i := 0; i < min(len(s1), len(s2), 4); i++ {
		if s1[i] != s2[i] {
			break
		}
		prefix++
	}
	return j + float64(prefix)*0.1*(1-j)
}

// round4 is Python's round(x, 4) (correctly rounded decimal, ties to even).
func round4(x float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 4, 64), 64)
	return f
}

// CompareLeads is dedup.compare_leads(...).score.
func CompareLeads(a, b Lead) float64 {
	var company, domain, phone, email, city float64
	na, nb := NormalizeCompany(a.Company), NormalizeCompany(b.Company)
	if na != "" && nb != "" {
		company = jaroWinkler(na, nb)
	}
	da, db := NormalizeDomain(a.Website), NormalizeDomain(b.Website)
	if da != "" && db != "" {
		if da == db {
			domain = 1
		} else {
			domain = jaroWinkler(da, db)
		}
	}
	pa, pb := NormalizePhone(a.Phone), NormalizePhone(b.Phone)
	if pa != "" && pb != "" && len([]rune(pa)) >= 7 && pa == pb {
		phone = 1
	}
	ea, eb := normalizeEmail(a.Email), normalizeEmail(b.Email)
	if strings.Contains(ea, "@") && strings.Contains(eb, "@") && strings.Split(ea, "@")[1] == strings.Split(eb, "@")[1] {
		email = 1
	}
	ca, cb := strings.ToLower(pyStrip(a.City)), strings.ToLower(pyStrip(b.City))
	if ca != "" && cb != "" {
		if ca == cb {
			city = 1
		} else {
			city = jaroWinkler(ca, cb)
		}
	}
	total := 0.0
	total += company * 0.35
	total += domain * 0.30
	total += phone * 0.15
	total += email * 0.10
	total += city * 0.10
	if domain == 1.0 {
		total = max(total, 0.90)
	}
	if phone == 1.0 {
		total = max(total, 0.92)
	}
	return round4(total)
}
