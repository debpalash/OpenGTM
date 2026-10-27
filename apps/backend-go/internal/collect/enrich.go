package collect

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// The enrichment modules the pipeline runs in process: website contact scraping
// (enrichment/website_scraper.py), decision-maker search (enrichment/decision_maker_finder.py) and
// the email finder (enrichment/email_finder.py).

// ── website_scraper.enrich_leads_from_websites ──────────────────────────────

// EnrichFromWebsites scrapes the site (home, /contact, /contact-us, /about) of every lead that has
// a website but lacks a phone or an email, batchSize leads at a time in parallel. The website is
// reduced to scheme://host first; found values only fill empty fields.
func EnrichFromWebsites(ctx context.Context, w Web, leads []*Lead, batchSize int) {
	var needs []*Lead
	for _, l := range leads {
		if l.HasWebsite() && (!l.HasPhone() || !l.HasEmail()) {
			needs = append(needs, l)
		}
	}
	for i := 0; i < len(needs); i += batchSize {
		if ctx.Err() != nil {
			return
		}
		batch := needs[i:min(i+batchSize, len(needs))]
		res := make([]leadweb.SiteContacts, len(batch))
		var wg sync.WaitGroup
		for k, l := range batch {
			l.Website = NormalizeURL(l.Website)
			wg.Add(1)
			go func(k int, site string) {
				defer wg.Done()
				res[k] = leadweb.ScanSite(site, func(page string) (string, bool) {
					return w.Fetch(ctx, page, 2, 12*time.Second)
				})
			}(k, l.Website)
		}
		wg.Wait()
		for k, l := range batch {
			r := res[k]
			if len(r.Phones) > 0 && !l.HasPhone() {
				l.Phone = r.Phones[0]
			}
			if len(r.Emails) > 0 && !l.HasEmail() {
				l.Email = r.Emails[0]
			}
			if r.Social.LinkedIn != "" && !l.HasLinkedin() {
				l.LinkedinURL = r.Social.LinkedIn
			}
			if r.Social.Twitter != "" && l.TwitterURL == "" {
				l.TwitterURL = r.Social.Twitter
			}
			if r.Description != "" && l.Description == "" {
				l.Description = r.Description
			}
		}
	}
}

// ── decision_maker_finder ───────────────────────────────────────────────────

// dmTitles is TARGET_TITLES, ordered by decision-making authority.
var dmTitles = []string{"Founder", "CEO", "Managing Director", "CHRO", "HR Director", "CTO", "COO", "VP HR"}

type decisionMaker struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Linkedin string `json:"linkedin"`
}

func searchDecisionMaker(ctx context.Context, w Web, company, title string) *decisionMaker {
	q := `site:linkedin.com/in "` + company + `" "` + title + `" India`
	for _, r := range w.Search(ctx, q, 2) {
		if !strings.Contains(r.Href, "/in/") {
			continue
		}
		name := pyStrip(strings.Split(strings.Split(r.Title, " - ")[0], " | ")[0])
		if name == "" || pyLower(name) == "linkedin" {
			continue
		}
		if n := runeLen(name); n < 3 || n > 60 {
			continue
		}
		return &decisionMaker{Name: name, Title: title, Linkedin: strings.TrimRight(strings.Split(r.Href, "?")[0], "/")}
	}
	return nil
}

// FindDecisionMakersForLead is find_decision_makers_for_lead: up to maxContacts distinct people
// over the target titles, 1.5 s apart; the first becomes the contact person (and LinkedIn URL)
// when the lead has none.
func FindDecisionMakersForLead(ctx context.Context, w Web, sleep func(context.Context, time.Duration) error, l *Lead, maxContacts int) {
	if l.Company == "" || runeLen(l.Company) < 3 {
		return
	}
	if l.DecisionMakers != "" {
		var existing []any
		if json.Unmarshal([]byte(l.DecisionMakers), &existing) == nil && len(existing) >= maxContacts {
			return
		}
	}
	var found []decisionMaker
	for _, title := range dmTitles {
		if len(found) >= maxContacts || ctx.Err() != nil {
			break
		}
		if r := searchDecisionMaker(ctx, w, l.Company, title); r != nil {
			dup := false
			for _, f := range found {
				if pyLower(f.Name) == pyLower(r.Name) {
					dup = true
					break
				}
			}
			if !dup {
				found = append(found, *r)
			}
		}
		if sleep(ctx, 1500*time.Millisecond) != nil {
			break
		}
	}
	if len(found) == 0 {
		return
	}
	l.DecisionMakers = pyDumps(dmList(found))
	if l.ContactPerson == "" {
		l.ContactPerson, l.ContactTitle = found[0].Name, found[0].Title
	}
	if l.LinkedinURL == "" && found[0].Linkedin != "" {
		l.LinkedinURL = found[0].Linkedin
	}
}

func dmList(ds []decisionMaker) []any {
	out := make([]any, len(ds))
	for i, d := range ds {
		out[i] = dict{{"name", d.Name}, {"title", d.Title}, {"linkedin", d.Linkedin}}
	}
	return out
}

// EnrichDecisionMakers is enrich_decision_makers: leads with a company and neither decision makers
// nor a contact person, concurrency at a time.
func EnrichDecisionMakers(ctx context.Context, w Web, sleep func(context.Context, time.Duration) error, leads []*Lead, concurrency, maxContacts int) {
	sem := make(chan struct{}, max(concurrency, 1))
	var wg sync.WaitGroup
	for _, l := range leads {
		if l.Company == "" || l.DecisionMakers != "" || l.ContactPerson != "" {
			continue
		}
		wg.Add(1)
		go func(l *Lead) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			FindDecisionMakersForLead(ctx, w, sleep, l, maxContacts)
		}(l)
	}
	wg.Wait()
}

// ── email_finder ────────────────────────────────────────────────────────────

var finderEmailRe = mustRx(`[\w\.\-]+@[\w\.\-]+\.\w{2,}`)

// DomainFromURL is email_finder._domain_from_url.
func DomainFromURL(u string) string {
	if u == "" {
		return ""
	}
	u = pyStrip(pyLower(u))
	if !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	return strings.TrimPrefix(urlparse(u).Netloc, "www.")
}

func splitName(full string) (string, string) {
	parts := pySplit(full)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return pyLower(parts[0]), ""
	}
	return pyLower(parts[0]), pyLower(parts[len(parts)-1])
}

// PersonalPatterns is generate_personal_patterns.
func PersonalPatterns(first, last, domain string) []string {
	if first == "" || domain == "" {
		return nil
	}
	fi := string([]rune(first)[0])
	if last == "" {
		return []string{first + "@" + domain}
	}
	li := string([]rune(last)[0])
	return []string{first + "." + last + "@" + domain, first + last + "@" + domain, fi + last + "@" + domain,
		first + li + "@" + domain, first + "_" + last + "@" + domain, last + "." + first + "@" + domain,
		fi + "." + last + "@" + domain, last + fi + "@" + domain, first + "-" + last + "@" + domain, last + "@" + domain,
		first + "@" + domain, fi + li + "@" + domain, last + "_" + first + "@" + domain}
}

func personalViaSearch(ctx context.Context, w Web, name, company, domain string) string {
	if name == "" || domain == "" {
		return ""
	}
	first, last := splitName(name)
	if first == "" {
		return ""
	}
	q := `"` + name + `" email @` + domain
	if company != "" {
		q += ` "` + company + `"`
	}
	for _, r := range w.Search(ctx, q, 5) {
		for _, e := range finderEmailRe.FindAll(r.Title + " " + r.Body) {
			low := pyLower(e)
			if strings.Contains(low, pyLower(domain)) && (strings.Contains(low, first) || (last != "" && strings.Contains(low, last))) {
				return e
			}
		}
	}
	return ""
}

// FindPersonalEmail is find_personal_email: a search hit naming the person at the domain
// ("verified"); else, when the domain has MX, the SMTP-confirmed pattern ("smtp_verified" or what
// the probe says), nothing when the server rejected every pattern, else the most common pattern
// ("pattern").
func FindPersonalEmail(ctx context.Context, w Web, e Enrichers, name, company, domain string) (string, string) {
	if name == "" || domain == "" {
		return "", ""
	}
	if email := personalViaSearch(ctx, w, name, company, domain); email != "" {
		return email, "verified"
	}
	if !e.HasMX(ctx, domain) {
		return "", ""
	}
	first, last := splitName(name)
	patterns := PersonalPatterns(first, last, domain)
	if len(patterns) == 0 {
		return "", ""
	}
	email, conf, deliverable := e.PickBestEmail(ctx, patterns)
	if email != "" && conf != "" {
		return email, conf
	}
	if deliverable != nil && !*deliverable {
		return "", ""
	}
	return patterns[0], "pattern"
}

// EnrichPersonalEmails is enrich_personal_emails: for leads with a contact person and a website
// whose email is not already personal ("verified" / "pattern"), find the person's address; it
// replaces the email (the old one is kept in secondary_emails) and is copied into the matching
// decision maker.
func EnrichPersonalEmails(ctx context.Context, w Web, e Enrichers, sleep func(context.Context, time.Duration) error, leads []*Lead, delay time.Duration) {
	for _, l := range leads {
		if ctx.Err() != nil {
			return
		}
		if l.ContactPerson == "" || l.Website == "" {
			continue
		}
		domain := DomainFromURL(l.Website)
		if domain == "" || l.EmailConfidence == "verified" || l.EmailConfidence == "pattern" {
			continue
		}
		email, conf := FindPersonalEmail(ctx, w, e, l.ContactPerson, l.Company, domain)
		if email != "" && conf != "" {
			old := l.Email
			l.Email, l.EmailConfidence = email, conf
			if l.DecisionMakers != "" {
				if dms, ok := decodeDicts(l.DecisionMakers); ok {
					for _, dm := range dms {
						if n, _ := dm.get("name"); pyLower(pyStr(n)) == pyLower(l.ContactPerson) {
							dm.set("email", email)
							dm.set("email_confidence", conf)
						}
					}
					l.DecisionMakers = pyDumps(dictsAny(dms))
				}
			}
			if old != "" && old != email && !strings.Contains(l.SecondaryEmails, old) {
				if l.SecondaryEmails != "" {
					l.SecondaryEmails += "|" + old
				} else {
					l.SecondaryEmails = old
				}
			}
		}
		_ = sleep(ctx, delay)
	}
}

// FindEmailViaSearch is find_email_via_search: an address in the search hits, preferring the
// company's domain, skipping placeholder addresses.
func FindEmailViaSearch(ctx context.Context, w Web, company, city, domain string) string {
	q := company + " email contact"
	if city != "" {
		q += " " + city
	}
	if domain != "" {
		q += " @" + domain
	}
	for _, r := range w.Search(ctx, q, 5) {
		emails := finderEmailRe.FindAll(r.Title + " " + r.Body)
		for _, e := range emails {
			if domain != "" && strings.Contains(pyLower(e), domain) {
				return e
			}
		}
		for _, e := range emails {
			if low := pyLower(e); !anyIn(low, []string{"example", "domain", "email.com"}) {
				return e
			}
		}
	}
	return ""
}

// EnrichEmails is enrich_emails for one lead (bulk "find_emails"): a searched address ("verified",
// disposable domains skipped), else the SMTP-checked info@<domain> guess ("smtp_verified" when the
// mailbox answered, "generic" when unknown, nothing when the server rejected it).
func EnrichEmails(ctx context.Context, w Web, e Enrichers, l *Lead) {
	if l.HasEmail() || l.Company == "" {
		return
	}
	domain := DomainFromURL(l.Website)
	if email := FindEmailViaSearch(ctx, w, l.Company, l.City, domain); email != "" {
		if e.Disposable(email) {
			return
		}
		l.Email, l.EmailConfidence = email, "verified"
		return
	}
	if domain == "" {
		return
	}
	guess := "info@" + domain
	d := e.VerifyEmail(ctx, guess)
	if d != nil && !*d {
		return
	}
	l.Email = guess
	l.EmailConfidence = "generic"
	if d != nil && *d {
		l.EmailConfidence = "smtp_verified"
	}
}

// ── ordered JSON object lists (decision_makers) ─────────────────────────────

type odict struct{ d dict }

func (o *odict) get(k string) (any, bool) { return o.d.get(k) }

func (o *odict) set(k string, v any) {
	for i := range o.d {
		if o.d[i].K == k {
			o.d[i].V = v
			return
		}
	}
	o.d = append(o.d, kv{k, v})
}

// decodeDicts decodes a JSON array of objects keeping key order; ok false when the text is not
// such an array (legacy: json.loads raising or a non-list skips the update).
func decodeDicts(s string) ([]*odict, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var raw []json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	out := make([]*odict, 0, len(raw))
	for _, r := range raw {
		d, ok := decodeOrdered(r)
		if !ok {
			return nil, false
		}
		out = append(out, &odict{d})
	}
	return out, true
}

func decodeOrdered(raw json.RawMessage) (dict, bool) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return nil, false
	}
	var d dict
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		d = append(d, kv{kt.(string), rawValue(v)})
	}
	return d, true
}

func rawValue(v json.RawMessage) any {
	if d, ok := decodeOrdered(v); ok {
		return d
	}
	dec := json.NewDecoder(strings.NewReader(string(v)))
	dec.UseNumber()
	var x any
	_ = dec.Decode(&x)
	switch n := x.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		f, _ := n.Float64()
		return f
	case []any:
		for i := range n {
			b, _ := json.Marshal(n[i])
			n[i] = rawValue(b)
		}
		return n
	}
	return x
}

func dictsAny(ds []*odict) []any {
	out := make([]any, len(ds))
	for i, d := range ds {
		out[i] = d.d
	}
	return out
}
