package scrape

import (
	"strings"
	"testing"
)

// Fuzz targets for hostile page text: HTML parsing / extraction and the Unicode regex helpers.
// `go test` runs the seed corpus; `go test -fuzz=FuzzX` explores further.

var fuzzSeeds = []string{
	"", "Ünïcødé Tech", "١٢٣٤ founded", "\xff\xfe\xc3", "<a href='mailto:a@b.io'>x</a>",
	`<script type="application/ld+json">{"@type":["Organization"],"address":{"addressCountry":{"x":1}}}</script>`,
	`<script type="application/ld+json">{"@type":7,"numberOfEmployees":{"value":"x"}}</script>`,
	`<div class="team-member"><h3>Jane Doe</h3><p>CEO</p></div>`, strings.Repeat("<li>", 400),
	"<meta name='description' content='\u0000'><meta property='og:type'>", "Tel +91 98765 43210 1800-123-4567",
}

func FuzzPageExtraction(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, page string) {
		doc := parseHTML(page)
		text := getText(doc, " ", true)
		structured(doc)
		teamMembers(doc, text)
		industryFromMeta(doc)
		anchorsWithHref(doc)
		companySize(text)
		foundingYear(text)
		lbPhone(text)
		lbAddress(text)
		lbRating(text)
		ciExtract(text)
		phone10(text)
	})
}

func FuzzPyRegex(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, re := range []interface{ FindAllString(string, int) []string }{dsEmailRe, dsPhone1, snippetEmailRe, fbAddrRe, clSplitRe} {
			for _, m := range re.FindAllString(s, -1) {
				if !strings.Contains(s, m) {
					t.Fatalf("match %q not in subject", m)
				}
			}
		}
		clSuffixRe.ReplaceAllString(s, "")
		wordKeywordIndex(s, "founded")
		digitsOnly(s)
		asciiDigits(s)
		trunc(s, 50)
		normalizeWebsiteURL(s)
		domainFromURL(s)
		bareDomain(s, true)
	})
}
