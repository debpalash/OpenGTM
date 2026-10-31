package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/connectors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

func lookupOf(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

// No env and no checkout: the defaults are the manifests and trust store embedded in the binary.
func TestConnectorSourcesDefaultToBuiltin(t *testing.T) {
	t.Chdir(t.TempDir()) // no repo-relative paths exist
	dir, trust := connectorSources(lookupOf(nil))
	idx, err := manifest.LoadIndex(dir, "optional", manifest.LoadTrustStore(trust))
	if err != nil || len(idx) != 5 {
		t.Fatalf("builtin index: %d manifests, err %v", len(idx), err)
	}
	if rep := connectors.ValidateDirectory(dir, "optional", manifest.LoadTrustStore(trust)); rep.Count != 5 {
		t.Fatalf("builtin validation saw %d manifests", rep.Count)
	}
	// An empty override is "unset", as before.
	dir2, _ := connectorSources(lookupOf(map[string]string{connectorManifestsEnv: ""}))
	if dir2 != dir {
		t.Fatalf("empty override changed dir: %q", dir2)
	}
}

func TestConnectorSourcesEnvOverridesWin(t *testing.T) {
	dir, trust := connectorSources(lookupOf(map[string]string{connectorManifestsEnv: "/x/m", connectorTrustStoreEnv: "/x/t.json"}))
	if dir != "/x/m" || trust != "/x/t.json" {
		t.Fatalf("overrides ignored: %q %q", dir, trust)
	}
	// One override does not displace the other default.
	dir, trust = connectorSources(lookupOf(map[string]string{connectorManifestsEnv: "/x/m"}))
	if dir != "/x/m" || trust == "" || trust == "/x/t.json" {
		t.Fatalf("partial override: %q %q", dir, trust)
	}
}
