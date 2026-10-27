package collect

import (
	"strings"
	"unicode"
)

// The lead quality gate (lead_validator.py).

var articlePatterns = rxAll(
	`(?i)\btop\s+\d+`,
	`(?i)\bbest\s+\d+`,
	`(?i)\b\d+\s+best\b`,
	`(?i)\bguide\b`,
	`(?i)\bupdated\s+20\d{2}`,
	`(?i)\b20(2[3-9]|3\d)\b`,
	`(?i)\bhow\s+to\b`,
	`(?i)\bwhat\s+(is|are)\b`,
	`(?i)\bdiscover\s+the\b`,
	`(?i)\bcomplete\s+(list|guide)\b`,
	`(?i)\b(list|ranking|review)\s+of\b`,
	`(?i)\b(companies|businesses|websites|customers)\s+that\s+(use|uses|using)\b`,
	`(?i)\bwebsites\s+using\b`,
	`(?i)\b(customers|clients)\s+of\b`,
	`(?i)\b(working|work)\s+at\b`,
	`(?i)\b(jobs?|careers?|salaries)\s+(at|in|for)\b`,
)

var placeholderNames = setOf("results", "home", "about", "contact", "contact us", "search results", "page not found",
	"404", "untitled", "no title", "loading", "error", "recruitment firm", "staffing agency", "hr agency",
	"it company", "consulting firm", "human resources", "staffing", "recruitment", "consulting", "outsourcing",
	"manpower", "placement", "headhunting", "it services", "hr services", "hr solutions", "it solutions",
	"software development", "web development", "digital marketing", "business consulting", "management consulting")

var publisherNames = func() map[string]bool {
	out := map[string]bool{}
	for _, n := range []string{"clutch", "goodfirms", "g2", "capterra", "softwaresuggest", "ambitionbox", "glassdoor",
		"indeed", "naukri", "shine", "linkedin", "twitter", "facebook", "instagram", "youtube", "wikipedia", "quora",
		"reddit", "medium", "trustpilot", "geeksforgeeks", "justdial", "sulekha", "indiamart", "built in",
		"builtinhyderabad", "techbehemoths", "consultingcase101", "mordorintelligence", "placementindia",
		"placement india", "timesjobs", "monster", "foundit", "apna", "upwork", "freelancer", "fiverr", "mouthshut",
		"shiksha", "careers360", "collegedunia", "the manifest", "manifest", "aeroleads", "lusha", "apollo",
		"zoominfo", "hunter", "snov", "rocketreach", "clearbit", "freelistingindia", "free listing india",
		"fundoodata", "grotal", "tradeindia", "exportersindia", "dial4trade", "urbanpro", "bark", "thumbtack", "yelp",
		"yellowpages", "manta", "crunchbase", "owler", "tracxn", "wellfound", "angellist", "f6s", "startupindia",
		"yourstory", "theirstack", "builtwith", "bayt", "economic times", "times of india", "the times of india"} {
		out[strings.ReplaceAll(n, " ", "")] = true
	}
	return out
}()

var genericTitlePatterns = rxAll(
	`(?i)^(IT|HR|Software|Staffing|Recruitment|Consulting|Consultancy)\s+(Services?|Agency|Firms?)\s+(in|of)\s+`,
	`(?i)^(Best|Top)\s+.*(in|of|for)\s+\w+$`,
	`(?i)\bcompanies?\s+in\s+\w+`,
	`(?i)\b(staffing|recruitment|consulting)\s+\&?\s*(recruitment|staffing)?\s+agency\s+in\b`,
	`(?i)^(ERP|CRM|SAP|IT)\s+\w+\s+(In|Of)\s+`,
	`(?i)^find\s+`,
	`(?i)^looking\s+for\s+`,
	`(?i)^search\s+for\s+`,
	`(?i)^get\s+(quotes?|details?)\s+`,
	`(?i)^hire\s+`,
	`(?i)^browse\s+`,
	`(?i)^explore\s+`,
	`(?i)\bfree\s+classifieds?\b`,
	`(?i)\bads?\s+(in|buy|sell)\b`,
	`(?i)^(all|list of)\s+`,
	`(?i)^page\s+\d+`,
	`(?i)^multi\s+(recruit|search|find)\b`,
	`(?i)\b(verified|trusted)\s+(list|suppliers?|vendors?)\b`,
	`(?i)\b\d+\s+\w+\s*(companies|agencies|firms|suppliers|vendors|based)\b`,
	`(?i)^(placement|recruiting|staffing)\s+(services?|agencies?|companies?)\s+(in|near|at)\s+`,
	`(?i)\b(directory|listing|catalogue|catalog)\b`,
	`(?i)^(HR|IT)\s+(consulting|staffing|outsourcing)\s+(service|firm|company|agency)\s+\w+$`,
	`(?i)^(HR|IT)\s+(staffing)\s+(\&|and)\s+(recruiting|recruitment)\s+`,
	`(?i)^top\s+.*(firms?|agencies?|companies?)\s+(in|of|for)\s+`,
	`(?i)\.com/`,
	`(?i)^(warehouse|construction|manufacturing)\s+(staffing|recruitment)\s+(agency|company|service)\s+(in|of)\s+`,
	`(?i)\bcompanies/agencies\b`,
	"(?i)^(HR|IT|BPO|KPO|RPO)\\s+(outsourcing|offshoring)\\s*[-–—]",
	`(?i)^(HR|IT|BPO|KPO)\s+(consulting|staffing|outsourcing|recruitment|solutions?)\s+(service|india|company|agency)s?$`,
	`(?i)^\w+\s+(staffing|recruitment|consulting|outsourcing)\s+(agency|service|firm|company)\s+(in|near|for)\s+`,
	`(?i)^(need|want|searching|seeking|require)\s+`,
	`(?i)^(compare|check|verify|view|see|read|know)\s+`,
	`(?i)\b(reviews?|ratings?|testimonials?)\s+(of|for|on)\b`,
	`(?i)^(next|prev|previous|back|home|menu|footer|header|sidebar)$`,
	`(?i)^(show|view|see)\s+(more|all|less)`,
	`(?i)^[a-z]+\.(com|co\.in|org|net|in)(/|$)`,
	`(?i)^(staffing|recruitment|consulting|consultancy|outsourcing|placement|manpower|headhunting)\s+(in|at|near|for)\s+`,
	`(?i)^(HR|IT)\s+.+\s+(in|at|near)\s+[A-Z]`,
	`(?i)\b(staffing|recruitment|consulting|consultancy|outsourcing|placement|manpower)\s+(services?\s+)?(in|at|near)\s+[A-Z]`,
	`(?i)\b\w+\s*[&]\s*.+\s+(in|at|near)\s+[A-Z]\w+$`,
	`[|:;]\s*$`,
	`(?i)\b(manufacturers?|suppliers?|wholesalers?|dealers?|distributors?|exporters?|importers?)\b`,
	`(?i)\bservices?\s+in\s+\w+\s+\w+\s+(manufacturers?|suppliers?)\b`,
	`(?i)\b(staffing|recruitment|consulting)\s+(agency\s+)?companies$`,
)

var publisherDomains = []string{"softwaresuggest.com", "goodfirms.co", "clutch.co", "ambitionbox.com", "glassdoor.com",
	"glassdoor.co.in", "indeed.com", "naukri.com", "linkedin.com", "twitter.com", "facebook.com", "instagram.com",
	"youtube.com", "wikipedia.org", "quora.com", "reddit.com", "medium.com", "mordorintelligence.com",
	"rankexdigital.com", "g2.com", "capterra.com", "trustpilot.com", "bing.com", "google.com", "google.co.in",
	"microsoft.com", "yahoo.com", "duckduckgo.com", "baidu.com", "yandex.com", "theirstack.com", "builtwith.com",
	"bayt.com", "indiatimes.com", "timesofindia.com", "economictimes.com", "themanifest.com", "techbehemoths.com",
	"placementindia.com", "timesjobs.com", "monster.com", "foundit.in", "apna.co"}

var jobTitleTerms = setOf("executive", "manager", "director", "officer", "president", "founder", "cofounder", "ceo",
	"cto", "coo", "cfo", "cmo", "cio", "vp", "svp", "evp", "head", "lead", "recruiter", "consultant", "analyst",
	"engineer", "developer", "designer", "intern", "associate", "specialist", "coordinator", "administrator",
	"representative", "agent", "principal", "supervisor", "strategist", "architect")

var searchEmailBrands = setOf("bing", "google", "microsoft", "yahoo", "outlook", "gmail", "hotmail", "duckduckgo",
	"baidu", "yandex", "rediffmail")

var knownEmailProviders = setOf("gmail.com", "yahoo.com", "hotmail.com", "outlook.com", "rediffmail.com",
	"ymail.com", "live.com", "aol.com")

var (
	nonIndiaPrefixes = []string{"+1", "+44", "+61", "+49", "+33", "+971"}
	assetExts        = []string{".png", ".jpg", ".svg", ".gif", ".webp", ".css", ".js", ".woff"}
)

func rxAll(ps ...string) []pyRx {
	out := make([]pyRx, len(ps))
	for i, p := range ps {
		out[i] = mustRx(p)
	}
	return out
}

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// nameRejection is _name_rejection: pure job titles and search/email brands.
func nameRejection(name string) string {
	low := pyStrip(pyLower(name))
	words := strings.FieldsFunc(low, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	if len(words) == 0 {
		return ""
	}
	if len(words) <= 3 && jobTitleTerms[words[len(words)-1]] {
		return "job_title_not_company"
	}
	if searchEmailBrands[strings.ReplaceAll(low, " ", "")] {
		return "search_or_email_brand"
	}
	return ""
}

func isPublisherDomain(domain string) bool {
	clean := strings.Trim(pyLower(domain), ".")
	for _, b := range publisherDomains {
		if clean == b || strings.HasSuffix(clean, "."+b) {
			return true
		}
	}
	return false
}

// nameVerdict is the name checks shared by validate_lead and validate_lead_light.
func nameVerdict(company string) string {
	name := pyStrip(company)
	switch n := runeLen(name); {
	case name == "":
		return "empty_name"
	case n < 2:
		return "name_too_short"
	case n > 60:
		return "name_too_long_likely_headline"
	}
	low := pyLower(name)
	if placeholderNames[low] {
		return "placeholder_name"
	}
	if publisherNames[strings.ReplaceAll(low, " ", "")] {
		return "publisher_name"
	}
	for _, p := range articlePatterns {
		if p.Match(name) {
			return "article_title_pattern"
		}
	}
	for _, p := range genericTitlePatterns {
		if p.Match(name) {
			return "generic_service_title"
		}
	}
	if nr := nameRejection(name); nr != "" {
		return nr
	}
	alpha := 0
	for _, r := range name {
		if isPyAlpha(r) {
			alpha++
		}
	}
	if alpha < 3 {
		return "insufficient_alpha_chars"
	}
	if words := pySplit(name); len(words) >= 5 {
		all := true
		for _, w := range words {
			if !pyIsAlphaStr(w) {
				continue
			}
			if !unicode.IsUpper([]rune(w)[0]) { // str.isupper() of the first character
				all = false
				break
			}
		}
		if all && runeLen(name) > 50 {
			return "likely_headline"
		}
	}
	return ""
}

func emailDomain(email string) string {
	if !strings.Contains(email, "@") {
		return ""
	}
	return pyLower(email[strings.LastIndex(email, "@")+1:])
}

func hasAssetExt(email string) bool {
	low := pyLower(email)
	for _, ext := range assetExts {
		if strings.Contains(low, ext) {
			return true
		}
	}
	return false
}

// extractDomain is lead_validator._extract_domain.
func extractDomain(u string) string {
	d := pyLower(urlparse(u).Netloc)
	return strings.TrimPrefix(d, "www.")
}

// NormalizeURL is _normalize_url / website_scraper.normalize_website_url: scheme://netloc.
func NormalizeURL(u string) string {
	if u == "" {
		return u
	}
	u = pyStrip(u)
	if !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	p := urlparse(u)
	return p.Scheme + "://" + p.Netloc
}

func cleanPhone(l *Lead, rejectForeign bool) {
	if l.Phone == "" {
		return
	}
	phone := pyStrip(l.Phone)
	switch phone {
	case "1234567890", "0000000000", "9999999999":
		l.Phone = ""
	}
	if rejectForeign {
		for _, p := range nonIndiaPrefixes {
			if strings.HasPrefix(phone, p) {
				l.Phone = ""
				break
			}
		}
	}
	digits := 0
	for _, r := range phone {
		if isPyDigitRune(r) {
			digits++
		}
	}
	if digits > 12 {
		l.Phone = ""
	}
}

func isPyDigitRune(r rune) bool { return unicode.IsDigit(r) }

func cleanWebsite(l *Lead) {
	if l.Website == "" {
		return
	}
	l.Website = NormalizeURL(l.Website)
	if isPublisherDomain(extractDomain(l.Website)) {
		l.Website = ""
	}
}

// ValidateLead is validate_lead: the name gate, then contact cleaning (publisher / asset /
// unrelated-domain emails, fake / foreign / over-long phones, publisher websites; the website is
// reduced to scheme://host), then at least one of website, email, phone or LinkedIn. It mutates l.
func ValidateLead(l *Lead) (bool, string) {
	if r := nameVerdict(l.Company); r != "" {
		return false, r
	}
	if l.Email != "" {
		dom := emailDomain(l.Email)
		if isPublisherDomain(dom) {
			l.Email = ""
		}
		if hasAssetExt(l.Email) {
			l.Email = ""
		}
		if l.Email != "" && l.Website != "" {
			wd := extractDomain(l.Website)
			if dom != "" && wd != "" && !strings.Contains(wd, dom) && !strings.Contains(dom, wd) && !knownEmailProviders[dom] {
				l.Email, l.EmailConfidence, l.EmailProvider = "", "", ""
			}
		}
	}
	cleanPhone(l, true)
	cleanWebsite(l)
	present := func(s string) bool { return s != "" && pyStrip(s) != "" && s != "N/A" }
	if !present(l.Website) && !present(l.Email) && !present(l.Phone) && !present(l.LinkedinURL) {
		return false, "no_contact_data"
	}
	return true, ""
}

// ValidateLeadLight is validate_lead_light: the name gate and contact cleaning, without the
// contact-data requirement, the foreign-phone rule or the email/website domain cross-check.
func ValidateLeadLight(l *Lead) (bool, string) {
	if r := nameVerdict(l.Company); r != "" {
		return false, r
	}
	if l.Email != "" {
		if isPublisherDomain(emailDomain(l.Email)) {
			l.Email = ""
		}
		if hasAssetExt(l.Email) {
			l.Email = ""
		}
	}
	cleanPhone(l, false)
	cleanWebsite(l)
	return true, ""
}

// Rejected is a lead and the reason the validator gave.
type Rejected struct {
	Lead   *Lead
	Reason string
}

// ValidateAll is validate_and_clean_leads (full) or validate_and_clean_leads_light.
func ValidateAll(leads []*Lead, light bool) (valid []*Lead, rejected []Rejected) {
	for _, l := range leads {
		var ok bool
		var reason string
		if light {
			ok, reason = ValidateLeadLight(l)
		} else {
			ok, reason = ValidateLead(l)
		}
		if ok {
			valid = append(valid, l)
		} else {
			rejected = append(rejected, Rejected{l, reason})
		}
	}
	return valid, rejected
}
