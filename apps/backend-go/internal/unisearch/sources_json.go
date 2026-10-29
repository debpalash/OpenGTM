package unisearch

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ── Crossref ────────────────────────────────────────────────────────────────

// CrossrefSource is apps/api/sources/crossref.py.
type CrossrefSource struct {
	BaseURL string
	g       getter
}

func (CrossrefSource) Name() string        { return "crossref" }
func (CrossrefSource) DisplayName() string { return "Crossref" }
func (CrossrefSource) Available() bool     { return true }

var tagRE = regexp.MustCompile(`<[^>]+>`)

func (s CrossrefSource) Search(ctx context.Context, query string, limit int) []Result {
	u := s.BaseURL + "?" + pyURLEncode("query", query, "rows", strconv.Itoa(limit),
		"select", "DOI,title,author,URL,published-print,published-online,abstract,link")
	a, ok := s.g.get(ctx, u, map[string]string{"User-Agent": "OpenGTM/3.0 (mailto:admin@yupcha.com)"})
	if !ok || a.Status != 200 {
		return nil
	}
	data, ok := decodeJSON(a.Body)
	if !ok {
		return nil
	}
	var out []Result
	for _, iv := range asList(asMap(asMap(data)["message"])["items"]) {
		item := asMap(iv)
		titles := asList(item["title"])
		if len(titles) == 0 {
			continue
		}
		title, ok := titles[0].(string)
		if !ok {
			continue
		}
		doi, ok := strField(item, "DOI", "")
		if !ok {
			continue
		}
		pageURL, ok := strField(item, "URL", "https://doi.org/"+doi)
		if !ok {
			continue
		}
		var pdf *string
		for _, lv := range asList(item["link"]) {
			l := asMap(lv)
			if l["content-type"] == "application/pdf" {
				if s, ok := l["URL"].(string); ok {
					pdf = &s
				}
				break
			}
		}
		var authors []string
		for _, av := range asList(item["author"]) {
			au := asMap(av)
			family, _ := au["family"].(string)
			given, _ := au["given"].(string)
			if family != "" && given != "" {
				authors = append(authors, given+" "+family)
			} else if family != "" {
				authors = append(authors, family)
			}
		}
		var author *string
		if len(authors) > 0 {
			author = sp(authorString(authors))
		}
		var year *int
		published := item["published-print"]
		if !truthy(published) {
			published = item["published-online"]
		}
		if pm := asMap(published); truthy(published) {
			if dp, has := pm["date-parts"]; has {
				if l := asList(dp); len(l) > 0 && truthy(l[0]) {
					if first := asList(l[0]); len(first) > 0 {
						y, ok := yearField(first[0])
						if !ok {
							continue
						}
						year = y
					}
				}
			}
		}
		var snippet *string
		if abs, _ := item["abstract"].(string); abs != "" {
			snippet = sp(head(tagRE.ReplaceAllString(abs, ""), 200) + "...")
		}
		ft := TypeOther
		if pdf != nil && *pdf != "" {
			ft = TypePDF
		}
		out = append(out, Result{ID: "crossref_" + strings.ReplaceAll(doi, "/", "_"), Title: title, URL: pageURL, Source: "crossref",
			DownloadURL: pdf, Author: author, Year: year, FileType: ft, Snippet: snippet})
	}
	return out
}

// ── DOAJ ────────────────────────────────────────────────────────────────────

// DOAJSource is apps/api/sources/doaj.py (the query is a path segment, as in Python).
type DOAJSource struct {
	BaseURL string
	g       getter
}

func (DOAJSource) Name() string        { return "doaj" }
func (DOAJSource) DisplayName() string { return "DOAJ" }
func (DOAJSource) Available() bool     { return true }

func (s DOAJSource) Search(ctx context.Context, query string, limit int) []Result {
	a, ok := s.g.get(ctx, s.BaseURL+"/"+pyQuote(query, "/")+"?pageSize="+strconv.Itoa(limit), nil)
	if !ok || a.Status != 200 {
		return nil
	}
	data, ok := decodeJSON(a.Body)
	if !ok {
		return nil
	}
	var out []Result
	for idx, iv := range asList(asMap(data)["results"]) {
		item := asMap(iv)
		bib := asMap(item["bibjson"])
		title, ok := strField(bib, "title", "Untitled")
		if !ok {
			continue
		}
		pageURL := ""
		for _, lv := range asList(bib["link"]) {
			l := asMap(lv)
			lu, _ := l["url"].(string)
			lt, _ := l["type"].(string)
			if pageURL == "" {
				pageURL = lu
			}
			if lt = strings.ToLower(lt); strings.Contains(lt, "fulltext") || strings.Contains(lt, "pdf") {
				break
			}
		}
		authList := asList(bib["author"])
		var names []string
		for i, av := range authList {
			if i == 3 {
				break
			}
			n, _ := asMap(av)["name"].(string)
			names = append(names, n)
		}
		var author *string
		if len(names) > 0 {
			s := strings.Join(names, ", ")
			if len(authList) > 3 {
				s += " et al."
			}
			author = &s
		}
		var year *int
		switch y := bib["year"].(type) {
		case string:
			if n, err := strconv.Atoi(pyStrip(y)); err == nil {
				year = &n
			}
		case float64:
			n := int(y)
			year = &n
		}
		abstract, _ := bib["abstract"].(string)
		snippet := head(abstract, 200)
		if journal, _ := asMap(bib["journal"])["title"].(string); journal != "" {
			snippet = journal + ": " + snippet
		}
		id := fmt.Sprint(idx)
		if v, has := item["id"]; has {
			id = pyStr(v)
		}
		out = append(out, Result{ID: "doaj_" + id, Title: title, URL: pageURL, Source: "doaj", Author: author, Year: year,
			FileType: TypePDF, Snippet: &snippet})
	}
	return out
}

// pyStr is str(value) for decoded JSON scalars.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// ── OpenLibrary ─────────────────────────────────────────────────────────────

// OpenLibrarySource is apps/api/sources/openlibrary.py.
type OpenLibrarySource struct {
	BaseURL string
	g       getter
}

func (OpenLibrarySource) Name() string        { return "openlibrary" }
func (OpenLibrarySource) DisplayName() string { return "OpenLibrary" }
func (OpenLibrarySource) Available() bool     { return true }

func (s OpenLibrarySource) Search(ctx context.Context, query string, limit int) []Result {
	u := s.BaseURL + "?" + pyURLEncode("q", query, "limit", strconv.Itoa(limit),
		"fields", "key,title,author_name,first_publish_year,isbn,ebook_access,cover_i")
	a, ok := s.g.get(ctx, u, nil)
	if !ok || a.Status != 200 {
		return nil
	}
	data, ok := decodeJSON(a.Body)
	if !ok {
		return nil
	}
	var out []Result
	for _, dv := range asList(asMap(data)["docs"]) {
		doc := asMap(dv)
		title, ok := strField(doc, "title", "Untitled")
		if !ok {
			continue
		}
		key, ok := strField(doc, "key", "")
		if !ok {
			continue
		}
		pageURL := ""
		if key != "" {
			pageURL = "https://openlibrary.org" + key
		}
		var names []string
		bad := false
		for _, nv := range asList(doc["author_name"]) {
			n, ok := nv.(string)
			if !ok {
				bad = true
				break
			}
			names = append(names, n)
		}
		if bad {
			continue
		}
		var author *string
		if len(names) > 0 {
			author = sp(authorString(names))
		}
		year, ok := yearField(doc["first_publish_year"])
		if !ok {
			continue
		}
		access, ok := strField(doc, "ebook_access", "no_ebook")
		if !ok {
			continue
		}
		var dl *string
		if access == "public" || access == "borrowable" {
			if isbns := asList(doc["isbn"]); len(isbns) > 0 {
				if isbn, ok := isbns[0].(string); ok && isbn != "" {
					dl = sp("https://archive.org/download/" + isbn + "/" + isbn + ".pdf")
				}
			}
		}
		out = append(out, Result{ID: "openlibrary_" + strings.ReplaceAll(key, "/", "_"), Title: title, URL: pageURL, Source: "openlibrary",
			DownloadURL: dl, Author: author, Year: year, FileType: TypePDF, Snippet: sp("eBook access: " + access)})
	}
	return out
}

// ── Semantic Scholar ────────────────────────────────────────────────────────

const (
	s2MaxRetries  = 2
	s2BaseBackoff = 2
	s2MaxWait     = 10
)

// SemanticScholarSource is apps/api/sources/semantic_scholar.py (429 retried twice with Retry-After capped at 10 s).
type SemanticScholarSource struct {
	BaseURL string
	g       getter
	// Sleep waits between retries (tests); nil sleeps for real, honouring ctx.
	Sleep func(ctx context.Context, d time.Duration)
}

func (SemanticScholarSource) Name() string        { return "semantic_scholar" }
func (SemanticScholarSource) DisplayName() string { return "Semantic Scholar" }
func (SemanticScholarSource) Available() bool     { return true }

func (s SemanticScholarSource) sleep(ctx context.Context, d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (s SemanticScholarSource) Search(ctx context.Context, query string, limit int) []Result {
	u := s.BaseURL + "?" + pyURLEncode("query", query, "limit", strconv.Itoa(limit),
		"fields", "title,url,authors,year,abstract,openAccessPdf,venue,citationCount")
	var body []byte
	for attempt := 0; attempt <= s2MaxRetries; attempt++ {
		a, ok := s.g.get(ctx, u, nil)
		if !ok {
			return nil
		}
		if a.Status == 429 {
			wait := s2BaseBackoff << attempt
			if h := a.Headers.Get("Retry-After"); h != "" {
				n, err := strconv.Atoi(pyStrip(h))
				if err != nil {
					return nil // int(header) raised
				}
				wait = n
			}
			if wait > s2MaxWait {
				wait = s2MaxWait
			}
			if attempt < s2MaxRetries {
				s.sleep(ctx, time.Duration(wait)*time.Second)
				if ctx.Err() != nil {
					return nil
				}
				continue
			}
			return nil
		}
		if a.Status != 200 {
			return nil
		}
		body = a.Body
		break
	}
	data, ok := decodeJSON(body)
	if !ok {
		return nil
	}
	var out []Result
	for _, ev := range asList(asMap(data)["data"]) {
		e := asMap(ev)
		title, ok := strField(e, "title", "Untitled")
		if !ok {
			continue
		}
		pageURL, ok := strField(e, "url", "")
		if !ok {
			continue
		}
		var cites float64
		if v, has := e["citationCount"]; has {
			c, ok := v.(float64)
			if !ok {
				continue // None > 0 raises
			}
			cites = c
		}
		year, ok := yearField(e["year"])
		if !ok {
			continue
		}
		var pdf *string
		if oa := asMap(e["openAccessPdf"]); oa != nil {
			if v, ok := oa["url"].(string); ok {
				pdf = &v
			}
		}
		var names []string
		bad := false
		for _, av := range asList(e["authors"]) {
			n, ok := asMap(av)["name"].(string)
			if !ok {
				bad = true
				break
			}
			names = append(names, n)
		}
		if bad {
			continue
		}
		var author *string
		if len(names) > 0 {
			author = sp(authorString(names))
		}
		var snippet *string
		if abs, _ := e["abstract"].(string); abs != "" {
			snippet = sp(head(abs, 200) + "...")
		}
		if cites > 0 {
			prefix := "[" + pyStr(cites) + " citations] "
			if snippet != nil {
				prefix += *snippet
			}
			snippet = &prefix
		}
		ft := TypeOther
		if pdf != nil && *pdf != "" {
			ft = TypePDF
		}
		out = append(out, Result{ID: "s2_" + pyStr(e["paperId"]), Title: title, URL: pageURL, Source: "semantic_scholar", DownloadURL: pdf,
			Author: author, Year: year, FileType: ft, Snippet: snippet})
	}
	return out
}
