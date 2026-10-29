package personintel

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ws approximates Python's unicode \s (Go's \s is ASCII only).
const ws = `\s\p{Z}\x{85}\x1c-\x1f`

// EmailPattern is EMAIL_PATTERN.
const EmailPattern = `[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`

var (
	emailRe    = regexp.MustCompile(EmailPattern)
	locationRe = regexp.MustCompile(`(?i)(?:based in|located in|from)[` + ws + `]+([A-Z][a-zA-Z` + ws + `,]+)`)
	companyRes = []*regexp.Regexp{
		regexp.MustCompile(`(?:at|@)[` + ws + `]+([A-Z][a-zA-Z` + ws + `&]+?)(?:[` + ws + `]*[-·|]|[` + ws + `]*$)`),
		regexp.MustCompile(`(?:works?[` + ws + `]+(?:at|for))[` + ws + `]+([A-Z][a-zA-Z` + ws + `&]+)`),
	}
	socialRes = map[string][]*regexp.Regexp{
		"twitter": {regexp.MustCompile(`(?:twitter|x)\.com/([a-zA-Z0-9_]+)`)},
		"github":  {regexp.MustCompile(`github\.com/([a-zA-Z0-9_-]+)`)},
		"blog": {
			regexp.MustCompile(`medium\.com/@?([a-zA-Z0-9_-]+)`),
			regexp.MustCompile(`([a-zA-Z0-9_-]+)\.substack\.com`),
		},
	}
	junkEmails = []string{"example.com", "email.com", "test.com", "domain.com", "yourname", "sentry.io", "wixpress"}
)

// ExtractLinkedIn fills name, headline, summary, location and companies from
// LinkedIn search results (_extract_linkedin_data). Fields already set are kept.
func ExtractLinkedIn(p *Profile, rs []Result) {
	for _, r := range rs {
		if !strings.Contains(r.URL, "linkedin.com/in/") {
			continue
		}
		parts := strings.ReplaceAll(strings.ReplaceAll(r.Title, " | LinkedIn", ""), " - LinkedIn", "")
		var segs []string
		for _, s := range strings.Split(parts, " - ") {
			if s = strings.TrimSpace(s); s != "" {
				segs = append(segs, s)
			}
		}
		if len(segs) > 0 {
			if p.Name == "" {
				p.Name = segs[0]
			}
			if len(segs) > 1 && p.Headline == "" {
				p.Headline = strings.Join(segs[1:], " - ")
			}
		}
		if r.Snippet != "" && p.Summary == "" {
			p.Summary = r.Snippet
		}
		if m := locationRe.FindStringSubmatch(r.Snippet); m != nil && p.Location == "" {
			p.Location = strings.TrimSpace(m[1])
		}
		for _, re := range companyRes {
			for _, m := range re.FindAllStringSubmatch(r.Snippet, -1) {
				c := strings.TrimSpace(m[1])
				if c != "" && !contains(p.Companies, c) && utf8.RuneCountInString(c) > 2 {
					p.Companies = append(p.Companies, c)
				}
			}
		}
	}
}

// ExtractSocial records the first matching profile URL per platform
// (_extract_social_links); existing links are kept. Platforms are independent,
// so the map order does not matter, but a new key is appended after existing
// ones in twitter, github, blog order (the order Python passes them in).
func ExtractSocial(p *Profile, results map[string][]Result) {
	for _, platform := range []string{"twitter", "github", "blog"} {
		for _, r := range results[platform] {
			for _, re := range socialRes[platform] {
				if re.MatchString(r.URL) {
					if !p.social().Has(platform) {
						p.social().Set(platform, r.URL)
					}
					goto next
				}
			}
			continue
		next:
			break
		}
	}
}

func orWeb(s string) string {
	if s == "" {
		return "web"
	}
	return s
}

// ExtractArticlesAndMentions splits results into authored articles and
// mentions (_extract_articles_and_mentions).
func ExtractArticlesAndMentions(p *Profile, articles, general []Result) {
	seen := map[string]struct{}{}
	kws := []string{"by " + strings.ToLower(p.Username), "author", "published", "wrote", "presentation by", "talk by"}
	for _, r := range articles {
		if _, ok := seen[r.URL]; ok {
			continue
		}
		seen[r.URL] = struct{}{}
		e := Entry{Title: r.Title, URL: r.URL, Snippet: r.Snippet, Source: orWeb(r.Source)}
		text := strings.ToLower(e.Title + " " + e.Snippet)
		authored := false
		for _, kw := range kws {
			if strings.Contains(text, kw) {
				authored = true
				break
			}
		}
		if authored {
			p.Articles = append(p.Articles, e)
		} else {
			p.Mentions = append(p.Mentions, e)
		}
	}
	for _, r := range general {
		if _, ok := seen[r.URL]; ok || strings.Contains(r.URL, "linkedin.com") {
			continue
		}
		seen[r.URL] = struct{}{}
		p.Mentions = append(p.Mentions, Entry{Title: r.Title, URL: r.URL, Snippet: r.Snippet, Source: orWeb(r.Source)})
	}
}

// ExtractEmails adds every address found in snippets and scraped text that is
// not junk (_extract_all_emails). Python iterates a set (unspecified order);
// Go appends in first-occurrence order, after the emails already present.
func ExtractEmails(p *Profile, rs []Result) {
	var b strings.Builder
	for i, r := range rs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(r.Snippet + " " + r.ScrapedText)
	}
	seen := map[string]struct{}{}
	for _, e := range emailRe.FindAllString(b.String(), -1) {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		low := strings.ToLower(e)
		junk := false
		for _, j := range junkEmails {
			if strings.Contains(low, j) {
				junk = true
				break
			}
		}
		if junk || contains(p.Emails, e) {
			continue
		}
		p.Emails = append(p.Emails, e)
	}
}
