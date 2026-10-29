package firmo

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	greenhouseFmt = "https://boards-api.greenhouse.io/v1/boards/%s/jobs"
	leverFmt      = "https://api.lever.co/v0/postings/%s?mode=json"
	ashbyFmt      = "https://api.ashbyhq.com/posting-api/job-board/%s"
	atsUA         = "Mozilla/5.0 (compatible; YupchaBot/1.0)"
)

var deptKeywords = []struct {
	dept string
	kws  []string
}{
	{"engineering", []string{"engineer", "developer", "swe", "devops", "sre", "data ", "ml ", "machine learning", "backend", "frontend", "full stack", "platform"}},
	{"sales", []string{"sales", "account executive", "ae ", "sdr", "bdr", "revenue", "account manager"}},
	{"marketing", []string{"marketing", "growth", "demand gen", "content", "seo", "brand"}},
	{"product", []string{"product manager", "product owner", "pm ", "design", "ux", "ui "}},
	{"operations", []string{"operations", "ops ", "supply", "logistics"}},
	{"people", []string{"recruiter", "talent", "people ops", "hr ", "human resources"}},
	{"finance", []string{"finance", "accounting", "controller", "fp&a"}},
}

var techKeywords = []string{
	"python", "java", "javascript", "typescript", "react", "node", "go ", "golang",
	"rust", "kubernetes", "aws", "gcp", "azure", "terraform", "ruby", "rails",
	"django", "postgres", "kafka", "spark", "snowflake", "databricks",
}

var (
	nonAlnumRE = regexp.MustCompile(`[^a-z0-9]+`)
)

// slugCandidates is _slug_candidates.
func slugCandidates(in providers.Input) []string {
	var cands []string
	website := strings.ToLower(lead(in, "website"))
	if website != "" {
		d := website
		for _, p := range []string{"https://", "http://", "www."} {
			d = strings.TrimPrefix(d, p)
		}
		root, _, _ := strings.Cut(d, "/")
		root, _, _ = strings.Cut(root, "?")
		root, _, _ = strings.Cut(root, ".")
		if root != "" {
			cands = append(cands, root)
		}
	}
	company := strings.ToLower(lead(in, "company"))
	if company != "" {
		compact := nonAlnumRE.ReplaceAllString(company, "")
		if compact != "" {
			cands = append(cands, compact)
		}
		dashed := strings.Trim(nonAlnumRE.ReplaceAllString(company, "-"), "-")
		if dashed != "" && dashed != compact {
			cands = append(cands, dashed)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// titlesToSignals is _titles_to_signals (without the trailing ats/slug keys).
func titlesToSignals(titles []string) jo {
	depts := jo{}
	deptCount := map[string]int64{}
	tech := map[string]bool{}
	for _, t := range titles {
		tl := strings.ToLower(t)
		for _, d := range deptKeywords {
			for _, k := range d.kws {
				if strings.Contains(tl, k) {
					if _, ok := deptCount[d.dept]; !ok {
						depts = append(depts, kv{d.dept, nil})
					}
					deptCount[d.dept]++
					break
				}
			}
		}
		for _, tk := range techKeywords {
			if strings.Contains(tl, tk) {
				tech[providers.PyStrip(tk)] = true
			}
		}
	}
	for i := range depts {
		depts[i].v = deptCount[depts[i].k]
	}
	techs := make([]string, 0, len(tech))
	for k := range tech {
		techs = append(techs, k)
	}
	sort.Strings(techs)
	technologies, signal, velocity := analyzeJobText(strings.Join(titles, " \n "), len(titles))
	sample := titles
	if len(sample) > 8 {
		sample = sample[:8]
	}
	return jo{
		{"open_roles", int64(len(titles))},
		{"departments", depts},
		{"tech_hiring", techs},
		{"is_hiring", len(titles) > 0},
		{"eng_hiring", deptCount["engineering"] > 0},
		{"sample_titles", sample},
		{"technologies", technologies},
		{"tech_adoption_signal", signal},
		{"hiring_velocity", velocity},
	}
}

// atsHiring ports providers/ats_hiring.py. Redirects on the board APIs are
// followed like httpx (20 hops) with every hop URL-checked. An uncaught Python exception (a board
// whose JSON has an unexpected shape) is reported as the provider error.
type atsHiring struct{}

func (atsHiring) Name() string { return "ats_hiring" }

func (p atsHiring) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	slugs := slugCandidates(in)
	if len(slugs) == 0 {
		return fail("no_slug"), nil
	}
	out, err := p.run(ctx, c, slugs, in)
	return finish(ctx, out, err, 200)
}

type atsFetch struct {
	name string
	fmt  string
	// titles extracts the role titles from the decoded board, or ok=false when
	// the board has no usable shape.
	titles func(data any) ([]string, bool, error)
}

var atsFetches = []atsFetch{
	{"greenhouse", greenhouseFmt, func(data any) ([]string, bool, error) { return boardTitles(data, "title") }},
	{"lever", leverFmt, func(data any) ([]string, bool, error) {
		l, ok := data.([]any)
		if !ok {
			return nil, false, nil
		}
		t, err := collectTitles(l, "text")
		return t, true, err
	}},
	{"ashby", ashbyFmt, func(data any) ([]string, bool, error) { return boardTitles(data, "title") }},
}

// boardTitles is `data and isinstance(data.get("jobs"), list)` then the
// titles of the jobs.
func boardTitles(data any, key string) ([]string, bool, error) {
	if !providers.Truthy(data) {
		return nil, false, nil
	}
	jobs, err := get(data, "jobs")
	if err != nil {
		return nil, false, err
	}
	l, ok := jobs.([]any)
	if !ok {
		return nil, false, nil
	}
	t, err := collectTitles(l, key)
	return t, true, err
}

// collectTitles is [j.get(key, "") for j in jobs if j.get(key)].
func collectTitles(jobs []any, key string) ([]string, error) {
	var out []string
	for _, j := range jobs {
		v, err := providers.PyGet(j, key, "")
		if err != nil {
			return nil, err
		}
		if !providers.Truthy(v) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			// a non-string title only fails later, on .lower()
			return nil, &providers.PyException{Msg: "'" + providers.TypeName(v) + "' object has no attribute 'lower'"}
		}
		out = append(out, s)
	}
	return out, nil
}

func (p atsHiring) run(ctx context.Context, c *http.Client, slugs []string, in providers.Input) (providers.Output, error) {
	for _, slug := range slugs {
		for _, f := range atsFetches {
			data, ok, err := atsGet(ctx, c, strings.Replace(f.fmt, "%s", slug, 1))
			if err != nil {
				return providers.Output{}, err
			}
			if !ok {
				continue
			}
			titles, usable, err := f.titles(data)
			if err != nil {
				return providers.Output{}, err
			}
			if !usable || len(titles) == 0 {
				continue
			}
			signals := titlesToSignals(titles)
			signals.set("ats", f.name)
			signals.set("slug", slug)
			fl := newFields()
			fl.Set("hiring_signals", dumps(signals))
			fl.Set("open_roles", int64(len(titles)))
			return success(in, fl, 0.8), nil
		}
	}
	return fail("no_board"), nil
}

// atsGet is _fetch_json: anything but a 200 with a JSON body is None.
func atsGet(ctx context.Context, c *http.Client, url string) (any, bool, error) {
	resp, err := guardedGet(ctx, c, url, hopOpts{maxHops: httpxMaxRedirects, header: [][2]string{{"User-Agent", atsUA}}})
	if cerr := ctx.Err(); cerr != nil {
		return nil, false, cerr
	}
	if err != nil || resp.Status != 200 {
		return nil, false, nil
	}
	d, perr := providers.ParseJSON(resp.Body)
	if perr != nil {
		return nil, false, nil
	}
	return d, true, nil
}
