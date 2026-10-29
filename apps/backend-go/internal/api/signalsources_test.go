//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func newSourcesEnv(t *testing.T) sigEnv {
	return newSourcesEnvFlags(t, config.Flags{PGLeadStore: true})
}

func newSourcesEnvFlags(t *testing.T, flags config.Flags) sigEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: flags}
	registerSignalSources(r, d)
	e := sigEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedSignals(t) // users and workspaces only matter here
	return e
}

// TestSignalSourcesMatchesFixture compares GET /api/signals/sources with the FastAPI recordings
// (testdata/contract/signal_sources/sources_*.json). No certification evidence is configured, so every
// source is "beta" with certification null, as in the recording.
func TestSignalSourcesMatchesFixture(t *testing.T) {
	e := newSourcesEnv(t)
	for _, c := range []struct{ name, who string }{{"signal_sources/sources_catalog", "alice"}, {"signal_sources/sources_no_auth", ""}} {
		fx := contract.Load(t, c.name)
		_, got := e.req("GET", "/api/signals/sources", c.who, "", "")
		contract.AssertEqual(t, got, fx.Response)
	}
}

// certFile writes one attested controlled-live certificate for subject and returns its path.
func certFile(t *testing.T, subject, key, build string, mutate func(map[string]any)) string {
	t.Helper()
	checks := map[string]any{}
	for _, name := range certification.RequiredChecks(subject) {
		checks[name] = map[string]any{"passed": true, "evidence": "run log " + name}
	}
	now := time.Now().UTC()
	c := map[string]any{
		"subject_id": subject, "status": "supported", "mode": "controlled_live", "validation_run_id": "run-1",
		"build_sha": build, "validated_at": now.Add(-time.Hour).Format("2006-01-02T15:04:05+00:00"),
		"expires_at":   now.Add(30 * 24 * time.Hour).Format("2006-01-02T15:04:05+00:00"),
		"evidence_url": "https://evidence.example.com/run-1", "evidence_sha256": strings.Repeat("ab", 32),
		"external_system_id_hash": strings.Repeat("cd", 32), "checks": checks,
	}
	if mutate != nil {
		mutate(c)
	}
	att, err := certification.Attest(c, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := canonjson.Marshal([]any{att}, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "certs.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sourceMaturity(t *testing.T, body []byte) map[string]struct {
	Maturity      string          `json:"maturity"`
	Certification json.RawMessage `json:"certification"`
} {
	t.Helper()
	var doc struct {
		Sources []struct {
			ID            string          `json:"id"`
			Maturity      string          `json:"maturity"`
			Certification json.RawMessage `json:"certification"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct {
		Maturity      string          `json:"maturity"`
		Certification json.RawMessage `json:"certification"`
	}{}
	for _, s := range doc.Sources {
		out[s.ID] = struct {
			Maturity      string          `json:"maturity"`
			Certification json.RawMessage `json:"certification"`
		}{s.Maturity, s.Certification}
	}
	return out
}

// TestSignalSourcesCertifierWired: a valid attested certificate (internal/certification) makes its source
// "supported" with the certification projection; a wrong key, a stale build sha and other sources stay "beta".
func TestSignalSourcesCertifierWired(t *testing.T) {
	const key, build = "test-cert-key-not-a-secret", "build-abc123"
	for _, c := range []struct {
		name      string
		flags     func(path string) config.Flags
		supported bool
	}{
		{"valid", func(p string) config.Flags {
			return config.Flags{PGLeadStore: true, CertificationPath: p, CertificationKey: config.Secret(key), BuildSHA: build}
		}, true},
		{"wrong key", func(p string) config.Flags {
			return config.Flags{PGLeadStore: true, CertificationPath: p, CertificationKey: config.Secret("other-key"), BuildSHA: build}
		}, false},
		{"stale build sha", func(p string) config.Flags {
			return config.Flags{PGLeadStore: true, CertificationPath: p, CertificationKey: config.Secret(key), BuildSHA: "build-new"}
		}, false},
		{"no key configured", func(p string) config.Flags {
			return config.Flags{PGLeadStore: true, CertificationPath: p, BuildSHA: build}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := certFile(t, "jobspy", key, build, nil)
			e := newSourcesEnvFlags(t, c.flags(path))
			_, got := e.req("GET", "/api/signals/sources", "alice", "", "")
			if got.Status != 200 {
				t.Fatalf("status %d %s", got.Status, got.Body)
			}
			m := sourceMaturity(t, got.Body)
			if len(m) != 5 {
				t.Fatalf("sources = %d", len(m))
			}
			for id, s := range m {
				wantSupported := c.supported && id == "jobspy"
				if (s.Maturity == "supported") != wantSupported {
					t.Errorf("%s maturity = %s (supported wanted: %v)", id, s.Maturity, wantSupported)
				}
				if !wantSupported && string(s.Certification) != "null" {
					t.Errorf("%s certification = %s, want null", id, s.Certification)
				}
			}
			if c.supported {
				var cert map[string]any
				if err := json.Unmarshal(m["jobspy"].Certification, &cert); err != nil {
					t.Fatal(err)
				}
				if cert["build_sha"] != build || cert["mode"] != "controlled_live" || cert["validation_run_id"] != "run-1" || len(cert) != 7 {
					t.Fatalf("certification projection = %v", cert)
				}
			}
			if strings.Contains(string(got.Body), key) {
				t.Fatal("certification key leaked into the response")
			}
		})
	}
}
