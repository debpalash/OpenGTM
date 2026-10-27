// Package scrapers is the Go side of the legacy leadgen scrapers (apps/api/services/leadgen/
// scrapers) the collection pipeline still runs: Google Maps discovery (google_maps.py
// scrape_google_maps) and the LinkedIn decision-maker fallback (linkedin.py find_decision_makers).
// The browser service does the page and search work and returns raw places / hits; request
// building, mapping into leads and result parsing stay here. The other legacy scrapers (job
// boards, review and web directories, Crunchbase, Google search, CSV import) were reachable only
// from the dropped dev pipeline (leadgen/pipeline.run_scrapers, plan 15 inventory G2) and are not
// ported.
package scrapers

import (
	"context"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
)

// Browser is the part of *browserclient.Client the scrapers use (fakes in tests).
type Browser interface {
	GoogleMaps(ctx context.Context, req browserclient.GoogleMapsRequest) (browserclient.GoogleMapsResult, error)
	LinkedIn(ctx context.Context, req browserclient.LinkedInRequest) (browserclient.SearchResult, error)
}

// Place is one Google Maps listing mapped the way scrape_google_maps builds its Lead.
type Place struct {
	Company        string
	Website        string
	Phone          string
	City           string
	Specialization string
	Notes          string
	Source         string
}

// MapsTimeout bounds one Maps scrape in the browser service (legacy: a 20 s navigation plus about
// 2 s per listing for 20 listings, inside the 180 s strategy budget).
const MapsTimeout = 150 * time.Second

// GoogleMaps is scrape_google_maps(query, city, max_results): "<query> in <city>" on Google Maps,
// one lead per distinct listing name (company, website, phone, city, category or the query as
// specialization, "Address: ..." notes, source google_maps). A service error is returned; the
// caller's strategy reports it and yields no leads, as legacy did on any scrape exception.
func GoogleMaps(ctx context.Context, b Browser, query, city string, maxResults int, proxy browserclient.ProxyURL) ([]Place, error) {
	res, err := b.GoogleMaps(ctx, browserclient.GoogleMapsRequest{Query: query, City: city, MaxResults: maxResults,
		UpstreamProxy: proxy, Timeout: MapsTimeout})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]Place, 0, len(res.Places))
	for _, p := range res.Places {
		if p.Name == "" || seen[p.Name] { // the service already dedupes; kept as legacy's guard
			continue
		}
		seen[p.Name] = true
		spec := p.Category
		if spec == "" {
			spec = query
		}
		notes := ""
		if p.Address != "" {
			notes = "Address: " + p.Address
		}
		out = append(out, Place{Company: p.Name, Website: p.Website, Phone: p.Phone, City: city,
			Specialization: spec, Notes: notes, Source: "google_maps"})
	}
	return out, nil
}

// Person is one decision maker of find_decision_makers.
type Person struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	LinkedinURL string `json:"linkedin_url"`
}

// FindDecisionMakers is linkedin.find_decision_makers(company, titles, max_results): one
// 'site:linkedin.com/in "<company>" "<title>" India' search per title (the service's legacy title
// list when titles is empty, two hits each, 1.5 s apart), and per title the first hit whose URL
// is a /in/ profile and whose title's name part is not "LinkedIn". Fix: legacy ignored
// max_results; here at most maxResults people are returned (0 = no cap).
func FindDecisionMakers(ctx context.Context, b Browser, company string, titles []string, maxResults int,
	proxy browserclient.ProxyURL) ([]Person, error) {
	res, err := b.LinkedIn(ctx, browserclient.LinkedInRequest{Mode: "people", CompanyName: company, Titles: titles,
		SearchOptions: browserclient.SearchOptions{MaxResults: 2, UpstreamProxy: proxy}})
	if err != nil {
		return nil, err
	}
	var people []Person
	for _, run := range res.Runs {
		for _, h := range run.Hits {
			if !strings.Contains(h.URL, "/in/") {
				continue
			}
			name := strings.Split(strings.Split(h.Title, " - ")[0], " | ")[0]
			name = strings.TrimFunc(name, isSpace)
			if name != "" && strings.ToLower(name) != "linkedin" {
				people = append(people, Person{Name: name, Title: run.Term, LinkedinURL: h.URL})
				break
			}
		}
		if maxResults > 0 && len(people) >= maxResults {
			break
		}
	}
	return people, nil
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
