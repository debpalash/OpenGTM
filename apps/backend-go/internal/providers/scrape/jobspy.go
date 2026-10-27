package scrape

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
)

// ── jobspy_signals.py (provider "jobspy") ──────────────────────────────────

var (
	jsGTM   = []string{"sales", "marketing", "business development", "account executive", "sdr", "bdr", "demand generation", "growth", "partnerships", "customer success", "revenue", "outbound"}
	jsTech  = []string{"python", "node", "react", "aws", "azure", "gcp", "devops", "data engineer", "ml engineer", "ai", "machine learning", "full stack", "backend", "frontend", "cloud"}
	jsClean = regexp.MustCompile(`\s*[\|·]\s*(Indeed|Naukri|LinkedIn).*$`)
)

// JobSpy is the jobspy provider (hiring and tech intent from job-search
// results). MaxJobs 0 means 5.
type JobSpy struct {
	gated
	MaxJobs int
}

func (JobSpy) Name() string { return "jobspy" }

type job struct{ title, description, source string }

func (j JobSpy) maxJobs() int {
	if j.MaxJobs > 0 {
		return j.MaxJobs
	}
	return 5
}

func (j JobSpy) searchJobs(ctx context.Context, s Search, company, city string) ([]job, error) {
	queries := []string{`site:indeed.com "` + company + `" jobs`, `site:naukri.com "` + company + `" jobs`}
	if city != "" {
		for i := range queries {
			queries[i] += " " + city
		}
	}
	var jobs []job
	for _, q := range queries {
		for _, r := range s.Text(ctx, q, j.maxJobs()) {
			t := r.Title
			if strings.Contains(t, " - ") {
				t = pyStrip(strings.SplitN(t, " - ", 2)[0])
			}
			t = jsClean.ReplaceAllString(t, "")
			if t != "" && runeLen(t) < 100 {
				src := "naukri"
				if strings.Contains(q, "indeed") {
					src = "indeed"
				}
				jobs = append(jobs, job{title: t, description: trunc(r.Body, 200), source: src})
			}
		}
		if err := sleep(ctx, time.Second); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var unique []job
	for _, jb := range jobs {
		k := pyStrip(strings.ToLower(jb.title))
		if !seen[k] {
			seen[k] = true
			unique = append(unique, jb)
		}
	}
	if n := j.maxJobs() * 2; len(unique) > n {
		unique = unique[:n]
	}
	return unique, nil
}

// analyzeJobs is _analyze_jobs (keys in Python's insertion order).
func analyzeJobs(jobs []job) (*providers.Obj, []string) {
	sig := providers.NewObj()
	sig.Set("total_jobs", len(jobs))
	sig.Set("growth_signal", "none")
	sig.Set("gtm_expansion", false)
	sig.Set("tech_hiring", false)
	sig.Set("roles", []any{})
	sig.Set("score_boost", 0)
	sig.Set("technologies", []any{})
	sig.Set("tech_adoption_signal", []any{})
	if len(jobs) == 0 {
		return sig, nil
	}
	gtm, tech := 0, 0
	roles := []any{}
	for _, jb := range jobs {
		combined := strings.ToLower(jb.title) + " " + strings.ToLower(jb.description)
		roles = append(roles, jb.title)
		if containsAny(combined, jsGTM) {
			gtm++
		}
		if containsAny(combined, jsTech) {
			tech++
		}
	}
	total, boost := len(jobs), 0
	switch {
	case total >= 10:
		sig.Set("growth_signal", "hypergrowth")
		boost = 20
	case total >= 5:
		sig.Set("growth_signal", "rapid_growth")
		boost = 15
	case total >= 2:
		sig.Set("growth_signal", "growing")
		boost = 10
	case total >= 1:
		sig.Set("growth_signal", "hiring")
		boost = 5
	}
	sig.Set("gtm_expansion", gtm >= 2)
	sig.Set("tech_hiring", tech >= 2)
	if gtm >= 2 {
		boost += 5
	}
	if tech >= 2 {
		boost += 5
	}
	texts := make([]string, 0, len(jobs))
	for _, jb := range jobs {
		texts = append(texts, jb.title+" "+jb.description)
	}
	techs, signals, vel := firmo.AnalyzeJobText(strings.Join(texts, " \n "), total)
	ts := make([]any, 0, len(techs))
	for _, t := range techs {
		ts = append(ts, t)
	}
	sl := make([]any, 0, len(signals))
	competitor := false
	for _, s := range signals {
		o := providers.NewObj()
		o.Set("tech", s.Tech)
		o.Set("category", s.Category)
		o.Set("intent", s.Intent)
		sl = append(sl, o)
		if s.Category == "Competitor Tools" {
			competitor = true
		}
	}
	sig.Set("technologies", ts)
	sig.Set("tech_adoption_signal", sl)
	v := providers.NewObj()
	v.Set("open_roles", vel.OpenRoles)
	v.Set("recent_roles", 0)
	v.Set("recency_days", nil)
	v.Set("level", vel.Level)
	sig.Set("hiring_velocity", v)
	if competitor {
		boost += 5
	}
	sig.Set("score_boost", boost)
	if len(roles) > 5 {
		roles = roles[:5]
	}
	sig.Set("roles", roles)
	return sig, techs
}

func (j JobSpy) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" {
		return fail(in, "no_company_name"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	jobs, err := j.searchJobs(ctx, s, company, ls(in.Lead, "city"))
	if err != nil {
		return providers.Output{}, err
	}
	sig, techs := analyzeJobs(jobs)
	if len(jobs) == 0 {
		return fail(in, "no_jobs_found"), nil
	}
	f := providers.NewObj()
	f.Set("hiring_signals", pyDumps(sig, true))
	if len(techs) > 0 {
		f.Set("technologies", strings.Join(techs, ", "))
	}
	return result(in, f, 0.7, ""), nil
}
