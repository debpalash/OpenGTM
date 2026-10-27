package scrape

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// searchSeam returns the search seam or nil (the provider then answers errNoWeb).
func searchSeam() Search { return current().Search }

// webSearch is a web_search.DDGS(...).text call that swallows failures (the
// legacy wrappers caught every exception and the 10 s wait_for timeout).
func webSearch(ctx context.Context, s Search, q string, n int) []Hit {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hits, _ := s.Web(cctx, q, n, false)
	return hits
}

// ── social_finder_provider.py ──────────────────────────────────────────────

type socialFinder struct{ gated }

func (socialFinder) Name() string { return "social_finder" }

func (socialFinder) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "No company name available"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	f := providers.NewObj()
	if ls(in.Lead, "linkedin_url") == "" {
		for _, r := range s.Text(ctx, `site:linkedin.com/company "`+company+`"`, 2) {
			if strings.Contains(r.Href, "linkedin.com/company") {
				f.Set("linkedin_url", r.Href)
				break
			}
		}
	}
	if ls(in.Lead, "twitter_url") == "" {
		for _, r := range s.Text(ctx, `site:twitter.com OR site:x.com "`+company+`"`, 2) {
			if strings.Contains(r.Href, "twitter.com/") || strings.Contains(r.Href, "x.com/") {
				f.Set("twitter_url", r.Href)
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	if f.Len() > 0 {
		return result(in, f, 0.55, ""), nil
	}
	return fail(in, "No social profiles found"), nil
}

// ── ddg_company_provider.py ────────────────────────────────────────────────

type ddgCompany struct{ gated }

func (ddgCompany) Name() string { return "ddg_company" }

var (
	ddgCompanySkip   = []string{"justdial", "sulekha", "indiamart", "linkedin", "facebook", "twitter", "glassdoor", "ambitionbox", "wikipedia", "youtube"}
	ddgCompanyPhones = []*regexp.Regexp{
		pyRe(`\+?91[\-\s]?\d{5}[\-\s]?\d{5}`),
		pyRe(`\+?91[\-\s]?\d{10}`),
		pyRe(`1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}`),
		pyRe(`0\d{2,4}[\-\s]?\d{6,8}`),
	}
	snippetEmailRe = pyRe(`[\w\.\-]+@[\w\.\-]+\.\w{2,}`)
)

func (ddgCompany) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "No company name available"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	parts := []string{company}
	if city := ls(in.Lead, "city"); city != "" {
		parts = append(parts, city)
	}
	parts = append(parts, "official website", "contact")
	found := map[string]string{}
	for _, r := range s.Text(ctx, strings.Join(parts, " "), 5) {
		text := r.Title + " " + r.Body
		href := r.Href
		if _, has := found["website"]; !has && href != "" && strings.Contains(href, "http") {
			_, netloc, _ := urlSplit(href)
			if !containsAny(strings.ToLower(netloc), ddgCompanySkip) {
				found["website"] = href
			}
		}
		if _, has := found["phone"]; !has {
			for _, re := range ddgCompanyPhones {
				if m := re.FindString(text); m != "" {
					found["phone"] = pyStrip(m)
					break
				}
			}
		}
		if _, has := found["email"]; !has {
			for _, e := range snippetEmailRe.FindAllString(text, -1) {
				if !containsAny(strings.ToLower(e), []string{"example", "domain.com", ".png"}) {
					found["email"] = e
					break
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	f := providers.NewObj()
	for _, k := range []string{"website", "phone", "email"} {
		if v := found[k]; v != "" && ls(in.Lead, k) == "" {
			f.Set(k, v)
		}
	}
	if f.Len() > 0 {
		return result(in, f, 0.5, ""), nil
	}
	return fail(in, "No new data found via search"), nil
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// ── facebook_pages.py ──────────────────────────────────────────────────────

type facebookPages struct{ gated }

func (facebookPages) Name() string { return "facebook_pages" }

var (
	fbPhoneRe = pyRe(`(?:\+91[\s\-]?)?(?:\d{2,4}[\s\-]?\d{6,8}|\d{10,12})`)
	fbSepRe   = pyRe(`[\s\-]`)
	fbAddrRe  = pyRe(`(?i)(?:located\s+(?:at|in)|address|located)\s*[:.]?\s*(.{20,80})`)
)

func (facebookPages) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "no_company_name"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	q := `site:facebook.com "` + company + `"`
	if city := ls(in.Lead, "city"); city != "" {
		q += ` "` + city + `"`
	}
	q += " business"
	var data *providers.Obj
	for _, r := range s.Text(ctx, q, 5) {
		hl := strings.ToLower(r.Href)
		if !strings.Contains(hl, "facebook.com/") || containsAny(hl, []string{"/groups/", "/events/", "/marketplace/"}) {
			continue
		}
		combined := r.Title + " " + r.Body
		d := providers.NewObj()
		if ph := fbPhoneRe.FindAllString(combined, -1); len(ph) > 0 {
			if p := fbSepRe.ReplaceAllString(ph[0], ""); runeLen(p) >= 10 {
				d.Set("phone", p)
			}
		}
		if em := snippetEmailRe.FindAllString(combined, -1); len(em) > 0 {
			e := strings.ToLower(em[0])
			if !containsAny(e, []string{"facebook", "example", "domain"}) {
				d.Set("email", e)
			}
		}
		if m := fbAddrRe.FindStringSubmatch(combined); m != nil {
			d.Set("address", strings.TrimRight(pyStrip(m[1]), "."))
		}
		if d.Len() > 0 {
			data = d
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	if data == nil {
		return fail(in, "no_facebook_page"), nil
	}
	f := providers.NewObj()
	for _, k := range []string{"phone", "email", "address"} {
		if v, ok := data.Get(k); ok && v != "" {
			f.Set(k, v)
		}
	}
	if f.Len() == 0 {
		return fail(in, "no_contact_on_page"), nil
	}
	return result(in, f, 0.5, ""), nil
}

// ── local_business.py ──────────────────────────────────────────────────────

type localBusiness struct{ gated }

func (localBusiness) Name() string { return "local_business" }

var (
	lbPhone1   = pyRe(`\+?\d{1,3}[\s\-.]?\(?\d{2,4}\)?[\s\-.]?\d{3,4}[\s\-.]?\d{3,4}`)
	lbPhone3   = pyRe(`1800[\s\-.]?\d{2,3}[\s\-.]?\d{4,6}`)
	lbAddrUS   = pyRe(`(?i)\d+\s+[\w\s]+(?:St|Ave|Rd|Dr|Blvd|Ln|Way|Ct|Pl|Terr)[.\s,]+[\w\s]+,\s*[A-Z]{2}\s*\d{5}`)
	lbAddrTag  = pyRe(`(?i)(?:Address|Location|HQ|Office)[:\s]+([^|<\n]{10,120})`)
	lbRatingRe = pyRe(`(?i)(\d\.?\d?)\s*(?:/5|stars?|out of 5|rating)`)
	anyDigitRe = pyRe(`\d`)
)

func lbPhone(text string) string {
	keep := func(m string) bool {
		n := runeLen(digitsOnly(pyStrip(m)))
		return n >= 7 && n <= 15
	}
	if m := lbPhone1.FindString(text); m != "" && keep(m) {
		return pyStrip(m)
	}
	if ms := phone10(text); len(ms) > 0 && keep(ms[0]) {
		return pyStrip(ms[0])
	}
	if m := lbPhone3.FindString(text); m != "" && keep(m) {
		return pyStrip(m)
	}
	return ""
}

func lbAddress(text string) string {
	valid := func(a string) bool { n := runeLen(a); return n > 10 && n < 200 && anyDigitRe.MatchString(a) }
	if m := lbAddrUS.FindString(text); m != "" {
		if a := pyStrip(m); valid(a) {
			return a
		}
	}
	if m := lbAddrTag.FindStringSubmatch(text); m != nil {
		if a := pyStrip(m[1]); valid(a) {
			return a
		}
	}
	return ""
}

func lbRating(text string) string {
	m := lbRatingRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	r, ok := pyFloat(m[1])
	if !ok || r < 1 || r > 5 {
		return ""
	}
	return floatRepr(r) + "/5"
}

func (localBusiness) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "No company name available"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	q := `"` + company + `"`
	if city := ls(in.Lead, "city"); city != "" {
		q += ` "` + city + `"`
	}
	f := providers.NewObj()
	for _, r := range webSearch(ctx, s, q+" phone address contact", 8) {
		text := r.Title + " " + r.Body
		if _, has := f.Get("phone"); !has {
			if p := lbPhone(text); p != "" && runeLen(digitsOnly(p)) >= 10 {
				f.Set("phone", p)
			}
		}
		if _, has := f.Get("address"); !has {
			if a := lbAddress(text); a != "" {
				f.Set("address", a)
			}
		}
		if _, has := f.Get("google_rating"); !has {
			if g := lbRating(text); g != "" {
				f.Set("google_rating", g)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	if f.Len() > 0 {
		return result(in, f, 0.65, ""), nil
	}
	return fail(in, "No local business data found"), nil
}

// ── company_intel.py ───────────────────────────────────────────────────────

type companyIntel struct{ gated }

func (companyIntel) Name() string { return "company_intel" }

var (
	ciFunding = []*regexp.Regexp{
		pyRe(`(?i)(?:raised|secured|closed|announced|received)\s*\$?([\d.]+)\s*(million|billion|M|B|mn|bn)`),
		pyRe(`(?i)\$([\d.]+)\s*(million|billion|M|B|mn|bn)\s*(?:in\s+)?(?:funding|round|investment|capital)`),
		pyRe(`(?i)(?:Series\s+[A-F]|Seed|Pre-Seed|Bridge)\s*(?:round)?[^$]*\$([\d.]+)\s*(million|billion|M|B|mn|bn)`),
	}
	ciStages = []struct {
		re  *regexp.Regexp
		fmt func(m []string) string
	}{
		{pyRe(`(?i)(?:Series\s+([A-F]))`), func(m []string) string { return "Series " + m[1] }},
		{pyRe(`(?i)(?:Seed\s+round|seed\s+funding)`), func([]string) string { return "Seed" }},
		{pyRe(`(?i)(?:Pre-Seed)`), func([]string) string { return "Pre-Seed" }},
		{pyRe(`(?i)(?:IPO|went\s+public)`), func([]string) string { return "IPO" }},
		{pyRe(`(?i)(?:Series\s+([A-F])\+?)`), func(m []string) string { return "Series " + m[1] }},
	}
	ciInvestors = pyRe(`(?i)(?:backed by|led by|investors?(?:\s+include)?|participated)\s+([A-Z][\w\s,&]+?)(?:\.|,\s*and|\s*$)`)
	ciRevenue   = pyRe(`(?i)(?:revenue|ARR|annual\s+recurring)\s*(?:of|reached|hit|crossed)?\s*\$?([\d.]+)\s*(million|billion|M|B|mn|bn)`)
	ciEmployees = pyRe(`(?i)(\d[\d,]+)\+?\s*(?:employees|team\s+members|people|staff)`)
)

func ciMillions(num, unit string) (string, bool) {
	a, ok := pyFloat(num)
	if !ok {
		return "", false // deviation: Python raised ValueError on "1.2.3" and failed the whole call
	}
	switch strings.ToLower(unit) {
	case "billion", "b", "bn":
		a *= 1000
	}
	return "$" + floatRepr(a) + "M", true
}

// ciExtract is _extract_funding_from_text (keys in Python's insertion order).
func ciExtract(text string) *providers.Obj {
	info := providers.NewObj()
	for _, re := range ciFunding {
		if m := re.FindStringSubmatch(text); m != nil {
			if v, ok := ciMillions(m[1], m[2]); ok {
				info.Set("last_funding_amount", v)
				break
			}
		}
	}
	for _, st := range ciStages {
		if m := st.re.FindStringSubmatch(text); m != nil {
			info.Set("funding_stage", st.fmt(m))
			break
		}
	}
	if m := ciInvestors.FindStringSubmatch(text); m != nil {
		if inv := pyStrip(m[1]); runeLen(inv) < 200 {
			info.Set("investors", inv)
		}
	}
	if m := ciRevenue.FindStringSubmatch(text); m != nil {
		if v, ok := ciMillions(m[1], m[2]); ok {
			info.Set("revenue_estimate", v)
		}
	}
	if m := ciEmployees.FindStringSubmatch(text); m != nil {
		count := strings.ReplaceAll(m[1], ",", "")
		if n, ok := pyInt(count); ok && n >= 1 && n <= 1_000_000 {
			info.Set("company_size", count)
		}
	}
	return info
}

func (companyIntel) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "No company name available"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	lower := strings.ToLower(company)
	mentions := func(r Hit) bool {
		return strings.Contains(strings.ToLower(r.Title), lower) || strings.Contains(strings.ToLower(r.Body), lower)
	}
	var all strings.Builder
	for _, r := range webSearch(ctx, s, `site:crunchbase.com "`+company+`"`, 3) {
		if mentions(r) {
			all.WriteString(" " + r.Title + " " + r.Body)
		}
	}
	if err := sleep(ctx, 300*time.Millisecond); err != nil {
		return providers.Output{}, err
	}
	funding := webSearch(ctx, s, `"`+company+`" funding OR raised OR series OR news`, 8)
	for _, r := range funding {
		if mentions(r) {
			all.WriteString(" " + r.Title + " " + r.Body)
		}
	}
	var news []string
	for _, r := range funding {
		if !mentions(r) {
			continue
		}
		h := trunc(r.Title, 120)
		if h != "" && !contains(news, h) {
			news = append(news, h)
		}
	}
	if len(news) > 5 {
		news = news[:5]
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	f := providers.NewObj()
	info := ciExtract(all.String())
	for _, k := range info.Keys() {
		if v, _ := info.Get(k); v != "" {
			f.Set(k, v)
		}
	}
	if len(news) > 0 {
		if len(news) > 3 {
			news = news[:3]
		}
		f.Set("recent_news", strings.Join(news, " | "))
	}
	if f.Len() > 0 {
		return result(in, f, 0.68, ""), nil
	}
	return fail(in, "No company intelligence found"), nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ── email_harvester.py ─────────────────────────────────────────────────────

type emailHarvester struct{ gated }

func (emailHarvester) Name() string { return "email_harvester" }

var (
	ehEmailRe       = pyRe(`(?i)[\w.\-+]+@[\w.\-]+\.(?:com|org|net|io|co|in|ai|dev|tech|biz|info|us|uk|eu|de|fr|ca|au)`)
	ehNameRe        = regexp.MustCompile(`([A-Z][a-z]+ [A-Z][a-z]+)`)
	ehBlacklistLoc  = []string{"noreply", "no-reply", "mailer-daemon", "postmaster", "abuse", "webmaster", "hostmaster", "admin@example"}
	ehBlacklistDoms = map[string]bool{"example.com": true, "domain.com": true, "email.com": true, "test.com": true,
		"sentry.io": true, "w3.org": true, "schema.org": true, "googleapis.com": true, "cloudflare.com": true,
		"wordpress.org": true, "github.com": true, "gravatar.com": true, "facebook.com": true, "twitter.com": true}
)

func ehValid(email string) bool {
	e := pyStrip(strings.ToLower(email))
	local, domain, found := strings.Cut(e, "@")
	if !found || local == "" || domain == "" {
		return false
	}
	if n := runeLen(local); n < 2 || n > 64 {
		return false
	}
	if ehBlacklistDoms[domain] || containsAny(local, ehBlacklistLoc) {
		return false
	}
	return !containsAny(e, []string{".png", ".jpg", ".css", ".js", ".svg"})
}

func ehRank(email, target string) int {
	parts := strings.Split(email, "@")
	local := strings.ToLower(parts[0])
	domain := ""
	if len(parts) > 1 {
		domain = strings.ToLower(parts[1])
	}
	if target == "" || !strings.Contains(domain, target) {
		return 10
	}
	switch {
	case strings.Contains(local, ".") && isAlpha(trunc(local, 1)):
		return 0
	case contains([]string{"ceo", "founder", "director", "cto", "coo"}, local):
		return 1
	case contains([]string{"info", "contact", "hello", "sales"}, local):
		return 2
	case contains([]string{"support", "help", "admin", "office"}, local):
		return 3
	}
	return 4
}

func (emailHarvester) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	domain := bareDomain(ls(in.Lead, "website"), true)
	if domain == "" {
		return fail(in, "No website/domain available"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	all := map[string]bool{}
	type named struct{ name, email string }
	var names []named
	for _, q := range []string{`site:` + domain + ` email OR contact OR "@` + domain + `"`, `"@` + domain + `" -site:` + domain} {
		for _, r := range webSearch(ctx, s, q, 8) {
			emails := ehEmailRe.FindAllString(r.Title+" "+r.Body, -1)
			for _, e := range emails {
				if ehValid(e) {
					all[strings.TrimRight(strings.ToLower(e), ".")] = true
				}
			}
			for _, e := range emails {
				if !ehValid(e) {
					continue
				}
				prefix := ""
				if i := strings.Index(r.Body, e); i >= 0 {
					prefix = r.Body[:i]
				}
				if m := ehNameRe.FindString(prefix); m != "" {
					names = append(names, named{m, strings.ToLower(e)})
				}
			}
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return providers.Output{}, err
		}
	}
	emails := make([]string, 0, len(all))
	for e := range all {
		emails = append(emails, e)
	}
	// Python sorted a set by rank only (ties in hash order); ties are
	// alphabetical here (deviation: deterministic).
	sort.Slice(emails, func(i, j int) bool {
		ri, rj := ehRank(emails[i], domain), ehRank(emails[j], domain)
		if ri != rj {
			return ri < rj
		}
		return emails[i] < emails[j]
	})
	if len(emails) == 0 {
		return fail(in, "No emails found across search results"), nil
	}
	f := providers.NewObj()
	f.Set("email", emails[0])
	if len(emails) > 1 {
		end := min(4, len(emails))
		f.Set("secondary_emails", strings.Join(emails[1:end], " | "))
	}
	if len(names) > 5 {
		names = names[:5]
	}
	for _, n := range names {
		if n.email == emails[0] {
			f.Set("contact_person", n.name)
			break
		}
	}
	return result(in, f, 0.72, ""), nil
}
