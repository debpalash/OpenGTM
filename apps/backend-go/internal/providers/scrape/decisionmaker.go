package scrape

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ── decision_maker_provider.py + decision_maker_finder.py ──────────────────

var dmTargetTitles = []string{"Founder", "CEO", "Managing Director", "CHRO", "HR Director", "CTO", "COO", "VP HR"}

type decisionMaker struct{ gated }

func (decisionMaker) Name() string { return "decision_maker" }

type dmPerson struct{ name, title, linkedin string }

func dmSearch(ctx context.Context, s Search, company, title string) (dmPerson, bool) {
	for _, r := range s.Text(ctx, `site:linkedin.com/in "`+company+`" "`+title+`" India`, 2) {
		if !strings.Contains(r.Href, "/in/") {
			continue
		}
		name := pyStrip(strings.SplitN(strings.SplitN(r.Title, " - ", 2)[0], " | ", 2)[0])
		if name == "" || strings.ToLower(name) == "linkedin" {
			continue
		}
		if n := runeLen(name); n < 3 || n > 60 {
			continue
		}
		return dmPerson{name: name, title: title, linkedin: strings.TrimRight(strings.SplitN(r.Href, "?", 2)[0], "/")}, true
	}
	return dmPerson{}, false
}

// jsonLen is len(json.loads(s)) for a list, dict or string; ok=false when
// Python would raise (invalid JSON, or a value without len()).
func jsonLen(s string) (int, bool) {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return 0, false
	}
	switch t := v.(type) {
	case []any:
		return len(t), true
	case map[string]any:
		return len(t), true
	case string:
		return runeLen(t), true
	}
	return 0, false
}

func (decisionMaker) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := ls(in.Lead, "company")
	if company == "" || runeLen(company) < 3 {
		return fail(in, "No company name available"), nil
	}
	s := searchSeam()
	if s == nil {
		return fail(in, errNoWeb), nil
	}
	const maxContacts = 3
	person, title := ls(in.Lead, "contact_person"), ls(in.Lead, "contact_title")
	linkedin, dms := ls(in.Lead, "linkedin_url"), ls(in.Lead, "decision_makers")
	run := true
	if dms != "" {
		if n, ok := jsonLen(dms); ok && n >= maxContacts {
			run = false
		}
	}
	if run {
		var found []dmPerson
		for _, t := range dmTargetTitles {
			if len(found) >= maxContacts {
				break
			}
			if p, ok := dmSearch(ctx, s, company, t); ok {
				dup := false
				for _, f := range found {
					if strings.ToLower(f.name) == strings.ToLower(p.name) {
						dup = true
					}
				}
				if !dup {
					found = append(found, p)
				}
			}
			if err := sleep(ctx, 1500*time.Millisecond); err != nil {
				return providers.Output{}, err
			}
		}
		if len(found) > 0 {
			list := make([]any, 0, len(found))
			for _, f := range found {
				o := providers.NewObj()
				o.Set("name", f.name)
				o.Set("title", f.title)
				o.Set("linkedin", f.linkedin)
				list = append(list, o)
			}
			dms = pyDumps(list, true)
			if person == "" {
				person, title = found[0].name, found[0].title
			}
			if linkedin == "" && found[0].linkedin != "" {
				linkedin = found[0].linkedin
			}
		}
	}
	f := providers.NewObj()
	if person != "" {
		f.Set("contact_person", person)
	}
	if title != "" {
		f.Set("contact_title", title)
	}
	// Deviation (fix): Python compared the finder's lead with the same object
	// it had mutated in place, so a newly found linkedin_url was never returned.
	if linkedin != "" && linkedin != ls(in.Lead, "linkedin_url") {
		f.Set("linkedin_url", linkedin)
	}
	if dms != "" {
		f.Set("decision_makers", dms)
	}
	if f.Len() > 0 {
		return result(in, f, 0.5, ""), nil
	}
	return fail(in, "No decision makers found"), nil
}
