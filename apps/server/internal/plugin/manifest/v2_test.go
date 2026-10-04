package manifest

import (
	"bufio"
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

const contractSchema = "../../../../../packages/contracts/plugin-manifest.v2.schema.json"

type v2fixture struct {
	name, verdict, expect, schema string
	body                          []byte
}

func loadV2Fixtures(t *testing.T) []v2fixture {
	t.Helper()
	paths, err := filepath.Glob("testdata/v2/*.yaml")
	if err != nil || len(paths) < 50 {
		t.Fatalf("v2 corpus missing or too small: %d (%v)", len(paths), err)
	}
	var out []v2fixture
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f := v2fixture{name: strings.TrimSuffix(filepath.Base(p), ".yaml"), body: body}
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			line := sc.Text()
			if rest, ok := strings.CutPrefix(line, "# expect: "); ok {
				f.verdict, f.expect, _ = strings.Cut(rest, " ")
			} else if rest, ok := strings.CutPrefix(line, "# schema: "); ok {
				f.schema = rest
			}
		}
		out = append(out, f)
	}
	return out
}

// TestV2Fixtures checks the Go verdict and message for every v2 fixture.
func TestV2Fixtures(t *testing.T) {
	for _, f := range loadV2Fixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			p, err := LoadBytes(f.name, f.body)
			switch f.verdict {
			case "valid":
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				if p.ManifestVersion != "2" {
					t.Fatalf("manifest_version = %s", p.ManifestVersion)
				}
			case "invalid":
				if err == nil {
					t.Fatalf("expected invalid (%s)", f.expect)
				}
				if !strings.Contains(err.Error(), f.expect) {
					t.Fatalf("error %q does not contain %q", err, f.expect)
				}
			default:
				t.Fatalf("fixture header missing")
			}
		})
	}
}

// TestV2SchemaConsistency asserts that the published JSON Schema
// (packages/contracts/plugin-manifest.v2.schema.json) and the Go validator
// agree on every fixture: Go-valid manifests are schema-valid, structural
// rejections are schema-invalid, and the documented semantic-only rules are
// the only ones the schema lets through.
func TestV2SchemaConsistency(t *testing.T) {
	raw, err := os.ReadFile(contractSchema)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pycompat.LoadJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	plainSchema, err := pycompat.ToPlain(doc)
	if err != nil {
		t.Fatal(err)
	}
	sch, err := CompileSchema(plainSchema)
	if err != nil {
		t.Fatalf("contract schema does not compile: %v", err)
	}
	semantic := 0
	for _, f := range loadV2Fixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			v, err := pycompat.LoadYAML(f.body)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := pycompat.ToPlain(v)
			if err != nil {
				t.Fatal(err)
			}
			norm, err := normalizeJSON(plain)
			if err != nil {
				t.Fatal(err)
			}
			schemaErr := sch.Validate(norm)
			_, goErr := LoadBytes(f.name, f.body)
			goValid := goErr == nil
			schemaValid := schemaErr == nil
			if f.schema == "semantic" {
				semantic++
				if goValid || !schemaValid {
					t.Fatalf("semantic fixture: want go-invalid/schema-valid, got go=%v schema=%v (%v)", goErr, schemaValid, schemaErr)
				}
				return
			}
			if goValid != schemaValid {
				t.Fatalf("verdicts differ: go=%v schema=%v", goErr, schemaErr)
			}
		})
	}
	if semantic == 0 {
		t.Fatal("expected semantic-only fixtures")
	}
}

func TestNetworkPatterns(t *testing.T) {
	cases := []struct {
		pattern, url string
		ok           bool
	}{
		{"https://*.example.com", "https://example.com/x", true},
		{"https://*.example.com", "https://a.b.example.com/", true},
		{"https://*.example.com", "https://example.com.evil.net/", false},
		{"https://*.example.com", "https://notexample.com/", false},
		{"https://*.example.com", "http://a.example.com/", false},
		{"https://*.example.com", "https://a.example.com:8443/", false},
		{"https://*.example.com", "https://a.example.com:443/", true},
		{"https://*.example.com", "https://A.EXAMPLE.com./x", true},
		{"https://*.example.com", "https://user:pw@a.example.com/", false},
		{"https://api.example.com", "https://api.example.com/anything", true},
		{"https://api.example.com", "https://www.example.com/", false},
		{"https://api.example.com/v1", "https://api.example.com/v1", true},
		{"https://api.example.com/v1", "https://api.example.com/v1/x?q=1", true},
		{"https://api.example.com/v1", "https://api.example.com/v10", false},
		{"https://api.example.com/v1", "https://api.example.com/v1/../admin", false},
		{"https://api.example.com/v1/", "https://api.example.com/v1/a%2F..%2F..%2Fadmin", false},
		{"http://127.0.0.1:*", "http://127.0.0.1:5555/x", true},
		{"http://127.0.0.1:*", "https://127.0.0.1:5555/x", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8081/", false},
		{"https://*", "https://anything.example/", true},
		{"https://*", "http://anything.example/", false},
		{"https://[::1]:8443", "https://[::1]:8443/", true},
	}
	for _, c := range cases {
		p, err := ParsePattern(c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Match(u); got != c.ok {
			t.Errorf("%s vs %s: got %v want %v", c.pattern, c.url, got, c.ok)
		}
	}
	for _, bad := range []string{"https://", "https://*.", "https://a*.example", "ftp://x", "https://Example.com", "https://x.com?q", "https://x.com/#f", "*.example.com"} {
		if _, err := ParsePattern(bad); err == nil {
			t.Errorf("pattern %q should be rejected", bad)
		}
	}
}

func TestV1LoadsAsV2Provider(t *testing.T) {
	text := []byte("manifest_version: \"1\"\nname: acme_email\ncapability: email\nauth: {type: bearer, env_var: ACME_KEY}\nrequest:\n  url: https://api.acme.example:8443/find\n  timeout: 9\nresponse:\n  mappings: {email: $.email}\n")
	p, err := LoadBytes("x.yaml", text)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "provider" || p.Runtime != "declarative" || p.V1 == nil {
		t.Fatalf("unexpected: %+v", p)
	}
	if got := p.Capabilities.Network; len(got) != 1 || got[0] != "https://api.acme.example:8443" {
		t.Fatalf("network = %v", got)
	}
	if got := p.Capabilities.Secrets; len(got) != 1 || got[0] != "ACME_KEY" {
		t.Fatalf("secrets = %v", got)
	}
	if p.Limits.TimeoutSeconds != 9 {
		t.Fatalf("timeout = %v", p.Limits.TimeoutSeconds)
	}
	if err := p.Network().Allows("https://api.acme.example:8443/find"); err != nil {
		t.Fatal(err)
	}
	if err := p.Network().Allows("https://other.example/find"); err == nil {
		t.Fatal("other host must be denied")
	}
	templated, err := LoadBytes("t.yaml", bytes.Replace(text, []byte("api.acme.example:8443"), []byte("{{input.domain}}"), 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := templated.Capabilities.Network; len(got) != 1 || got[0] != "https://*" {
		t.Fatalf("templated host network = %v", got)
	}
	// v1 rules still apply through Load.
	if _, err := LoadBytes("h.yaml", bytes.Replace(text, []byte("https://"), []byte("http://"), 1)); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("v1 http endpoint must be rejected, got %v", err)
	}
}

func TestInputsValidation(t *testing.T) {
	p, err := LoadBytes("s.yaml", mustRead(t, "testdata/v2/valid_rfc_scraper.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateInputs(map[string]any{"domain": "acme.example"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateInputs(map[string]any{}); err == nil {
		t.Fatal("missing required input must fail")
	}
	if err := p.ValidateInputs(map[string]any{"domain": 5}); err == nil {
		t.Fatal("wrong input type must fail")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
