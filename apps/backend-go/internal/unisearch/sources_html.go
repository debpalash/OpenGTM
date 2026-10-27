package unisearch

import (
	"context"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// GutenbergSource is apps/api/sources/gutenberg.py (HTML search results page).
type GutenbergSource struct {
	BaseURL string
	g       getter
}

func (GutenbergSource) Name() string        { return "gutenberg" }
func (GutenbergSource) DisplayName() string { return "Project Gutenberg" }
func (GutenbergSource) Available() bool     { return true }

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

// findAll is soup.find_all(tag, class_=class): matching elements in document order, at most limit (0 = no limit).
func findAll(n *html.Node, tag, class string, limit int) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(p *html.Node) {
		for c := p.FirstChild; c != nil; c = c.NextSibling {
			if limit > 0 && len(out) >= limit {
				return
			}
			if c.Type == html.ElementNode && c.Data == tag && hasClass(c, class) {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

func findFirst(n *html.Node, tag, class string) *html.Node {
	if r := findAll(n, tag, class, 1); len(r) > 0 {
		return r[0]
	}
	return nil
}

// strippedText is tag.get_text(strip=True): every non-empty stripped string below n, joined with nothing.
func strippedText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(p *html.Node) {
		for c := p.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				b.WriteString(pyStrip(c.Data))
			case html.ElementNode:
				if c.Data == "script" || c.Data == "style" {
					continue
				}
				walk(c)
			}
		}
	}
	walk(n)
	return b.String()
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (s GutenbergSource) Search(ctx context.Context, query string, limit int) []Result {
	a, ok := s.g.get(ctx, s.BaseURL+"/ebooks/search/?query="+pyQuote(query, "/"), nil)
	if !ok || a.Status != 200 {
		return nil
	}
	doc, err := html.Parse(strings.NewReader(string(a.Body)))
	if err != nil {
		return nil
	}
	var out []Result
	for _, book := range findAll(doc, "li", "booklink", limit) {
		titleEl := findFirst(book, "span", "title")
		if titleEl == nil {
			continue
		}
		title := strippedText(titleEl)
		linkEl := findFirst(book, "a", "link")
		if linkEl == nil {
			continue
		}
		href := attrOf(linkEl, "href")
		pageURL := href
		if strings.HasPrefix(href, "/") {
			pageURL = s.BaseURL + href
		}
		bookID := ""
		if i := strings.LastIndex(href, "/ebooks/"); i >= 0 {
			bookID = strings.Trim(href[i+len("/ebooks/"):], "/")
		}
		var author *string
		if au := findFirst(book, "span", "subtitle"); au != nil {
			author = sp(strippedText(au))
		}
		var dl *string
		if bookID != "" {
			dl = sp("https://www.gutenberg.org/files/" + bookID + "/" + bookID + "-pdf.pdf")
		}
		id := bookID
		if id == "" {
			id = strconv.Itoa(len(out))
		}
		out = append(out, Result{ID: "gutenberg_" + id, Title: title, URL: pageURL, Source: "gutenberg", DownloadURL: dl, Author: author,
			FileType: TypePDF, Snippet: sp("Public domain classic")})
	}
	return out
}
