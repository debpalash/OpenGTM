package collect

import (
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// The JobRunner helpers (job_runner.py), byte-for-byte on the recorded vectors.

// targetCities is ICP["target_cities"] (leadgen/config.py), the first source of _extract_city.
var targetCities = []string{"Bangalore", "Mumbai", "Delhi", "Hyderabad", "Pune", "Chennai", "Noida", "Gurgaon",
	"Kolkata", "Ahmedabad"}

var fallbackCities = []struct{ key, val string }{
	{"bangalore", "Bangalore"}, {"mumbai", "Mumbai"}, {"delhi", "Delhi"}, {"hyderabad", "Hyderabad"},
	{"pune", "Pune"}, {"chennai", "Chennai"}, {"kolkata", "Kolkata"}, {"noida", "Noida"}, {"gurgaon", "Gurgaon"},
	{"ahmedabad", "Ahmedabad"}, {"bengaluru", "Bangalore"}, {"gurugram", "Gurgaon"},
}

var locationCities = []string{"bangalore", "mumbai", "delhi", "hyderabad", "pune", "chennai", "kolkata", "noida",
	"gurgaon", "ahmedabad", "bengaluru", "gurugram"}

// ExtractCity is _extract_city: the first ICP target city contained in the query (case-insensitive),
// else the fallback map, else "".
func ExtractCity(query string) string {
	low := pyLower(query)
	for _, c := range targetCities {
		if strings.Contains(low, pyLower(c)) {
			return c
		}
	}
	for _, kv := range fallbackCities {
		if strings.Contains(low, kv.key) {
			return kv.val
		}
	}
	return ""
}

// IsLocationQuery is _is_location_query.
func IsLocationQuery(query string) bool {
	low := pyLower(query)
	for _, c := range targetCities {
		if strings.Contains(low, pyLower(c)) {
			return true
		}
	}
	for _, c := range locationCities {
		if strings.Contains(low, c) {
			return true
		}
	}
	return false
}

var (
	indianCities = []string{"mumbai", "delhi", "bangalore", "bengaluru", "hyderabad", "chennai", "kolkata", "pune",
		"ahmedabad", "jaipur", "lucknow", "kanpur", "nagpur", "indore", "thane", "bhopal", "visakhapatnam", "patna",
		"vadodara", "ghaziabad", "ludhiana", "agra", "nashik", "faridabad", "meerut", "rajkot", "varanasi", "srinagar",
		"noida", "gurgaon", "gurugram", "coimbatore", "kochi", "chandigarh", "mysore", "mysuru", "surat", "ranchi",
		"bhubaneswar", "tiruchirappalli", "trivandrum", "thiruvananthapuram", "salem", "hubli", "mangalore"}
	indianKeywords = []string{"india", "indian", "pvt ltd", "private limited", "nse", "bse"}
	usCities       = []string{"new york", "los angeles", "chicago", "houston", "phoenix", "philadelphia", "san antonio",
		"san diego", "dallas", "san jose", "austin", "seattle", "denver", "boston", "nashville", "portland",
		"las vegas", "atlanta", "miami", "san francisco", "charlotte", "minneapolis"}
	euKeywords = []string{"london", "berlin", "paris", "amsterdam", "munich", "barcelona", "europe", "uk", "germany",
		"france"}
)

func anyIn(text string, subs []string) bool {
	for _, s := range subs {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// DetectRegion is _detect_region (substring matches, so "uk" also hits "Sukhumvit": legacy).
func DetectRegion(query, city string) string {
	text := pyLower(query + " " + city)
	switch {
	case anyIn(text, indianCities) || anyIn(text, indianKeywords):
		return "india"
	case anyIn(text, usCities) || strings.Contains(text, "usa") || strings.Contains(text, "united states"):
		return "us"
	case anyIn(text, euKeywords):
		return "eu"
	}
	return "global"
}

var sizeRe = mustRx(`(?i)(\d[\d,]+)\s*(?:employees|people|staff|workers)`)

// sizeBucket is the shared 1-50 / 51-200 / 201-500 / 500+ bucketing.
func sizeBucket(digits string) string {
	n, err := strconv.Atoi(strings.ReplaceAll(digits, ",", ""))
	if err != nil {
		return "500+" // an int() too large for Go is "500+" in Python as well
	}
	switch {
	case n < 50:
		return "1-50"
	case n < 200:
		return "51-200"
	case n < 500:
		return "201-500"
	}
	return "500+"
}

// ExtractSize is _extract_size.
func ExtractSize(text string) string {
	if m := sizeRe.FindFolded(text); m != nil {
		return sizeBucket(m[1])
	}
	return ""
}

// ExpandQuery is _expand_query: up to four search variations of the query.
func ExpandQuery(query string) []string {
	city := ExtractCity(query)
	base := pyLower(query)
	if city != "" {
		base = pyStrip(strings.ReplaceAll(base, pyLower(city), ""))
	}
	or := func(s, d string) string {
		if s == "" {
			return d
		}
		return s
	}
	out := []string{query}
	for _, s := range []string{"companies", "agencies", "firms", "consultancies"} {
		if !strings.Contains(base, s) {
			q := base + " " + s
			if city != "" {
				q += " " + city
			}
			out = append(out, q)
			break
		}
	}
	out = append(out, "top "+base+" "+or(city, "India"), base+" list "+or(city, "India")+" 2024")
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

var (
	articlePartRe = mustRx(`(?i)\b(top|best|guide|list|ranking)\s+\d*`)
	yearPartRe    = mustRx(`(?i)\b20(2[3-9]|3\d)\b`)
	titleSeps     = []string{" | ", " - ", " — ", " – ", " · "}
)

// ExtractBusinessName is _extract_business_name: the first title part (on the first separator the
// title contains) that does not look like an article, cleaned; else the whole title cleaned (when
// at most 60 characters).
func ExtractBusinessName(title string) string {
	if title == "" {
		return ""
	}
	for _, sep := range titleSeps {
		if !strings.Contains(title, sep) {
			continue
		}
		for _, part := range strings.Split(title, sep) {
			part = pyStrip(part)
			if articlePartRe.Match(part) || yearPartRe.Match(part) {
				continue
			}
			if n := runeLen(part); n > 60 || n < 3 {
				continue
			}
			return CleanName(part)
		}
	}
	cleaned := CleanName(title)
	if runeLen(cleaned) <= 60 {
		return cleaned
	}
	return ""
}

var noiseWords = []string{"Reviews", "Company Profile", "LinkedIn", "Glassdoor", "Clutch.co", "GoodFirms", "Careers",
	"Jobs", "Hiring", "| Clutch", "| GoodFirms", "Company", "Profile"}

// CleanName is _clean_name.
func CleanName(name string) string {
	for _, n := range noiseWords {
		name = strings.ReplaceAll(name, n, "")
	}
	return truncRunes(pyStrip(name), 80)
}

var (
	tldSuffixes = []string{".co.in", ".com", ".in", ".net", ".org", ".io", ".co"}
	splitDashRe = mustRx(`[-_]`)
	camelRe     = mustRx(`([a-z])([A-Z])`)
	alphaNumRe  = mustRx(`([a-zA-Z])(\d)`)
	numAlphaRe  = mustRx(`(\d)([a-zA-Z])`)
)

// insertSpace is re.sub(r'(x)(y)', r'\1 \2', s) for the two-group patterns above.
func insertSpace(re pyRx, s string) string {
	f := re.fold(s)
	locs := re.re.FindAllStringSubmatchIndex(f.t, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		b.WriteString(f.slice(last, l[0]))
		b.WriteString(f.slice(l[2], l[3]) + " " + f.slice(l[4], l[5]))
		last = l[1]
	}
	b.WriteString(f.src[f.at(last):])
	return b.String()
}

// CompanyFromDomain is _company_from_domain: "www.datamatics.com" -> "Datamatics".
func CompanyFromDomain(domain string) string {
	if domain == "" {
		return ""
	}
	domain = strings.TrimPrefix(domain, "www.")
	for _, tld := range tldSuffixes {
		if strings.HasSuffix(domain, tld) {
			domain = domain[:len(domain)-len(tld)]
			break
		}
	}
	if leadweb.IsSkipDomain(domain + ".com") {
		return ""
	}
	if domain == "" || runeLen(domain) < 3 {
		return ""
	}
	var parts []string
	f := splitDashRe.fold(domain)
	last := 0
	for _, loc := range splitDashRe.re.FindAllStringIndex(f.t, -1) {
		parts = append(parts, f.slice(last, loc[0]))
		last = loc[1]
	}
	parts = append(parts, f.src[f.at(last):])
	for i, p := range parts {
		p = insertSpace(camelRe, p)
		p = insertSpace(alphaNumRe, p)
		parts[i] = insertSpace(numAlphaRe, p)
	}
	return pyTitle(strings.Join(parts, " "))
}

// IsSkipDomain is _is_skip_domain (aggregators, social networks, directories).
func IsSkipDomain(domain string) bool { return leadweb.IsSkipDomain(domain) }
