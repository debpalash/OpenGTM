package leadpeople

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

var verificationStatuses = []string{"independent_role_evidence", "profile_reconfirmed", "not_corroborated", "verification_timeout"}

// publicSearch is _public_search: failures and a stalled search are an empty
// result, never an error.
func (s *Service) publicSearch(ctx context.Context, query string, maxResults int) []leadweb.Hit {
	if s.Search == nil {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, dur(s.SearchTimeout, 16*time.Second))
	defer cancel()
	return s.Search.Text(sctx, query, leadweb.SearchOptions{MaxResults: maxResults, Workspace: s.Workspace})
}

// VerifyPeopleAtCompany is verify_people_at_company: it re-checks people
// against fresh public search evidence. This is role verification, not contact
// enrichment: independent pages corroborate a candidate, a fresh LinkedIn hit
// can only reconfirm profile evidence, and absence of corroboration stays
// explicit. maxPeople <= 0 takes the Python default of 15.
func (s *Service) VerifyPeopleAtCompany(ctx context.Context, company, function string, people []map[string]any, maxPeople int) (map[string]any, error) {
	company = trunc(clean(company), 120)
	function = trunc(clean(function), 100)
	if maxPeople <= 0 {
		maxPeople = 15
	}
	candidates := make([]map[string]any, 0, len(people))
	for _, p := range people {
		if p == nil {
			continue
		}
		c := make(map[string]any, len(p)+1)
		for k, v := range p {
			c[k] = v
		}
		candidates = append(candidates, c)
	}
	if len(candidates) > maxPeople {
		candidates = candidates[:maxPeople]
	}
	for _, p := range candidates {
		p["person_id"] = PersonEntityID(company, p)
	}
	checkedAt := isoDate(s.now())
	terms := functionTerms(function, nil)

	vctx, cancel := context.WithTimeout(ctx, dur(s.VerifyTimeout, 40*time.Second))
	defer cancel()
	verifyOne := func(person map[string]any) map[string]any {
		out := cloneMap(person)
		name := trunc(clean(getStr(person, "name")), 120)
		if name == "" {
			out["verification_status"] = "not_corroborated"
			out["verification_confidence"] = 0.0
			out["verification_sources"] = []any{}
			out["checked_at"] = checkedAt
			return out
		}
		title := trunc(clean(getStr(person, "title")), 140)
		query := `"` + name + `" "` + company + `" "` + function + `"`
		hits := s.publicSearch(vctx, query, 8)

		sources := []map[string]any{}
		originalURL := strings.ToLower(strings.TrimRight(firstStr(person, "linkedin_url", "evidence_url"), "/"))
		for _, h := range hits {
			url := providers.PyStrip(h.Href)
			hitTitle := providers.PyStrip(h.Title)
			snippet := providers.PyStrip(h.Body)
			evidence := hitTitle + ". " + snippet
			if !exactNameMatch(evidence, name) ||
				!verificationCompanyMatch(evidence, company, name) ||
				!verificationFunctionMatch(hitTitle, snippet, company, name, terms) {
				continue
			}
			host, _ := pyURL(url)
			sourceType := "independent_web"
			if strings.Contains(host, "linkedin.com") || strings.ToLower(strings.TrimRight(url, "/")) == originalURL {
				sourceType = "linkedin_profile"
			}
			sources = append(sources, map[string]any{
				"url": url, "title": trunc(hitTitle, 240), "snippet": trunc(snippet, 320), "source_type": sourceType,
			})
			if len(sources) >= 3 {
				break
			}
		}
		independent, profile := false, false
		for _, src := range sources {
			if src["source_type"] == "independent_web" {
				independent = true
			} else {
				profile = true
			}
		}
		var status string
		var conf float64
		switch {
		case independent:
			status, conf = "independent_role_evidence", 0.85
		case profile:
			status, conf = "profile_reconfirmed", max(0.75, toFloat(person["confidence"]))
		default:
			c := toFloat(person["confidence"])
			if c == 0 {
				c = 0.5
			}
			status, conf = "not_corroborated", min(0.5, c)
		}
		out["title"] = title
		out["verification_status"] = status
		out["verification_confidence"] = conf
		out["verification_sources"] = sources
		out["checked_at"] = checkedAt
		return out
	}

	verified := make([]map[string]any, len(candidates))
	done := make(chan struct{})
	go func() {
		defer close(done)
		sem := make(chan struct{}, s.concurrency())
		var wg sync.WaitGroup
		for i, p := range candidates {
			select {
			case sem <- struct{}{}:
			case <-vctx.Done():
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				verified[i] = verifyOne(p)
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-vctx.Done():
		<-done
	}
	timedOut := false
	for _, v := range verified {
		if v == nil {
			timedOut = true
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timedOut || vctx.Err() != nil {
		for i, p := range candidates {
			out := cloneMap(p)
			out["verification_status"] = "verification_timeout"
			out["verification_confidence"] = 0.0
			out["verification_sources"] = []any{}
			out["checked_at"] = checkedAt
			verified[i] = out
		}
	}

	summary := map[string]any{}
	for _, st := range verificationStatuses {
		n := 0
		for _, v := range verified {
			if v["verification_status"] == st {
				n++
			}
		}
		summary[st] = n
	}
	resolution := map[string]any{"status": "unresolved", "company": company, "canonical_domain": ""}
	for _, p := range candidates {
		if cr, ok := p["company_resolution"].(map[string]any); ok && cr["status"] == "resolved" {
			resolution = cloneMap(cr)
			break
		}
	}
	return map[string]any{
		"ok": true, "company": company, "function": function, "company_resolution": resolution,
		"result_set_id": PeopleResultSetID(company, function, verified), "people": verified, "count": len(verified),
		"summary": summary, "checked_at": checkedAt,
		"methodology": "Fresh exact-name/company/function web searches. Independent pages can " +
			"support a role; LinkedIn-only matches remain profile-level evidence.",
		"warning": "Not corroborated does not prove a person is incorrect; it means fresh supporting evidence was not found.",
	}, nil
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+6)
	for k, v := range m {
		out[k] = v
	}
	return out
}
