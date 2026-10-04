package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// RecordOptions configures a live recording.
type RecordOptions struct {
	Case    string
	Inputs  map[string]any
	Config  map[string]any
	Secrets map[string]string // real values for this run; never written
	Deps    Deps
	// ClientOptions are used for the live egress client (proxy, version...).
	ClientOptions egress.Options
	Overwrite     bool
}

// keptHeaders are the response headers stored in fixtures.
var keptHeaders = []string{"Content-Type", "Location", "Content-Language"}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slug(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return "body"
	}
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(parsed.Host+parsed.Path), "-"), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "body"
	}
	return s
}

func extFor(contentType string) string {
	mt, _, _ := mime.ParseMediaType(contentType)
	switch {
	case strings.Contains(mt, "json"):
		return ".json"
	case strings.Contains(mt, "html"):
		return ".html"
	case strings.Contains(mt, "xml"):
		return ".xml"
	}
	return ".txt"
}

func redactAll(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, v, "REDACTED")
		if q := url.QueryEscape(v); q != v {
			s = strings.ReplaceAll(s, q, "REDACTED")
		}
	}
	return s
}

// Record runs the plugin live and writes fixtures/<case>/ with every
// exchange (secrets and credentials redacted) and the observed outcome as
// the expectation. Declared secrets are stored as the literal "REDACTED" so
// that replays render the same (redacted) URLs.
func Record(ctx context.Context, p *manifest.Plugin, opts RecordOptions) (string, *Outcome, error) {
	if opts.Case == "" {
		opts.Case = "recorded"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(opts.Case) {
		return "", nil, fmt.Errorf("case name must match ^[A-Za-z0-9_-]+$")
	}
	dir := filepath.Join(p.Dir, "fixtures", opts.Case)
	if _, err := os.Stat(dir); err == nil && !opts.Overwrite {
		return "", nil, fmt.Errorf("%s already exists (use --overwrite)", dir)
	}
	var mu sync.Mutex
	type captured struct {
		method, url string
		resp        *egress.Response
	}
	var exchanges []captured
	co := opts.ClientOptions
	co.OnResponse = func(method, u string, resp *egress.Response) {
		mu.Lock()
		exchanges = append(exchanges, captured{method, u, resp})
		mu.Unlock()
	}
	client, err := egress.New(co)
	if err != nil {
		return "", nil, err
	}
	out, err := Execute(ctx, p, client, opts.Inputs, opts.Config, opts.Secrets, opts.Deps)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	c := Case{Input: opts.Inputs, Config: opts.Config, Description: "recorded with opengtm plugin record", Secrets: map[string]string{}}
	for _, name := range p.Capabilities.Secrets {
		c.Secrets[name] = "REDACTED"
	}
	if len(c.Secrets) == 0 {
		c.Secrets = nil
	}
	for i, ex := range exchanges {
		h := map[string]string{}
		for _, k := range keptHeaders {
			if v := ex.resp.Header.Get(k); v != "" {
				h[k] = redactAll(v, opts.Secrets)
			}
		}
		file := fmt.Sprintf("%02d-%s%s", i+1, slug(ex.url), extFor(ex.resp.Header.Get("Content-Type")))
		body := redactAll(string(ex.resp.Body), opts.Secrets)
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			return "", nil, err
		}
		c.HTTP = append(c.HTTP, Exchange{Method: ex.method, URL: redactAll(ex.url, opts.Secrets), Status: ex.resp.Status, Headers: h, BodyFile: file})
	}
	c.Expect = Expect{Match: "exact", Fields: out.Fields, Records: out.Records, Error: redactAll(out.Error, opts.Secrets)}
	if p.Kind == "scraper" {
		c.Expect.Fields = nil
	} else {
		c.Expect.Records = nil
	}
	// Round-trip expectations through JSON so YAML gets plain types.
	if c.Expect.Fields != nil {
		b, _ := json.Marshal(c.Expect.Fields)
		_ = json.Unmarshal([]byte(redactAll(string(b), opts.Secrets)), &c.Expect.Fields)
	}
	data, err := yaml.Marshal(&c)
	if err != nil {
		return "", nil, err
	}
	header := "# Recorded fixture. Request credentials and Authorization headers are never stored;\n# declared secrets are replayed as \"REDACTED\".\n"
	if err := os.WriteFile(filepath.Join(dir, "case.yaml"), append([]byte(header), data...), 0o644); err != nil {
		return "", nil, err
	}
	return dir, out, nil
}
