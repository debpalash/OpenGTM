package personintel

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ddgEndpoint is a var so tests can point at a local stub.
var ddgEndpoint = "https://lite.duckduckgo.com/lite/"

// MaxParallelSearches bounds concurrent DuckDuckGo requests.
const MaxParallelSearches = 8

var skipDomains = map[string]struct{}{
	"google.com": {}, "duckduckgo.com": {}, "bing.com": {}, "yahoo.com": {},
	"facebook.com": {}, "instagram.com": {}, "pinterest.com": {}, "tiktok.com": {},
	"youtube.com": {}, "wikipedia.org": {}, "amazon.com": {}, "ebay.com": {},
}

// SearchDDGLite scrapes DuckDuckGo Lite. Like Python a non-200 answer yields
// no results and no error; transport failures and bodies over the 1 MiB cap
// are returned as errors (the caller treats them as an empty category).
func SearchDDGLite(ctx context.Context, c *http.Client, q string, max int) ([]Result, error) {
	form := url.Values{"q": {q}, "kl": {"wt-wt"}}
	resp, err := providers.Do(ctx, c, providers.Req{
		Method: http.MethodPost,
		URL:    ddgEndpoint,
		Header: [][2]string{
			{"User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36"},
			{"Content-Type", "application/x-www-form-urlencoded"},
			{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		},
		Body: []byte(form.Encode()),
	})
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return []Result{}, nil
	}
	return parseDDGLite(resp.Body, max), nil
}

func parseDDGLite(body []byte, max int) []Result {
	results := []Result{}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return results
	}
	var links []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" && hasClass(n, "result-url") {
			links = append(links, n)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)
	for _, a := range links {
		title := textStrip(a)
		href := attr(a, "href")
		if !strings.HasPrefix(href, "http") && strings.HasPrefix(href, "//") {
			href = "https:" + href
		}
		snippet := ""
		if tr := parentTR(a); tr != nil {
			if next := nextTR(tr); next != nil {
				if td := findTD(next, "result-snippet"); td != nil {
					snippet = textStrip(td)
				}
			}
		}
		if title == "" || href == "" {
			continue
		}
		domain := strings.ReplaceAll(netloc(href), "www.", "")
		if _, skip := skipDomains[domain]; skip || strings.Contains(domain, "duckduckgo.com") {
			continue
		}
		if strings.Contains(href, "linkedin.com/in") {
			if i := strings.Index(href, "?"); i >= 0 {
				href = href[:i]
			}
			href = strings.TrimRight(href, "/")
		}
		dup := false
		for _, r := range results {
			if r.URL == href {
				dup = true
				break
			}
		}
		if !dup {
			results = append(results, Result{Title: cut(title, 200), URL: href, Snippet: cut(snippet, 500), Source: "duckduckgo"})
		}
		if len(results) >= max {
			break
		}
	}
	return results
}

// netloc mimics urllib.parse.urlparse(u).netloc without validation.
func netloc(u string) string {
	rest := u
	if i := strings.Index(u, ":"); i > 0 {
		ok := true
		for j, r := range u[:i] {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || (j > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'))) {
				ok = false
			}
		}
		if ok {
			rest = u[i+1:]
		}
	}
	if !strings.HasPrefix(rest, "//") {
		return ""
	}
	rest = rest[2:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func attr(n *html.Node, k string) string {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, c string) bool {
	for _, f := range strings.Fields(attr(n, "class")) {
		if f == c {
			return true
		}
	}
	return false
}

// textStrip is BeautifulSoup get_text(strip=True): each text node stripped,
// joined with no separator.
func textStrip(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(strings.TrimSpace(n.Data))
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

func parentTR(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "tr" {
			return p
		}
	}
	return nil
}

func nextTR(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode && s.Data == "tr" {
			return s
		}
	}
	return nil
}

func findTD(n *html.Node, class string) *html.Node {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && ch.Data == "td" && hasClass(ch, class) {
			return ch
		}
		if f := findTD(ch, class); f != nil {
			return f
		}
	}
	return nil
}

// CategoryResults is the outcome of one named search.
type CategoryResults struct {
	Category string
	Results  []Result
}

// batchSize and batchDelay mirror Python's staggering (batches of 3, 1 s
// apart) to avoid DuckDuckGo rate limiting. batchDelay is a var for tests.
const batchSize = 3

var batchDelay = time.Second

// RunSearches runs the queries in batches of batchSize (concurrent inside a
// batch, batchDelay between batches, so never more than MaxParallelSearches in
// flight) and returns the categories in query order. A failed search yields an
// empty category (as Python); a cancelled ctx stops the remaining batches.
// progress (optional) is called once per finished search, possibly
// concurrently.
func RunSearches(ctx context.Context, c *http.Client, queries *Obj, max int, progress func(category string, count int)) []CategoryResults {
	keys := queries.Keys()
	out := make([]CategoryResults, len(keys))
	for i, k := range keys {
		out[i] = CategoryResults{Category: k, Results: []Result{}}
	}
	for start := 0; start < len(keys) && ctx.Err() == nil; start += batchSize {
		end := min(start+batchSize, len(keys))
		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			qs, _ := queries.Get(keys[i])
			wg.Add(1)
			go func(i int, q string) {
				defer wg.Done()
				r, err := SearchDDGLite(ctx, c, q, max)
				if err != nil || r == nil {
					r = []Result{}
				}
				out[i].Results = r
				if progress != nil {
					progress(keys[i], len(r))
				}
			}(i, qs)
		}
		wg.Wait()
		if end < len(keys) {
			t := time.NewTimer(batchDelay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
			}
		}
	}
	return out
}
