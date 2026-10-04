package fixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/kernels"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

const examples = "../../../../../plugins/examples"

func runAll(t *testing.T, dir string, deps Deps) []Result {
	t.Helper()
	p, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(p.Dir)
	if err != nil || len(cases) == 0 {
		t.Fatalf("cases: %v (%d)", err, len(cases))
	}
	var out []Result
	for _, c := range cases {
		out = append(out, RunCase(context.Background(), p, c, deps))
	}
	return out
}

func TestExampleProviderFixturesPass(t *testing.T) {
	results := runAll(t, filepath.Join(examples, "declarative-provider"), Deps{})
	if len(results) != 3 {
		t.Fatalf("want 3 cases, got %d", len(results))
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("%s: %v", r.Case, r.Problems)
		}
	}
}

func TestExampleWasmFixturesPass(t *testing.T) {
	for _, r := range runAll(t, filepath.Join(examples, "wasm-echo-provider"), Deps{}) {
		if !r.Pass {
			t.Errorf("%s: %v", r.Case, r.Problems)
		}
	}
}

// cannedExtractor stands in for the Rust kernel: it returns fixed records
// per page URL and does no parsing.
type cannedExtractor map[string]kernels.ExtractResult

func (c cannedExtractor) Extract(_ context.Context, req kernels.ExtractRequest) (kernels.ExtractResult, error) {
	key := req.BaseURL
	if _, nav := req.Fields["next"]; nav && len(req.Fields) == 1 {
		key += "#next"
	}
	return c[key], nil
}

func TestScraperFixturesNeedKernelAndCompareRecords(t *testing.T) {
	dir := filepath.Join(examples, "declarative-scraper")
	results := runAll(t, dir, Deps{})
	if results[0].Pass || !strings.Contains(strings.Join(results[0].Problems, " "), "extraction kernel") {
		t.Fatalf("without a kernel the scraper case must fail clearly: %+v", results[0])
	}
	canned := cannedExtractor{
		"https://acme.example/team": {Records: []map[string]string{
			{"full_name": "Ada Lovelace", "title": "Chief Executive Officer", "linkedin_url": "https://www.linkedin.com/in/ada-example"},
			{"full_name": "Alan Turing", "title": "Head of Research", "linkedin_url": "https://www.linkedin.com/in/alan-example"},
		}},
		"https://acme.example/team#next":        {Records: []map[string]string{{"next": "https://acme.example/team?page=2"}}},
		"https://acme.example/team?page=2":      {Records: []map[string]string{{"full_name": "Grace Hopper", "title": "VP Engineering"}}},
		"https://acme.example/team?page=2#next": {},
	}
	results = runAll(t, dir, Deps{Extractor: canned})
	if !results[0].Pass {
		t.Fatalf("scraper case: %v", results[0].Problems)
	}
	// A wrong record is reported field by field.
	canned["https://acme.example/team?page=2"] = kernels.ExtractResult{Records: []map[string]string{{"full_name": "Grace H."}}}
	results = runAll(t, dir, Deps{Extractor: canned})
	msg := strings.Join(results[0].Problems, "; ")
	if results[0].Pass || !strings.Contains(msg, "records[2].full_name") || !strings.Contains(msg, "records[2].title: missing") {
		t.Fatalf("diff: %s", msg)
	}
}

func TestUnrecordedRequestFails(t *testing.T) {
	p, err := manifest.Load(filepath.Join(examples, "declarative-provider"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Case{Name: "miss", Dir: t.TempDir(), Input: map[string]any{"domain": "other.example"}, Secrets: map[string]string{"ACME_DATA_API_KEY": "k"}, Expect: Expect{Match: "exact"}}
	r := RunCase(context.Background(), p, c, Deps{})
	if r.Pass || !strings.Contains(strings.Join(r.Problems, " "), "unrecorded request: GET https://api.acme-data.example/v1/companies?domain=other.example") {
		t.Fatalf("%+v", r)
	}
	if !errors.Is(fmt.Errorf("x: %w", ErrUnrecorded), ErrUnrecorded) {
		t.Fatal("sentinel")
	}
}

func TestRecordThenReplay(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "live-secret-123" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=abc")
		io.WriteString(w, `{"person": {"email": "ada@acme.example", "echo": "live-secret-123"}}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	text := fmt.Sprintf(`manifest_version: "2"
name: recorded_provider
kind: provider
runtime: declarative
version: 0.1.0
capabilities: {network: ["https://127.0.0.1:*"], secrets: [LIVE_KEY]}
inputs: {domain: string}
capability: email
auth: {type: query, env_var: LIVE_KEY}
request: {method: GET, url: "%s/lookup?d={{input.domain}}"}
response: {mappings: {email: $.person.email, echo: $.person.echo}}
`, srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	caseDir, out, err := Record(context.Background(), p, RecordOptions{
		Case: "live", Inputs: map[string]any{"domain": "acme.example"}, Secrets: map[string]string{"LIVE_KEY": "live-secret-123"},
		ClientOptions: egress.Options{AllowPrivateForTesting: true, DefaultRPS: 1000, TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Fields["email"] != "ada@acme.example" {
		t.Fatalf("live outcome %+v", out)
	}
	var all strings.Builder
	files, _ := os.ReadDir(caseDir)
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(caseDir, f.Name()))
		all.Write(b)
	}
	if strings.Contains(all.String(), "live-secret-123") || strings.Contains(all.String(), "session=abc") {
		t.Fatalf("recorded fixture leaks credentials:\n%s", all.String())
	}
	if !strings.Contains(all.String(), "api_key=REDACTED") {
		t.Fatalf("expected redacted URL:\n%s", all.String())
	}
	c, err := LoadCase(filepath.Join(caseDir, "case.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if r := RunCase(context.Background(), p, c, Deps{AllowPrivateForTesting: true}); !r.Pass {
		t.Fatalf("replay of recorded case: %v", r.Problems)
	}
	if _, _, err := Record(context.Background(), p, RecordOptions{Case: "live"}); err == nil {
		t.Fatal("existing case must not be overwritten")
	}
}

var _ declarative.Extractor = cannedExtractor{}
