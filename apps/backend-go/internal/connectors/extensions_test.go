package connectors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// The validator refuses the Go-only extensions that break the credential rules.
func TestCheckManifestExtensions(t *testing.T) {
	const ok = `{"name":"vend_x","capability":"email","default_confidence":0.8,
	 "auth":{"type":"header","param":"X-Key","env_var":"VEND_KEY","secondary":{"type":"query","param":"secret","env_var":"VEND_SECRET"}},
	 "request":{"method":"POST","url":"https://api.vend.example/v1/submit","timeout":10},
	 "response":{"mappings":{"email":"$.data.email"},"success_when":{"path":"$.status","equals":"ok"}},
	 "poll":{"request":{"method":"GET","url":"https://api.vend.example/v1/jobs/{{response.id}}","timeout":10},
	  "interval":2,"max_attempts":10,"done_when":{"path":"$.state","equals":"done"}}}`
	check := func(js string) string {
		var m manifest.Manifest
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		return checkManifest(m, map[string]bool{})
	}
	if got := check(ok); got != "" {
		t.Fatalf("valid manifest refused: %s", got)
	}
	bad := map[string]string{
		"secondary denied":     strings.Replace(ok, `"env_var":"VEND_SECRET"`, `"env_var":"INTERNAL_SERVICE_TOKEN"`, 1),
		"primary denied":       strings.Replace(ok, `"env_var":"VEND_KEY"`, `"env_var":"SECRETS_MASTER_KEY"`, 1),
		"secondary same":       strings.Replace(ok, `"env_var":"VEND_SECRET"`, `"env_var":"VEND_KEY"`, 1),
		"secret in path":       strings.Replace(ok, `v1/submit"`, `v1/${env:VEND_SECRET}"`, 1),
		"poll over http":       strings.Replace(ok, `https://api.vend.example/v1/jobs`, `http://api.vend.example/v1/jobs`, 1),
		"poll other host":      strings.Replace(ok, `https://api.vend.example/v1/jobs`, `https://evil.example/v1/jobs`, 1),
		"poll host templated":  strings.Replace(ok, `https://api.vend.example/v1/jobs/{{response.id}}`, `https://{{response.id}}.vend.example/`, 1),
		"poll too many tries":  strings.Replace(ok, `"max_attempts":10`, `"max_attempts":1000`, 1),
		"success_when no path": strings.Replace(ok, `"path":"$.status","equals":"ok"`, `"equals":"ok"`, 1),
	}
	for name, js := range bad {
		if got := check(js); got == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}
