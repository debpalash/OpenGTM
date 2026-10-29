package unisearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// vectors.json is recorded from the real Python sources (legacy/python/scripts/record_unisearch.py) running against
// a local fixture server; the same responses are replayed here and the requests and results compared.

type fixtureResp struct {
	Path    string            `json:"path"`
	Status  int               `json:"status"`
	Ctype   string            `json:"ctype"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

type recordedReq struct {
	Path  string `json:"path"`
	Query string `json:"query"`
	UA    string `json:"ua"`
}

type sourceCase struct {
	Name      string           `json:"name"`
	Source    string           `json:"source"`
	Query     string           `json:"query"`
	Limit     int              `json:"limit"`
	Env       map[string]any   `json:"env"`
	Responses []fixtureResp    `json:"responses"`
	Requests  []recordedReq    `json:"requests"`
	Available bool             `json:"available"`
	Results   []map[string]any `json:"results"`
}

type unifiedCase struct {
	Name        string           `json:"name"`
	Args        map[string]any   `json:"args"`
	Total       int              `json:"total"`
	Results     []map[string]any `json:"results"`
	PythonError *string          `json:"python_error"`
}

type vectorFile struct {
	Sources []sourceCase `json:"sources"`
	Unified struct {
		Fakes map[string][]map[string]any `json:"fakes"`
		Cases []unifiedCase               `json:"cases"`
	} `json:"unified"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

type seenReq struct{ path, query, ua string }

// fixtureServer replays responses like the recorder: the next unused response whose path prefix matches.
func fixtureServer(t *testing.T, resps []fixtureResp) (*httptest.Server, func() []seenReq) {
	t.Helper()
	var mu sync.Mutex
	queue := append([]fixtureResp(nil), resps...)
	var seen []seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, seenReq{r.URL.Path, r.URL.RawQuery, r.UserAgent()})
		for i, f := range queue {
			if strings.HasPrefix(r.URL.Path, f.Path) {
				queue = append(queue[:i], queue[i+1:]...)
				for k, v := range f.Headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", f.Ctype)
				w.WriteHeader(f.Status)
				_, _ = w.Write([]byte(f.Body))
				return
			}
		}
		http.Error(w, "no fixture", 404)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenReq { mu.Lock(); defer mu.Unlock(); return append([]seenReq(nil), seen...) }
}

func newSource(c sourceCase, base string) Source {
	g := getter{hc: http.DefaultClient, timeout: 5 * time.Second}
	env := func(k string) string {
		s, _ := c.Env[k].(string)
		return s
	}
	switch c.Source {
	case "arxiv":
		return ArxivSource{BaseURL: base + "/arxiv", g: g}
	case "crossref":
		return CrossrefSource{BaseURL: base + "/crossref", g: g}
	case "doaj":
		return DOAJSource{BaseURL: base + "/doaj", g: g}
	case "gutenberg":
		return GutenbergSource{BaseURL: base + "/gutenberg", g: g}
	case "openlibrary":
		return OpenLibrarySource{BaseURL: base + "/openlibrary", g: g}
	case "pubmed":
		return PubMedSource{SearchURL: base + "/pubmed/esearch", SummaryURL: base + "/pubmed/esummary", g: g}
	case "semantic_scholar":
		return SemanticScholarSource{BaseURL: base + "/s2", g: g, Sleep: func(context.Context, time.Duration) {}}
	case "google":
		return GoogleSearchSource{BaseURL: base + "/google", APIKey: env("api_key"), CSEID: env("cse"), g: g}
	case "archive":
		return ArchiveSource{BaseURL: base + "/archive", g: g}
	case "google_books":
		return GoogleBooksSource{BaseURL: base + "/gbooks", APIKey: env("books_key"), g: g}
	case "slideshare":
		return SlideShareSource{}
	}
	panic("unknown source " + c.Source)
}

func toMaps(t *testing.T, rs []Result) []map[string]any {
	t.Helper()
	b, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func TestSourcesMatchRecordedPython(t *testing.T) {
	v := loadVectors(t)
	if len(v.Sources) < 25 {
		t.Fatalf("vectors truncated: %d source cases", len(v.Sources))
	}
	for _, c := range v.Sources {
		t.Run(c.Name, func(t *testing.T) {
			srv, seen := fixtureServer(t, c.Responses)
			src := newSource(c, srv.URL)
			if src.Available() != c.Available {
				t.Errorf("Available = %v, Python %v", src.Available(), c.Available)
			}
			got := toMaps(t, src.Search(context.Background(), c.Query, c.Limit))
			want := c.Results
			if want == nil {
				want = []map[string]any{}
			}
			wb, _ := json.Marshal(want)
			wantSub := []map[string]any{}
			if err := json.Unmarshal([]byte(strings.ReplaceAll(string(wb), "{{BASE}}", srv.URL)), &wantSub); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, wantSub) {
				gb, _ := json.MarshalIndent(got, "", " ")
				wb, _ := json.MarshalIndent(wantSub, "", " ")
				t.Errorf("results differ\n got: %s\nwant: %s", gb, wb)
			}
			reqs := seen()
			if len(reqs) != len(c.Requests) {
				t.Fatalf("%d requests, Python made %d: %+v", len(reqs), len(c.Requests), reqs)
			}
			for i, want := range c.Requests {
				if reqs[i].path != want.Path {
					t.Errorf("request %d path %q, Python %q", i, reqs[i].path, want.Path)
				}
				gq, _ := url.ParseQuery(reqs[i].query)
				wq, _ := url.ParseQuery(want.Query)
				if !reflect.DeepEqual(gq, wq) {
					t.Errorf("request %d query %v, Python %v", i, gq, wq)
				}
				if c.Source == "crossref" && reqs[i].ua != "OpenGTM/3.0 (mailto:admin@yupcha.com)" {
					t.Errorf("crossref user agent %q", reqs[i].ua)
				}
			}
		})
	}
}

type fakeSource struct {
	name  string
	items []Result
	delay time.Duration
	avail bool
}

func (f fakeSource) Name() string        { return f.name }
func (f fakeSource) DisplayName() string { return f.name }
func (f fakeSource) Available() bool     { return f.avail }
func (f fakeSource) Search(ctx context.Context, _ string, limit int) []Result {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil
		}
	}
	if limit < len(f.items) {
		return f.items[:limit]
	}
	return f.items
}

func resultsFromMaps(t *testing.T, ms []map[string]any) []Result {
	t.Helper()
	b, _ := json.Marshal(ms)
	var out []Result
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSearchUnifiedMatchesRecordedPython(t *testing.T) {
	v := loadVectors(t)
	reg := NewRegistryOf(
		fakeSource{name: "fa", items: resultsFromMaps(t, v.Unified.Fakes["fa"]), avail: true},
		fakeSource{name: "fb", items: resultsFromMaps(t, v.Unified.Fakes["fb"]), avail: true},
		fakeSource{name: "fc", items: resultsFromMaps(t, v.Unified.Fakes["fc"]), avail: true},
	)
	for _, c := range v.Unified.Cases {
		if c.PythonError != nil {
			continue // covered by TestSearchUnifiedFileTypeFilters
		}
		t.Run(c.Name, func(t *testing.T) {
			var names []string
			if l, ok := c.Args["sources"].([]any); ok {
				for _, n := range l {
					names = append(names, n.(string))
				}
			}
			limit := 10
			if f, ok := c.Args["limit_per_source"].(float64); ok {
				limit = int(f)
			}
			sortBy, _ := c.Args["sort_by"].(string)
			if sortBy == "" {
				sortBy = "relevance"
			}
			got := reg.SearchUnified(context.Background(), "q", names, limit, sortBy, "")
			if got.Total != c.Total || got.Total != len(got.Results) {
				t.Errorf("total %d (len %d), Python %d", got.Total, len(got.Results), c.Total)
			}
			want := c.Results
			if want == nil {
				want = []map[string]any{}
			}
			if g := toMaps(t, got.Results); !reflect.DeepEqual(g, want) {
				gb, _ := json.MarshalIndent(g, "", " ")
				wb, _ := json.MarshalIndent(want, "", " ")
				t.Errorf("results differ\n got: %s\nwant: %s", gb, wb)
			}
		})
	}
}

// Python raised AttributeError for any file_type (use_enum_values makes file_type a str, which has no .value): the
// recorded vector pins that, and Go filters instead.
func TestSearchUnifiedFileTypeFilters(t *testing.T) {
	v := loadVectors(t)
	pinned := false
	for _, c := range v.Unified.Cases {
		if c.Name == "file_type_python_crash" && c.PythonError != nil && *c.PythonError == "AttributeError" {
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("vector for the Python file_type crash is missing")
	}
	reg := NewRegistryOf(
		fakeSource{name: "fa", items: resultsFromMaps(t, v.Unified.Fakes["fa"]), avail: true},
		fakeSource{name: "fb", items: resultsFromMaps(t, v.Unified.Fakes["fb"]), avail: true},
	)
	got := reg.SearchUnified(context.Background(), "q", nil, 10, "relevance", "epub")
	if got.Total == 0 {
		t.Fatal("epub filter returned nothing")
	}
	for _, r := range got.Results {
		if r.FileType != "epub" {
			t.Errorf("result %s has file_type %s", r.ID, r.FileType)
		}
	}
}

func TestSearchUnifiedKeepsFinishedSourcesOnTimeout(t *testing.T) {
	v := loadVectors(t)
	reg := NewRegistryOf(
		fakeSource{name: "fast", items: resultsFromMaps(t, v.Unified.Fakes["fa"]), avail: true},
		fakeSource{name: "slow", items: resultsFromMaps(t, v.Unified.Fakes["fb"]), avail: true, delay: 5 * time.Second},
		fakeSource{name: "off", items: resultsFromMaps(t, v.Unified.Fakes["fc"]), avail: false},
	)
	reg.Timeout = 100 * time.Millisecond
	start := time.Now()
	got := reg.SearchUnified(context.Background(), "q", nil, 10, "relevance", "")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("search blocked on the slow source for %v", time.Since(start))
	}
	if got.Total != 3 {
		t.Errorf("total %d, want the 3 results of the fast source (slow dropped, unavailable skipped)", got.Total)
	}
}

func TestRegistryOrderMatchesPython(t *testing.T) {
	r := NewRegistry(Config{GoogleAPIKey: "k", GoogleCSEID: "c"}, http.DefaultClient, URLs{})
	want := []string{"arxiv", "gutenberg", "doaj", "openlibrary", "pubmed", "semantic_scholar", "crossref", "slideshare", "archive", "google_books", "google"}
	if !reflect.DeepEqual(r.Names(), want) {
		t.Errorf("order %v", r.Names())
	}
	if n := NewRegistry(Config{}, http.DefaultClient, URLs{}).Names(); len(n) != 10 || n[len(n)-1] != "google_books" {
		t.Errorf("unconfigured google must not register: %v", n)
	}
}
