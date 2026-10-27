package leadpeople

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ErrCrossLinkedUnavailable is returned when no crosslinked people finder is
// injected and the provider registry has none that implements CrossLinked.
var ErrCrossLinkedUnavailable = errors.New("leadpeople: crosslinked people finder is not registered")

// PeopleQuery is one find_people_by_titles call: one DDG query per title
// (`site:linkedin.com/in "<company>" "<title>"` + optional quoted geo).
type PeopleQuery struct {
	Company          string
	Titles           []string
	Geo              string
	MaxPeople        int
	MaxSearches      int
	ResultsPerSearch int
	Delay            time.Duration // pause between searches (Python delay=0.2)
}

// PeopleFound is its result: people are {name, title, linkedin, evidence_url,
// evidence_title, evidence_snippet}; SearchFailures counts searches that failed
// outright (not "no results").
type PeopleFound struct {
	People         []map[string]any
	SearchesUsed   int
	SearchFailures int
}

// CrossLinked is the crosslinked provider's people discovery (WP-PROVIDERS
// registers it as providers.Get("crosslinked")). An error that wraps
// context.DeadlineExceeded means the search budget was exhausted.
type CrossLinked interface {
	FindPeopleByTitles(ctx context.Context, q PeopleQuery) (PeopleFound, error)
}

// IdentityResolver resolves one company name to its canonical identity (the
// WikidataProvider.resolve_identity dict). A nil map is an invalid response.
type IdentityResolver interface {
	ResolveIdentity(ctx context.Context, company string) (map[string]any, error)
}

// Searcher is the part of *leadweb.Searcher verification uses.
type Searcher interface {
	Text(ctx context.Context, query string, o leadweb.SearchOptions) []leadweb.Hit
}

// Service bundles the dependencies of research, verification and contact
// enrichment. Zero timeouts and a nil Now take the Python values.
type Service struct {
	CrossLinked CrossLinked      // nil: providers.Get("crosslinked") must implement CrossLinked
	Identity    IdentityResolver // nil: every company resolves as resolver_unavailable
	Search      Searcher
	Discover    EmailDiscoverer
	Verify      EmailVerifier

	// CrossLinkedName is the registry name looked up when CrossLinked is nil; "" = "crosslinked".
	CrossLinkedName string

	Workspace string // salts the search cache (tenant isolation); "" = none
	Now       func() time.Time
	Mono      func() time.Time // monotonic source for attempt durations (tests)

	ResearchTimeout time.Duration // 45s
	IdentityTimeout time.Duration // 15s
	SearchTimeout   time.Duration // 16s per verification search
	VerifyTimeout   time.Duration // 40s for the whole verification
	Concurrency     int           // 3 verifications / contact lookups at once
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) mono() time.Time {
	if s.Mono != nil {
		return s.Mono()
	}
	return time.Now()
}

func dur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (s *Service) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return 3
}

func (s *Service) crossLinked() (CrossLinked, error) {
	if s.CrossLinked != nil {
		return s.CrossLinked, nil
	}
	name := s.CrossLinkedName
	if name == "" {
		name = "crosslinked"
	}
	if p, ok := providers.Get(name); ok {
		if cl, ok := p.(CrossLinked); ok {
			return cl, nil
		}
	}
	return nil, ErrCrossLinkedUnavailable
}

// ErrorClass names an error the way Python's type(exc).__name__ did, for the
// error_class field of an unavailable resolver.
func ErrorClass(err error) string {
	var c interface{ ErrorClass() string }
	if errors.As(err, &c) {
		return c.ErrorClass()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TimeoutError"
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

// ResolveCompanyIdentity is resolve_company_identity: one target company
// resolved without guessing through an ambiguous name. A domain input resolves
// to itself; a name goes to the Identity resolver.
func (s *Service) ResolveCompanyIdentity(ctx context.Context, company string) map[string]any {
	company = trunc(clean(company), 120)
	if domainRe.MatchString(company) {
		raw := company
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		host, _ := pyURL(raw)
		host = strings.TrimPrefix(host, "www.")
		return map[string]any{
			"status": "resolved", "company": company, "canonical_domain": host, "website": "https://" + host,
			"confidence": 1.0, "observed_at": isoDate(s.now()), "evidence_url": "https://" + host,
			"source": "user_supplied_domain",
		}
	}
	if s.Identity == nil {
		return map[string]any{"status": "resolver_unavailable", "company": company, "canonical_domain": "",
			"error_class": "ResolverNotConfigured"}
	}
	rctx, cancel := context.WithTimeout(ctx, dur(s.IdentityTimeout, 15*time.Second))
	defer cancel()
	res, err := s.Identity.ResolveIdentity(rctx, company)
	if err != nil {
		return map[string]any{"status": "resolver_unavailable", "company": company, "canonical_domain": "",
			"error_class": ErrorClass(err)}
	}
	if res == nil {
		return map[string]any{"status": "invalid_response", "company": company, "canonical_domain": ""}
	}
	canonical := NormalizeDomain(providers.PyStrip(getStr(res, "canonical_domain")))
	out := make(map[string]any, len(res)+2)
	for k, v := range res {
		out[k] = v
	}
	out["company"] = company
	if res["status"] != "resolved" || canonical == "" {
		out["canonical_domain"] = ""
		return out
	}
	out["canonical_domain"] = canonical
	return out
}

// ResearchOptions are the keyword arguments of research_people_at_company.
type ResearchOptions struct {
	Titles   []string
	Location string
	Limit    int // 0 = 8; clamped to 1..15
	// Workspace is the calling workspace: it salts the finder's process-wide search cache.
	Workspace string
}

// ResearchPeopleAtCompany is research_people_at_company: evidence-gated people
// candidates from public LinkedIn search results, never persisted. A candidate
// needs an exact company-employment pattern AND a matching functional remit in
// its evidence text; search-query terms alone never count and no email is
// inferred. The error is non-nil only for infrastructure failures (the caller's
// ctx ended, the finder failed for a reason other than its time budget).
func (s *Service) ResearchPeopleAtCompany(ctx context.Context, company, function string, o ResearchOptions) (map[string]any, error) {
	company = trunc(clean(company), 120)
	function = trunc(clean(function), 100)
	if company == "" {
		return map[string]any{"ok": false, "error": "company_required", "people": []any{}, "count": 0}, nil
	}
	if function == "" {
		return map[string]any{"ok": false, "error": "function_required", "people": []any{}, "count": 0}, nil
	}
	limit := o.Limit
	if limit == 0 {
		limit = 8
	}
	limit = max(1, min(limit, 15))
	qTitles := queryTitles(function, o.Titles)

	idCtx, idCancel := context.WithCancel(ctx)
	defer idCancel()
	idDone := make(chan map[string]any, 1)
	go func() { idDone <- s.ResolveCompanyIdentity(idCtx, company) }()

	cl, err := s.crossLinked()
	if err != nil {
		idCancel()
		<-idDone
		return nil, err
	}
	maxPeople := max(limit*3, 15)
	fctx, fcancel := context.WithTimeout(ctx, dur(s.ResearchTimeout, 45*time.Second))
	found, err := cl.FindPeopleByTitles(fctx, PeopleQuery{
		Company: company, Titles: qTitles, Geo: trunc(providers.PyStrip(o.Location), 80),
		MaxPeople: maxPeople, MaxSearches: 5, ResultsPerSearch: 12, Delay: 200 * time.Millisecond,
	})
	fcancel()
	if err != nil {
		if ctx.Err() != nil {
			idCancel()
			<-idDone
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			resolution := <-idDone
			return map[string]any{
				"ok": false, "error": "people_research_timeout", "company": company, "function": function,
				"company_resolution": resolution, "people": []any{}, "count": 0,
				"message": "Targeted people research exceeded its 45-second safety limit.",
			}, nil
		}
		idCancel()
		<-idDone
		return nil, err
	}
	resolution := <-idDone
	if len(found.People) == 0 && found.SearchesUsed > 0 && found.SearchFailures >= found.SearchesUsed {
		// Every search errored: this is not evidence that nobody matches.
		return map[string]any{
			"ok": false, "error": "people_search_unavailable", "company": company, "function": function,
			"company_resolution": resolution, "people": []any{}, "count": 0,
			"searches_used": found.SearchesUsed, "search_failures": found.SearchFailures,
			"message": "Public people search failed for every query. No people were ruled out; retry later or configure a search backend.",
		}, nil
	}
	canonicalDomain := ""
	if resolution["status"] == "resolved" {
		canonicalDomain = strings.ToLower(providers.PyStrip(getStr(resolution, "canonical_domain")))
	}

	terms := functionTerms(function, qTitles)
	retrievedAt := isoDate(s.now())
	location := providers.PyStrip(o.Location)
	accepted := []map[string]any{}
	rejectedCompany, rejectedFunction := 0, 0
	aliases := companyAliases(company)
	for _, person := range found.People {
		evTitle := providers.PyStrip(getStr(person, "evidence_title"))
		evSnippet := providers.PyStrip(getStr(person, "evidence_snippet"))
		evidence := providers.PyStrip(evTitle + ". " + evSnippet)
		if !ExactCompanyRelationship(evidence, company) {
			rejectedCompany++
			continue
		}
		status, conf, ok := FunctionEvidenceStrength(evTitle, evSnippet, terms, company)
		if !ok {
			rejectedFunction++
			continue
		}
		title := providers.PyStrip(getStr(person, "title"))
		if strings.ToUpper(title) == "N/A" {
			title = ""
		}
		for _, a := range aliases {
			if strings.EqualFold(title, a) {
				title = ""
				break
			}
		}
		linkedin := getStr(person, "linkedin")
		evURL := firstStr(person, "evidence_url", "linkedin")
		cand := map[string]any{
			"name": providers.PyStrip(getStr(person, "name")), "title": title, "company": company,
			"canonical_company_domain": canonicalDomain, "company_resolution": resolution, "function": function,
			"location": location, "linkedin_url": linkedin, "evidence_url": evURL,
			"evidence_title": trunc(evTitle, 240), "evidence_snippet": trunc(evSnippet, 280),
			"retrieved_at": retrievedAt, "confidence": conf, "verification_status": status, "email": nil,
		}
		cand["person_id"] = PersonEntityID(company, cand)
		accepted = append(accepted, cand)
		if len(accepted) >= limit {
			break
		}
	}
	return map[string]any{
		"ok": true, "company": company, "function": function, "company_resolution": resolution,
		"result_set_id": PeopleResultSetID(company, function, accepted), "people": accepted, "count": len(accepted),
		"searches_used": found.SearchesUsed, "search_failures": found.SearchFailures,
		"candidates_rejected": map[string]any{
			"company_relationship_missing": rejectedCompany, "function_evidence_missing": rejectedFunction,
		},
		"methodology": "Public LinkedIn-profile search results; exact target-company employment " +
			"and function terms are both required. No email addresses are guessed.",
		"warning": "Public profile snippets can be stale. Treat matches as research candidates " +
			"and re-check the profile before outreach.",
	}, nil
}

var _ Searcher = (*leadweb.Searcher)(nil)
