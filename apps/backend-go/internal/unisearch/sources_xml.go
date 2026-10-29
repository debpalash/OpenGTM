package unisearch

import (
	"context"
	"strconv"
	"strings"
)

const atomNS = "http://www.w3.org/2005/Atom"

// ArxivSource is apps/api/sources/arxiv.py. BaseURL defaults to the https form (the Python http:// URL relied on
// following a redirect).
type ArxivSource struct {
	BaseURL string
	g       getter
}

func (ArxivSource) Name() string        { return "arxiv" }
func (ArxivSource) DisplayName() string { return "arXiv" }
func (ArxivSource) Available() bool     { return true }

func (s ArxivSource) Search(ctx context.Context, query string, limit int) []Result {
	u := s.BaseURL + "?" + pyURLEncode("search_query", "all:"+query, "start", "0", "max_results", strconv.Itoa(limit),
		"sortBy", "relevance", "sortOrder", "descending")
	a, ok := s.g.get(ctx, u, nil)
	if !ok || a.Status != 200 {
		return nil
	}
	root, err := parseXML(a.Body)
	if err != nil {
		return nil
	}
	var out []Result
entries:
	for _, e := range root.children(atomNS, "entry") {
		title := "Untitled"
		if t := e.child(atomNS, "title"); t != nil {
			if !t.HasText {
				continue // .text is None: .strip() raises, the entry is skipped
			}
			title = strings.ReplaceAll(pyStrip(t.Text), "\n", " ")
		}
		id := ""
		if i := e.child(atomNS, "id"); i != nil {
			if !i.HasText {
				continue // url=None fails validation
			}
			id = i.Text
		}
		var pdf *string
		for _, l := range e.children(atomNS, "link") {
			if l.Attr["title"] == "pdf" {
				if h, ok := l.Attr["href"]; ok {
					pdf = sp(h)
				}
				break
			}
		}
		var authors []string
		for _, au := range e.children(atomNS, "author") {
			if n := au.child(atomNS, "name"); n != nil {
				if !n.HasText {
					continue entries // join(None) raises
				}
				authors = append(authors, n.Text)
			}
		}
		var snippet *string
		if sm := e.child(atomNS, "summary"); sm != nil {
			if !sm.HasText {
				continue
			}
			snippet = sp(head(pyStrip(sm.Text), 200))
		}
		var year *int
		if p := e.child(atomNS, "published"); p != nil {
			if n, err := strconv.Atoi(head(p.Text, 4)); err == nil {
				year = &n
			}
		}
		var author *string
		if len(authors) > 0 {
			author = sp(authorString(authors))
		}
		parts := strings.Split(id, "/")
		out = append(out, Result{ID: "arxiv_" + parts[len(parts)-1], Title: title, URL: id, Source: "arxiv", DownloadURL: pdf,
			Author: author, Year: year, FileType: TypePDF, Snippet: snippet})
	}
	return out
}

// PubMedSource is apps/api/sources/pubmed.py (NCBI E-utilities over the pmc database).
type PubMedSource struct {
	SearchURL, SummaryURL string
	g                     getter
}

func (PubMedSource) Name() string        { return "pubmed" }
func (PubMedSource) DisplayName() string { return "PubMed Central" }
func (PubMedSource) Available() bool     { return true }

func (s PubMedSource) Search(ctx context.Context, query string, limit int) []Result {
	a, ok := s.g.get(ctx, s.SearchURL+"?"+pyURLEncode("db", "pmc", "term", query, "retmax", strconv.Itoa(limit), "retmode", "xml"), nil)
	if !ok || a.Status != 200 {
		return nil
	}
	root, err := parseXML(a.Body)
	if err != nil {
		return nil
	}
	list := root.child("", "IdList")
	if list == nil {
		return nil
	}
	var ids []string
	for _, i := range list.children("", "Id") {
		if !i.HasText {
			return nil
		}
		ids = append(ids, i.Text)
	}
	if len(ids) == 0 {
		return nil
	}
	a, ok = s.g.get(ctx, s.SummaryURL+"?"+pyURLEncode("db", "pmc", "id", strings.Join(ids, ","), "retmode", "xml"), nil)
	if !ok || a.Status != 200 {
		return nil
	}
	root, err = parseXML(a.Body)
	if err != nil {
		return nil
	}
	var out []Result
sums:
	for _, ds := range root.descendants("", "DocSum") {
		idEl := ds.child("", "Id")
		if idEl == nil || !idEl.HasText {
			continue
		}
		pmc := idEl.Text
		var title, pubDate string
		var haveTitle, havePub bool
		var authors []string
		for _, it := range ds.children("", "Item") {
			switch it.Attr["Name"] {
			case "Title":
				title, haveTitle = it.Text, it.HasText
			case "AuthorList":
				authors = authors[:0]
				for _, a := range it.descendants("", "Item") {
					if !a.HasText {
						continue sums // join(None) raises
					}
					authors = append(authors, a.Text)
				}
				if len(authors) > 3 {
					authors = authors[:3]
				}
			case "PubDate":
				pubDate, havePub = it.Text, it.HasText
			}
		}
		if !haveTitle || title == "" {
			continue
		}
		var author *string
		if len(authors) > 0 {
			author = sp(strings.Join(authors, ", "))
		}
		var year *int
		if havePub {
			if f := strings.Fields(pubDate); len(f) > 0 {
				if n, err := strconv.Atoi(f[0]); err == nil {
					year = &n
				}
			}
		}
		out = append(out, Result{ID: "pubmed_" + pmc, Title: title, URL: "https://www.ncbi.nlm.nih.gov/pmc/articles/PMC" + pmc + "/",
			Source: "pubmed", DownloadURL: sp("https://www.ncbi.nlm.nih.gov/pmc/articles/PMC" + pmc + "/pdf/"), Author: author, Year: year,
			FileType: TypePDF, Snippet: sp("PubMed Central free full-text")})
	}
	return out
}
