package leadweb

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// parseHTML parses like a browser with scripting off, so <noscript> content
// is markup (bs4's html.parser does not treat it as raw text either).
func parseHTML(src string) *html.Node {
	doc, err := html.ParseWithOptions(strings.NewReader(src), html.ParseOptionEnableScripting(false))
	if err != nil {
		return &html.Node{Type: html.DocumentNode}
	}
	return doc
}

// bs4 keeps the strings of these elements in their own string classes, which
// get_text() leaves out (html.parser builder string_containers).
var skipTextIn = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Template: true, atom.Rt: true, atom.Rp: true}

// getText is BeautifulSoup's tag.get_text(separator=sep): every text string
// under n in document order, joined with sep; comments and the strings of
// script/style/template/rt/rp are left out.
func getText(n *html.Node, sep string) string {
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			parts = append(parts, n.Data)
			return
		case html.ElementNode:
			if skipTextIn[n.DataAtom] {
				return
			}
		case html.CommentNode, html.DoctypeNode:
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(parts, sep)
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// elements returns the element nodes under n (n included) in document order
// for which keep is true.
func elements(n *html.Node, keep func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && keep(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func anchorsWithHref(n *html.Node) []string {
	var out []string
	for _, a := range elements(n, func(e *html.Node) bool { return e.DataAtom == atom.A }) {
		if h, ok := attr(a, "href"); ok {
			out = append(out, h)
		}
	}
	return out
}

// ── enrichment/website_scraper.py (_scrape_via_http) ──

var (
	sitePhonePatterns = compileAll(
		`\+?91[\-\s]?\d{5}[\-\s]?\d{5}`,
		`\+?91[\-\s]?\d{10}`,
		`1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}`,
		`0\d{2,4}[\-\s]?\d{6,8}`,
	)
	siteEmailRe     = pyRe(`[\w\.\-\+]+@[\w\.\-]+\.\w{2,}`)
	siteEmailJunk   = []string{"example.com", "domain.com", "email.com", ".png", ".jpg", ".css", ".js"}
	telKeep         = pyRe(`[^\d\+\-\s]`)
	footerAttrRe    = regexp.MustCompile(`(?i)footer|bottom|contact-bar`)
	sitePagesToScan = []string{"", "/contact", "/contact-us", "/about"}
)

// SocialLinks are the last matching profile link of each kind on the main page.
type SocialLinks struct {
	LinkedIn  string `json:"linkedin"`
	Twitter   string `json:"twitter"`
	Facebook  string `json:"facebook"`
	Instagram string `json:"instagram"`
}

// SiteContacts is what ScanSite found (legacy _scrape_via_http result).
type SiteContacts struct {
	Phones      []string    `json:"phones"`
	Emails      []string    `json:"emails"`
	Social      SocialLinks `json:"social"`
	Description string      `json:"description"`
}

// sitePhones is website_scraper._extract_phones.
func sitePhones(text string) []string {
	var out []string
	keep := func(m string) {
		m = pyStrip(m)
		if n := runeLen(pyDigits(m)); n >= 7 && n <= 13 {
			out = append(out, m)
		}
	}
	for _, re := range sitePhonePatterns {
		for _, m := range re.FindAllString(text, -1) {
			keep(m)
		}
	}
	for _, m := range tenDigitWords(text) {
		keep(m)
	}
	return out
}

// siteEmails is website_scraper._extract_emails.
func siteEmails(text string) []string {
	var out []string
	for _, e := range siteEmailRe.FindAllString(text, -1) {
		if !containsAny(strings.ToLower(e), siteEmailJunk) {
			out = append(out, e)
		}
	}
	return out
}

func telPhones(n *html.Node) []string {
	var out []string
	for _, h := range anchorsWithHref(n) {
		if strings.HasPrefix(h, "tel:") {
			p := telKeep.ReplaceAllString(pyStrip(strings.ReplaceAll(h, "tel:", "")), "")
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// ScanSite is the legacy website contact scan: the home page plus /contact,
// /contact-us and /about (fetch returns ok=false for a page that failed),
// phones from page text and tel: links (footer-like blocks only while nothing
// was found yet), emails from text and mailto: links, social links and the
// meta description from the home page. At most five phones and five emails.
func ScanSite(siteURL string, fetch func(pageURL string) (html string, ok bool)) SiteContacts {
	var res SiteContacts
	for _, p := range sitePagesToScan {
		pageURL := siteURL
		if p != "" {
			pageURL = urljoin(siteURL, p)
		}
		src, ok := fetch(pageURL)
		if !ok {
			continue
		}
		doc := parseHTML(src)
		plain := getText(doc, " ")
		res.Phones = append(res.Phones, sitePhones(plain)...)
		res.Phones = append(res.Phones, telPhones(doc)...)
		if len(res.Phones) == 0 {
			for _, f := range footerBlocks(doc) {
				res.Phones = append(res.Phones, sitePhones(getText(f, " "))...)
				res.Phones = append(res.Phones, telPhones(f)...)
			}
		}
		res.Emails = append(res.Emails, siteEmails(plain)...)
		for _, h := range anchorsWithHref(doc) {
			if strings.HasPrefix(h, "mailto:") {
				e := strings.ReplaceAll(h, "mailto:", "")
				e = pyStrip(strings.SplitN(e, "?", 2)[0])
				if e != "" && strings.Contains(e, "@") {
					res.Emails = append(res.Emails, e)
				}
			}
		}
		if pageURL == siteURL {
			res.Social = socialLinks(doc)
			if d := firstMeta(doc, "name", "description"); d != nil {
				if c, _ := attr(d, "content"); c != "" {
					res.Description = truncRunes(c, 300)
				}
			}
		}
	}
	res.Phones = head(uniq(res.Phones), 5)
	res.Emails = head(uniq(res.Emails), 5)
	return res
}

// footerBlocks: <footer> elements, then elements whose id, then class, looks
// like a footer / bottom bar (legacy order; duplicates by identity skipped).
func footerBlocks(doc *html.Node) []*html.Node {
	out := elements(doc, func(e *html.Node) bool { return e.DataAtom == atom.Footer })
	seen := map[*html.Node]bool{}
	for _, f := range out {
		seen[f] = true
	}
	for _, key := range []string{"id", "class"} {
		for _, e := range elements(doc, func(e *html.Node) bool {
			v, ok := attr(e, key)
			return ok && footerAttrRe.MatchString(v)
		}) {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}

func socialLinks(doc *html.Node) SocialLinks {
	var s SocialLinks
	for _, h := range anchorsWithHref(doc) {
		l := strings.ToLower(h)
		switch {
		case strings.Contains(l, "linkedin.com/company") || strings.Contains(l, "linkedin.com/in/"):
			s.LinkedIn = h
		case strings.Contains(l, "twitter.com/") || strings.Contains(l, "x.com/"):
			s.Twitter = h
		case strings.Contains(l, "facebook.com/"):
			s.Facebook = h
		case strings.Contains(l, "instagram.com/"):
			s.Instagram = h
		}
	}
	return s
}

func firstMeta(doc *html.Node, key, val string) *html.Node {
	ms := elements(doc, func(e *html.Node) bool {
		if e.DataAtom != atom.Meta {
			return false
		}
		v, ok := attr(e, key)
		return ok && v == val
	})
	if len(ms) == 0 {
		return nil
	}
	return ms[0]
}

// Meta returns the page title (stripped, at most 1000 characters) and the
// meta description, falling back to og:description (at most 2000), as the
// browser service's page_meta does.
func Meta(src string) (title, description string) {
	doc := parseHTML(src)
	if ts := elements(doc, func(e *html.Node) bool { return e.DataAtom == atom.Title }); len(ts) > 0 {
		var parts []string
		for c := ts[0].FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				if s := pyStrip(c.Data); s != "" {
					parts = append(parts, s)
				}
			}
		}
		title = truncRunes(strings.Join(parts, ""), 1000)
	}
	m := firstMeta(doc, "name", "description")
	if m == nil {
		m = firstMeta(doc, "property", "og:description")
	}
	if m != nil {
		c, _ := attr(m, "content")
		description = truncRunes(c, 2000)
	}
	return title, description
}

// Links returns the http(s) targets of the page's <a href> links resolved
// against base (Python urljoin semantics; <base href> is not applied, as in
// legacy), deduplicated in document order.
func Links(src, base string) []string {
	var out []string
	for _, h := range anchorsWithHref(parseHTML(src)) {
		u := urljoin(base, h)
		if s, _, ok := splitScheme(cleanURL(u)); ok && (s == "http" || s == "https") {
			out = append(out, u)
		}
	}
	return uniq(out)
}
