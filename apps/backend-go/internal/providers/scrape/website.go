package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ── website_scraper_provider.py ────────────────────────────────────────────

type websiteScraper struct{ gated }

func (websiteScraper) Name() string { return "website_scraper" }

func (websiteScraper) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	website := ls(in.Lead, "website")
	if website == "" {
		return fail(in, "No website URL available"), nil
	}
	fe := current().Fetch
	if fe == nil {
		return fail(in, errNoWeb), nil
	}
	site := normalizeWebsiteURL(website)
	data := leadweb.ScanSite(site, func(u string) (string, bool) {
		r := fe.Fetch(ctx, u, leadweb.FetchOptions{Tier: 2, Timeout: 12 * time.Second})
		return r.Text, r.OK()
	})
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	f := providers.NewObj()
	if len(data.Emails) > 0 {
		f.Set("email", data.Emails[0])
		if len(data.Emails) > 1 {
			f.Set("secondary_emails", strings.Join(data.Emails[1:min(3, len(data.Emails))], "|"))
		}
	}
	if len(data.Phones) > 0 {
		f.Set("phone", data.Phones[0])
		if len(data.Phones) > 1 {
			f.Set("secondary_phones", strings.Join(data.Phones[1:min(3, len(data.Phones))], "|"))
		}
	}
	if data.Social.LinkedIn != "" {
		f.Set("linkedin_url", data.Social.LinkedIn)
	}
	if data.Social.Twitter != "" {
		f.Set("twitter_url", data.Social.Twitter)
	}
	if data.Social.Facebook != "" {
		f.Set("facebook_url", data.Social.Facebook)
	}
	if data.Description != "" {
		f.Set("description", data.Description)
	}
	if f.Len() > 0 {
		return result(in, f, 0.65, ""), nil
	}
	return fail(in, "No contact info found on website"), nil
}

// ── deep_scraper.py ────────────────────────────────────────────────────────

var (
	dsPages = []string{"/", "/about", "/about-us", "/about-us/", "/contact", "/contact-us", "/team", "/our-team",
		"/leadership", "/people", "/management", "/careers", "/jobs", "/privacy"}
	dsSize = []struct {
		re   *regexp.Regexp
		kind string
	}{
		{pyRe(`(?i)(\d[\d,]+)\+?\s*(?:employees|team members|people|staff|professionals)`), "count"},
		{pyRe(`(?i)(?:team|staff|employees?)\s*(?:of|:)\s*(\d[\d,]+)`), "count"},
		{pyRe(`(?i)(?:over|more than|approximately|about|~)\s*(\d[\d,]+)\s*(?:employees|people|team)`), "count"},
		{pyRe(`(?i)(\d+)\s*-\s*(\d+)\s*(?:employees|people)`), "range"},
		{pyRe(`(?i)(?:small|startup|early.?stage)`), "1-10"},
		{pyRe(`(?i)(?:mid.?size|medium|growing)`), "11-50"},
		{pyRe(`(?i)(?:large|enterprise|global)`), "200+"},
	}
	dsYear = []*regexp.Regexp{
		pyRe(`(?i)(?:founded|established|since|started|incorporated|est\.?)\s*(?:in\s*)?(\d{4})`),
		pyRe(`(?i)(?:since|est\.?)\s*(\d{4})`),
		pyRe(`(?i)©\s*(\d{4})`),
	}
	dsLeadership = []string{"ceo", "cto", "coo", "cfo", "cmo", "cio", "chro", "cpo", "founder", "co-founder", "cofounder",
		"president", "vice president", "vp", "director", "managing director", "head of", "head -", "chief", "partner", "principal", "owner"}
	dsNoise = []string{"explore", "learn", "discover", "see", "view", "our", "your", "read", "more", "find", "get", "try",
		"start", "click", "download", "subscribe", "join", "sign", "products", "services", "solutions", "contact",
		"flagship", "features", "platform"}
	dsCardSelectors = []*regexp.Regexp{
		regexp.MustCompile(`(?i)team|member|leader|executive|people|staff|person`),
		regexp.MustCompile(`(?i)card|profile|bio`),
	}
	dsTitleClass = regexp.MustCompile(`(?i)title|role|position|designation`)
	dsInlineRe   = regexp.MustCompile(`([A-Z][a-z]+ [A-Z][a-z]+)\s*[-–—|,]\s*((?:CEO|CTO|COO|CFO|CMO|Founder|Co-Founder|Director|VP|Vice President|Head of|Managing Director|Partner|Owner)[^<\n]{0,40})`)
	dsEmailRe    = pyRe(`(?i)[\w.\-+]+@[\w.\-]+\.(?:com|org|net|io|co|in|ai|dev|tech|biz|info|us|uk|eu)`)
	dsEmailJunk  = []string{"example.com", "domain.com", "email.com", ".png", ".jpg", ".css", ".js", "wixpress", "sentry.io", "webpack"}
	dsPhone1     = pyRe(`\+?(?:91|1|44|61|49|33|971)[\s\-.]?\(?\d{2,4}\)?[\s\-.]?\d{3,4}[\s\-.]?\d{3,4}`)
	dsPhone3     = pyRe(`1800[\s\-.]?\d{2,3}[\s\-.]?\d{4,6}`)
	dsTelKeep    = pyRe(`[^\d+\-\s()]`)
)

type deepScraper struct {
	gated
	MaxPages int           // 0 = 8
	Timeout  time.Duration // 0 = 10 s
}

func (deepScraper) Name() string { return "deep_scraper" }

type teamMember struct{ name, title string }

// jsonVal is a decoded JSON-LD value (UseNumber).
func jsonVal(m map[string]any, k string) any { return m[k] }

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// pyStrOf is str(v) of a decoded JSON value (strings and numbers; others as
// Python would print the common cases).
func pyStrOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			f, _ := t.Float64()
			return floatRepr(f)
		}
		return string(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	}
	return pyDumps(v, false)
}

// structured is _extract_structured_data (keys in Python's insertion order).
func structured(doc *html.Node) *providers.Obj {
	data := providers.NewObj()
	setdefault := func(k string, v any) {
		if _, ok := data.Get(k); !ok {
			data.Set(k, v)
		}
	}
	for _, sc := range findAll(doc, func(n *html.Node) bool {
		if n.DataAtom != atom.Script {
			return false
		}
		t, ok := attr(n, "type")
		return ok && t == "application/ld+json"
	}) {
		src, _ := scriptString(sc)
		dec := json.NewDecoder(strings.NewReader(src))
		dec.UseNumber()
		var ld any
		if dec.Decode(&ld) != nil || dec.More() {
			continue
		}
		items := []any{ld}
		if l, ok := ld.([]any); ok {
			items = l
		}
		if m, ok := ld.(map[string]any); ok {
			if g, has := m["@graph"]; has {
				switch gt := g.(type) {
				case []any:
					items = gt
				case map[string]any:
					items = nil // iterating a dict yields its keys (never dicts)
				default:
					items = nil
				}
			}
		}
		for _, it := range items {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			// Deviation (fix): a non-string @type raised AttributeError and
			// failed the whole provider in Python; it is simply not a match here.
			typ, _ := item["@type"].(string)
			typ = strings.ToLower(typ)
			switch typ {
			case "organization", "corporation", "localbusiness", "store", "restaurant", "company":
				if truthy(item["name"]) {
					data.Set("legal_name", item["name"])
				}
				if truthy(item["description"]) {
					data.Set("description", trunc(pyStrOf(item["description"]), 500))
				}
				if truthy(item["email"]) {
					data.Set("email", item["email"])
				}
				if truthy(item["telephone"]) {
					data.Set("phone", item["telephone"])
				}
				if emp := item["numberOfEmployees"]; truthy(emp) {
					if em, ok := emp.(map[string]any); ok {
						if v, has := em["value"]; has {
							data.Set("employee_count", v)
						} else {
							data.Set("employee_count", pyStrOf(emp))
						}
					} else {
						data.Set("employee_count", pyStrOf(emp))
					}
				}
				if truthy(item["foundingDate"]) {
					data.Set("founding_year", trunc(pyStrOf(item["foundingDate"]), 4))
				}
				if truthy(item["industry"]) {
					data.Set("industry", item["industry"])
				}
				if addr, ok := item["address"].(map[string]any); ok {
					var parts []string
					for _, k := range []string{"streetAddress", "addressLocality", "addressRegion", "postalCode", "addressCountry"} {
						// Deviation (fix): Python joined raw values and a nested
						// object (addressCountry {"name": ...}) raised TypeError,
						// dropping the rest of the script; non-strings are skipped.
						if s, ok := addr[k].(string); ok && s != "" {
							parts = append(parts, s)
						}
					}
					if full := strings.Join(parts, ", "); full != "" {
						data.Set("address", full)
					}
				}
				var same []any
				switch sa := item["sameAs"].(type) {
				case string:
					same = []any{sa}
				case []any:
					same = sa
				}
				for _, l := range same {
					link, ok := l.(string)
					if !ok {
						continue
					}
					ll := strings.ToLower(link)
					switch {
					case strings.Contains(ll, "linkedin.com"):
						data.Set("linkedin", link)
					case strings.Contains(ll, "twitter.com") || strings.Contains(ll, "x.com"):
						data.Set("twitter", link)
					case strings.Contains(ll, "facebook.com"):
						data.Set("facebook", link)
					case strings.Contains(ll, "instagram.com"):
						data.Set("instagram", link)
					}
				}
			}
			_, hasCP := item["contactPoint"]
			if typ == "contactpoint" || hasCP {
				var cp any = item
				if typ != "contactpoint" {
					cp = item["contactPoint"]
					if cp == nil {
						cp = map[string]any{}
					}
				}
				if l, ok := cp.([]any); ok {
					if len(l) > 0 {
						cp = l[0]
					} else {
						cp = map[string]any{}
					}
				}
				if cm, ok := cp.(map[string]any); ok {
					if truthy(cm["email"]) {
						setdefault("email", cm["email"])
					}
					if truthy(cm["telephone"]) {
						setdefault("phone", cm["telephone"])
					}
				}
			}
		}
	}
	return data
}

// teamMembers is _extract_team_members.
func teamMembers(doc *html.Node, text string) []teamMember {
	var people []teamMember
	seen := map[string]bool{}
	for _, sel := range dsCardSelectors {
		for _, el := range findAll(doc, func(n *html.Node) bool {
			return tagIn(atom.Div, atom.Li, atom.Article, atom.Section)(n) && classMatches(n, sel)
		}) {
			nameEl := find(el, tagIn(atom.H2, atom.H3, atom.H4, atom.H5, atom.Strong))
			if nameEl == nil {
				continue
			}
			name := getText(nameEl, "", true)
			if name == "" || runeLen(name) > 60 || len(pySplit(name)) < 2 {
				continue
			}
			if containsAny(strings.ToLower(name), dsNoise) {
				continue
			}
			first, _ := utf8.DecodeRuneInString(name)
			if !unicode.IsUpper(first) || !strings.Contains(name, " ") {
				continue
			}
			title := ""
			if te := find(el, func(n *html.Node) bool {
				return tagIn(atom.P, atom.Span, atom.Small)(n) && classMatches(n, dsTitleClass)
			}); te != nil {
				title = getText(te, "", true)
			} else {
				for _, sib := range []*html.Node{find(el, tagIn(atom.P)), findNext(nameEl, tagIn(atom.P))} {
					if sib == nil {
						continue
					}
					t := getText(sib, "", true)
					if t != "" && runeLen(t) < 80 && containsAny(strings.ToLower(t), dsLeadership) {
						title = t
						break
					}
				}
			}
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				if title == "" {
					title = "N/A"
				}
				people = append(people, teamMember{name, title})
			}
		}
	}
	if len(people) < 3 {
		for _, m := range dsInlineRe.FindAllStringSubmatch(text, -1) {
			name, title := pyStrip(m[1]), pyStrip(m[2])
			if !seen[strings.ToLower(name)] && len(pySplit(name)) >= 2 {
				seen[strings.ToLower(name)] = true
				people = append(people, teamMember{name, title})
			}
		}
	}
	if len(people) > 10 {
		people = people[:10]
	}
	return people
}

func companySize(text string) string {
	for _, p := range dsSize {
		m := p.re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		switch p.kind {
		case "count":
			if n, ok := pyInt(strings.ReplaceAll(m[1], ",", "")); ok && n >= 1 && n <= 1_000_000 {
				return itoa(n)
			}
		case "range":
			return m[1] + "-" + m[2]
		default:
			return p.kind
		}
	}
	return ""
}

func foundingYear(text string) string {
	for _, re := range dsYear {
		if m := re.FindStringSubmatch(text); m != nil {
			if y, ok := pyInt(m[1]); ok && y >= 1800 && y <= 2026 {
				return itoa(y)
			}
		}
	}
	return ""
}

func itoa(n int) string { return str(n) }

func industryFromMeta(doc *html.Node) string {
	for _, m := range findAll(doc, tagIn(atom.Meta)) {
		name, _ := attr(m, "name")
		if name == "" {
			name, _ = attr(m, "property")
		}
		name = strings.ToLower(name)
		content, _ := attr(m, "content")
		if name == "keywords" && content != "" {
			kw := strings.Split(content, ",")
			if len(kw) > 3 {
				kw = kw[:3]
			}
			for i := range kw {
				kw[i] = pyStrip(kw[i])
			}
			return strings.Join(kw, ", ")
		}
		if name == "article:section" && content != "" {
			return content
		}
	}
	return ""
}

// errTooManyRedirects ends a guarded fetch (url_guard.guarded_get).
var errTooManyRedirects = errors.New("too many redirects")

const deepPageCap = 2 << 20

// guardedGet is url_guard.guarded_get over the caller's strict egress client:
// redirects are followed by hand (at most six), every hop re-checked with the
// egress URL guard (the client also refuses private addresses at dial time).
func guardedGet(ctx context.Context, c *http.Client, rawURL string, hdr map[string]string) (int, string, string, error) {
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cur := rawURL
	for hop := 0; hop <= 6; hop++ {
		if err := egress.CheckURL(cur, true); err != nil {
			return 0, "", "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return 0, "", "", err
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := cc.Do(req)
		if err != nil {
			return 0, "", "", err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, deepPageCap))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			loc := resp.Header.Get("Location")
			if loc == "" {
				return resp.StatusCode, resp.Header.Get("Content-Type"), string(body), rerr
			}
			base, err := url.Parse(cur)
			if err != nil {
				return 0, "", "", err
			}
			next, err := base.Parse(loc)
			if err != nil {
				return 0, "", "", err
			}
			cur = next.String()
			continue
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), decodeBody(body, resp.Header.Get("Content-Type")), rerr
	}
	return 0, "", "", errTooManyRedirects
}

// decodeBody is httpx's response.text for UTF-8 and Latin-1 pages.
func decodeBody(b []byte, ctype string) string {
	if _, params, err := mime.ParseMediaType(ctype); err == nil {
		switch strings.ToLower(params["charset"]) {
		case "iso-8859-1", "latin-1", "latin1":
			r := make([]rune, len(b))
			for i, c := range b {
				r[i] = rune(c)
			}
			return string(r)
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

var deepHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Language": "en-US,en;q=0.9",
}

func dsEmailRank(e string) int {
	switch {
	case containsAny(e, []string{"ceo@", "founder@", "director@"}):
		return 0
	case containsAny(e, []string{"info@", "contact@", "hello@", "sales@"}):
		return 1
	case containsAny(e, []string{"support@", "help@", "admin@"}):
		return 2
	}
	return 3
}

func dsTeamRank(t string) int {
	t = strings.ToLower(t)
	switch {
	case containsAny(t, []string{"ceo", "founder", "owner", "md"}):
		return 0
	case containsAny(t, []string{"cto", "coo", "cfo", "cmo", "cio"}):
		return 1
	case containsAny(t, []string{"vp", "vice president", "director"}):
		return 2
	case containsAny(t, []string{"head", "lead", "manager"}):
		return 3
	}
	return 4
}

// employeeCount is the company_size branch over a JSON-LD numberOfEmployees.
func employeeCount(v any) (string, bool) {
	var count int
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			count = int(n)
		} else if f, err := t.Float64(); err == nil && !math.IsInf(f, 0) {
			count = int(f)
		}
	case bool:
		if t {
			count = 1
		}
	case string:
		s := strings.ReplaceAll(t, ",", "")
		if s == "" || strings.IndexFunc(s, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
			return "", false
		}
		n, ok := pyInt(s)
		if !ok {
			return "", false
		}
		count = n
	default:
		return "", false
	}
	if count != 0 && count <= 500_000 {
		return itoa(count), true
	}
	return "", false
}

func (p deepScraper) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	website := ls(in.Lead, "website")
	if website == "" {
		return fail(in, "No website URL available"), nil
	}
	if c == nil {
		return fail(in, errNoWeb), nil
	}
	maxPages, timeout := p.MaxPages, p.Timeout
	if maxPages <= 0 {
		maxPages = 8
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	base := normalizeWebsiteURL(website)
	domain := bareDomain(base, false)
	fields := providers.NewObj()
	setdefault := func(k string, v any) {
		if _, ok := fields.Get(k); !ok {
			fields.Set(k, v)
		}
	}
	var emails, phones []string
	social := map[string]string{}
	var team []teamMember
	st := providers.NewObj()
	var all strings.Builder
	fetched := 0
	for _, path := range dsPages {
		if fetched >= maxPages {
			break
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		status, ctype, body, err := guardedGet(pctx, c, base+path, deepHeaders)
		cancel()
		if cerr := ctx.Err(); cerr != nil {
			return providers.Output{}, cerr
		}
		if err != nil || status != 200 {
			continue
		}
		if !strings.Contains(ctype, "text/html") && !strings.Contains(ctype, "application/xhtml") {
			continue
		}
		fetched++
		doc := parseHTML(trunc(body, 300_000))
		pageText := getText(doc, " ", true)
		all.WriteString(" " + pageText)
		if path == "/" {
			sd := structured(doc)
			for _, k := range sd.Keys() {
				v, _ := sd.Get(k)
				st.Set(k, v)
			}
		}
		hrefs := anchorsWithHref(doc)
		for _, h := range hrefs {
			if strings.HasPrefix(h, "mailto:") {
				e := pyStrip(strings.SplitN(strings.ReplaceAll(h, "mailto:", ""), "?", 2)[0])
				if e != "" && strings.Contains(e, "@") {
					emails = append(emails, e)
				}
			} else if strings.HasPrefix(h, "tel:") {
				ph := dsTelKeep.ReplaceAllString(pyStrip(strings.ReplaceAll(h, "tel:", "")), "")
				if ph != "" && runeLen(digitsOnly(ph)) >= 7 {
					phones = append(phones, ph)
				}
			}
		}
		for _, e := range dsEmailRe.FindAllString(pageText, -1) {
			if !containsAny(strings.ToLower(e), dsEmailJunk) {
				emails = append(emails, e)
			}
		}
		keepPhone := func(m string) {
			m = pyStrip(m)
			if n := runeLen(digitsOnly(m)); n >= 7 && n <= 15 {
				phones = append(phones, m)
			}
		}
		for _, m := range dsPhone1.FindAllString(pageText, -1) {
			keepPhone(m)
		}
		for _, m := range phone10(pageText) {
			keepPhone(m)
		}
		for _, m := range dsPhone3.FindAllString(pageText, -1) {
			keepPhone(m)
		}
		for _, h := range hrefs {
			hl := strings.ToLower(h)
			key := ""
			switch {
			case strings.Contains(hl, "linkedin.com/company") || strings.Contains(hl, "linkedin.com/in/"):
				key = "linkedin"
			case (strings.Contains(hl, "twitter.com/") || strings.Contains(hl, "x.com/")) && !strings.Contains(hl, "/intent/"):
				key = "twitter"
			case strings.Contains(hl, "facebook.com/") && !strings.Contains(hl, "facebook.com/sharer"):
				key = "facebook"
			case strings.Contains(hl, "instagram.com/") && !strings.Contains(hl, "instagram.com/p/"):
				key = "instagram"
			}
			if _, has := social[key]; key != "" && !has {
				social[key] = h
			}
		}
		if containsAny(path, []string{"/team", "/leadership", "/people", "/management", "/about"}) {
			team = append(team, teamMembers(doc, pageText)...)
		}
		if path == "/" {
			if m := metaByAttr(doc, "name", "description"); m != nil {
				if c, _ := attr(m, "content"); c != "" {
					setdefault("description", trunc(c, 500))
				}
			}
			if m := metaByAttr(doc, "property", "og:description"); m != nil {
				if c, _ := attr(m, "content"); c != "" {
					setdefault("description", trunc(c, 500))
				}
			}
			if ind := industryFromMeta(doc); ind != "" {
				fields.Set("industry_tags", ind)
			}
		}
	}

	seenE := map[string]bool{}
	var uniqE []string
	for _, e := range emails {
		if l := strings.ToLower(e); !seenE[l] {
			seenE[l] = true
			uniqE = append(uniqE, l)
		}
	}
	seenP := map[string]bool{}
	var uniqP []string
	for _, ph := range phones {
		if !seenP[ph] {
			seenP[ph] = true
			uniqP = append(uniqP, ph)
		}
	}
	get := func(k string) any { v, _ := st.Get(k); return v }
	if truthy(get("email")) {
		fields.Set("email", get("email"))
	} else if len(uniqE) > 0 {
		ranked := append([]string(nil), uniqE...)
		sort.SliceStable(ranked, func(i, j int) bool { return dsEmailRank(ranked[i]) < dsEmailRank(ranked[j]) })
		fields.Set("email", strings.TrimRight(ranked[0], "."))
	}
	if truthy(get("phone")) {
		fields.Set("phone", get("phone"))
	} else if len(uniqP) > 0 {
		fields.Set("phone", uniqP[0])
	}
	if truthy(get("employee_count")) {
		if s, ok := employeeCount(get("employee_count")); ok {
			fields.Set("company_size", s)
		}
	} else if s := companySize(all.String()); s != "" {
		fields.Set("company_size", s)
	}
	if truthy(get("founding_year")) {
		fields.Set("founding_year", get("founding_year"))
	} else if y := foundingYear(all.String()); y != "" {
		fields.Set("founding_year", y)
	}
	if truthy(get("address")) {
		fields.Set("address", get("address"))
	}
	if truthy(get("description")) {
		setdefault("description", get("description"))
	}
	if truthy(get("industry")) {
		fields.Set("industry_tags", get("industry"))
	}
	for _, k := range []string{"linkedin", "twitter", "facebook", "instagram"} {
		if v, ok := st.Get(k); ok {
			social[k], _ = v.(string)
		}
	}
	if social["linkedin"] != "" {
		fields.Set("linkedin_url", social["linkedin"])
	}
	if social["twitter"] != "" {
		fields.Set("twitter_url", social["twitter"])
	}
	if social["facebook"] != "" {
		fields.Set("facebook_url", social["facebook"])
	}
	seenT := map[string]bool{}
	var uniqT []teamMember
	for _, m := range team {
		if l := strings.ToLower(m.name); !seenT[l] {
			seenT[l] = true
			uniqT = append(uniqT, m)
		}
	}
	if len(uniqT) > 0 {
		sort.SliceStable(uniqT, func(i, j int) bool { return dsTeamRank(uniqT[i].title) < dsTeamRank(uniqT[j].title) })
		list := []any{}
		for _, m := range uniqT[:min(5, len(uniqT))] {
			o := providers.NewObj()
			o.Set("name", m.name)
			o.Set("title", m.title)
			list = append(list, o)
		}
		fields.Set("decision_makers", pyDumps(list, true))
		fields.Set("contact_person", uniqT[0].name)
		fields.Set("contact_title", uniqT[0].title)
		if _, has := fields.Get("email"); domain != "" && !has {
			if parts := pySplit(uniqT[0].name); len(parts) >= 2 {
				first, last := strings.ToLower(pyStrip(parts[0])), strings.ToLower(pyStrip(parts[len(parts)-1]))
				if first != "" && last != "" {
					fields.Set("email", first+"."+last+"@"+domain)
					setdefault("email_confidence", "pattern_guess")
				}
			}
		}
	}
	if fields.Len() > 0 {
		return result(in, fields, 0.70, ""), nil
	}
	return fail(in, "No data extracted from "+itoa(fetched)+" pages"), nil
}
