package enrich

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/pluginrun"
)

// mustStrictClient is an egress client without the loopback override.
func mustStrictClient(t testing.TB) *egress.Client {
	t.Helper()
	c, err := egress.New(egress.Options{Version: "test", DefaultRPS: 5000})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mapConnectors is an in-memory Connectors built from v1 manifest files.
type mapConnectors map[string]*pluginrun.Entry

func (m mapConnectors) Get(name string) (*pluginrun.Entry, bool) {
	e, ok := m[name]
	return e, ok
}

// loadConnectors parses every *.yaml in dir as a v1 connector (the same code
// path as the catalog, without the directory report).
func loadConnectors(t testing.TB, dir string) mapConnectors {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := mapConnectors{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		p, err := manifest.LoadBytes(f, raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if p.V1 == nil {
			t.Fatalf("%s is not a v1 connector", f)
		}
		out[p.Name] = &pluginrun.Entry{Plugin: p, Source: "connector"}
	}
	if len(out) == 0 {
		t.Fatalf("no connectors in %s", dir)
	}
	return out
}
