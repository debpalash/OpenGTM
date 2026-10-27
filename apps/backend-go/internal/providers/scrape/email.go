package scrape

import (
	"context"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ── enrichment/email_finder.py ─────────────────────────────────────────────

func splitName(full string) (first, last string) {
	parts := pySplit(pyStrip(full))
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return strings.ToLower(parts[0]), ""
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[len(parts)-1])
}

// personalPatterns is generate_personal_patterns.
func personalPatterns(first, last, domain string) []string {
	if first == "" || domain == "" {
		return nil
	}
	fi := trunc(first, 1)
	if last == "" {
		return []string{first + "@" + domain}
	}
	li := trunc(last, 1)
	at := "@" + domain
	return []string{first + "." + last + at, first + last + at, fi + last + at, first + li + at, first + "_" + last + at,
		last + "." + first + at, fi + "." + last + at, last + fi + at, first + "-" + last + at, last + at, first + at,
		fi + li + at, last + "_" + first + at}
}

// personalViaSearch is find_personal_email_via_search.
func personalViaSearch(ctx context.Context, s Search, name, company, domain string) string {
	if name == "" || domain == "" {
		return ""
	}
	first, last := splitName(name)
	if first == "" {
		return ""
	}
	parts := []string{`"` + name + `"`, "email", "@" + domain}
	if company != "" {
		parts = append(parts, `"`+company+`"`)
	}
	for _, r := range s.Text(ctx, strings.Join(parts, " "), 5) {
		for _, e := range snippetEmailRe.FindAllString(r.Title+" "+r.Body, -1) {
			el := strings.ToLower(e)
			if strings.Contains(el, strings.ToLower(domain)) &&
				(strings.Contains(el, first) || (last != "" && strings.Contains(el, last))) {
				return e
			}
		}
	}
	return ""
}

// findPersonalEmail is find_personal_email: search, MX, then SMTP over the
// pattern candidates.
func findPersonalEmail(ctx context.Context, d *Deps, name, company, domain string) (string, string, error) {
	if name == "" || domain == "" {
		return "", "", nil
	}
	if e := personalViaSearch(ctx, d.Search, name, company, domain); e != "" {
		return e, "verified", nil
	}
	if d.Mail == nil || !d.Mail.HasMX(ctx, domain) {
		return "", "", ctx.Err()
	}
	first, last := splitName(name)
	pats := personalPatterns(first, last, domain)
	if len(pats) == 0 {
		return "", "", nil
	}
	r, err := d.Mail.PickBest(ctx, pats)
	if err != nil {
		return "", "", err
	}
	if r.Email != "" && r.Confidence != "" {
		return r.Email, r.Confidence, nil
	}
	if r.Deliverable != nil && !*r.Deliverable {
		return "", "", nil
	}
	return pats[0], "pattern", nil
}

// emailViaSearch is find_email_via_search.
func emailViaSearch(ctx context.Context, s Search, company, city, domain string) string {
	parts := []string{company, "email", "contact"}
	if city != "" {
		parts = append(parts, city)
	}
	if domain != "" {
		parts = append(parts, "@"+domain)
	}
	for _, r := range s.Text(ctx, strings.Join(parts, " "), 5) {
		emails := snippetEmailRe.FindAllString(r.Title+" "+r.Body, -1)
		for _, e := range emails {
			if domain != "" && strings.Contains(strings.ToLower(e), domain) {
				return e
			}
		}
		for _, e := range emails {
			if !containsAny(strings.ToLower(e), []string{"example", "domain", "email.com"}) {
				return e
			}
		}
	}
	return ""
}

// ── ddg_email_provider.py ──────────────────────────────────────────────────

type ddgEmail struct{ gated }

func (ddgEmail) Name() string { return "ddg_email" }

func (ddgEmail) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company, website := ls(in.Lead, "company"), ls(in.Lead, "website")
	if company == "" && website == "" {
		return fail(in, "No company name or website available"), nil
	}
	d := current()
	if d.Search == nil {
		return fail(in, errNoWeb), nil
	}
	domain := ""
	if website != "" {
		domain = domainFromURL(website)
	}
	if person := ls(in.Lead, "contact_person"); person != "" && domain != "" {
		email, conf, err := findPersonalEmail(ctx, d, person, company, domain)
		if err != nil {
			return providers.Output{}, err
		}
		if email != "" {
			c := 0.4
			if conf == "verified" {
				c = 0.7
			}
			return ok(in, c, "email", email, "email_confidence", conf), nil
		}
	}
	if email := emailViaSearch(ctx, d.Search, company, ls(in.Lead, "city"), domain); email != "" {
		return ok(in, 0.6, "email", email, "email_confidence", "verified"), nil
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	if domain != "" {
		return ok(in, 0.2, "email", "info@"+domain, "email_confidence", "generic"), nil
	}
	return fail(in, "No email found"), nil
}

// ── mailscout_verify.py ────────────────────────────────────────────────────

type mailScout struct{ gated }

func (mailScout) Name() string { return "mailscout" }

func (mailScout) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	email := ls(in.Lead, "email")
	if email == "" || !strings.Contains(email, "@") {
		return fail(in, "no_email_to_verify"), nil
	}
	m := current().Mail
	if m == nil {
		return fail(in, "smtp_unverified"), nil
	}
	r, err := m.Verify(ctx, email)
	if err != nil {
		return providers.Output{}, err
	}
	if r.Confidence == "smtp_verified" {
		return ok(in, 0.95, "email", email, "email_confidence", "smtp_verified"), nil
	}
	if r.CatchAll {
		return ok(in, 0.5, "email", email, "email_confidence", "pattern"), nil
	}
	person, website := ls(in.Lead, "contact_person"), ls(in.Lead, "website")
	if r.Deliverable != nil && !*r.Deliverable && person != "" && website != "" {
		if domain := domainFromURL(website); domain != "" {
			first, last := splitName(person)
			pats := personalPatterns(first, last, domain)
			if len(pats) > 3 {
				pats = pats[:3]
			}
			picked, err := m.PickBest(ctx, pats)
			if err != nil {
				return providers.Output{}, err
			}
			if picked.Confidence == "smtp_verified" {
				return ok(in, 0.9, "email", picked.Email, "email_confidence", "smtp_verified"), nil
			}
		}
	}
	return fail(in, "smtp_unverified"), nil
}
