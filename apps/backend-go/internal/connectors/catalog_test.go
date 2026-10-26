package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// fakeSecrets mirrors secrets.Resolver.Get: workspace value, then global.
type fakeSecrets struct {
	ws     map[string]string // "<ws>/<key>"
	global map[string]string
	err    error
}

func (f fakeSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	if v := f.ws[ws+"/"+key]; v != "" {
		return v, "workspace", nil
	}
	if v := f.global[key]; v != "" {
		return v, "global", nil
	}
	return "", "default", nil
}

type certMeta struct {
	BuildSHA  string            `json:"build_sha"`
	Key       string            `json:"certification_key"`
	Manifests map[string]string `json:"manifest_sha256"`
	PinnedNow string            `json:"pinned_now"`
}

func loadMeta(t *testing.T) certMeta {
	t.Helper()
	raw, err := os.ReadFile("../certification/testdata/certification_meta.json")
	if err != nil {
		t.Fatal(err)
	}
	var m certMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func certOptions(t *testing.T) certification.Options {
	t.Helper()
	m := loadMeta(t)
	now, err := time.Parse(time.RFC3339, strings.Replace(m.PinnedNow, "+00:00", "Z", 1))
	if err != nil {
		t.Fatal(err)
	}
	return certification.Options{
		Path: "../certification/testdata/certifications.json", Key: m.Key, BuildSHA: m.BuildSHA,
		Now: func() time.Time { return now },
	}
}

var alphaSecrets = fakeSecrets{ws: map[string]string{
	"ws-alpha/CONTRACT_TRUSTED_KEY": "alpha-secret-value", "ws-alpha/HUBSPOT_TOKEN": "x",
}}

// dirWithout copies testdata/manifests to a temp dir, skipping the named files.
func dirWithout(t *testing.T, skip ...string) string {
	t.Helper()
	dst := t.TempDir()
	src := "testdata/manifests"
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		for _, s := range skip {
			if d.Name() == s {
				return nil
			}
		}
		rel, _ := filepath.Rel(src, p)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, body, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func deps(t *testing.T, dir, policy string, sec SecretGetter) Deps {
	t.Helper()
	return Deps{
		Dir: dir, Policy: policy, Trust: manifest.LoadTrustStore("testdata/trusted-publishers.json"),
		Secrets: sec, Cert: certOptions(t),
	}
}

func fixtureResp(t *testing.T, name string, c CatalogResponse) (contract.Resp, contract.Resp) {
	t.Helper()
	fx := contract.Load(t, "connectors/"+name)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: b}, fx.Response
}

func TestCatalogMatchesFixture(t *testing.T) {
	ctx := context.Background()
	ok := dirWithout(t, "contract_badname.yaml") // the catalog_ok recording profile
	full := dirWithout(t)
	cases := []struct {
		fixture, dir, policy, ws string
	}{
		{"catalog_optional_alice_alpha", ok, "optional", "ws-alpha"},
		{"catalog_optional_bob_beta", ok, "optional", "ws-beta"},
		{"catalog_required_alice_alpha", full, "required", "ws-alpha"},
		{"catalog_required_bob_beta", full, "required", "ws-beta"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			got, err := Catalog(ctx, deps(t, c.dir, c.policy, alphaSecrets), c.ws)
			if err != nil {
				t.Fatal(err)
			}
			g, w := fixtureResp(t, c.fixture, got)
			contract.AssertEqual(t, g, w)
		})
	}
}

func TestCatalogBadManifestNameIsClientError(t *testing.T) {
	// Python answers 500 here (certification subject is unknown); Go reports a
	// hardened client error (plan D4, documented in testdata/contract/README.md).
	fx := contract.Load(t, "connectors/catalog_optional_badname_crash_alice")
	if fx.Response.Status != 500 {
		t.Fatalf("fixture drifted: %d", fx.Response.Status)
	}
	_, err := Catalog(context.Background(), deps(t, dirWithout(t), "optional", alphaSecrets), "ws-alpha")
	if !errors.Is(err, ErrUnknownConnectorSubject) {
		t.Fatalf("want ErrUnknownConnectorSubject, got %v", err)
	}
	if UnknownSubjectStatus < 400 || UnknownSubjectStatus > 499 {
		t.Fatalf("status %d is not 4xx", UnknownSubjectStatus)
	}
	if strings.Contains(err.Error(), "Bad-Name") {
		t.Fatalf("error must not echo the manifest name: %v", err)
	}
}

func catalogByID(t *testing.T, ws string, sec SecretGetter) map[string][]CatalogEntry {
	t.Helper()
	c, err := Catalog(context.Background(), deps(t, dirWithout(t, "contract_badname.yaml"), "optional", sec), ws)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]CatalogEntry{}
	for _, e := range c.Connectors {
		out[e.ID] = append(out[e.ID], e)
	}
	if c.Total != len(c.Connectors) {
		t.Fatalf("total %d != %d", c.Total, len(c.Connectors))
	}
	return out
}

func TestConfiguredReflectsWorkspaceSecret(t *testing.T) {
	m := catalogByID(t, "ws-alpha", alphaSecrets)
	if !m["contract_trusted"][0].Configured {
		t.Fatal("alpha has CONTRACT_TRUSTED_KEY")
	}
	if !m["contract_unsigned"][0].Configured {
		t.Fatal("auth type none is always configured")
	}
	// the global fallback (single-tenant installs) counts, as Python get_secret
	g := fakeSecrets{global: map[string]string{"CONTRACT_TRUSTED_KEY": "g"}}
	if !catalogByID(t, "ws-beta", g)["contract_trusted"][0].Configured {
		t.Fatal("global fallback must configure")
	}
}

func TestOtherWorkspaceSecretNotConfigured(t *testing.T) {
	m := catalogByID(t, "ws-beta", alphaSecrets)
	for _, id := range []string{"contract_trusted", "contract_forged", "contract_http"} {
		if m[id][0].Configured {
			t.Fatalf("%s configured from another workspace's secret", id)
		}
	}
	if !m["contract_unsigned"][0].Configured {
		t.Fatal("auth none stays configured")
	}
}

func TestSecretLookupFailureIsAnError(t *testing.T) {
	_, err := Catalog(context.Background(), deps(t, dirWithout(t, "contract_badname.yaml"), "optional", fakeSecrets{err: errors.New("boom")}), "ws-alpha")
	if err == nil {
		t.Fatal("a failing secret store must not render every connector unconfigured")
	}
}

func TestSignatureStatusesRendered(t *testing.T) {
	m := catalogByID(t, "ws-alpha", alphaSecrets)
	want := map[string]string{"contract_trusted": "trusted", "contract_forged": "trusted", "contract_unsigned": "unsigned", "contract_http": "unknown"}
	for id, st := range want {
		var got string
		switch s := m[id][0].Signature.(type) {
		case Signature:
			got = s.Status
		case UnknownSignature:
			got = s.Status
		default:
			t.Fatalf("%s: %T", id, s)
		}
		if got != st {
			t.Errorf("%s signature %q, want %q", id, got, st)
		}
	}
	for _, id := range []string{"contract_untrusted", "contract_tampered"} {
		if _, there := m[id]; there {
			t.Errorf("%s must not be listed (untrusted/invalid)", id)
		}
	}
}

func TestPackageSHAEqualsPython(t *testing.T) {
	meta := loadMeta(t)
	m := catalogByID(t, "ws-alpha", alphaSecrets)
	for id, sha := range meta.Manifests {
		es, listed := m[id]
		if !listed {
			continue
		}
		for _, e := range es {
			if e.PackageSHA256 == nil {
				continue
			}
			if *e.PackageSHA256 != sha {
				t.Errorf("%s package_sha256 = %s, want %s", id, *e.PackageSHA256, sha)
			}
		}
	}
	if m["contract_http"][0].PackageSHA256 != nil {
		t.Error("a manifest the report rejects has no package_sha256")
	}
	// the SHA in the catalog is manifest.Entry.SHA256 (LoadIndex), never recomputed
	idx, err := manifest.LoadIndex(dirWithout(t, "contract_badname.yaml"), "optional", manifest.LoadTrustStore("testdata/trusted-publishers.json"))
	if err != nil {
		t.Fatal(err)
	}
	for id, e := range idx {
		if es, ok := m[id]; ok && len(es) == 1 && es[0].PackageSHA256 != nil && *es[0].PackageSHA256 != e.SHA256 {
			t.Errorf("%s: catalog %s != LoadIndex %s", id, *es[0].PackageSHA256, e.SHA256)
		}
	}
}

func TestCertificationMaturityKeys(t *testing.T) {
	m := catalogByID(t, "ws-alpha", alphaSecrets)
	trusted, forged := m["contract_trusted"][0], m["contract_forged"][0]
	if trusted.Maturity != "supported" || trusted.Certification == nil {
		t.Fatalf("valid certification must be supported: %+v", trusted)
	}
	if forged.Maturity != "beta" || forged.Certification != nil {
		t.Fatalf("forged certification must never render as certified: %+v", forged)
	}
	if len(trusted.RequiredChecks) == 0 {
		t.Fatal("required_checks missing")
	}
	b, _ := json.Marshal(trusted)
	var keys []string
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range []string{"certification", "maturity", "required_checks", "package_sha256", "signature", "auth_type", "configured"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing key %s in %v", k, keys)
		}
	}
	// Python key order: catalog entry, then configured, auth_type, signature, package_sha256, maturity keys
	order := []string{`"manifest_version"`, `"id"`, `"cost_per_lookup"`, `"default_confidence"`, `"configured"`, `"auth_type"`, `"signature"`, `"package_sha256"`, `"required_checks"`, `"maturity"`, `"certification"`}
	pos := -1
	for _, k := range order {
		i := strings.Index(string(b), k)
		if i < pos {
			t.Errorf("key %s out of order in %s", k, b)
		}
		pos = i
	}
}

func TestTamperedManifestNeverCertified(t *testing.T) {
	// edit a certified manifest after signing: the signature turns invalid, the
	// manifest is dropped, and its certification cannot render
	dir := dirWithout(t, "contract_badname.yaml")
	p := filepath.Join(dir, "email", "contract_trusted.yaml")
	body, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(body), "Contract fixture connector", "Edited", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Catalog(context.Background(), deps(t, dir, "optional", alphaSecrets), "ws-alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Connectors {
		if e.ID == "contract_trusted" {
			t.Fatalf("tampered manifest listed: %+v", e)
		}
	}
}

func TestCatalogNeverReturnsCredentialKey(t *testing.T) {
	c, err := Catalog(context.Background(), deps(t, dirWithout(t, "contract_badname.yaml"), "optional", alphaSecrets), "ws-alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	for _, bad := range []string{"credential_key", "CONTRACT_TRUSTED_KEY", "alpha-secret-value"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("catalog leaked %q", bad)
		}
	}
}

func TestLoadedMatchesVectors(t *testing.T) {
	for _, c := range loadVec(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, dir, trust := materialise(t, c)
			var got []string
			for _, l := range scanDirectory(dir, EffectivePolicy(c.Policy, true), trust).loaded {
				got = append(got, l.Entry.Manifest.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.Loaded, ",") {
				t.Fatalf("loaded %v, want %v", got, c.Loaded)
			}
		})
	}
}

func TestCompatibilityMatchesFixture(t *testing.T) {
	const prefix = "../../apps/backend-go/internal/connectors/"
	for _, f := range []struct{ fixture, policy string }{
		{"compat_alice_alpha", "optional"}, {"compat_bob_beta", "optional"}, {"compat_required_alice_alpha", "required"},
	} {
		t.Run(f.fixture, func(t *testing.T) {
			rep := Compatibility(ValidateDirectory("testdata/manifests", f.policy, manifest.LoadTrustStore("testdata/trusted-publishers.json")))
			b, _ := json.Marshal(rep)
			fx := contract.Load(t, "connectors/"+f.fixture)
			want := strings.ReplaceAll(string(fx.Response.Body), prefix, "")
			contract.AssertEqual(t, contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: b},
				contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: json.RawMessage(want)},
				contract.IgnoreShapeOf())
		})
	}
}
