package personintel

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// githubAPI is a var so tests can point at a local stub.
var githubAPI = "https://api.github.com"

// githubLoginRe is what GitHub accepts as a login (no underscore, max 39); a
// match from the URL that fails it is never put into the API path.
var githubLoginRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)

var githubUserRe = regexp.MustCompile(`github\.com/([a-zA-Z0-9_-]+)`)

// EnrichFromGitHub reads the public GitHub profile and recent repos of
// social_links["github"] (_enrich_from_github_api). Like Python every failure
// (transport error, non-200, bad JSON) is swallowed and leaves the profile as
// is; only a cancelled ctx is reported. A transport error on the user call
// skips the repos call.
func EnrichFromGitHub(ctx context.Context, c *http.Client, p *Profile) error {
	link, _ := p.social().Get("github")
	if link == "" {
		return nil
	}
	m := githubUserRe.FindStringSubmatch(link)
	if m == nil {
		return nil
	}
	user := m[1]
	if !githubLoginRe.MatchString(user) {
		return nil
	}
	hdr := [][2]string{{"Accept", "application/vnd.github.v3+json"}, {"User-Agent", "OpenGTM"}}

	res, err := providers.Do(ctx, c, providers.Req{Method: http.MethodGet, URL: githubAPI + "/users/" + user, Header: hdr})
	if err != nil {
		return ctx.Err()
	}
	if res.Status == http.StatusOK {
		var d struct {
			Email, Bio, Location, Company *string
		}
		if json.Unmarshal(res.Body, &d) == nil {
			if d.Email != nil && *d.Email != "" && !contains(p.Emails, *d.Email) {
				p.Emails = append(p.Emails, *d.Email)
			}
			if d.Bio != nil && *d.Bio != "" && p.Summary == "" {
				p.Summary = *d.Bio
			}
			if d.Location != nil && *d.Location != "" && p.Location == "" {
				p.Location = *d.Location
			}
			if d.Company != nil && *d.Company != "" {
				co := strings.TrimLeft(*d.Company, "@")
				if !contains(p.Companies, co) {
					p.Companies = append(p.Companies, co)
				}
			}
		}
	}

	rres, err := providers.Do(ctx, c, providers.Req{
		Method: http.MethodGet, URL: githubAPI + "/users/" + user + "/repos",
		Query:  [][2]string{{"sort", "updated"}, {"per_page", "10"}},
		Header: hdr,
	})
	if err != nil {
		return ctx.Err()
	}
	if rres.Status == http.StatusOK {
		var repos []struct {
			Language *string `json:"language"`
		}
		if json.Unmarshal(rres.Body, &repos) == nil {
			for _, r := range repos {
				if r.Language != nil && *r.Language != "" && !contains(p.Skills, *r.Language) {
					p.Skills = append(p.Skills, *r.Language)
				}
			}
		}
	}
	return nil
}
