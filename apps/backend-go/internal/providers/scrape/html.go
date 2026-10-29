package scrape

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// A small BeautifulSoup emulation over x/net/html (parsed like a browser with
// scripting off, so <noscript> content is markup, as html.parser keeps it).

func parseHTML(src string) *html.Node {
	doc, err := html.ParseWithOptions(strings.NewReader(src), html.ParseOptionEnableScripting(false))
	if err != nil {
		return &html.Node{Type: html.DocumentNode}
	}
	return doc
}

// bs4 keeps these elements' strings in their own classes, which get_text()
// leaves out (html.parser string_containers).
var skipTextIn = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Template: true, atom.Rt: true, atom.Rp: true}

func textStrings(n *html.Node) []string {
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			parts = append(parts, n.Data)
			return
		case html.ElementNode:
			if skipTextIn[n.DataAtom] {
				return
			}
		case html.CommentNode, html.DoctypeNode:
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return parts
}

// getText is tag.get_text(separator=sep, strip=strip).
func getText(n *html.Node, sep string, strip bool) string {
	parts := textStrings(n)
	if strip {
		kept := parts[:0]
		for _, p := range parts {
			if p = pyStrip(p); p != "" {
				kept = append(kept, p)
			}
		}
		parts = kept
	}
	return strings.Join(parts, sep)
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// findAll returns the elements under n (n excluded) in document order for
// which keep is true.
func findAll(n *html.Node, keep func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && keep(c) {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

// find is the first findAll match or nil.
func find(n *html.Node, keep func(*html.Node) bool) *html.Node {
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && keep(c) {
				found = c
				return true
			}
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(n)
	return found
}

// findNext is tag.find_next(...): the first matching element after n in
// document order (n's descendants first).
func findNext(n *html.Node, keep func(*html.Node) bool) *html.Node {
	if f := find(n, keep); f != nil {
		return f
	}
	for cur := n; cur != nil; cur = cur.Parent {
		for s := cur.NextSibling; s != nil; s = s.NextSibling {
			if s.Type == html.ElementNode && keep(s) {
				return s
			}
			if f := find(s, keep); f != nil {
				return f
			}
		}
	}
	return nil
}

func tagIn(names ...atom.Atom) func(*html.Node) bool {
	return func(n *html.Node) bool {
		for _, a := range names {
			if n.DataAtom == a {
				return true
			}
		}
		return false
	}
}

// classMatches is bs4's attrs={"class": re} on the multi-valued class
// attribute: the pattern is searched in each class value (and the joined
// value), which for word patterns is a search over the whole attribute.
func classMatches(n *html.Node, re *regexp.Regexp) bool {
	v, ok := attr(n, "class")
	if !ok {
		return false
	}
	vals := pySplit(v)
	for _, c := range vals {
		if re.MatchString(c) {
			return true
		}
	}
	return re.MatchString(strings.Join(vals, " "))
}

func anchorsWithHref(doc *html.Node) []string {
	var out []string
	for _, a := range findAll(doc, tagIn(atom.A)) {
		if h, ok := attr(a, "href"); ok {
			out = append(out, h)
		}
	}
	return out
}

// metaContent is soup.find("meta", attrs={key: val}) and its content.
func metaByAttr(doc *html.Node, key, val string) *html.Node {
	return find(doc, func(n *html.Node) bool {
		if n.DataAtom != atom.Meta {
			return false
		}
		v, ok := attr(n, key)
		return ok && v == val
	})
}

// scriptString is script.string: the text of a script with one text child.
func scriptString(n *html.Node) (string, bool) {
	if n.FirstChild != nil && n.FirstChild == n.LastChild && n.FirstChild.Type == html.TextNode {
		return n.FirstChild.Data, true
	}
	return "", false
}
