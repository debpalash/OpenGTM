package leadweb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Vectors recorded from the legacy Python (legacy/python/scripts/
// record_leadweb_extract.py) over the synthetic pages in testdata/pages, and
// page_meta from the browser service (bs4 + lxml).

type siteVec struct {
	Description string            `json:"description"`
	Emails      []string          `json:"emails"`
	Phones      []string          `json:"phones"`
	Social      map[string]string `json:"social"`
}

type pageVec struct {
	RunnerEmails      []string        `json:"runner_emails"`
	RunnerPhones      []string        `json:"runner_phones"`
	FetchEmailsSorted []string        `json:"fetch_emails_sorted"`
	FetchPhones       []string        `json:"fetch_phones"`
	Challenge         map[string]bool `json:"challenge"`
	Site              siteVec         `json:"site"`
	Links             []string        `json:"links"`
}

type extractVectors struct {
	Pages      map[string]pageVec `json:"pages"`
	SiteCombos []struct {
		Main      string   `json:"main"`
		ContactUs string   `json:"contact_us"`
		Fetched   []string `json:"fetched"`
		Site      siteVec  `json:"site"`
	} `json:"site_combos"`
	IsJunkURL        map[string]bool `json:"is_junk_url"`
	IsSkipDomain     map[string]bool `json:"is_skip_domain"`
	IsPublisherEmail map[string]bool `json:"is_publisher_email"`
	IsValidPhone     map[string]bool `json:"is_valid_phone"`
}

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func readPage(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pages", name+".html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestExtractVectorsCoverEnoughPages(t *testing.T) {
	var v extractVectors
	loadJSON(t, "extract_vectors.json", &v)
	if len(v.Pages) < 30 {
		t.Fatalf("only %d recorded pages", len(v.Pages))
	}
}

func TestRunnerExtractorsMatchPython(t *testing.T) {
	var v extractVectors
	loadJSON(t, "extract_vectors.json", &v)
	for name, want := range v.Pages {
		html := readPage(t, name)
		if got := nz(EmailsFromHTML(html)); !slices.Equal(got, nz(want.RunnerEmails)) {
			t.Errorf("%s EmailsFromHTML\n got %q\nwant %q", name, got, want.RunnerEmails)
		}
		if got := nz(PhonesFromHTML(html)); !slices.Equal(got, nz(want.RunnerPhones)) {
			t.Errorf("%s PhonesFromHTML\n got %q\nwant %q", name, got, want.RunnerPhones)
		}
	}
}

func TestFetchExtractorsMatchPython(t *testing.T) {
	var v extractVectors
	loadJSON(t, "extract_vectors.json", &v)
	for name, want := range v.Pages {
		html := readPage(t, name)
		got := slices.Clone(FetchEmails(html))
		sort.Strings(got)
		if !slices.Equal(nz(got), nz(want.FetchEmailsSorted)) {
			t.Errorf("%s FetchEmails\n got %q\nwant %q", name, got, want.FetchEmailsSorted)
		}
		if got := nz(FetchPhones(html)); !slices.Equal(got, nz(want.FetchPhones)) {
			t.Errorf("%s FetchPhones\n got %q\nwant %q", name, got, want.FetchPhones)
		}
		for st, w := range want.Challenge {
			code, _ := strconv.Atoi(st)
			if g := IsChallenge(code, html); g != w {
				t.Errorf("%s IsChallenge(%d) = %v, want %v", name, code, g, w)
			}
		}
	}
}

func TestFetchEmailsKeepFirstOccurrenceOrder(t *testing.T) {
	// legacy returned list(set(...)) (hash order); Go keeps document order
	got := FetchEmails("b@acme.in a@acme.in b@acme.in c@acme.in")
	if !slices.Equal(got, []string{"b@acme.in", "a@acme.in", "c@acme.in"}) {
		t.Fatal(got)
	}
}

func TestScanSiteMatchesPython(t *testing.T) {
	var v extractVectors
	loadJSON(t, "extract_vectors.json", &v)
	for name, want := range v.Pages {
		html := readPage(t, name)
		got := ScanSite("https://acme.in", func(u string) (string, bool) {
			return html, u == "https://acme.in"
		})
		checkSite(t, name, got, want.Site)
	}
	for _, c := range v.SiteCombos {
		pages := map[string]string{
			"https://acme.in":            readPage(t, c.Main),
			"https://acme.in/contact-us": readPage(t, c.ContactUs),
		}
		var fetched []string
		got := ScanSite("https://acme.in", func(u string) (string, bool) {
			fetched = append(fetched, u)
			h, ok := pages[u]
			return h, ok
		})
		if !slices.Equal(fetched, c.Fetched) {
			t.Errorf("combo %s+%s fetched %q want %q", c.Main, c.ContactUs, fetched, c.Fetched)
		}
		checkSite(t, c.Main+"+"+c.ContactUs, got, c.Site)
	}
}

func checkSite(t *testing.T, name string, got SiteContacts, want siteVec) {
	t.Helper()
	if !slices.Equal(nz(got.Phones), nz(want.Phones)) {
		t.Errorf("%s site phones\n got %q\nwant %q", name, got.Phones, want.Phones)
	}
	if !slices.Equal(nz(got.Emails), nz(want.Emails)) {
		t.Errorf("%s site emails\n got %q\nwant %q", name, got.Emails, want.Emails)
	}
	if got.Description != want.Description {
		t.Errorf("%s description %q want %q", name, got.Description, want.Description)
	}
	soc := map[string]string{"linkedin": got.Social.LinkedIn, "twitter": got.Social.Twitter,
		"facebook": got.Social.Facebook, "instagram": got.Social.Instagram}
	if len(want.Social) > 0 {
		for k, w := range want.Social {
			if soc[k] != w {
				t.Errorf("%s social %s = %q want %q", name, k, soc[k], w)
			}
		}
	}
}

func TestLinksMatchPython(t *testing.T) {
	var v extractVectors
	loadJSON(t, "extract_vectors.json", &v)
	for name, want := range v.Pages {
		if got := nz(Links(readPage(t, name), "https://acme.in/sub/page")); !slices.Equal(got, nz(want.Links)) {
			t.Errorf("%s Links\n got %q\nwant %q", name, got, want.Links)
		}
	}
}

func TestMetaMatchesBrowserService(t *testing.T) {
	var v map[string][2]string
	loadJSON(t, "meta_vectors.json", &v)
	for name, want := range v {
		title, desc := Meta(readPage(t, name))
		if title != want[0] || desc != want[1] {
			t.Errorf("%s Meta = %q, %q; want %q, %q", name, title, desc, want[0], want[1])
		}
	}
}

func TestURLFiltersMatchPython(t *testing.T) {
	var v extractVectors
	loadJSON(t, "extract_vectors.json", &v)
	for in, w := range v.IsJunkURL {
		if g := IsJunkURL(in); g != w {
			t.Errorf("IsJunkURL(%q) = %v", in, g)
		}
	}
	for in, w := range v.IsSkipDomain {
		if g := IsSkipDomain(in); g != w {
			t.Errorf("IsSkipDomain(%q) = %v", in, g)
		}
	}
	for in, w := range v.IsPublisherEmail {
		if g := IsPublisherEmail(in); g != w {
			t.Errorf("IsPublisherEmail(%q) = %v", in, g)
		}
	}
	for in, w := range v.IsValidPhone {
		if g := IsValidPhone(in); g != w {
			t.Errorf("IsValidPhone(%q) = %v", in, g)
		}
	}
}

func TestURLJoinMatchesPythonSemantics(t *testing.T) {
	base := "https://acme.in/a/b/c?q=1#f"
	for ref, want := range map[string]string{
		"":                   base,
		"d":                  "https://acme.in/a/b/d",
		"../d":               "https://acme.in/a/d",
		"../../../../d":      "https://acme.in/d",
		"./":                 "https://acme.in/a/b/",
		"?x=2":               "https://acme.in/a/b/c?x=2",
		"#g":                 "https://acme.in/a/b/c?q=1#g",
		"//h.example/p":      "https://h.example/p",
		"HTTP://X.example/Y": "HTTP://X.example/Y",
		"ftp://x/y":          "ftp://x/y",
		"\t /d e ":           "https://acme.in/d e ",
		"..":                 "https://acme.in/a/",
	} {
		if got := urljoin(base, ref); got != want && !strings.EqualFold(got, want) {
			t.Errorf("urljoin(%q) = %q, want %q", ref, got, want)
		}
	}
}
