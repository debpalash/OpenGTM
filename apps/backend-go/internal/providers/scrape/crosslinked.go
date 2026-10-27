package scrape

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ── crosslinked.py ─────────────────────────────────────────────────────────

var (
	clTitleKeywords = []string{"CEO", "CTO", "COO", "CFO", "CMO", "CIO", "CHRO", "Founder", "Co-Founder", "Co Founder",
		"Director", "VP", "Vice President", "Head of", "Head -", "Managing Director", "MD", "Partner", "Principal",
		"Manager", "Lead", "Owner", "Proprietor"}
	clRoleWords = map[string]bool{"ceo": true, "cto": true, "coo": true, "cfo": true, "cmo": true, "cio": true, "chro": true,
		"cro": true, "cpo": true, "vp": true, "svp": true, "evp": true, "avp": true, "md": true, "gm": true, "founder": true,
		"cofounder": true, "co-founder": true, "director": true, "manager": true, "head": true, "lead": true, "partner": true,
		"principal": true, "owner": true, "proprietor": true, "president": true, "chairman": true, "engineer": true,
		"consultant": true, "advisor": true, "intern": true, "recruiter": true, "specialist": true, "analyst": true,
		"associate": true, "executive": true, "officer": true}
	clNotTitles = map[string]bool{"linkedin": true, "n/a": true, "profile": true, "page": true}

	clSuffixRe  = pyRe(`(?i)\s*[\|·\-–]\s*LinkedIn.*$`)
	clSplitRe   = pyRe(`\s+[\-–—]\s+|\s*[\|·,]\s*`)
	clHonorific = regexp.MustCompile(`^(Dr|Mr|Mrs|Ms|Prof)\.\s*`)
	clAtRe      = pyRe(`(?i)\s+at\s+.*$`)
	clPipeRe    = pyRe(`\s*[\|·]\s*.*$`)
	clURLRe     = regexp.MustCompile(`(https?://(?:www\.)?linkedin\.com/in/[a-zA-Z0-9\-_%]+)`)
)

func clName(text string) string {
	if text == "" {
		return ""
	}
	text = clSuffixRe.ReplaceAllString(text, "")
	part := pyStrip(clSplitRe.Split(text, -1)[0])
	part = clHonorific.ReplaceAllString(part, "")
	words := pySplit(part)
	if len(words) < 2 || len(words) > 5 {
		return ""
	}
	for _, w := range words {
		if !isAlpha(strings.NewReplacer(".", "", "'", "", "-", "").Replace(w)) {
			return ""
		}
		if clRoleWords[strings.Trim(strings.ToLower(w), ".")] {
			return ""
		}
	}
	return part
}

func clTitle(text string) string {
	if text == "" {
		return ""
	}
	text = clSuffixRe.ReplaceAllString(text, "")
	parts := strings.Split(text, " - ")
	if len(parts) < 2 {
		return ""
	}
	t := pyStrip(parts[1])
	t = clAtRe.ReplaceAllString(t, "")
	t = clPipeRe.ReplaceAllString(t, "")
	if t != "" && runeLen(t) < 80 && !clNotTitles[strings.ToLower(pyStrip(t))] {
		return pyStrip(t)
	}
	return ""
}

// Person is one LinkedIn candidate found through search (crosslinked
// _parse_result), with the discovery evidence attached.
type Person struct {
	Name, Title, LinkedIn                        string
	EvidenceURL, EvidenceTitle, EvidenceSnippet string
}

func (p Person) obj() *providers.Obj {
	o := providers.NewObj()
	o.Set("name", p.Name)
	o.Set("title", p.Title)
	o.Set("linkedin", p.LinkedIn)
	o.Set("evidence_url", p.EvidenceURL)
	o.Set("evidence_title", p.EvidenceTitle)
	o.Set("evidence_snippet", p.EvidenceSnippet)
	return o
}

func clParse(r Hit) (Person, bool) {
	u := clURLRe.FindString(r.Href)
	if u == "" {
		return Person{}, false
	}
	name := clName(r.Title)
	if name == "" || containsAny(strings.ToLower(name), []string{"linkedin", "company", "page", "profile"}) {
		return Person{}, false
	}
	title := clTitle(r.Title)
	if title == "" {
		for _, kw := range clTitleKeywords {
			if i := wordKeywordIndex(r.Body, kw); i >= 0 {
				chunk := trunc(r.Body[i:], 40)
				chunk = strings.SplitN(chunk, ".", 2)[0]
				chunk = strings.SplitN(chunk, ",", 2)[0]
				title = pyStrip(chunk)
				break
			}
		}
	}
	if title == "" {
		title = "N/A"
	}
	return Person{Name: name, Title: title, LinkedIn: u, EvidenceURL: u,
		EvidenceTitle: trunc(r.Title, 300), EvidenceSnippet: trunc(r.Body, 500)}, true
}

// clSearch is _ddg_linkedin_search: up to three attempts through web_search
// with raise_errors, pausing 0.5 s and 1 s between them. ok=false means
// every attempt failed (Python returned None).
func clSearch(ctx context.Context, s Search, q string, n int) ([]Hit, bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		hits, err := s.Web(ctx, q, n, true)
		if err == nil {
			return hits, true, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, false, cerr
		}
		if attempt < 2 {
			if err := sleep(ctx, time.Duration(attempt+1)*500*time.Millisecond); err != nil {
				return nil, false, err
			}
		}
	}
	return nil, false, nil
}

func clRank(p Person) int {
	t := strings.ToUpper(p.Title)
	switch {
	case containsAny(t, []string{"CEO", "FOUNDER", "OWNER", "MD"}):
		return 0
	case containsAny(t, []string{"CTO", "COO", "CFO", "CMO", "CIO"}):
		return 1
	case containsAny(t, []string{"VP", "VICE PRESIDENT", "DIRECTOR"}):
		return 2
	case containsAny(t, []string{"HEAD", "LEAD", "MANAGER"}):
		return 3
	}
	return 4
}

// CrossLinked is the crosslinked provider. It also implements
// leadpeople.CrossLinked (FindPeopleByTitles), which people research reaches
// through providers.Get("crosslinked").
type CrossLinked struct {
	gated
	MaxPeople int           // 0 = 5
	Delay     time.Duration // 0 = 1 s
}

func (CrossLinked) Name() string { return "crosslinked" }

func (c CrossLinked) maxPeople() int {
	if c.MaxPeople > 0 {
		return c.MaxPeople
	}
	return 5
}

func (c CrossLinked) delay() time.Duration {
	if c.Delay > 0 {
		return c.Delay
	}
	return time.Second
}

func (c CrossLinked) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "no_company_name"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	people, err := c.findPeople(ctx, s, company, ls(in.Lead, "city"))
	if err != nil {
		return providers.Output{}, err
	}
	if len(people) == 0 {
		return fail(in, "no_people_found"), nil
	}
	list := make([]any, 0, len(people))
	for _, p := range people {
		list = append(list, p.obj())
	}
	return ok(in, 0.65, "decision_makers", pyDumps(list, true), "contact_person", people[0].Name,
		"contact_title", people[0].Title), nil
}

func (c CrossLinked) findPeople(ctx context.Context, s Search, company, city string) ([]Person, error) {
	var people []Person
	seen := map[string]bool{}
	add := func(hits []Hit) {
		for _, r := range hits {
			if p, ok := clParse(r); ok && !seen[strings.ToLower(p.Name)] {
				seen[strings.ToLower(p.Name)] = true
				people = append(people, p)
			}
		}
	}
	q := `site:linkedin.com/in "` + company + `"`
	if city != "" {
		q += ` "` + city + `"`
	}
	hits, _, err := clSearch(ctx, s, q, 15)
	if err != nil {
		return nil, err
	}
	add(hits)
	if len(people) < 3 {
		if err := sleep(ctx, c.delay()); err != nil {
			return nil, err
		}
		for _, titles := range []string{"CEO OR CTO OR Founder", "Director OR VP OR Head"} {
			hits, _, err := clSearch(ctx, s, `site:linkedin.com/in "`+company+`" `+titles, 8)
			if err != nil {
				return nil, err
			}
			add(hits)
			if len(people) >= c.maxPeople() {
				break
			}
			if err := sleep(ctx, c.delay()); err != nil {
				return nil, err
			}
		}
	}
	sort.SliceStable(people, func(i, j int) bool { return clRank(people[i]) < clRank(people[j]) })
	if len(people) > c.maxPeople() {
		people = people[:c.maxPeople()]
	}
	return people, nil
}

// FindPeopleByTitles is find_people_by_titles: one search per title
// (`site:linkedin.com/in "<company>" "<title>"` plus the optional quoted geo),
// never more than MaxSearches queries (0 = no budget). An error wraps the
// context error (context.DeadlineExceeded when the budget ran out).
func (c CrossLinked) FindPeopleByTitles(ctx context.Context, pq leadpeople.PeopleQuery) (leadpeople.PeopleFound, error) {
	res := leadpeople.PeopleFound{People: []map[string]any{}}
	s := searchSeam()
	if s == nil {
		return res, errNoSeam
	}
	maxPeople := pq.MaxPeople
	if maxPeople <= 0 {
		maxPeople = 10
	}
	per := pq.ResultsPerSearch
	if per <= 0 {
		per = 10
	}
	delay := pq.Delay
	if delay <= 0 {
		delay = c.delay()
	}
	titles := pq.Titles
	if len(titles) == 0 {
		titles = []string{""}
	}
	seen := map[string]bool{}
	for i, title := range titles {
		if pq.MaxSearches > 0 && res.SearchesUsed >= pq.MaxSearches {
			break
		}
		if len(res.People) >= maxPeople {
			break
		}
		q := `site:linkedin.com/in "` + pq.Company + `"`
		for _, t := range []string{title, pq.Geo} {
			if t != "" {
				q += ` "` + t + `"`
			}
		}
		if i > 0 {
			if err := sleep(ctx, delay); err != nil {
				return res, fmt.Errorf("crosslinked: %w", err)
			}
		}
		hits, okSearch, err := clSearch(ctx, s, q, per)
		if err != nil {
			return res, fmt.Errorf("crosslinked: %w", err)
		}
		res.SearchesUsed++
		if !okSearch {
			res.SearchFailures++
		}
		for _, r := range hits {
			p, ok := clParse(r)
			if !ok {
				continue
			}
			key := strings.ToLower(p.LinkedIn)
			if key == "" {
				key = strings.ToLower(p.Name)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			res.People = append(res.People, map[string]any{"name": p.Name, "title": p.Title, "linkedin": p.LinkedIn,
				"evidence_url": p.EvidenceURL, "evidence_title": p.EvidenceTitle, "evidence_snippet": p.EvidenceSnippet})
			if len(res.People) >= maxPeople {
				break
			}
		}
	}
	return res, nil
}

var _ leadpeople.CrossLinked = CrossLinked{}
