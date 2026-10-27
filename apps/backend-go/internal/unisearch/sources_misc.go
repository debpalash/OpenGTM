package unisearch

import (
	"context"
	"strconv"
	"strings"
)

// ── Google Custom Search ────────────────────────────────────────────────────

// GoogleSearchSource is apps/api/sources/google_search.py: keyed (GOOGLE_API_KEY and GOOGLE_CSE_ID), PDF-only,
// at most ten results. The key travels in the query string as Google requires; it is never logged.
type GoogleSearchSource struct {
	BaseURL string
	APIKey  string
	CSEID   string
	g       getter
}

func (GoogleSearchSource) Name() string         { return "google" }
func (GoogleSearchSource) DisplayName() string  { return "Google Search" }
func (s GoogleSearchSource) Available() bool    { return s.APIKey != "" && s.CSEID != "" }
func (GoogleSearchSource) RequiresConfig() bool { return true }

func (s GoogleSearchSource) Search(ctx context.Context, query string, limit int) []Result {
	if !s.Available() {
		return nil
	}
	num := limit
	if num > 10 {
		num = 10
	}
	u := s.BaseURL + "?" + pyURLEncode("key", s.APIKey, "cx", s.CSEID, "q", query+" filetype:pdf", "num", strconv.Itoa(num))
	a, ok := s.g.get(ctx, u, nil)
	if !ok || a.Status != 200 {
		return nil
	}
	data, ok := decodeJSON(a.Body)
	if !ok {
		return nil
	}
	var out []Result
	for idx, iv := range asList(asMap(data)["items"]) {
		item := asMap(iv)
		title, ok := strField(item, "title", "Untitled")
		if !ok {
			continue
		}
		link, ok := strField(item, "link", "")
		if !ok {
			continue
		}
		snippet, ok := strField(item, "snippet", "")
		if !ok {
			continue
		}
		out = append(out, Result{ID: "google_" + strconv.Itoa(idx) + "_" + pyQuote(head(title, 30), "/"), Title: title, URL: link, Source: "google",
			DownloadURL: sp(link), FileType: TypePDF, Snippet: &snippet})
	}
	return out
}

// ── SlideShare (inert stub kept for parity) ─────────────────────────────────

// SlideShareSource is the disabled legacy stub: registered, never available.
type SlideShareSource struct{}

func (SlideShareSource) Name() string                                 { return "slideshare" }
func (SlideShareSource) DisplayName() string                          { return "SlideShare" }
func (SlideShareSource) Available() bool                              { return false }
func (SlideShareSource) Search(context.Context, string, int) []Result { return nil }

// ── Internet Archive ────────────────────────────────────────────────────────

// ArchiveSource is ArchiveSource in sources/legacy.py (archive.org Advanced Search).
type ArchiveSource struct {
	BaseURL string
	g       getter
}

func (ArchiveSource) Name() string        { return "archive" }
func (ArchiveSource) DisplayName() string { return "Internet Archive" }
func (ArchiveSource) Available() bool     { return true }

var archiveFields = []string{"identifier", "title", "creator", "year", "date", "mediatype", "format", "downloads", "description"}

var archiveFormats = []struct {
	key, ft string
}{{"pdf", TypePDF}, {"epub", TypeEPUB}, {"docx", TypeDOCX}, {"doc", TypeDOCX}, {"mobi", TypeMOBI}, {"txt", TypeTXT}, {"text", TypeTXT}, {"html", TypeHTML}}

func (s ArchiveSource) Search(ctx context.Context, query string, limit int) []Result {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	rows := limit
	if rows < 1 {
		rows = 1
	}
	pairs := []string{"q", query}
	for _, f := range archiveFields {
		pairs = append(pairs, "fl[]", f)
	}
	pairs = append(pairs, "rows", strconv.Itoa(rows), "page", "1", "output", "json")
	a, ok := s.g.get(ctx, s.BaseURL+"?"+pyURLEncode(pairs...), nil)
	if !ok || a.Status != 200 {
		return nil
	}
	data, ok := decodeJSON(a.Body)
	if !ok {
		return nil
	}
	var out []Result
	for _, dv := range asList(asMap(asMap(data)["response"])["docs"]) {
		if r, ok := archiveMap(asMap(dv)); ok {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func firstTruthy(l []any) any {
	for _, v := range l {
		if truthy(v) {
			return v
		}
	}
	return nil
}

func archiveMap(doc jm) (Result, bool) {
	if !truthy(doc["identifier"]) {
		return Result{}, false
	}
	ident := pyStr(doc["identifier"])
	title := ident
	switch t := doc["title"].(type) {
	case string:
		if t != "" {
			title = t
		}
	case []any:
		var parts []string
		for _, v := range t {
			if truthy(v) {
				parts = append(parts, pyStr(v))
			}
		}
		if len(parts) > 0 {
			title = strings.Join(parts, ", ")
		}
	default:
		if truthy(t) {
			title = pyStr(t)
		}
	}
	var author *string
	switch c := doc["creator"].(type) {
	case []any:
		var names []string
		for _, v := range c {
			if truthy(v) {
				names = append(names, pyStr(v))
			}
		}
		if len(names) > 0 {
			author = sp(authorString(names))
		}
	case string:
		if x := pyStrip(c); x != "" {
			author = sp(x)
		}
	}
	year := archiveYear(doc["year"], doc["date"])
	var snippet *string
	sn := doc["description"]
	if l, ok := sn.([]any); ok {
		sn = firstTruthy(l)
	}
	if str, ok := sn.(string); ok {
		if x := pyStrip(str); x != "" {
			if len([]rune(x)) > 300 {
				x = head(x, 297) + "..."
			}
			snippet = &x
		}
	}
	q := pyQuote(ident, "")
	return Result{ID: "archive_" + ident, Title: title, URL: "https://archive.org/details/" + q, Source: "archive",
		DownloadURL: sp("https://archive.org/download/" + q), Thumbnail: sp("https://archive.org/services/img/" + q), Author: author,
		Year: year, FileType: archiveFileType(doc), Snippet: snippet}, true
}

// archiveYear is _parse_year: the first run of four digits in year, then date, that reads 1..2100.
func archiveYear(vals ...any) *int {
	for _, v := range vals {
		if l, ok := v.([]any); ok {
			v = firstTruthy(l)
		}
		if v == nil {
			continue
		}
		text := pyStr(v)
		digits := ""
		for _, ch := range text {
			if ch >= '0' && ch <= '9' {
				digits += string(ch)
				if len(digits) == 4 {
					break
				}
			} else {
				digits = ""
			}
		}
		if len(digits) == 4 {
			if n, err := strconv.Atoi(digits); err == nil && n > 0 && n <= 2100 {
				return &n
			}
		}
	}
	return nil
}

func archiveFileType(doc jm) string {
	var formats []string
	switch f := doc["format"].(type) {
	case []any:
		for _, v := range f {
			if truthy(v) {
				formats = append(formats, strings.ToLower(pyStr(v)))
			}
		}
	case string:
		formats = []string{strings.ToLower(f)}
	}
	for _, kv := range archiveFormats {
		for _, f := range formats {
			if strings.Contains(f, kv.key) {
				return kv.ft
			}
		}
	}
	if mt := doc["mediatype"]; truthy(mt) && strings.ToLower(pyStr(mt)) == "texts" {
		return TypePDF
	}
	return TypeOther
}

// ── Google Books ────────────────────────────────────────────────────────────

// GoogleBooksSource is GoogleBooksSource in sources/legacy.py (public Volumes API; the key is optional).
type GoogleBooksSource struct {
	BaseURL string
	APIKey  string
	g       getter
}

func (GoogleBooksSource) Name() string        { return "google_books" }
func (GoogleBooksSource) DisplayName() string { return "Google Books" }
func (GoogleBooksSource) Available() bool     { return true }

const gbooksMax = 40

func (s GoogleBooksSource) Search(ctx context.Context, query string, limit int) []Result {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	n := limit
	if n > gbooksMax {
		n = gbooksMax
	}
	if n < 1 {
		n = 1
	}
	pairs := []string{"q", query, "maxResults", strconv.Itoa(n), "printType", "books"}
	if s.APIKey != "" {
		pairs = append(pairs, "key", s.APIKey)
	}
	a, ok := s.g.get(ctx, s.BaseURL+"?"+pyURLEncode(pairs...), nil)
	if !ok || a.Status != 200 {
		return nil
	}
	data, ok := decodeJSON(a.Body)
	if !ok {
		return nil
	}
	var out []Result
	for _, iv := range asList(asMap(data)["items"]) {
		if r, ok := gbooksMap(asMap(iv)); ok {
			out = append(out, r)
		}
	}
	return out
}

func gbooksMap(item jm) (Result, bool) {
	if !truthy(item["id"]) {
		return Result{}, false
	}
	vid := pyStr(item["id"])
	info := asMap(item["volumeInfo"])
	access := asMap(item["accessInfo"])
	title := "Untitled"
	if t, _ := info["title"].(string); t != "" {
		title = t
	}
	if sub, _ := info["subtitle"].(string); sub != "" {
		title += ": " + sub
	}
	pageURL, _ := info["infoLink"].(string)
	if pageURL == "" {
		pageURL, _ = item["selfLink"].(string)
	}
	if pageURL == "" {
		pageURL = "https://books.google.com/books?id=" + pyQuote(vid, "/")
	}
	var names []string
	for _, a := range asList(info["authors"]) {
		n, ok := a.(string)
		if !ok {
			return Result{}, false
		}
		names = append(names, n)
	}
	var author *string
	if len(names) > 0 {
		author = sp(authorString(names))
	}
	var year *int
	if pub := pyStr(info["publishedDate"]); truthy(info["publishedDate"]) {
		h := head(pub, 4)
		if isDigits(h) {
			n, _ := strconv.Atoi(h)
			year = &n
		}
	}
	var thumb *string
	if il := asMap(info["imageLinks"]); len(il) > 0 {
		if t, _ := il["thumbnail"].(string); t != "" {
			thumb = &t
		} else if t, _ := il["smallThumbnail"].(string); t != "" {
			thumb = &t
		}
	}
	var dl *string
	ft := TypeOther
	pdf, epub := asMap(access["pdf"]), asMap(access["epub"])
	link := func(m jm) *string {
		if l, _ := m["downloadLink"].(string); l != "" {
			return &l
		}
		return nil
	}
	switch {
	case truthy(pdf["isAvailable"]) && link(pdf) != nil:
		dl, ft = link(pdf), TypePDF
	case truthy(pdf["isAvailable"]):
		ft = TypePDF
	case truthy(epub["isAvailable"]) && link(epub) != nil:
		dl, ft = link(epub), TypeEPUB
	case truthy(epub["isAvailable"]):
		ft = TypeEPUB
	}
	var snippet *string
	raw := asMap(item["searchInfo"])["textSnippet"]
	if !truthy(raw) {
		raw = info["description"]
	}
	if truthy(raw) {
		snippet = sp(head(pyStr(raw), 300))
	}
	return Result{ID: "google_books_" + vid, Title: title, URL: pageURL, Source: "google_books", DownloadURL: dl, Thumbnail: thumb,
		Author: author, Year: year, FileType: ft, Snippet: snippet}, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
