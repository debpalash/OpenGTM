package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

type v1Expected struct {
	Cases map[string]struct {
		ExactMessages   bool     `json:"exact_messages"`
		Policy          string   `json:"policy"`
		Trust           string   `json:"trust"`
		OK              bool     `json:"ok"`
		SignaturePolicy string   `json:"signature_policy"`
		Count           int      `json:"count"`
		Connectors      []string `json:"connectors"`
		Errors          []struct {
			Path     string `json:"path"`
			Error    string `json:"error"`
			Pydantic []struct {
				Loc  []any  `json:"loc"`
				Type string `json:"type"`
			} `json:"pydantic"`
		} `json:"errors"`
		JSON string `json:"json"`
	} `json:"cases"`
	Real struct {
		OK         bool     `json:"ok"`
		Count      int      `json:"count"`
		Connectors []string `json:"connectors"`
	} `json:"real"`
}

func loadV1Expected(t *testing.T) v1Expected {
	t.Helper()
	raw, err := os.ReadFile("testdata/v1/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var exp v1Expected
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	return exp
}

func issueKey(loc []any, typ string) string {
	parts := make([]string, len(loc))
	for i, l := range loc {
		switch v := l.(type) {
		case float64: // JSON numbers from the fixture
			parts[i] = pycompat.FloatRepr(v)
			if v == float64(int64(v)) {
				parts[i] = strings.TrimSuffix(parts[i], ".0")
			}
		case int64:
			parts[i] = pycompat.Repr(v)
		case int:
			parts[i] = pycompat.Repr(int64(v))
		default:
			parts[i] = pycompat.Str(v)
		}
	}
	return strings.Join(parts, ".") + "|" + typ
}

// TestV1ParityWithPython runs every corpus directory through ValidateDirectory
// and compares against validate_manifest_directory() output recorded by
// testdata/gen_v1_parity.py.
func TestV1ParityWithPython(t *testing.T) {
	exp := loadV1Expected(t)
	if len(exp.Cases) < 100 {
		t.Fatalf("corpus too small: %d cases", len(exp.Cases))
	}
	names := make([]string, 0, len(exp.Cases))
	for n := range exp.Cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := exp.Cases[name]
		t.Run(name, func(t *testing.T) {
			dir, _ := filepath.Abs(filepath.Join("testdata/v1/cases", name))
			trust, _ := filepath.Abs(filepath.Join("testdata/v1", "trust-"+c.Trust+".json"))
			policy := c.Policy
			if policy != "optional" && policy != "required" {
				t.Setenv("CONNECTOR_SIGNATURE_POLICY", policy)
				policy = ""
			}
			rep := ValidateDirectory(dir, DirectoryOptions{SignaturePolicy: policy, TrustStore: trust})
			if rep.OK != c.OK {
				t.Fatalf("ok: go=%v py=%v; go errors=%+v", rep.OK, c.OK, rep.Errors)
			}
			if rep.SignaturePolicy != c.SignaturePolicy {
				t.Errorf("policy: go=%s py=%s", rep.SignaturePolicy, c.SignaturePolicy)
			}
			if len(rep.Connectors) != c.Count {
				t.Fatalf("count: go=%d py=%d", len(rep.Connectors), c.Count)
			}
			conns := rep.PyValue()
			list, _ := conns.Get("connectors")
			for i, want := range c.Connectors {
				got, err := pycompat.Canonical(list.([]any)[i])
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Errorf("connector %d\n go: %s\n py: %s", i, got, want)
				}
			}
			if len(rep.Errors) != len(c.Errors) {
				t.Fatalf("errors: go=%d %+v py=%d %+v", len(rep.Errors), rep.Errors, len(c.Errors), c.Errors)
			}
			for i, want := range c.Errors {
				got := rep.Errors[i]
				rel, _ := filepath.Rel(dir, got.Path)
				if rel != want.Path {
					t.Errorf("error %d path: go=%s py=%s", i, rel, want.Path)
				}
				msg := strings.ReplaceAll(got.Error, dir, "<DIR>")
				if want.Pydantic != nil {
					var me *ModelError
					if !errors.As(got.Err, &me) {
						t.Fatalf("python raised ValidationError %v, go: %v", want.Pydantic, got.Err)
					}
					var gotKeys, wantKeys []string
					for _, is := range me.Issues {
						gotKeys = append(gotKeys, issueKey(is.Loc, is.Type))
					}
					for _, is := range want.Pydantic {
						wantKeys = append(wantKeys, issueKey(is.Loc, is.Type))
					}
					sort.Strings(gotKeys)
					sort.Strings(wantKeys)
					if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
						t.Errorf("pydantic issues\n go: %v\n py: %v", gotKeys, wantKeys)
					}
					firstLine := strings.SplitN(want.Error, "\n", 2)[0]
					if !strings.HasPrefix(msg, firstLine) {
						t.Errorf("message header: go=%q py=%q", msg, firstLine)
					}
					continue
				}
				if c.ExactMessages {
					if msg != want.Error {
						t.Errorf("message\n go: %q\n py: %q", msg, want.Error)
					}
				} else if strings.HasPrefix(want.Error, "connector signature is ") {
					prefix := strings.SplitN(want.Error, ":", 2)[0]
					if !strings.HasPrefix(msg, prefix) {
						t.Errorf("message prefix: go=%q py=%q", msg, want.Error)
					}
				}
			}
			if c.ExactMessages && rep.OK {
				out, err := rep.JSON()
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.ReplaceAll(string(out), dir, "<DIR>"); got != c.JSON {
					t.Errorf("--json report differs\n go: %s\n py: %s", got, c.JSON)
				}
			}
		})
	}
}

// TestV1RealManifestsMatchPython validates the bundled production manifests.
func TestV1RealManifestsMatchPython(t *testing.T) {
	exp := loadV1Expected(t)
	dir := filepath.Join("..", "..", "..", "..", "api", "services", "leadgen", "enrichment", "declarative", "manifests")
	trust := filepath.Join("..", "..", "..", "..", "..", "docs", "connectors", "trusted-publishers.json")
	rep := ValidateDirectory(dir, DirectoryOptions{SignaturePolicy: "optional", TrustStore: trust})
	if !rep.OK || len(rep.Connectors) != exp.Real.Count || exp.Real.Count == 0 {
		t.Fatalf("real manifests: ok=%v count=%d errors=%+v (python count %d)", rep.OK, len(rep.Connectors), rep.Errors, exp.Real.Count)
	}
	list, _ := rep.PyValue().Get("connectors")
	for i, want := range exp.Real.Connectors {
		got, _ := pycompat.Canonical(list.([]any)[i])
		if string(got) != want {
			t.Errorf("connector %d\n go: %s\n py: %s", i, got, want)
		}
	}
}
