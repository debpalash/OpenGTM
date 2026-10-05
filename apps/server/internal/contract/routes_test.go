package contract

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// patternLiteral matches a ServeMux pattern for a Go-owned /api/v2 route as it
// is written in the source: "GET /api/v2/plugin-runs/{id}".
var patternLiteral = regexp.MustCompile(`"((?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) /api/v2/[^"\s]*)"`)

// TestEveryServedV2RouteIsDocumented fails when a handler registers a
// /api/v2 route that packages/contracts/openapi.v2.yaml does not describe. The
// opposite direction (a documented operation that is not served) is checked by
// the tests of the packages that serve them.
//
// Routes are found by scanning non-test Go sources for ServeMux pattern
// literals, so a new route cannot be added without this test noticing, whatever
// package registers it.
func TestEveryServedV2RouteIsDocumented(t *testing.T) {
	spec := Load(t)
	documented := map[string]bool{}
	for _, op := range spec.Operations() {
		documented[op.String()] = true
	}

	_, file, _, _ := runtime.Caller(0)
	serverRoot := filepath.Join(filepath.Dir(file), "..", "..")
	served := map[string]string{}
	err := filepath.WalkDir(serverRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range patternLiteral.FindAllStringSubmatch(string(src), -1) {
			rel, _ := filepath.Rel(serverRoot, path)
			served[m[1]] = rel
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(served) == 0 {
		t.Fatal("found no /api/v2 route patterns in the server sources; has the registration style changed?")
	}

	var undocumented []string
	for pattern, where := range served {
		if !documented[pattern] {
			undocumented = append(undocumented, pattern+" (registered in "+where+")")
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("routes served but missing from %s; document them (then run `bun run --cwd apps/web gen:api`):\n  %s",
			SpecPath, strings.Join(undocumented, "\n  "))
	}
}
