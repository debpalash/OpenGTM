// Package contract compares REST responses recorded from the FastAPI
// reference implementation against Go responses. It depends on the standard
// library only.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Req is the recorded request. Authorization and X-Workspace-Id header values
// are stored as "<redacted>".
type Req struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Resp is a recorded or observed response. Body holds the JSON body (or a JSON
// string for non-JSON bodies, or empty/null for no body).
type Resp struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Fixture is one recorded request/response pair.
type Fixture struct {
	Request  Req  `json:"request"`
	Response Resp `json:"response"`
}

// allowedHeaders are the only response headers compared (lower-case).
var allowedHeaders = []string{"content-type", "www-authenticate", "allow"}

// naiveISO matches pydantic naive-UTC datetimes: no zone, optional fraction.
var naiveISO = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?$`)

type config struct {
	ignore map[string]bool
	shape  map[string]bool
}

// Option customises AssertEqual.
type Option func(*config)

// IgnorePaths skips the given JSON paths (e.g. "$.id", "$.items[0].created_at").
func IgnorePaths(paths ...string) Option {
	return func(c *config) {
		for _, p := range paths {
			c.ignore[p] = true
		}
	}
}

// IgnoreShapeOf requires only the same JSON type at the given paths. Strings
// that are naive ISO timestamps in the expectation must stay naive ISO.
func IgnoreShapeOf(paths ...string) Option {
	return func(c *config) {
		for _, p := range paths {
			c.shape[p] = true
		}
	}
}

// Load reads testdata/contract/<name>.json relative to the module root.
func Load(t testing.TB, name string) Fixture {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("contract: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("contract: go.mod not found above working directory")
		}
		dir = parent
	}
	path := filepath.Join(dir, "testdata", "contract", filepath.FromSlash(name)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("contract: read fixture %s: %v", name, err)
	}
	var fx Fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("contract: parse fixture %s: %v", name, err)
	}
	return fx
}

// AssertEqual compares got against want: status and allow-listed headers
// exactly, bodies structurally (key order and whitespace ignored). It reports
// the first differing JSON path.
func AssertEqual(t testing.TB, got Resp, want Resp, opts ...Option) {
	t.Helper()
	cfg := &config{ignore: map[string]bool{}, shape: map[string]bool{}}
	for _, o := range opts {
		o(cfg)
	}
	if got.Status != want.Status {
		t.Errorf("contract: status: got %d, want %d", got.Status, want.Status)
		return
	}
	gh, wh := lowerHeaders(got.Headers), lowerHeaders(want.Headers)
	for _, name := range allowedHeaders {
		g, gok := gh[name]
		w, wok := wh[name]
		if gok != wok || g != w {
			t.Errorf("contract: header %s: got %q (present=%v), want %q (present=%v)", name, g, gok, w, wok)
			return
		}
	}
	gb, err := decode(got.Body)
	if err != nil {
		t.Errorf("contract: decode got body: %v", err)
		return
	}
	wb, err := decode(want.Body)
	if err != nil {
		t.Errorf("contract: decode want body: %v", err)
		return
	}
	if p, msg := diff("$", gb, wb, cfg); p != "" {
		t.Errorf("contract: body differs at %s: %s", p, msg)
	}
}

func lowerHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = v
	}
	return out
}

func decode(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// diff returns the first differing path and a message, or "" when equal.
func diff(path string, got, want any, cfg *config) (string, string) {
	if cfg.ignore[path] {
		return "", ""
	}
	if cfg.shape[path] {
		if kind(got) != kind(want) {
			return path, fmt.Sprintf("type got %s, want %s", kind(got), kind(want))
		}
		if ws, ok := want.(string); ok && naiveISO.MatchString(ws) {
			if gs, _ := got.(string); !naiveISO.MatchString(gs) {
				return path, "value is not a naive ISO timestamp"
			}
		}
		return "", ""
	}
	if kind(got) != kind(want) {
		return path, fmt.Sprintf("type got %s, want %s", kind(got), kind(want))
	}
	switch w := want.(type) {
	case map[string]any:
		g := got.(map[string]any)
		keys := make([]string, 0, len(w)+len(g))
		seen := map[string]bool{}
		for k := range w {
			keys = append(keys, k)
			seen[k] = true
		}
		for k := range g {
			if !seen[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "." + k
			gv, gok := g[k]
			wv, wok := w[k]
			switch {
			case gok && !wok:
				if !cfg.ignore[p] {
					return p, "unexpected key in got"
				}
			case !gok && wok:
				if !cfg.ignore[p] {
					return p, "missing key in got"
				}
			default:
				if dp, msg := diff(p, gv, wv, cfg); dp != "" {
					return dp, msg
				}
			}
		}
	case []any:
		g := got.([]any)
		n := len(w)
		if len(g) < n {
			n = len(g)
		}
		for i := 0; i < n; i++ {
			if dp, msg := diff(fmt.Sprintf("%s[%d]", path, i), g[i], w[i], cfg); dp != "" {
				return dp, msg
			}
		}
		if len(g) != len(w) {
			return path, fmt.Sprintf("array length got %d, want %d", len(g), len(w))
		}
	case json.Number:
		if got.(json.Number).String() != w.String() {
			return path, fmt.Sprintf("got %s, want %s", got, w)
		}
	default:
		if got != want {
			return path, fmt.Sprintf("got %v, want %v", got, want)
		}
	}
	return "", ""
}
