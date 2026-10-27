package collect

import (
	"strings"

	"golang.org/x/net/html"
)

// A minimal CSS selector engine for the directory-card selectors of
// _extract_companies_from_directory (BeautifulSoup .select / .select_one through soupsieve):
// type selectors, .class, [attr] and [attr=value], compounds of those, the descendant combinator
// and comma groups. Matching is document-wide (an ancestor in the selector may lie outside the
// element select is called on); results are descendants in document order, without duplicates.

type simpleSel struct {
	tag     string
	classes []string
	attrs   [][2]string // name, value ("" + hasValue false = presence)
	hasVal  []bool
}

type complexSel []simpleSel // left to right, descendant combinators

type selectorGroup []complexSel

func parseSelector(s string) selectorGroup {
	var g selectorGroup
	for _, part := range strings.Split(s, ",") {
		var cx complexSel
		for _, comp := range strings.Fields(part) {
			cx = append(cx, parseCompound(comp))
		}
		if len(cx) > 0 {
			g = append(g, cx)
		}
	}
	return g
}

func parseCompound(s string) simpleSel {
	var sel simpleSel
	i := 0
	readIdent := func() string {
		j := i
		for j < len(s) && s[j] != '.' && s[j] != '[' {
			j++
		}
		id := s[i:j]
		i = j
		return id
	}
	sel.tag = strings.ToLower(readIdent())
	for i < len(s) {
		switch s[i] {
		case '.':
			i++
			sel.classes = append(sel.classes, readIdent())
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				end = len(s) - i
			}
			body := s[i+1 : i+end]
			i += end + 1
			name, val, ok := strings.Cut(body, "=")
			sel.attrs = append(sel.attrs, [2]string{strings.ToLower(name), strings.Trim(val, `"'`)})
			sel.hasVal = append(sel.hasVal, ok)
		default:
			i++
		}
	}
	return sel
}

func attrVal(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func (s simpleSel) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if s.tag != "" && n.Data != s.tag {
		return false
	}
	if len(s.classes) > 0 {
		cls, _ := attrVal(n, "class")
		have := strings.Fields(cls)
		for _, want := range s.classes {
			if !contains(have, want) {
				return false
			}
		}
	}
	for i, a := range s.attrs {
		v, ok := attrVal(n, a[0])
		if !ok || (s.hasVal[i] && v != a[1]) {
			return false
		}
	}
	return true
}

func (c complexSel) matches(n *html.Node) bool {
	if !c[len(c)-1].matches(n) {
		return false
	}
	k := len(c) - 2
	for p := n.Parent; p != nil && k >= 0; p = p.Parent {
		if c[k].matches(p) {
			k--
		}
	}
	return k < 0
}

func (g selectorGroup) matches(n *html.Node) bool {
	for _, c := range g {
		if c.matches(n) {
			return true
		}
	}
	return false
}

// cssSelect is root.select(sel): matching descendants of root in document order.
func cssSelect(root *html.Node, sel string) []*html.Node {
	g := parseSelector(sel)
	var out []*html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if g.matches(c) {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(root)
	return out
}

// cssSelectOne is root.select_one(sel).
func cssSelectOne(root *html.Node, sel string) *html.Node {
	if xs := cssSelect(root, sel); len(xs) > 0 {
		return xs[0]
	}
	return nil
}

// textStrip is Tag.get_text(strip=True): every text node stripped, empty ones dropped, joined
// without a separator.
func textStrip(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(pyStrip(n.Data))
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
