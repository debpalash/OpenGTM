package firmo

import (
	"strings"

	"golang.org/x/net/html"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// htmlMeta is what _MetadataHTMLParser collects: the text of every
// <script type="application/ld+json"> block and the og:* meta properties.
type htmlMeta struct {
	jsonld []string
	og     map[string]string
}

// rawTextTags are the elements the x/net tokenizer reads as raw text but
// Python's HTMLParser (3.13) parses as markup. script and style stay raw in
// both (title and textarea are RCDATA in both).
var rawTextOnlyInXNet = map[string]bool{
	"iframe": true, "noembed": true, "noframes": true, "noscript": true, "plaintext": true, "xmp": true,
}

// parseHTMLMeta feeds text through the x/net tokenizer, following the
// behaviour of Python's html.parser.HTMLParser for the cases that matter to
// metadata extraction.
func parseHTMLMeta(text string) htmlMeta {
	m := htmlMeta{og: map[string]string{}}
	z := html.NewTokenizer(strings.NewReader(text))
	var parts *[]string
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return m
		case html.TextToken:
			if parts != nil {
				*parts = append(*parts, string(z.Raw()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			nameB, hasAttr := z.TagName()
			name := string(nameB)
			attrs := map[string]string{}
			present := map[string]bool{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[strings.ToLower(string(k))] = string(v)
				present[strings.ToLower(string(k))] = true
			}
			self := tt == html.SelfClosingTagToken
			switch {
			case name == "script" && strings.ToLower(providers.PyStrip(attrs["type"])) == "application/ld+json":
				p := []string{}
				parts = &p
				if self { // handle_startendtag: start then end
					m.jsonld = append(m.jsonld, "")
					parts = nil
				}
			case name == "meta":
				key := attrs["property"]
				if key == "" {
					key = attrs["name"]
				}
				if strings.HasPrefix(strings.ToLower(key), "og:") && present["content"] {
					m.og[strings.ToLower(key[3:])] = attrs["content"]
				}
			}
			if self || rawTextOnlyInXNet[name] {
				z.NextIsNotRawText()
			}
		case html.EndTagToken:
			nameB, _ := z.TagName()
			if string(nameB) == "script" && parts != nil {
				m.jsonld = append(m.jsonld, strings.Join(*parts, ""))
				parts = nil
			}
		}
	}
}
