package personintel

import (
	"errors"
	"regexp"
	"strings"
)

var usernameRe = regexp.MustCompile(`linkedin\.com/in/([a-zA-Z0-9_-]+)`)

// ParseUsername extracts the LinkedIn username (parse_linkedin_username).
func ParseUsername(rawURL string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if m := usernameRe.FindStringSubmatch(u); m != nil {
		return m[1], nil
	}
	return "", errors.New("Could not parse LinkedIn username from: " + u)
}

// ValidLinkedInURL is the route gate: the raw (unstripped) string must contain
// "linkedin.com/in/", case-sensitively.
func ValidLinkedInURL(u string) bool { return strings.Contains(u, "linkedin.com/in/") }

// BuildQueries returns the categorised search queries in Python's dict order.
func BuildQueries(username string) *Obj {
	q := &Obj{}
	q.Set("linkedin", `"`+username+`" site:linkedin.com/in`)
	q.Set("general", `"`+username+`" linkedin`)
	q.Set("email", `"`+username+`" email contact`)
	q.Set("twitter", `"`+username+`" site:twitter.com OR site:x.com`)
	q.Set("github", `"`+username+`" site:github.com`)
	q.Set("blog", `"`+username+`" site:medium.com OR site:substack.com OR blog`)
	q.Set("articles", `"`+username+`" article OR presentation OR conference OR speaker`)
	return q
}
