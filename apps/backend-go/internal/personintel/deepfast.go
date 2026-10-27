package personintel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

const (
	// maxPageBytes caps one fetched page body; the rest is dropped.
	maxPageBytes = 2 << 20
	// pageFetchTimeout bounds one page fetch, redirects included.
	pageFetchTimeout = 15 * time.Second
	maxPageRedirects = 5
)

// vetDeepURL is the pre-check before any fetch or worker call: http(s), no
// userinfo, ports 80 and 443 only, and the egress guard's literal-address
// rules. Name resolution is vetted at dial time by the guarded client.
func vetDeepURL(raw string) error {
	if err := egress.CheckURL(raw, true); err != nil {
		return err
	}
	u, err := url.Parse(strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(strings.TrimSpace(raw)))
	if err != nil || u.User != nil {
		return errors.New("url not allowed")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return errors.New("port not allowed")
	}
	return nil
}

// PageFetcher returns the body (at most maxPageBytes) of a 200 answer.
type PageFetcher func(ctx context.Context, rawURL string) ([]byte, error)

var emailScanRe = regexp.MustCompile(EmailPattern)

// fetchPage is the default PageFetcher over the egress-guarded client c: the
// client resolves once, refuses private/loopback/metadata addresses and dials the vetted IP, so DNS rebinding
// cannot redirect the request. Redirects are followed by hand, only to the
// same host, and every hop is vetted again.
func fetchPage(c *http.Client) PageFetcher {
	return func(ctx context.Context, rawURL string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, pageFetchTimeout)
		defer cancel()
		cur := rawURL
		start, err := url.Parse(rawURL)
		if err != nil {
			return nil, err
		}
		for hop := 0; hop <= maxPageRedirects; hop++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; OpenGTM person-intel)")
			resp, err := c.Do(req)
			if err != nil {
				return nil, err
			}
			switch resp.StatusCode {
			case 301, 302, 303, 307, 308:
				loc := resp.Header.Get("Location")
				_ = resp.Body.Close()
				next, err := url.Parse(cur)
				if err == nil && loc != "" {
					var ref *url.URL
					if ref, err = url.Parse(loc); err == nil {
						next = next.ResolveReference(ref)
					}
				}
				if err != nil || loc == "" || !strings.EqualFold(next.Host, start.Host) {
					return nil, errors.New("redirect refused")
				}
				cur = next.String()
				continue
			case http.StatusOK:
				body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
				_ = resp.Body.Close()
				return body, err
			}
			_ = resp.Body.Close()
			return nil, errors.New("unexpected status " + resp.Status)
		}
		return nil, errors.New("too many redirects")
	}
}

// needsBrowser reports a JavaScript wall (Python UniversalScraper's test): the
// page carries no content for a plain fetch.
func needsBrowser(body []byte) bool {
	l := strings.ToLower(string(body))
	return strings.Contains(l, "enable javascript") || (strings.Contains(l, "please wait...") && len(body) < 2000)
}

var blockTags = map[string]bool{"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "section": true, "article": true, "ul": true, "ol": true, "table": true, "header": true, "footer": true}

// extractPage turns fetched HTML into the deepOutput the Python provider
// returned: visible body text (script, style, noscript, iframe and svg dropped),
// the emails in it and its word count.
func extractPage(body []byte) json.RawMessage {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var sb strings.Builder
	var walk func(n *html.Node, inBody bool)
	walk = func(n *html.Node, inBody bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "iframe", "svg":
				return
			case "body":
				inBody = true
			}
			if blockTags[n.Data] {
				sb.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode && inBody {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inBody)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			sb.WriteByte('\n')
		}
	}
	walk(doc, false)
	lines := strings.Split(sb.String(), "\n")
	var out []string
	for _, ln := range lines {
		if ln = strings.Join(strings.Fields(ln), " "); ln != "" {
			out = append(out, ln)
		}
	}
	text := strings.Join(out, "\n")
	seen := map[string]bool{}
	var emails []string
	for _, e := range emailScanRe.FindAllString(text, -1) {
		if !seen[e] && len(emails) < maxScrapedMails {
			seen[e] = true
			emails = append(emails, e)
		}
	}
	prev := []rune(text)
	if len(prev) > maxScrapedText {
		prev = prev[:maxScrapedText]
	}
	b, _ := json.Marshal(deepOutput{Success: true, PreviewText: string(prev), ExtractedEmails: emails, WordCount: len(strings.Fields(text))})
	return b
}
