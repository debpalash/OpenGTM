//go:build integration

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	connManifests = "../connectors/testdata/manifests"
	connTrust     = "../connectors/testdata/trusted-publishers.json"
	connCerts     = "../certification/testdata/certifications.json"
)

type connMeta struct {
	BuildSHA  string `json:"build_sha"`
	Key       string `json:"certification_key"`
	PinnedNow string `json:"pinned_now"`
}

type connEnv struct {
	mcpEnv
	meta   connMeta
	alice  map[string]string
	bob    map[string]string
	erin   map[string]string
	secret string
}

func newConnEnv(t *testing.T) *connEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	e := &connEnv{mcpEnv: mcpEnv{env: env{st: identity.NewStore(pool)}, pool: pool}, secret: "alpha-secret-value"}
	raw, err := os.ReadFile("../certification/testdata/certification_meta.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &e.meta); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, strings.Replace(e.meta.PinnedNow, "+00:00", "Z", 1))
	if err != nil {
		t.Fatal(err)
	}
	prev := connectorsNow
	connectorsNow = func() time.Time { return now }
	t.Cleanup(func() { connectorsNow = prev })

	_, e.alice = e.admin(t, "ws-alpha", "alice")
	_, e.bob = e.admin(t, "ws-beta", "bob")
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.erin = bearer("erin")
	e.erin["X-Workspace-Id"] = "ws-beta"

	ctx := context.Background()
	clean := func() {
		for _, ws := range []string{"ws-alpha", "ws-beta"} {
			_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM workspace_settings WHERE workspace_id=$1 AND key IN ('CONTRACT_TRUSTED_KEY','HUBSPOT_TOKEN')`, ws)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)
	err = tenancy.InWorkspace(ctx, pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES
			('ws-alpha','CONTRACT_TRUSTED_KEY',$1), ('ws-alpha','HUBSPOT_TOKEN','x')`, e.secret)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// serve rebuilds the router over a manifests directory and signature policy.
func (e *connEnv) serve(t *testing.T, dir, policy string) {
	t.Helper()
	vars := map[string]string{}
	if policy != "" {
		vars["CONNECTOR_SIGNATURE_POLICY"] = policy
	}
	e.serveVars(t, dir, vars)
}

// serveVars is serve with an explicit environment (a key present with an empty value is "set").
func (e *connEnv) serveVars(t *testing.T, dir string, vars map[string]string) {
	t.Helper()
	vars["OPENGTM_CONNECTOR_MANIFESTS_DIR"], vars["OPENGTM_CONNECTOR_TRUST_STORE"] = dir, connTrust
	prev := connectorsLookupEnv
	connectorsLookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	t.Cleanup(func() { connectorsLookupEnv = prev })
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "conn-routes-test-key", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	RegisterConnectors(r, Deps{
		Pool: e.pool, Signer: testSigner, Identity: e.st, Envelope: env,
		Getenv: func(k string) string { return vars[k] },
		Flags: config.Flags{PGLeadStore: true, GoConnectorRoutesEnabled: true,
			CertificationPath: connCerts, CertificationKey: config.Secret(e.meta.Key), BuildSHA: e.meta.BuildSHA},
	})
	e.h = r.Handler()
}

func (e *connEnv) replayer(seen map[string]bool) *replayer {
	return &replayer{seen: seen, do: func(m, p, b string, h map[string]string) contract.Resp {
		_, got := e.do(m, p, "application/json", b, h)
		return got
	}}
}

// copyManifests copies the manifests tree, skipping the named files.
func copyManifests(t *testing.T, skip ...string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(connManifests, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		for _, s := range skip {
			if d.Name() == s {
				return nil
			}
		}
		rel, _ := filepath.Rel(connManifests, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestCatalogMatchesFixture(t *testing.T) {
	e := newConnEnv(t)
	seen := map[string]bool{}
	rp := e.replayer(seen)
	ok, full := copyManifests(t, "contract_badname.yaml"), copyManifests(t)

	e.serve(t, ok, "optional")
	rp.replay(t, "connectors/catalog_optional_alice_alpha", e.alice)
	rp.replay(t, "connectors/catalog_optional_bob_beta", e.bob)
	e.serve(t, full, "required")
	rp.replay(t, "connectors/catalog_required_alice_alpha", e.alice)
	rp.replay(t, "connectors/catalog_required_bob_beta", e.bob)

	// Python answers 500 (certification subject is unknown) for the bad manifest name;
	// Go answers the documented 422 without echoing the name.
	e.serve(t, full, "optional")
	fx := contract.Load(t, "connectors/catalog_optional_badname_crash_alice")
	seen["connectors/catalog_optional_badname_crash_alice"] = true
	if fx.Response.Status != 500 {
		t.Fatalf("fixture drifted: %d", fx.Response.Status)
	}
	rec, _ := e.do("GET", "/api/connectors/catalog", "", "", e.alice)
	if rec.Code != 422 || strings.Contains(rec.Body.String(), "Bad-Name") {
		t.Fatalf("bad manifest name: %d %s", rec.Code, rec.Body.String())
	}
	var d map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &d) != nil || d["detail"] == nil {
		t.Fatalf("422 body not a detail object: %s", rec.Body.String())
	}

	rp.replay(t, "connectors/catalog_nonmember_erin_beta", e.erin)
	rp.replay(t, "connectors/catalog_no_auth", nil)
	for _, n := range []string{"compat_alice_alpha", "compat_bob_beta", "compat_required_alice_alpha", "compat_no_auth"} {
		seen["connectors/"+n] = true // replayed by TestCompatibilityMatchesFixture
	}
	rp.assertAllReplayed(t, "connectors")
}

func TestCompatibilityMatchesFixture(t *testing.T) {
	e := newConnEnv(t)
	const fixturePrefix = "../../apps/backend-go/internal/connectors/"
	abs, err := filepath.Abs("../connectors") // the loader refuses ".." in the served directory
	if err != nil {
		t.Fatal(err)
	}
	servedPrefix := abs + "/testdata/manifests/"
	norm := func(r contract.Resp, prefix string) contract.Resp {
		r.Body = json.RawMessage(strings.ReplaceAll(string(r.Body), prefix, ""))
		return r
	}
	for _, c := range []struct{ fixture, policy string }{
		{"compat_alice_alpha", "optional"}, {"compat_bob_beta", "optional"}, {"compat_required_alice_alpha", "required"},
	} {
		e.serve(t, abs+"/testdata/manifests", c.policy)
		auth := e.alice
		if c.fixture == "compat_bob_beta" {
			auth = e.bob
		}
		fx := contract.Load(t, "connectors/"+c.fixture)
		_, got := e.do("GET", "/api/connectors/compatibility", "", "", auth)
		t.Run(c.fixture, func(t *testing.T) {
			if strings.Contains(string(got.Body), abs) {
				t.Fatalf("compatibility leaks the absolute manifest path: %s", got.Body)
			}
			contract.AssertEqual(t, norm(got, servedPrefix), norm(fx.Response, fixturePrefix+"testdata/manifests/"))
		})
	}
	e.serve(t, abs+"/testdata/manifests", "optional")
	nf := contract.Load(t, "connectors/compat_no_auth")
	_, got := e.do("GET", "/api/connectors/compatibility", "", "", nil)
	contract.AssertEqual(t, got, nf.Response)
	nm, _ := e.do("GET", "/api/connectors/compatibility", "", "", e.erin)
	if nm.Code != 403 {
		t.Fatalf("nonmember compat: %d", nm.Code)
	}
}

func TestCatalogRequiresWorkspace(t *testing.T) {
	e := newConnEnv(t)
	e.serve(t, copyManifests(t, "contract_badname.yaml"), "optional")
	for _, p := range []string{"/api/connectors/catalog", "/api/connectors/compatibility"} {
		if rec, _ := e.do("GET", p, "", "", nil); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s no auth: %d", p, rec.Code)
		}
		if rec, _ := e.do("GET", p, "", "", e.erin); rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace access denied") {
			t.Errorf("%s nonmember: %d %s", p, rec.Code, rec.Body.String())
		}
		noWS := bearer("alice")
		noWS["X-Workspace-Id"] = "ws-does-not-exist"
		if rec, _ := e.do("GET", p, "", "", noWS); rec.Code != 403 && rec.Code != 404 {
			t.Errorf("%s unknown workspace: %d", p, rec.Code)
		}
	}
}

func TestCompatibilityHasNoCredentialKey(t *testing.T) {
	e := newConnEnv(t)
	for _, dir := range []string{copyManifests(t, "contract_badname.yaml"), copyManifests(t)} {
		e.serve(t, dir, "optional")
		for _, p := range []string{"/api/connectors/compatibility", "/api/connectors/catalog"} {
			rec, _ := e.do("GET", p, "", "", e.alice)
			body := rec.Body.String()
			for _, bad := range []string{"credential_key", e.secret, e.meta.Key, "CONTRACT_TRUSTED_KEY\":"} {
				if strings.Contains(body, bad) {
					t.Errorf("%s leaks %q", p, bad)
				}
			}
		}
	}
}

func TestCatalogConfiguredPerWorkspace(t *testing.T) {
	e := newConnEnv(t)
	e.serve(t, copyManifests(t, "contract_badname.yaml"), "optional")
	configured := func(h map[string]string) map[string]bool {
		rec, _ := e.do("GET", "/api/connectors/catalog", "", "", h)
		var body struct {
			Connectors []struct {
				ID         string `json:"id"`
				Configured bool   `json:"configured"`
			} `json:"connectors"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("catalog: %d", rec.Code)
		}
		out := map[string]bool{}
		for _, c := range body.Connectors {
			out[c.ID] = out[c.ID] || c.Configured
		}
		return out
	}
	a, b := configured(e.alice), configured(e.bob)
	if !a["contract_trusted"] || !a["contract_http"] {
		t.Errorf("ws-alpha holds the secret but sees %v", a)
	}
	if b["contract_trusted"] || b["contract_http"] {
		t.Errorf("ws-alpha secret served ws-beta: %v", b)
	}
}

// Python: CONNECTOR_SIGNATURE_POLICY set but empty is not "optional|required", so it is "required".
func TestEmptySignaturePolicyIsRequired(t *testing.T) {
	e := newConnEnv(t)
	dir := copyManifests(t, "contract_badname.yaml")
	policyOf := func(vars map[string]string) string {
		e.serveVars(t, dir, vars)
		rec, _ := e.do("GET", "/api/connectors/compatibility", "", "", e.alice)
		var b struct {
			P string `json:"signature_policy"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &b) != nil {
			t.Fatalf("compat: %d", rec.Code)
		}
		return b.P
	}
	if got := policyOf(map[string]string{"CONNECTOR_SIGNATURE_POLICY": ""}); got != "required" {
		t.Errorf("set-but-empty policy = %q, want required", got)
	}
	if got := policyOf(map[string]string{}); got != "optional" {
		t.Errorf("unset policy = %q, want optional", got)
	}
}
