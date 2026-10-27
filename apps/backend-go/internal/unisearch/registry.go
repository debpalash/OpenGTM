package unisearch

import (
	"net/http"
	"time"
)

// Config carries the keys the keyed sources need (GOOGLE_API_KEY, GOOGLE_CSE_ID, GOOGLE_BOOKS_API_KEY).
type Config struct {
	GoogleAPIKey   string
	GoogleCSEID    string
	GoogleBooksKey string
}

// URLs are the upstream endpoints; the zero value means the public ones. Tests point them at httptest servers.
type URLs struct {
	Arxiv, Crossref, DOAJ, Gutenberg, OpenLibrary, PubMedSearch, PubMedSummary, SemanticScholar, Google, Archive, GoogleBooks string
}

func (u URLs) orDefaults() URLs {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&u.Arxiv, "https://export.arxiv.org/api/query")
	def(&u.Crossref, "https://api.crossref.org/works")
	def(&u.DOAJ, "https://doaj.org/api/search/articles")
	def(&u.Gutenberg, "https://www.gutenberg.org")
	def(&u.OpenLibrary, "https://openlibrary.org/search.json")
	def(&u.PubMedSearch, "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esearch.fcgi")
	def(&u.PubMedSummary, "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esummary.fcgi")
	def(&u.SemanticScholar, "https://api.semanticscholar.org/graph/v1/paper/search")
	def(&u.Google, "https://www.googleapis.com/customsearch/v1")
	def(&u.Archive, "https://archive.org/advancedsearch.php")
	def(&u.GoogleBooks, "https://www.googleapis.com/books/v1/volumes")
	return u
}

// NewRegistry registers the Python registry's sources in its order: arxiv, gutenberg, doaj, openlibrary, pubmed,
// semantic_scholar, crossref, slideshare (inert), archive, google_books, then google when configured. hc must be an
// SSRF-guarded client (egress.NewClientForEnv).
func NewRegistry(cfg Config, hc *http.Client, urls URLs) *Registry {
	u := urls.orDefaults()
	g := getter{hc: hc, timeout: 15 * time.Second}
	srcs := []Source{
		ArxivSource{BaseURL: u.Arxiv, g: g},
		GutenbergSource{BaseURL: u.Gutenberg, g: g},
		DOAJSource{BaseURL: u.DOAJ, g: g},
		OpenLibrarySource{BaseURL: u.OpenLibrary, g: g},
		PubMedSource{SearchURL: u.PubMedSearch, SummaryURL: u.PubMedSummary, g: g},
		SemanticScholarSource{BaseURL: u.SemanticScholar, g: g},
		CrossrefSource{BaseURL: u.Crossref, g: g},
		SlideShareSource{},
		ArchiveSource{BaseURL: u.Archive, g: g},
		GoogleBooksSource{BaseURL: u.GoogleBooks, APIKey: cfg.GoogleBooksKey, g: g},
	}
	if gs := (GoogleSearchSource{BaseURL: u.Google, APIKey: cfg.GoogleAPIKey, CSEID: cfg.GoogleCSEID,
		g: getter{hc: hc, timeout: 10 * time.Second}}); gs.Available() {
		srcs = append(srcs, gs)
	}
	return NewRegistryOf(srcs...)
}
