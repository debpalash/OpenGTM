package personintel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

func deepHTTP(t *testing.T, urls []string) *http.Client {
	t.Helper()
	return withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(urls))) })
}

func okCaller() *fakeCaller {
	return &fakeCaller{fn: func(context.Context, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		return okOutput("from python", []string{"py@acme.io"}, 2), nil
	}}
}

func TestDeepFastPathScrapesInGoWithoutPython(t *testing.T) {
	fastDeep(t)
	c := deepHTTP(t, manyURLs(3))
	fc := okCaller()
	fetch := func(context.Context, string) ([]byte, error) {
		return []byte(`<html><head><title>x</title><script>var a="evil@bad.io"</script></head><body><h1>Jane Doe</h1><p>Mail jane@acme.io today</p><style>p{}</style></body></html>`), nil
	}
	p, err := Enrich(context.Background(), Deps{HTTP: c, PyWorker: fc, Fetch: fetch}, "https://www.linkedin.com/in/jane-doe", true, nil)
	if err != nil || p.Status != "completed" {
		t.Fatalf("%q %v", p.Status, err)
	}
	if len(fc.calls) != 0 {
		t.Fatalf("python called %d times for fetchable pages", len(fc.calls))
	}
	if !contains(p.Emails, "jane@acme.io") || contains(p.Emails, "evil@bad.io") {
		t.Fatalf("emails %v", p.Emails)
	}
	n := 0
	for _, s := range p.RawSources {
		if s.ScrapedText != "" {
			n++
			if !strings.Contains(s.ScrapedText, "Jane Doe") || strings.Contains(s.ScrapedText, "evil") || s.ScrapedWordCount < 5 {
				t.Fatalf("scraped %q %d", s.ScrapedText, s.ScrapedWordCount)
			}
		}
	}
	if n != 3 {
		t.Fatalf("scraped pages %d", n)
	}
}

func TestDeepJSWallFallsBackToPythonOnlyForVettedURL(t *testing.T) {
	fastDeep(t)
	c := deepHTTP(t, manyURLs(2))
	fc := okCaller()
	wall := func(context.Context, string) ([]byte, error) {
		return []byte(`<html><body>Please enable JavaScript to continue</body></html>`), nil
	}
	if _, err := Enrich(context.Background(), Deps{HTTP: c, PyWorker: fc, Fetch: wall}, "https://www.linkedin.com/in/jane-doe", true, nil); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 2 {
		t.Fatalf("js-wall pages should reach python, calls %d", len(fc.calls))
	}
}

func TestDeepBlockedURLsNeverFetchedNorSentToPython(t *testing.T) {
	fastDeep(t)
	urls := []string{"http://169.254.169.254/latest", "http://127.0.0.1:8080/x", "https://user:pw@site.example.org/p", "https://site.example.org:6379/p", "http://[::1]/", "ftp://site.example.org/x"}
	c := deepHTTP(t, urls)
	fc := okCaller()
	var fetched atomic.Int32
	fetch := func(context.Context, string) ([]byte, error) {
		fetched.Add(1)
		return []byte(`<body>enable javascript</body>`), nil
	}
	r := &recorder{}
	if _, err := Enrich(context.Background(), Deps{HTTP: c, PyWorker: fc, Fetch: fetch}, "https://www.linkedin.com/in/jane-doe", true, r.notify); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 0 || fetched.Load() != 0 {
		t.Fatalf("blocked urls leaked: python %d fetch %d (calls %v)", len(fc.calls), fetched.Load(), fc.calls)
	}
}

func TestDeepFetchErrorSkipsPageWithoutPython(t *testing.T) {
	fastDeep(t)
	c := deepHTTP(t, manyURLs(2))
	fc := okCaller()
	fetch := func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") }
	r := &recorder{}
	p, err := Enrich(context.Background(), Deps{HTTP: c, PyWorker: fc, Fetch: fetch}, "https://www.linkedin.com/in/jane-doe", true, r.notify)
	if err != nil || p.Status != "completed" || len(fc.calls) != 0 {
		t.Fatalf("%q %v calls %d", p.Status, err, len(fc.calls))
	}
	skipped := 0
	for _, e := range r.ev {
		if e.step == "scrape_progress" && e.data["skipped"] == true {
			skipped++
		}
	}
	if skipped != 2 {
		t.Fatalf("skipped progress events %d", skipped)
	}
}

func TestFetchPageCapsBodyAndRefusesCrossHostRedirect(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			_, _ = io.Copy(w, io.LimitReader(zeroReader{}, 5<<20))
		case "/same":
			http.Redirect(w, r, "/small", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, "http://other.invalid/x", http.StatusFound)
		case "/small":
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer big.Close()
	c := stubClient(t, big)
	body, err := fetchPage(c)(context.Background(), big.URL+"/big")
	if err != nil || len(body) > maxPageBytes {
		t.Fatalf("len %d err %v", len(body), err)
	}
	if body, err := fetchPage(c)(context.Background(), big.URL+"/same"); err != nil || string(body) != "ok" {
		t.Fatalf("same-host redirect %q %v", body, err)
	}
	if _, err := fetchPage(c)(context.Background(), big.URL+"/cross"); err == nil {
		t.Fatal("cross-host redirect followed")
	}
	// the guarded client refuses a loopback target that is not allow-listed
	if _, err := fetchPage(egress.NewClientForEnv("production", nil))(context.Background(), big.URL+"/small"); err == nil {
		t.Fatal("loopback fetched")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
