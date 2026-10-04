// Package fixture runs plugins offline against recorded HTTP fixtures and
// records new fixtures from live runs.
//
// Layout: <plugin>/fixtures/<case>/case.yaml (+ body files next to it):
//
//	description: optional text
//	input: {domain: acme.example}         # plugin inputs
//	secrets: {ACME_API_KEY: test-key}     # fake values for declared secrets
//	config: {}                            # wasm plugin config (optional)
//	http:                                 # recorded exchanges
//	  - method: GET
//	    url: https://acme.example/team
//	    status: 200
//	    headers: {Content-Type: text/html}
//	    body_file: 01-team.html           # or `body: "inline text"`
//	expect:
//	  match: exact                        # exact (default) or subset
//	  fields: {email: ada@acme.example}   # providers
//	  records: [{full_name: Ada}]         # scrapers
//	  error: http_404                     # expected provider error (optional)
//
// Replay serves exchanges from an in-process transport that fails on any
// unrecorded request (robots.txt, which the host fetches by itself, is
// answered 404 unless recorded). Matching is by method and URL with query
// parameters compared order-insensitively.
package fixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/wasmhost"
)

// Exchange is one recorded HTTP response.
type Exchange struct {
	Method   string            `yaml:"method"`
	URL      string            `yaml:"url"`
	Status   int               `yaml:"status"`
	Headers  map[string]string `yaml:"headers,omitempty"`
	BodyFile string            `yaml:"body_file,omitempty"`
	Body     string            `yaml:"body,omitempty"`
}

// Expect is the assertion block of a case.
type Expect struct {
	Match   string              `yaml:"match,omitempty"`
	Fields  map[string]any      `yaml:"fields,omitempty"`
	Records []map[string]string `yaml:"records,omitempty"`
	Error   string              `yaml:"error,omitempty"`
}

// Case is one fixtures/<case>/case.yaml.
type Case struct {
	Name        string            `yaml:"-"`
	Dir         string            `yaml:"-"`
	Description string            `yaml:"description,omitempty"`
	Input       map[string]any    `yaml:"input"`
	Secrets     map[string]string `yaml:"secrets,omitempty"`
	Config      map[string]any    `yaml:"config,omitempty"`
	HTTP        []Exchange        `yaml:"http"`
	Expect      Expect            `yaml:"expect"`
}

// LoadCases reads every fixtures/*/case.yaml of a plugin, sorted by name.
func LoadCases(pluginDir string) ([]*Case, error) {
	paths, err := filepath.Glob(filepath.Join(pluginDir, "fixtures", "*", "case.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []*Case
	for _, p := range paths {
		c, err := LoadCase(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// LoadCase reads one case.yaml.
func LoadCase(path string) (*Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Dir = filepath.Dir(path)
	c.Name = filepath.Base(c.Dir)
	if c.Expect.Match == "" {
		c.Expect.Match = "exact"
	}
	if c.Expect.Match != "exact" && c.Expect.Match != "subset" {
		return nil, fmt.Errorf("%s: expect.match must be exact or subset", path)
	}
	for i, ex := range c.HTTP {
		if ex.BodyFile != "" {
			if err := manifest.SafeRelPath(ex.BodyFile); err != nil {
				return nil, fmt.Errorf("%s: http[%d].body_file: %w", path, i, err)
			}
		}
		if ex.Status == 0 {
			c.HTTP[i].Status = 200
		}
		if c.HTTP[i].Method == "" {
			c.HTTP[i].Method = "GET"
		}
	}
	return &c, nil
}

// canonicalURL normalizes a URL for matching: lower-case scheme/host,
// default ports removed, no fragment, query parameters sorted.
func canonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = host + ":" + port
	}
	u.Host = host
	u.Fragment = ""
	u.RawQuery = u.Query().Encode()
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// Transport replays a case's exchanges and records misses.
type Transport struct {
	c      *Case
	mu     sync.Mutex
	used   map[int]int
	misses []string
}

// NewTransport builds the replay transport for a case.
func NewTransport(c *Case) *Transport { return &Transport{c: c, used: map[int]int{}} }

// ErrUnrecorded is returned for requests that have no recorded exchange.
var ErrUnrecorded = errors.New("fixture: unrecorded request")

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	want := canonicalURL(r.URL.String())
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, ex := range t.c.HTTP {
		if strings.EqualFold(ex.Method, r.Method) && canonicalURL(ex.URL) == want {
			t.used[i]++
			body := []byte(ex.Body)
			if ex.BodyFile != "" {
				b, err := os.ReadFile(filepath.Join(t.c.Dir, filepath.FromSlash(ex.BodyFile)))
				if err != nil {
					return nil, err
				}
				body = b
			}
			h := http.Header{}
			for k, v := range ex.Headers {
				h.Set(k, v)
			}
			return &http.Response{StatusCode: ex.Status, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
		}
	}
	if r.URL.Path == "/robots.txt" {
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	t.misses = append(t.misses, r.Method+" "+r.URL.String())
	return nil, fmt.Errorf("%w: %s %s", ErrUnrecorded, r.Method, r.URL)
}

// Misses lists unrecorded requests seen during replay.
func (t *Transport) Misses() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.misses...)
}

// Unused lists recorded exchanges that were never requested.
func (t *Transport) Unused() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for i, ex := range t.c.HTTP {
		if t.used[i] == 0 {
			out = append(out, ex.Method+" "+ex.URL)
		}
	}
	return out
}

// Deps are the optional engines a run needs.
type Deps struct {
	// Extractor is the extraction kernel; scrapers cannot run without it.
	Extractor declarative.Extractor
	// Version is used in the User-Agent.
	Version string
	// AllowPrivateForTesting lets replays use URLs recorded against local
	// test servers. Never set from configuration or the CLI.
	AllowPrivateForTesting bool
}

// Outcome is what a plugin produced for one input.
type Outcome struct {
	Fields   map[string]any      `json:"fields,omitempty"`
	Records  []map[string]string `json:"records,omitempty"`
	Error    string              `json:"error,omitempty"`
	Evidence any                 `json:"evidence,omitempty"`
	CostUSD  float64             `json:"cost_usd,omitempty"`
}

// Execute runs a plugin once through client and normalizes the outcome.
func Execute(ctx context.Context, p *manifest.Plugin, client *egress.Client, inputs map[string]any, config map[string]any, secrets map[string]string, deps Deps) (*Outcome, error) {
	switch {
	case p.Runtime == "declarative" && p.Kind == "provider":
		res, err := declarative.RunProvider(ctx, p, inputs, declarative.SecretMap(secrets), client)
		if err != nil {
			return nil, err
		}
		out := &Outcome{Fields: res.FieldsJSON, Error: res.Error, CostUSD: res.CostUSD}
		if res.Evidence != nil {
			out.Evidence = res.Evidence
		}
		if out.Fields == nil {
			out.Fields = map[string]any{}
		}
		return out, nil
	case p.Runtime == "declarative" && p.Kind == "scraper":
		if deps.Extractor == nil {
			return nil, declarative.ErrNoExtractor
		}
		res, err := declarative.RunScraper(ctx, p, inputs, declarative.ScraperDeps{Client: client, Extractor: deps.Extractor, Secrets: declarative.SecretMap(secrets)})
		if err != nil {
			return nil, err
		}
		out := &Outcome{Records: []map[string]string{}}
		var evidence []declarative.RecordEvidence
		for _, r := range res.Records {
			out.Records = append(out.Records, r.Fields)
			evidence = append(evidence, r.Evidence)
		}
		out.Evidence = evidence
		if res.Stopped != "" {
			out.Error = res.Stopped
		}
		return out, nil
	case p.Runtime == "wasm":
		host := wasmhost.New(wasmhost.Options{Client: client})
		pl, err := host.Load(ctx, p)
		if err != nil {
			return nil, err
		}
		defer pl.Close(ctx)
		raw, call, err := pl.Invoke(ctx, inputs, config, declarative.SecretMap(secrets))
		if err != nil {
			return nil, err
		}
		out := &Outcome{Evidence: call.Fetches}
		if f, ok := raw["fields"].(map[string]any); ok {
			out.Fields = f
		}
		if e, ok := raw["error"].(string); ok {
			out.Error = e
		}
		if recs, ok := raw["records"].([]any); ok {
			for _, r := range recs {
				m, _ := r.(map[string]any)
				fields, _ := m["fields"].(map[string]any)
				rec := map[string]string{}
				for k, v := range fields {
					rec[k] = fmt.Sprint(v)
				}
				out.Records = append(out.Records, rec)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("plugin runtime %s / kind %s cannot be run by this host yet", p.Runtime, p.Kind)
}

// Result is the verdict for one case.
type Result struct {
	Case     string   `json:"case"`
	Pass     bool     `json:"pass"`
	Problems []string `json:"problems,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// RunCase replays one case offline and compares the outcome.
func RunCase(ctx context.Context, p *manifest.Plugin, c *Case, deps Deps) Result {
	res := Result{Case: c.Name}
	tr := NewTransport(c)
	client, err := egress.New(egress.Options{Version: deps.Version, Transport: tr, ReplayWithoutRateLimit: true, AllowPrivateForTesting: deps.AllowPrivateForTesting})
	if err != nil {
		res.Problems = append(res.Problems, err.Error())
		return res
	}
	out, err := Execute(ctx, p, client, c.Input, c.Config, c.Secrets, deps)
	if err != nil {
		res.Problems = append(res.Problems, err.Error())
	} else {
		res.Problems = append(res.Problems, Compare(c.Expect, out)...)
	}
	for _, m := range tr.Misses() {
		res.Problems = append(res.Problems, "unrecorded request: "+m)
	}
	for _, u := range tr.Unused() {
		res.Warnings = append(res.Warnings, "recorded but not requested: "+u)
	}
	res.Pass = len(res.Problems) == 0
	return res
}

// Compare checks an outcome against expectations.
func Compare(exp Expect, out *Outcome) []string {
	var problems []string
	if exp.Error != out.Error {
		problems = append(problems, fmt.Sprintf("error: got %q, want %q", out.Error, exp.Error))
	}
	subset := exp.Match == "subset"
	if exp.Fields != nil || (out.Fields != nil && !subset && exp.Records == nil) {
		problems = append(problems, compareFields("fields", exp.Fields, out.Fields, subset)...)
	}
	if exp.Records != nil || (out.Records != nil && !subset && exp.Fields == nil) {
		if !subset && len(exp.Records) != len(out.Records) {
			problems = append(problems, fmt.Sprintf("records: got %d, want %d", len(out.Records), len(exp.Records)))
		}
		if subset && len(out.Records) < len(exp.Records) {
			problems = append(problems, fmt.Sprintf("records: got %d, want at least %d", len(out.Records), len(exp.Records)))
		}
		for i := 0; i < len(exp.Records) && i < len(out.Records); i++ {
			e := map[string]any{}
			for k, v := range exp.Records[i] {
				e[k] = v
			}
			a := map[string]any{}
			for k, v := range out.Records[i] {
				a[k] = v
			}
			problems = append(problems, compareFields(fmt.Sprintf("records[%d]", i), e, a, subset)...)
		}
	}
	return problems
}

func normalize(v any) any {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case float32:
		return float64(t)
	case map[string]any:
		m := map[string]any{}
		for k, x := range t {
			m[k] = normalize(x)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalize(x)
		}
		return out
	}
	return v
}

func compareFields(label string, want, got map[string]any, subset bool) []string {
	var problems []string
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	if !subset {
		for k := range got {
			keys[k] = true
		}
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		w, wok := want[k]
		g, gok := got[k]
		switch {
		case !gok:
			problems = append(problems, fmt.Sprintf("%s.%s: missing, want %v", label, k, w))
		case !wok:
			problems = append(problems, fmt.Sprintf("%s.%s: unexpected value %v", label, k, g))
		case !reflect.DeepEqual(normalize(w), normalize(g)):
			problems = append(problems, fmt.Sprintf("%s.%s: got %v, want %v", label, k, g, w))
		}
	}
	return problems
}
