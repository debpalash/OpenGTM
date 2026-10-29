package collect

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"golang.org/x/net/html"
)

// Fuzz targets for the paths that see hostile text: the Python-regex emulation (fold / map back)
// and HTML parsing (CleanPageText, ScanSite, the directory-card selectors). `go test` runs the
// seed corpus; `go test -fuzz=FuzzX` explores further.

var fuzzSeeds = []string{
	"", "a", "Ünïcødé Tech Pvt Ltd", "١٢٣ ٤٥٦", "foo bar baz", "x\vy\x1cz\x1f",
	"\xff\xfe broken utf8 \xc3", "контакт@пример.рф +91 98765 43210", "日本語の会社 株式会社",
	"<a href='mailto:x@y.com?subject=1'>mail</a>", "<div class=\"provider-row\"><h3><a>Co</a></h3></div>",
	"<<<>>><script>alert(1)</script><style>*{}</style>", strings.Repeat("<div>", 300) + "deep",
	"<footer>Tel: <a href=\"tel:+1 (555) 010-0000\">call</a></footer>", "Kſ K s",
}

func FuzzPyRx(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	pats := []pyRx{finderEmailRe, mustRx(`\b\w+\b`), mustRx(`(\d+)\s*(\w*)`), mustRx(`(?i)\bpvt\.?\s+ltd\b`), mustRx(`[^\w\s]`)}
	f.Fuzz(func(t *testing.T, s string) {
		for _, p := range pats {
			p.Match(s)
			for _, g := range p.Find(s) {
				if !strings.Contains(s, g) {
					t.Fatalf("group %q is not a substring of the subject", g)
				}
			}
			p.FindFolded(s)
			for _, m := range p.FindAll(s) {
				if !strings.Contains(s, m) {
					t.Fatalf("match %q is not a substring of the subject", m)
				}
			}
			p.Sub(s, "#")
		}
		if fd := fold(s); utf8.ValidString(s) && len(fd.orig) > 0 && len(fd.orig) != len(fd.t)+1 {
			t.Fatalf("offset map %d for folded %d bytes", len(fd.orig), len(fd.t))
		}
	})
}

func FuzzCleanPageText(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s, 200)
	}
	f.Fuzz(func(t *testing.T, src string, n int) {
		n = n % 5000
		if n < 0 {
			n = -n
		}
		if out := CleanPageText(src, n); runeLen(out) > n && n > 0 {
			t.Fatalf("clean text %d runes over the cap %d", runeLen(out), n)
		}
	})
}

func FuzzScanSite(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, page string) {
		leadweb.ScanSite("https://example.com", func(string) (string, bool) { return page, true })
	})
}

func FuzzDirectoryCards(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	sels := []string{"div.provider-row", "li.provider h3 a", "div[data-testid=card] a", "a[href]", "h3"}
	f.Fuzz(func(t *testing.T, page string) {
		doc, err := html.Parse(strings.NewReader(page))
		if err != nil {
			return
		}
		for _, s := range sels {
			for _, n := range cssSelect(doc, s) {
				textStrip(n)
			}
			cssSelectOne(doc, s)
		}
	})
}
