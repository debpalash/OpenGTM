package leadweb

import (
	"regexp"
	"strings"
)

// ── job_runner._extract_emails_from_html / _extract_phones_from_html ──

var (
	runnerEmailRe       = pyRe(`[\w.\-+]+@[\w.\-]+\.\w{2,}`)
	runnerPhonePatterns = compileAll(
		`\+?91[\-\s]?\d{5}[\-\s]?\d{5}`,
		`\+?91[\-\s]?\d{10}`,
		`1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}`,
		`0\d{2,4}[\-\s]?\d{6,8}`,
	)
	runnerTelRe = pyRe(`tel:([+\d\-\s]{7,15})`)

	runnerSkipEmailDomains = set("example.com", "domain.com", "email.com", "sentry.io", "wixpress.com", "w3.org",
		"schema.org", "googleapis.com", "gravatar.com", "wordpress.org", "cloudflare.com")
	runnerSkipEmailExt = []string{".png", ".jpg", ".svg", ".gif", ".css", ".js", ".woff", ".webp"}
	publisherDomains   = set("softwaresuggest.com", "goodfirms.co", "clutch.co", "g2.com", "capterra.com",
		"ambitionbox.com", "glassdoor.com", "mordorintelligence.com", "rankexdigital.com", "trustpilot.com")
)

func compileAll(ps ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(ps))
	for i, p := range ps {
		out[i] = pyRe(p)
	}
	return out
}

func set(xs ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}

// EmailsFromHTML is the legacy job_runner._extract_emails_from_html: regex
// over the raw HTML, junk domains/asset names and publisher addresses dropped,
// deduplicated case-insensitively (first spelling kept), at most five.
func EmailsFromHTML(html string) []string {
	var good []string
	seen := map[string]struct{}{}
	for _, e := range runnerEmailRe.FindAllString(html, -1) {
		low := strings.ToLower(e)
		domain := low[strings.LastIndex(low, "@")+1:]
		if _, skip := runnerSkipEmailDomains[domain]; skip {
			continue
		}
		if hasAnySuffix(low, runnerSkipEmailExt) {
			continue
		}
		if IsPublisherEmail(e) {
			continue
		}
		if _, ok := seen[low]; !ok {
			seen[low] = struct{}{}
			good = append(good, e)
		}
	}
	return head(good, 5)
}

// PhonesFromHTML is the legacy job_runner._extract_phones_from_html (Indian
// formats plus tel: links), deduplicated by digits, at most five.
func PhonesFromHTML(html string) []string {
	var phones []string
	seen := map[string]struct{}{}
	add := func(phone string) {
		phone = strings.ReplaceAll(pyStrip(phone), "tel:", "")
		digits := pyDigits(phone)
		n := runeLen(digits)
		if n < 7 || n > 13 {
			return
		}
		if _, dup := seen[digits]; dup {
			return
		}
		if !IsValidPhone(phone) {
			return
		}
		seen[digits] = struct{}{}
		phones = append(phones, phone)
	}
	for _, re := range runnerPhonePatterns {
		for _, m := range re.FindAllString(html, -1) {
			add(m)
		}
	}
	for _, m := range runnerTelRe.FindAllStringSubmatch(html, -1) {
		add(m[1])
	}
	return head(phones, 5)
}

// IsPublisherEmail is job_runner._is_publisher_email.
func IsPublisherEmail(email string) bool {
	i := strings.LastIndex(email, "@")
	if i < 0 {
		return true
	}
	_, ok := publisherDomains[strings.ToLower(email[i+1:])]
	return ok
}

var foreignPrefixes = []string{"+1", "+44", "+61", "+49", "+33"}

// IsValidPhone is job_runner._is_valid_phone (India-focused).
func IsValidPhone(phone string) bool {
	var b strings.Builder
	for _, r := range phone {
		if r == '+' || isPyDigit(r) {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	for _, p := range foreignPrefixes {
		if strings.HasPrefix(clean, p) {
			return false
		}
	}
	switch clean {
	case "1234567890", "0000000000", "9999999999":
		return false
	}
	n := runeLen(pyDigits(clean))
	return n >= 7 && n <= 13
}

// ── leadgen/http.py FetchResult.extract_emails / extract_phones ──

var (
	fetchEmailRe     = pyRe(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,24}`)
	fetchEmailJunk   = []string{"example.com", "domain.com", ".png", ".jpg", ".css", ".js", "sentry.io", "wixpress", "cloudflare"}
	fetchPhoneRes    = compileAll(`\+?91[\-\s]?\d{5}[\-\s]?\d{5}`, `\+?91[\-\s]?\d{10}`, `1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}`)
	fetchPhoneMaxOut = 10
)

// FetchEmails is FetchResult.extract_emails. Deviation: legacy returned
// list(set(...)), whose order is hash-random; this keeps first-occurrence order.
func FetchEmails(text string) []string {
	var out []string
	for _, e := range uniq(fetchEmailRe.FindAllString(text, -1)) {
		if !containsAny(strings.ToLower(e), fetchEmailJunk) {
			out = append(out, e)
		}
	}
	return out
}

// FetchPhones is FetchResult.extract_phones (at most ten).
func FetchPhones(text string) []string {
	var found []string
	keep := func(m string) {
		if n := runeLen(pyDigits(m)); n >= 7 && n <= 13 {
			found = append(found, pyStrip(m))
		}
	}
	for _, re := range fetchPhoneRes {
		for _, m := range re.FindAllString(text, -1) {
			keep(m)
		}
	}
	for _, m := range tenDigitWords(text) { // \b\d{10}\b
		keep(m)
	}
	return head(uniq(found), fetchPhoneMaxOut)
}

// ── challenge detection (leadgen/http.py _is_challenge) ──

var (
	challengeMarkers = []string{"Just a moment", "cf-challenge", "cf_clearance", "__cf_bm", "challenge-platform",
		"Checking your browser", "Attention Required", "Access Denied", "Please verify you are a human",
		"captcha-delivery.com", "geo.captcha-delivery.com"}
	jsRequiredMarkers = []string{"enable javascript", "please enable javascript", "javascript is required",
		"javascript is disabled", "you need to enable javascript", "<noscript>"}
	blockedStatus = map[int]bool{403: true, 429: true, 503: true, 520: true, 521: true, 522: true, 523: true, 524: true}
)

// tinyBodyThreshold: a 2xx page below this many characters is a JS shell.
const tinyBodyThreshold = 500

// IsChallenge reports a bot wall or JS-only shell that warrants escalating to
// a real browser (legacy _is_challenge).
func IsChallenge(status int, text string) bool {
	if blockedStatus[status] {
		return true
	}
	h := truncRunes(text, 5000)
	if containsAny(h, challengeMarkers) {
		return true
	}
	if containsAny(strings.ToLower(h), jsRequiredMarkers) {
		return true
	}
	n := runeLen(pyStrip(text))
	return status >= 200 && status < 300 && n > 0 && n < tinyBodyThreshold
}

// ── job_runner._is_junk_url / _is_skip_domain ──

var (
	junkExtensions = []string{".swf", ".pdf", ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".css", ".js", ".zip",
		".rar", ".mp4", ".mp3", ".xml", ".ico", ".woff"}
	junkSubstrings = []string{"doubleclick.net", "googlesyndication.com", "/cdn-cgi/", "/wp-content/uploads/",
		"media.", "/ads/", "adservice.", "/advertisement"}
	skipDomains = []string{
		"wikipedia.org", "youtube.com", "facebook.com", "twitter.com", "instagram.com", "reddit.com", "quora.com",
		"medium.com", "linkedin.com", "pinterest.com", "tiktok.com",
		"glassdoor.com", "glassdoor.co.in", "ambitionbox.com", "indeed.com", "naukri.com", "shine.com", "timesjobs.com",
		"monster.com", "foundit.in",
		"clutch.co", "goodfirms.co", "g2.com", "capterra.com", "softwaresuggest.com", "themanifest.com",
		"techbehemoths.com", "trustpilot.com", "mouthshut.com",
		"justdial.com", "sulekha.com", "indiamart.com", "placementindia.com", "tradeindia.com", "exportersindia.com",
		"grotal.com", "fundoodata.com", "freelistingindia.com", "urbanpro.com", "dial4trade.com",
		"aeroleads.com", "lusha.com", "apollo.io", "zoominfo.com", "rocketreach.co", "clearbit.com", "snov.io", "hunter.io",
		"crunchbase.com", "owler.com", "tracxn.com", "wellfound.com", "angellist.com", "yourstory.com",
		"mordorintelligence.com", "rankexdigital.com",
	}
)

// IsJunkURL is true for asset / ad / media URLs that are not company pages.
func IsJunkURL(href string) bool {
	if href == "" {
		return true
	}
	low := strings.ToLower(href)
	if hasAnySuffix(pyURLPath(low), junkExtensions) {
		return true
	}
	return containsAny(low, junkSubstrings)
}

// IsSkipDomain is true for aggregators, social networks and directories
// (substring match, as legacy).
func IsSkipDomain(domain string) bool { return containsAny(domain, skipDomains) }

// pyURLPath is urllib.parse.urlparse(u).path for the URLs seen here.
func pyURLPath(u string) string {
	s := u
	if i := strings.IndexAny(s, "#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexAny(s, "?"); i >= 0 {
		s = s[:i]
	}
	if scheme, rest, ok := splitScheme(s); ok && scheme != "" {
		s = rest
	}
	if strings.HasPrefix(s, "//") {
		s = s[2:]
		if i := strings.IndexByte(s, '/'); i >= 0 {
			s = s[i:]
		} else {
			s = ""
		}
	}
	if i := strings.LastIndexByte(s, ';'); i >= 0 && !strings.Contains(s[i:], "/") {
		s = s[:i] // urlparse splits ;params off the last segment
	}
	return s
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func hasAnySuffix(s string, sufs []string) bool {
	for _, x := range sufs {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}
