package browserclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Replays responses recorded from the real Python service (testdata/recorded,
// apps/browser-service/scripts/record_contract_fixtures.py: fake browser and
// ddgs, real trafilatura/markdownify). Checks that the Go call sends what the
// recording sent and decodes the recorded answer strictly.

type fixture struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  map[string]any  `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "recorded", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// replay serves one fixture and records the request body the client sent.
func replay(t *testing.T, f fixture) (*Client, *map[string]any) {
	t.Helper()
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != f.Method || r.URL.Path != f.Path {
			t.Errorf("%s %s, want %s %s", r.Method, r.URL.Path, f.Method, f.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(f.Status)
		_, _ = w.Write(f.Response)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, testToken, nil), &sent
}

// sameRequest: every field the recording sent is sent by Go with the same
// value (Go also sends timeout_s; the recording relied on the default).
func sameRequest(t *testing.T, name string, sent, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if !reflect.DeepEqual(sent[k], v) {
			t.Errorf("%s: request %s = %v, recorded %v", name, k, sent[k], v)
		}
	}
	for k := range sent {
		if _, ok := want[k]; !ok && k != "timeout_s" && k != "include_markdown" && k != "backends" {
			t.Errorf("%s: Go sends %s, the recording did not", name, k)
		}
	}
}

func strict(t *testing.T, name string, raw json.RawMessage, out any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("%s: recorded response has fields Go does not know: %v", name, err)
	}
}

func TestRecordedSearch(t *testing.T) {
	f := loadFixture(t, "search")
	c, sent := replay(t, f)
	out, err := c.Search(context.Background(), SearchRequest{Query: "acme robotics", Method: "news", MaxResults: 2})
	if err != nil {
		t.Fatal(err)
	}
	sameRequest(t, "search", *sent, f.Request)
	strict(t, "search", f.Response, &SearchOut{})
	if out.Error != "" || len(out.Hits) != 2 || out.Hits[1].URL != "https://news.example/acme" || out.Hits[1].Source != "Wire" {
		t.Fatalf("%+v", out)
	}
	f = loadFixture(t, "search_error")
	c, _ = replay(t, f)
	out, err = c.Search(context.Background(), SearchRequest{Query: "fail", MaxResults: 2})
	if err != nil || out.Error != "rate_limited" || len(out.Hits) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestRecordedExtract(t *testing.T) {
	f := loadFixture(t, "extract")
	c, sent := replay(t, f)
	out, err := c.Extract(context.Background(), ExtractRequest{HTML: f.Request["html"].(string), URL: "https://acme.example/a",
		IncludeMarkdown: true})
	if err != nil {
		t.Fatal(err)
	}
	sameRequest(t, "extract", *sent, f.Request)
	strict(t, "extract", f.Response, &ExtractResult{})
	if !out.Found || out.Author != "Jane Doe" || out.WordCount == 0 || out.Markdown == "" || out.Status != 0 {
		t.Fatalf("%+v", out)
	}
	f = loadFixture(t, "extract_url")
	c, _ = replay(t, f)
	out, err = c.Extract(context.Background(), ExtractRequest{URL: "https://acme.example/a", Engine: "browser", IncludeMarkdown: true})
	if err != nil || !out.Found || out.Status != 200 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestRecordedHoleheStaffSpyStats(t *testing.T) {
	f := loadFixture(t, "holehe")
	c, sent := replay(t, f)
	h, err := c.Holehe(context.Background(), HoleheRequest{Email: "jane@acme.example"})
	if err != nil {
		t.Fatal(err)
	}
	sameRequest(t, "holehe", *sent, f.Request)
	strict(t, "holehe", f.Response, &HoleheResult{})
	if !reflect.DeepEqual(h.ExistsOn, []string{"twitter"}) || h.TotalChecked != 2 || !h.Sites[1].RateLimited {
		t.Fatalf("%+v", h)
	}

	f = loadFixture(t, "staffspy")
	c, sent = replay(t, f)
	s, err := c.StaffSpy(context.Background(), StaffSpyRequest{CompanyName: "Acme",
		Cookies: []Cookie{{Name: "li_at", Value: "x", Domain: ".linkedin.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	strict(t, "staffspy", f.Response, &StaffSpyResult{})
	if s.Error != "" || len(s.Staff) != 1 || s.Staff[0].Title != "CEO" {
		t.Fatalf("%+v", s)
	}
	if ck := (*sent)["cookies"].([]any)[0].(map[string]any); ck["name"] != "li_at" || ck["value"] != "x" {
		t.Fatalf("cookies %v", ck)
	}

	f = loadFixture(t, "stats")
	c, _ = replay(t, f)
	st, err := c.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	strict(t, "stats", f.Response, &Stats{})
	if st.Ops["search"].Calls != 2 || st.Searches["rate_limited"] != 1 || st.Capacity != 4 {
		t.Fatalf("%+v", st)
	}
}

func TestRecordedValidationError(t *testing.T) {
	f := loadFixture(t, "render_bad_proxy")
	c, _ := replay(t, f)
	_, err := c.Render(context.Background(), RenderRequest{URL: "https://acme.example/", UpstreamProxy: "ftp://x:1"})
	var be *Error
	if !errors.As(err, &be) || !errors.Is(err, ErrBadRequest) || !reflect.DeepEqual(be.Fields, []string{"upstream_proxy"}) {
		t.Fatalf("%v", err)
	}
}
