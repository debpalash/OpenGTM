package collect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// The seven discovery strategies of JobRunner (job_runner.py _search_*). Each returns the leads
// it found; failures inside a strategy degrade to fewer leads, as legacy.

func (j *job) progress(stage, msg string, extra ...any) {
	j.emit("job_progress", append([]any{"job_id", j.id, "stage", stage, "message", msg}, extra...)...)
}

func (j *job) discovered(stage string, lead ...any) {
	j.emit("lead_discovered", "job_id", j.id, "stage", stage, "lead", obj(lead...))
}

func (j *job) search(ctx context.Context, q string, n int) []leadweb.Hit {
	ctx, cancel := context.WithTimeout(ctx, j.r.Knobs.SearchTimeout)
	defer cancel()
	return j.r.Web.Search(ctx, q, n)
}

func (j *job) fetch(ctx context.Context, url string, timeout time.Duration) (string, bool) {
	return j.r.Web.Fetch(ctx, url, 2, timeout)
}

func (j *job) sleep(ctx context.Context, d time.Duration) error {
	if j.r.Sleep != nil {
		return j.r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func hostOf(u string) string { return pyLower(urlparse(u).Netloc) }

// baseDomain is ".".join(domain.split(".")[-2:]).
func baseDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}

func first(xs []string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return ""
}

func slice(xs []string, a, b int) []string {
	if a > len(xs) {
		return nil
	}
	if b > len(xs) {
		b = len(xs)
	}
	return xs[a:b]
}

// ── Google Maps ─────────────────────────────────────────────────────────────

func (j *job) searchMaps(ctx context.Context) []*Lead {
	j.progress("maps", "🗺️ Searching Google Maps...")
	city := ExtractCity(j.query)
	if city == "" {
		return nil
	}
	clean := pyStrip(strings.ReplaceAll(pyLower(j.query), pyLower(city), ""))
	q := clean
	if q == "" {
		q = j.query
	}
	places, err := j.r.Discovery.Maps(ctx, q, city, 20)
	if err != nil {
		j.progress("maps", "⚠️ Maps error: "+err.Error())
		return nil
	}
	var leads []*Lead
	for _, p := range places {
		l := NewLead(Lead{Company: p.Company, Website: p.Website, Phone: p.Phone, City: p.City,
			Specialization: p.Specialization, Notes: p.Notes, Source: p.Source})
		leads = append(leads, l)
		j.discovered("maps", "company", l.Company, "email", l.Email, "phone", l.Phone)
	}
	return leads
}

// ── web search ──────────────────────────────────────────────────────────────

const aiBudget = 3

func (j *job) searchAndScrape(ctx context.Context) []*Lead {
	var leads []*Lead
	city := ExtractCity(j.query)
	aiUsed := 0
	var expanded []string
	ectx, cancel := context.WithTimeout(ctx, 15*time.Second)
	expanded = AIExpandQuery(ectx, j.llm, j.query)
	timedOut := ectx.Err() != nil && ctx.Err() == nil
	cancel()
	if timedOut {
		expanded = ExpandQuery(j.query)
	} else {
		j.progress("web", fmt.Sprintf("🤖 AI generated %d search queries", len(expanded)))
	}
	j.progress("web", fmt.Sprintf("🔍 Web search: %d queries...", len(expanded)))
	seen := map[string]bool{}
	for i, q := range expanded {
		if ctx.Err() != nil {
			break
		}
		j.progress("web", fmt.Sprintf("🔎 [%d/%d] %s", i+1, len(expanded), pyRepr(q)))
		for _, r := range j.search(ctx, q, 15) {
			if r.Href == "" {
				continue
			}
			domain := hostOf(r.Href)
			base := baseDomain(domain)
			if IsSkipDomain(domain) || seen[base] {
				continue
			}
			seen[base] = true
			page, ok := j.fetch(ctx, r.Href, 8*time.Second)
			if !ok {
				continue
			}
			emails := leadweb.EmailsFromHTML(page)
			phones := leadweb.PhonesFromHTML(page)
			name := CompanyFromDomain(domain)
			switch {
			case name != "" && runeLen(name) >= 3 && (len(emails) > 0 || len(phones) > 0):
				l := NewLead(Lead{Company: name, Website: r.Href, Email: first(emails), Phone: first(phones), City: city,
					Description: truncRunes(r.Body, 300), Specialization: j.query, Source: "web_search"})
				if len(emails) > 1 {
					l.SecondaryEmails = strings.Join(slice(emails, 1, 4), ", ")
				}
				if len(phones) > 1 {
					l.SecondaryPhones = strings.Join(slice(phones, 1, 3), ", ")
				}
				leads = append(leads, l)
				j.discovered("web", "company", l.Company, "email", l.Email, "phone", l.Phone)
			case aiUsed < aiBudget:
				actx, acancel := context.WithTimeout(ctx, 20*time.Second)
				data := AIExtractCompany(actx, j.llm, page, r.Href)
				acancel()
				aiUsed++
				if data == nil || !pyTruthyDefault(data, "is_real_company", true) {
					break
				}
				aiName, _ := data["company_name"].(string)
				if aiName == "" || runeLen(aiName) < 3 {
					aiName = name
				}
				if aiName == "" || runeLen(aiName) < 3 {
					break
				}
				l := NewLead(Lead{Company: aiName, Website: r.Href, Email: orStr(data["email"], ""),
					Phone: orStr(data["phone"], ""), City: orStr(data["city"], city), State: orStr(data["state"], ""),
					Address: orStr(data["address"], ""), Description: orStr(data["description"], truncRunes(r.Body, 300)),
					Specialization: orStr(data["specialization"], j.query), IndustryTags: orStr(data["industry_tags"], ""),
					CompanySize: orStr(data["employee_count"], ""), EmployeeCountExact: orInt(data["employee_count_exact"]),
					FoundedYear: orStr(data["founded_year"], ""), Technologies: orStr(data["technologies"], ""),
					ContactPerson: orStr(data["contact_person"], ""), Source: "web_search"})
				leads = append(leads, l)
				j.discovered("web", "company", l.Company, "email", l.Email)
			case name != "" && runeLen(name) >= 3:
				leads = append(leads, NewLead(Lead{Company: name, Website: r.Href, City: city,
					Description: truncRunes(r.Body, 300), Specialization: j.query, Source: "web_search"}))
			}
		}
		if j.sleep(ctx, 500*time.Millisecond) != nil {
			break
		}
	}
	j.progress("web", fmt.Sprintf("🔍 Web search done: %d leads (%d AI calls)", len(leads), aiUsed))
	return leads
}

// pyTruthyDefault is bool(d.get(k, def)).
func pyTruthyDefault(d map[string]any, k string, def bool) bool {
	v, ok := d[k]
	if !ok {
		return def
	}
	return pyTruthy(v)
}

// ── directories ─────────────────────────────────────────────────────────────

var directoryHosts = []string{"clutch.co", "goodfirms.co", "g2.com", "softwaresuggest.com"}

func (j *job) searchDirectories(ctx context.Context) []*Lead {
	var leads []*Lead
	city := ExtractCity(j.query)
	j.progress("directories", "📂 Searching directories...")
	core := pyLower(j.query)
	if city != "" {
		core = pyStrip(strings.ReplaceAll(strings.ReplaceAll(core, pyLower(city), ""), " in ", " "))
	}
	cityOr := or(city, "India")
	queries := []string{
		j.query + " site:clutch.co",
		j.query + " site:goodfirms.co",
		"site:clutch.co " + core + " companies " + cityOr,
		"site:goodfirms.co " + core + " companies " + cityOr,
		j.query + " companies list India",
	}
	if city != "" {
		queries = append(queries, j.query+" top firms "+city, "site:justdial.com "+core+" "+city)
	} else {
		queries = append(queries, j.query+" top firms India")
	}
	for _, dq := range queries {
		if ctx.Err() != nil {
			break
		}
		for _, r := range j.search(ctx, dq, 10) {
			if r.Href == "" || r.Title == "" {
				continue
			}
			domain := hostOf(r.Href)
			if anyIn(domain, directoryHosts) {
				leads = append(leads, j.companiesFromDirectory(ctx, r.Href, domain, city)...)
				continue
			}
			company := ExtractBusinessName(r.Title)
			if company == "" || runeLen(company) < 3 {
				continue
			}
			if ok, _ := ValidateLeadLight(&Lead{Company: company}); !ok {
				continue
			}
			l := NewLead(Lead{Company: company, Website: r.Href, City: city, Description: truncRunes(r.Body, 300),
				Source: "directory"})
			leads = append(leads, l)
			j.discovered("directories", "company", l.Company)
		}
		if j.sleep(ctx, 1500*time.Millisecond) != nil {
			break
		}
	}
	return leads
}

type dirCard struct{ name, link string }

// companiesFromDirectory is _extract_companies_from_directory: company cards of a Clutch,
// GoodFirms or G2 listing page (generic card patterns as a fallback), at most 30 per page.
func (j *job) companiesFromDirectory(ctx context.Context, url, domain, city string) []*Lead {
	page, ok := j.fetch(ctx, url, 12*time.Second)
	if !ok {
		return nil
	}
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		j.progress("directories", "⚠️ Directory page parse error: "+err.Error())
		return nil
	}
	var found []dirCard
	cards := func(cardSel, nameSel, prefix string) {
		for _, card := range cssSelect(doc, cardSel) {
			el := cssSelectOne(card, nameSel)
			if el == nil {
				continue
			}
			link, _ := attrVal(el, "href")
			if link != "" && !strings.HasPrefix(link, "http") {
				link = prefix + link
			}
			found = append(found, dirCard{textStrip(el), link})
		}
	}
	switch {
	case strings.Contains(domain, "clutch.co"):
		cards("[data-url], .provider-row, .directory-list li", "h3 a, .company_info a, .company-name a, a[data-link_text]", "https://clutch.co")
	case strings.Contains(domain, "goodfirms.co"):
		cards(".company-profile-row, .firm-wrapper, .agency-list-card", "a.company-name, h3 a, .profile-name a", "https://www.goodfirms.co")
	case strings.Contains(domain, "g2.com"):
		for _, card := range cssSelect(doc, ".product-listing__card, [data-product-name]") {
			name, _ := attrVal(card, "data-product-name")
			if name == "" {
				if el := cssSelectOne(card, "a.product-listing__product-name, h3 a"); el != nil {
					name = textStrip(el)
				}
			}
			if name != "" {
				found = append(found, dirCard{name, ""})
			}
		}
	}
	if len(found) == 0 {
		for _, card := range cssSelect(doc, "li.company, .card, .listing-item, article") {
			if el := cssSelectOne(card, "h2, h3, h4, .title, .name"); el != nil {
				if name := textStrip(el); name != "" && runeLen(name) > 3 && runeLen(name) < 60 {
					found = append(found, dirCard{name, ""})
				}
			}
		}
	}
	if len(found) > 30 {
		found = found[:30]
	}
	var leads []*Lead
	for _, c := range found {
		name := CleanName(c.name)
		if name == "" || runeLen(name) < 3 {
			continue
		}
		if ok, _ := ValidateLead(&Lead{Company: name}); !ok {
			continue
		}
		l := NewLead(Lead{Company: name, Website: c.link, City: city, Specialization: j.query, Source: "directory"})
		leads = append(leads, l)
		j.discovered("directories", "company", l.Company)
	}
	return leads
}

// ── LinkedIn companies ──────────────────────────────────────────────────────

var followersRe = mustRx(`(?i)(\d[\d,]+)\s*(?:employees|followers)`)

func (j *job) searchLinkedin(ctx context.Context) []*Lead {
	var leads []*Lead
	city := ExtractCity(j.query)
	j.progress("linkedin", "🔗 Searching LinkedIn companies...")
	second := `site:linkedin.com/company "` + j.query + `" India`
	if city != "" {
		second = `site:linkedin.com/company "` + j.query + `" ` + city
	}
	seen := map[string]bool{}
	for _, lq := range []string{`site:linkedin.com/company "` + j.query + `"`, second} {
		if ctx.Err() != nil {
			break
		}
		for _, r := range j.search(ctx, lq, 10) {
			if !strings.Contains(r.Href, "/company/") {
				continue
			}
			path := strings.TrimRight(urlparse(r.Href).Path, "/")
			liURL := "https://www.linkedin.com" + path
			if seen[liURL] {
				continue
			}
			seen[liURL] = true
			name := pyStrip(strings.Split(strings.Split(r.Title, " | ")[0], " - ")[0])
			if name == "" || pyLower(name) == "linkedin" {
				continue
			}
			size := ""
			if m := followersRe.FindFolded(r.Body); m != nil {
				size = sizeBucket(m[1])
			}
			desc := ""
			if r.Body != "" {
				desc = truncRunes(r.Body, 300)
			}
			l := NewLead(Lead{Company: name, City: or(city, "India"), LinkedinURL: liURL, SourceURL: liURL,
				CompanySize: size, Description: desc, Specialization: j.query, Source: "linkedin"})
			leads = append(leads, l)
			j.discovered("linkedin", "company", l.Company, "linkedin", liURL)
		}
		if j.sleep(ctx, 1500*time.Millisecond) != nil {
			break
		}
	}
	return leads
}

// ── job boards ──────────────────────────────────────────────────────────────

var (
	jobsSuffixRe    = mustRx(`-jobs?-?\d*$`)
	careersSuffixRe = mustRx(`-careers?$`)
	legalSuffixRe   = mustRx(`(?i)\s*(Pvt|Ltd|Private|Limited|India)\.?\s*$`)
	jobBoardSkip    = setOf("naukri", "indeed", "jobs", "careers", "foundit", "hiring", "search", "results", "apply",
		"login", "register", "salary")
)

func titleSlug(slug string) string { return pyStrip(pyTitle(strings.ReplaceAll(slug, "-", " "))) }

func (j *job) searchJobBoards(ctx context.Context) []*Lead {
	var leads []*Lead
	city := ExtractCity(j.query)
	j.progress("job_boards", "💼 Searching job boards for employers...")
	core := pyLower(j.query)
	if city != "" {
		core = pyStrip(strings.ReplaceAll(strings.ReplaceAll(core, pyLower(city), ""), " in ", " "))
	}
	c := or(city, "India")
	queries := []string{
		"site:naukri.com " + core + " jobs " + c,
		"site:indeed.com " + core + " company " + c,
		"naukri.com " + core + " employer " + c,
		core + " hiring " + c + " company",
		"site:foundit.in " + core + " " + c,
	}
	seen := map[string]bool{}
	for _, bq := range queries {
		if ctx.Err() != nil {
			break
		}
		for _, r := range j.search(ctx, bq, 10) {
			name := ""
			if r.Href != "" {
				p := urlparse(r.Href)
				path := strings.Trim(p.Path, "/")
				switch {
				case strings.Contains(p.Netloc, "naukri"):
					slug := ""
					if path != "" {
						parts := strings.Split(path, "/")
						slug = parts[len(parts)-1]
					}
					slug = jobsSuffixRe.Sub(slug, "")
					slug = careersSuffixRe.Sub(slug, "")
					name = titleSlug(slug)
				case strings.Contains(p.Netloc, "indeed") && strings.Contains(path, "/cmp/"):
					parts := strings.Split(path, "/cmp/")
					name = titleSlug(strings.Split(parts[len(parts)-1], "/")[0])
				}
			}
			if name == "" || runeLen(name) < 3 {
				name = strings.Split(strings.Split(strings.Split(r.Title, " Jobs")[0], " Careers")[0], " Hiring")[0]
				name = pyStrip(strings.Split(strings.Split(name, " - ")[0], " | ")[0])
				name = pyStrip(legalSuffixRe.Sub(name, ""))
			}
			if name == "" || runeLen(name) < 3 {
				continue
			}
			low := pyLower(name)
			if jobBoardSkip[low] || seen[low] {
				continue
			}
			seen[low] = true
			desc := ""
			if r.Body != "" {
				desc = truncRunes(r.Body, 200)
			}
			l := NewLead(Lead{Company: name, City: c, CompanySize: ExtractSize(r.Body), Specialization: j.query,
				Description: desc, Source: "job_board"})
			leads = append(leads, l)
			j.discovered("job_boards", "company", l.Company)
		}
		if j.sleep(ctx, time.Second) != nil {
			break
		}
	}
	return leads
}

// ── review sites ────────────────────────────────────────────────────────────

var (
	reviewSuffixRe    = mustRx(`-reviews?$`)
	glassdoorSuffixRe = mustRx(`-Reviews?-E\d+.*$`)
	listingTitleRes   = rxAll(`(?i)\b(companies|agencies|firms|jobs?)\s+(in|near|of|for)\s+`, `(?i)^\d+\s+\w+\s+jobs?\s+in`, `(?i)^top\s+`)
	listingKeywords   = []string{"hiring for", "companies in", "agencies in", "looking for"}
	ratingRe          = mustRx(`(?i)(\d\.\d)\s*(?:/5|out of 5|stars?|rating|★)`)
	reviewSkip        = setOf("ambitionbox", "glassdoor", "reviews", "review", "salary", "interview", "questions",
		"jobs", "login", "compare")
)

func (j *job) searchReviewSites(ctx context.Context) []*Lead {
	var leads []*Lead
	city := ExtractCity(j.query)
	j.progress("review_sites", "⭐ Searching review sites...")
	core := pyLower(j.query)
	if city != "" {
		core = pyStrip(strings.ReplaceAll(strings.ReplaceAll(core, pyLower(city), ""), " in ", " "))
	}
	c := or(city, "India")
	queries := []string{
		"site:ambitionbox.com " + core + " " + c + " reviews",
		"ambitionbox.com " + core + " company reviews " + c,
		"site:glassdoor.co.in " + core + " " + c,
		core + " company reviews " + c,
	}
	seen := map[string]bool{}
	for _, rq := range queries {
		if ctx.Err() != nil {
			break
		}
		for _, r := range j.search(ctx, rq, 10) {
			name := ""
			if r.Href != "" {
				p := urlparse(r.Href)
				path := strings.Trim(p.Path, "/")
				switch {
				case strings.Contains(p.Netloc, "ambitionbox") && strings.Contains(path, "/reviews/"):
					parts := strings.Split(path, "/reviews/")
					name = titleSlug(reviewSuffixRe.Sub(strings.Split(parts[len(parts)-1], "/")[0], ""))
				case strings.Contains(p.Netloc, "glassdoor") && strings.Contains(path, "/Reviews/"):
					parts := strings.Split(path, "/Reviews/")
					name = titleSlug(glassdoorSuffixRe.Sub(strings.Split(parts[len(parts)-1], "/")[0], ""))
				}
			}
			if name == "" || runeLen(name) < 3 {
				name = strings.Split(strings.Split(r.Title, " Reviews")[0], " Review")[0]
				name = pyStrip(strings.Split(strings.Split(name, " | ")[0], " - ")[0])
				name = pyStrip(legalSuffixRe.Sub(name, ""))
			}
			if name == "" || runeLen(name) < 3 {
				continue
			}
			low := pyLower(name)
			if reviewSkip[low] || seen[low] {
				continue
			}
			listing := false
			for _, re := range listingTitleRes {
				if re.Match(name) {
					listing = true
					break
				}
			}
			if listing || anyIn(low, listingKeywords) {
				continue
			}
			if ok, _ := ValidateLeadLight(&Lead{Company: name}); !ok {
				continue
			}
			seen[low] = true
			rating := ""
			if m := ratingRe.Find(r.Body); m != nil {
				rating = m[1]
			}
			desc, notes := "", ""
			if r.Body != "" {
				desc = truncRunes(r.Body, 200)
			}
			if r.Href != "" {
				notes = "Review profile: " + r.Href
			}
			l := NewLead(Lead{Company: name, City: c, CompanySize: ExtractSize(r.Body), GlassdoorRating: rating,
				Specialization: j.query, Description: desc, Notes: notes, Source: "review_site"})
			leads = append(leads, l)
			j.discovered("review_sites", "company", l.Company, "rating", rating)
		}
		if j.sleep(ctx, time.Second) != nil {
			break
		}
	}
	return leads
}

// ── registry sources ────────────────────────────────────────────────────────

var registryOverlap = setOf("clutch", "goodfirms", "ambitionbox", "linkedin_companies")

const registryBatch = 10

func (j *job) searchRegistrySources(ctx context.Context) []*Lead {
	var leads []*Lead
	city := ExtractCity(j.query)
	region := DetectRegion(j.query, city)
	sources := AllSources(region, true)
	if region != "global" {
		names := map[string]bool{}
		for _, s := range sources {
			names[s.Name] = true
		}
		for _, g := range AllSources("global", true) {
			if !names[g.Name] {
				sources = append(sources, g)
			}
		}
	}
	var kept []Source
	for _, s := range sources {
		if !registryOverlap[s.Name] && j.r.Sources.Enabled(ctx, s.Name) {
			kept = append(kept, s)
		}
	}
	sources = kept
	if disabled := j.r.Sources.HealthDisabled(ctx); len(disabled) > 0 {
		var removed []string
		kept = nil
		for _, s := range sources {
			if disabled[s.Name] {
				removed = append(removed, s.Name)
			} else {
				kept = append(kept, s)
			}
		}
		sources = kept
		if len(removed) > 0 {
			j.progress("registry_sources", fmt.Sprintf("🩺 Skipping %d auto-disabled (unhealthy) source(s): %s%s",
				len(removed), strings.Join(slice(removed, 0, 8), ", "), ellipsis(len(removed) > 8)))
		}
	}
	j.progress("registry_sources", fmt.Sprintf("🌐 Scanning %d enabled directories & marketplaces...", len(sources)))
	sorted := append([]Source(nil), sources...)
	stableByPriority(sorted)
	capN := j.r.Knobs.RegistrySourceCap
	var skipped []Source
	if capN < 0 { // a negative slice bound counts from the end in Python
		capN = max(len(sorted)+capN, 0)
	}
	if capN < len(sorted) {
		skipped, sources = sorted[capN:], sorted[:capN]
	} else {
		sources = sorted
	}
	if len(skipped) > 0 {
		var names []string
		for _, s := range slice2(skipped, 8) {
			names = append(names, s.Name)
		}
		j.progress("registry_sources", fmt.Sprintf("🌐 Running top %d of %d sources by priority (skipped %d: %s%s)",
			len(sources), len(sorted), len(skipped), strings.Join(names, ", "), ellipsis(len(skipped) > 8)))
	} else {
		j.progress("registry_sources", fmt.Sprintf("🌐 Running all %d sources by priority...", len(sources)))
	}
	for bi := 0; bi < len(sources); bi += registryBatch {
		if ctx.Err() != nil {
			break
		}
		batch := sources[bi:min(bi+registryBatch, len(sources))]
		results := make([][]*Lead, len(batch))
		var wg sync.WaitGroup
		for i, s := range batch {
			wg.Add(1)
			go func(i int, s Source) {
				defer wg.Done()
				results[i] = j.searchSource(ctx, s, city)
			}(i, s)
		}
		wg.Wait()
		count := 0
		for _, r := range results {
			leads = append(leads, r...)
			count += len(r)
		}
		if count > 0 {
			var labels []string
			for _, s := range slice2(batch, 3) {
				labels = append(labels, s.Label)
			}
			j.progress("registry_sources", fmt.Sprintf("📂 Batch %d: +%d leads from %s...", bi/registryBatch+1, count,
				strings.Join(labels, ", ")))
		}
		if j.sleep(ctx, time.Second) != nil {
			break
		}
	}
	j.progress("registry_sources", fmt.Sprintf("🌐 Registry sources done: %d leads from %d sources", len(leads), len(sources)))
	return leads
}

func ellipsis(b bool) string {
	if b {
		return "…"
	}
	return ""
}

func slice2[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

func stableByPriority(ss []Source) {
	for i := 1; i < len(ss); i++ { // insertion sort: stable, tiny inputs
		for k := i; k > 0 && ss[k].Priority < ss[k-1].Priority; k-- {
			ss[k], ss[k-1] = ss[k-1], ss[k]
		}
	}
}

// searchSource is _search_single_source: one query per source.
func (j *job) searchSource(ctx context.Context, s Source, city string) []*Lead {
	var out []*Lead
	qs := BuildQueries(s, j.query, city)
	if len(qs) == 0 {
		return nil
	}
	site := s.Site()
	for _, r := range j.search(ctx, qs[0], 8) {
		if r.Href == "" || r.Title == "" || leadweb.IsJunkURL(r.Href) {
			continue
		}
		domain := hostOf(r.Href)
		company, website := "", ""
		switch {
		case site != "" && !strings.Contains(domain, site):
			company, website = CompanyFromDomain(domain), r.Href
		case site == "":
			if !IsSkipDomain(domain) {
				company, website = CompanyFromDomain(domain), r.Href
			}
		}
		onAggregator := site != "" && strings.Contains(domain, site)
		if (company == "" || runeLen(company) < 3) && !(s.ExtractFromListing && onAggregator) {
			company = ExtractBusinessName(r.Title)
		}
		if company == "" || runeLen(company) < 3 {
			continue
		}
		if ok, _ := ValidateLeadLight(&Lead{Company: company}); !ok {
			continue
		}
		if website == "" && (site == "" || !strings.Contains(domain, site)) {
			website = r.Href
		}
		l := NewLead(Lead{Company: company, Website: website, City: city, Description: truncRunes(r.Body, 300),
			Source: "registry:" + s.Name, Specialization: j.query})
		if e := leadweb.EmailsFromHTML(r.Body); len(e) > 0 {
			l.Email = e[0]
		}
		if p := leadweb.PhonesFromHTML(r.Body); len(p) > 0 {
			l.Phone = p[0]
		}
		out = append(out, l)
	}
	_ = j.sleep(ctx, 500*time.Millisecond)
	return out
}
