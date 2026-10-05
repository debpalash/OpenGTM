package index

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/bundle"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

const schemaPath = "../../../../../packages/contracts/plugin-index.v1.schema.json"

// ---- fixtures ---------------------------------------------------------------

type publisher struct {
	keyID string
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
}

func newPublisher(t *testing.T, keyID string) publisher {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publisher{keyID, priv, pub}
}

func trustStore(t *testing.T, pubs ...publisher) string {
	t.Helper()
	var keys []any
	for _, p := range pubs {
		keys = append(keys, signing.TrustEntry(p.keyID, "Publisher "+p.keyID, p.pub))
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "keys": keys})
	path := filepath.Join(t.TempDir(), "trusted.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func manifestText(name, version string, extra string) string {
	return fmt.Sprintf(`manifest_version: "2"
name: %s
display_name: Plugin %s
kind: provider
runtime: declarative
version: %s
author: Test
license: Apache-2.0
description: A test plugin.
tags: [test, firmographics]
capabilities:
  network: ["https://api.example.com/v1"]
  secrets: [EXAMPLE_API_KEY]
inputs: { domain: string }
outputs: company
capability: company_size
auth: { type: header, param: X-Api-Key, env_var: EXAMPLE_API_KEY }
request:
  method: GET
  url: https://api.example.com/v1/companies
  query: { domain: "{{input.domain}}" }
response:
  mappings: { company_size: "$.employees" }
%s`, name, name, version, extra)
}

// publish creates, signs and packs name@version into dir.
func publish(t *testing.T, dir, name, version string, p publisher) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(src, "plugin.yaml")
	if err := os.WriteFile(mp, []byte(manifestText(name, version, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := signing.SignManifestWithKey(mp, p.priv, p.keyID); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name+"-"+version+".ogc")
	if _, err := bundle.Pack(src, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func buildIndex(t *testing.T, dir string, trust string, key *publisher, base string) *Index {
	t.Helper()
	res, err := Build(BuildOptions{Dir: dir, BaseURL: base, TrustStore: trust, GeneratedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var k ed25519.PrivateKey
	id := ""
	if key != nil {
		k, id = key.priv, key.keyID
	}
	if err := Write(filepath.Join(dir, IndexFile), res.Index, k, id); err != nil {
		t.Fatal(err)
	}
	return res.Index
}

func newClient(t *testing.T, source, trust, policy string) *Client {
	t.Helper()
	return &Client{Source: source, Root: filepath.Join(t.TempDir(), "plugins"), TrustStore: trust, Policy: policy, UserAgent: "test"}
}

func noStageLeftovers(t *testing.T, root string) {
	t.Helper()
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".opengtm-plugin-") || strings.HasSuffix(e.Name(), ".previous") || strings.HasSuffix(e.Name(), ".removed") {
			t.Errorf("leftover staging entry %s", e.Name())
		}
	}
}

// rewriteZip returns a copy of a bundle with one entry replaced.
func rewriteZip(t *testing.T, path, entry string, mutate func([]byte) []byte) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == entry {
			data = mutate(data)
		}
		w, _ := zw.Create(f.Name)
		w.Write(data)
	}
	zw.Close()
	out := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func readIndexFile(t *testing.T, dir string) *Index {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func writeIndexFile(t *testing.T, dir string, ix *Index, key *publisher) {
	t.Helper()
	var k ed25519.PrivateKey
	id := ""
	if key != nil {
		k, id = key.priv, key.keyID
	}
	if err := Write(filepath.Join(dir, IndexFile), ix, k, id); err != nil {
		t.Fatal(err)
	}
}

// ---- schema -----------------------------------------------------------------

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("mem:///index.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("mem:///index.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func schemaErr(t *testing.T, s *jsonschema.Schema, doc []byte) error {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(v)
}

func TestBuiltIndexMatchesSchemaAndIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	publish(t, dir, "acme_one", "1.1.0-beta.1", k)
	publish(t, dir, "acme_two", "0.2.0", k)
	buildIndex(t, dir, ts, &k, "https://cdn.example.com/plugins")
	doc, _ := os.ReadFile(filepath.Join(dir, IndexFile))

	if err := schemaErr(t, compileSchema(t), doc); err != nil {
		t.Fatalf("built index violates the contract schema: %v\n%s", err, doc)
	}
	ix, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Plugins) != 2 || ix.Plugins[0].Name != "acme_one" || len(ix.Plugins[0].Versions) != 2 {
		t.Fatalf("unexpected index: %+v", ix)
	}
	v := ix.Plugins[0].Versions[0]
	if v.Version != "1.1.0-beta.1" || v.Publisher != "Publisher acme-2026" || v.Signature.KeyID != "acme-2026" ||
		v.URL != "https://cdn.example.com/plugins/acme_one-1.1.0-beta.1.ogc" || v.Kind != "provider" ||
		len(v.Capabilities.Secrets) != 1 || v.Size <= 0 {
		t.Fatalf("entry: %+v", v)
	}
	// Same inputs, same bytes.
	again, err := Build(BuildOptions{Dir: dir, BaseURL: "https://cdn.example.com/plugins", TrustStore: ts, GeneratedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := Encode(again.Index)
	if !bytes.Equal(enc, doc) {
		t.Fatal("index build is not deterministic")
	}
	// The detached signature verifies over the exact bytes.
	sig, _ := os.ReadFile(filepath.Join(dir, IndexFile+".sig"))
	if r := signing.VerifyPayload(doc, sig, ts); r.Status != signing.StatusTrusted {
		t.Fatalf("index signature: %+v", r)
	}
}

func TestSchemaRejectsBadIndexes(t *testing.T) {
	s := compileSchema(t)
	good := `{"index_version":1,"generated_at":"2026-01-02T03:04:05Z","plugins":[{"name":"acme_one","versions":[{"version":"1.0.0","kind":"provider","runtime":"declarative","capabilities":{"network":[],"secrets":[],"browser":false},"url":"a.ogc","sha256":"` + strings.Repeat("a", 64) + `","publisher":"p","signature":{"algorithm":"Ed25519","key_id":"k","manifest_sha256":"` + strings.Repeat("b", 64) + `","signature":"c2ln"}}]}]}`
	if err := schemaErr(t, s, []byte(good)); err != nil {
		t.Fatalf("good document rejected: %v", err)
	}
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(string) string{
		"version":    func(d string) string { return strings.Replace(d, `"index_version":1`, `"index_version":2`, 1) },
		"semver":     func(d string) string { return strings.Replace(d, `"1.0.0"`, `"1.0"`, 1) },
		"sha":        func(d string) string { return strings.Replace(d, strings.Repeat("a", 64), "xyz", 1) },
		"name":       func(d string) string { return strings.Replace(d, `acme_one`, `Acme-One`, 1) },
		"kind":       func(d string) string { return strings.Replace(d, `"provider"`, `"magic"`, 1) },
		"no url":     func(d string) string { return strings.Replace(d, `"url":"a.ogc",`, ``, 1) },
		"algorithm":  func(d string) string { return strings.Replace(d, `"Ed25519"`, `"RSA"`, 1) },
		"no plugins": func(d string) string { return strings.Replace(d, `"plugins":[`, `"plugin":[`, 1) },
	} {
		bad := mutate(good)
		if schemaErr(t, s, []byte(bad)) == nil {
			t.Errorf("%s: schema accepted an invalid index", name)
		}
		if _, err := Parse([]byte(bad)); err == nil && name != "no plugins" {
			t.Errorf("%s: Parse accepted an invalid index", name)
		}
	}
	// A missing plugins array is a decode-level gap in Go (nil slice): Parse
	// accepts an empty index, the schema is stricter by design.
	if _, err := Parse([]byte(`{"index_version":1,"generated_at":"x","plugins":[]}`)); err != nil {
		t.Fatalf("an empty index is valid: %v", err)
	}
	dup := strings.Replace(good, `"plugins":[`, `"plugins":[`+good[strings.Index(good, `{"name"`):strings.LastIndex(good, `]}`)]+`,`, 1)
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate plugin must be rejected: %v", err)
	}
}

func TestSemverOrdering(t *testing.T) {
	order := []string{"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.10.0", "2.0.0"}
	for i := range order {
		for j := range order {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := CompareVersions(order[i], order[j]); got != want {
				t.Errorf("Compare(%s,%s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	if CompareVersions("1.0.0+build.1", "1.0.0+build.2") != 0 {
		t.Error("build metadata must not affect precedence")
	}
	if CompareVersions("garbage", "0.0.1") >= 0 {
		t.Error("unparsable versions sort lowest")
	}
}

func TestResolve(t *testing.T) {
	p := &Plugin{Name: "acme_one", Versions: []Version{
		{Version: "1.0.0"}, {Version: "1.2.0", Yanked: true}, {Version: "1.1.0"}, {Version: "2.0.0-rc.1"},
	}}
	if v, err := p.Resolve("", false); err != nil || v.Version != "1.1.0" {
		t.Fatalf("latest skips yanked and pre-releases: %+v %v", v, err)
	}
	if v, err := p.Resolve("1.2.0", false); err != nil || !v.Yanked {
		t.Fatalf("an exact yanked version resolves: %+v %v", v, err)
	}
	if _, err := p.Resolve("1.5.0", false); err == nil || !strings.Contains(err.Error(), "available") {
		t.Fatalf("unknown version: %v", err)
	}
	if _, err := p.Resolve("^1.0.0", false); err == nil {
		t.Fatal("ranges are rejected")
	}
	only := &Plugin{Name: "pre_only", Versions: []Version{{Version: "0.1.0-beta.1"}}}
	if _, err := only.Resolve("", false); err == nil {
		t.Fatal("a pre-release is not latest by default")
	}
	if v, err := only.Resolve("", true); err != nil || v.Version != "0.1.0-beta.1" {
		t.Fatalf("pre-release allowed on request: %+v %v", v, err)
	}
	for in, want := range map[string][2]string{"acme_one": {"acme_one", ""}, "acme_one@1.2.3": {"acme_one", "1.2.3"}} {
		n, v, err := SplitSpec(in)
		if err != nil || n != want[0] || v != want[1] {
			t.Errorf("SplitSpec(%q) = %q %q %v", in, n, v, err)
		}
	}
	for _, bad := range []string{"", "Bad", "../x", "a@b@c", "ab"} {
		if _, _, err := SplitSpec(bad); err == nil {
			t.Errorf("SplitSpec(%q) accepted", bad)
		}
	}
}

// ---- install ----------------------------------------------------------------

func TestInstallFromDirectoryIndexOffline(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	publish(t, dir, "acme_one", "1.1.0", k)
	buildIndex(t, dir, ts, &k, "")

	c := newClient(t, dir, ts, "required") // directory, signed index, required policy
	res, err := c.Install(context.Background(), "acme_one", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.1.0" || !res.IndexSigned || res.KeyID != "acme-2026" || res.Publisher != "Publisher acme-2026" || len(res.Warnings) != 0 {
		t.Fatalf("%+v", res)
	}
	list, _ := c.List()
	if len(list) != 1 || list[0].Version != "1.1.0" || list[0].Signature.Status != signing.StatusTrusted {
		t.Fatalf("list: %+v", list)
	}
	noStageLeftovers(t, c.Root)

	// Installing again needs an explicit decision.
	if _, err := c.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("second install: %v", err)
	}
	// Exact older version over a newer one is a downgrade.
	if _, err := c.Install(context.Background(), "acme_one@1.0.0", true); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade must be refused: %v", err)
	}
	got, _ := c.installedVersion("acme_one")
	if got != "1.1.0" {
		t.Fatalf("a refused downgrade changed the installed version to %s", got)
	}
}

func TestInstallOverHTTPAndIndexSignatures(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	other := newPublisher(t, "other-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	buildIndex(t, dir, ts, &k, "")
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if r.Header.Get("User-Agent") != "test" {
			t.Errorf("missing user agent on %s", r.URL.Path)
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL+"/index.json", ts, "required")
	res, err := c.Install(context.Background(), "acme_one@1.0.0", false)
	if err != nil {
		t.Fatalf("%v (requests %v)", err, requests)
	}
	if res.Version != "1.0.0" || !res.IndexSigned {
		t.Fatalf("%+v", res)
	}
	hits, err := c.Search(context.Background(), "FIRMOGRAPHICS test")
	if err != nil || len(hits) != 1 || hits[0].Installed != "1.0.0" || hits[0].Certification != CertUnreviewed {
		t.Fatalf("search: %+v %v", hits, err)
	}
	if hits, _ := c.Search(context.Background(), "no-such-thing"); len(hits) != 0 {
		t.Fatalf("search miss: %+v", hits)
	}
	info, sig, err := c.Info(context.Background(), "acme_one")
	if err != nil || info.Versions[0].Version != "1.0.0" || sig.Status != signing.StatusTrusted {
		t.Fatalf("info: %+v %v %v", info, sig, err)
	}

	// A trailing-slash URL means index.json.
	c2 := newClient(t, srv.URL+"/", ts, "optional")
	if _, err := c2.Install(context.Background(), "acme_one", false); err != nil {
		t.Fatalf("directory URL: %v", err)
	}

	// Tampered index (bytes changed after signing): refused, nothing installed.
	doc, _ := os.ReadFile(filepath.Join(dir, IndexFile))
	tampered := bytes.Replace(doc, []byte(`"unused"`), nil, 0)
	tampered = bytes.Replace(tampered, []byte("A test plugin."), []byte("A tested plugin."), 1)
	os.WriteFile(filepath.Join(dir, IndexFile), tampered, 0o644)
	c3 := newClient(t, srv.URL+"/index.json", ts, "optional")
	if _, err := c3.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "index signature is invalid") {
		t.Fatalf("tampered index must fail: %v", err)
	}
	if list, _ := c3.List(); len(list) != 0 {
		t.Fatalf("tampered index installed something: %+v", list)
	}

	// Index signed by a key the trust store does not know: untrusted, even
	// under the optional policy.
	buildIndex(t, dir, ts, &other, "")
	if _, err := c3.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "index signature is untrusted") {
		t.Fatalf("untrusted index signer must fail: %v", err)
	}

	// Unsigned index: fine under optional, refused under required.
	buildIndex(t, dir, ts, nil, "")
	if _, err := os.Stat(filepath.Join(dir, IndexFile+".sig")); err == nil {
		t.Fatal("a rebuild without a key must remove the stale signature")
	}
	res, err = c3.Install(context.Background(), "acme_one", false)
	if err != nil || res.IndexSigned {
		t.Fatalf("unsigned index under optional: %+v %v", res, err)
	}
	cReq := newClient(t, srv.URL+"/index.json", ts, "required")
	if _, err := cReq.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned index under required: %v", err)
	}
	// Unknown policy values fail closed to required.
	cOdd := newClient(t, srv.URL+"/index.json", ts, "whatever")
	if _, err := cOdd.Install(context.Background(), "acme_one", false); err == nil {
		t.Fatal("unknown policy must fail closed")
	}
}

func TestTamperedAndMislabelledBundles(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	b1 := publish(t, dir, "acme_one", "1.0.0", k)
	good := buildIndex(t, dir, ts, nil, "")

	// 1. Bundle bytes changed after the index was built: sha256 mismatch.
	orig, _ := os.ReadFile(b1)
	flipped := append([]byte(nil), orig...)
	flipped[len(flipped)/2] ^= 0xff
	os.WriteFile(b1, flipped, 0o644)
	c := newClient(t, dir, ts, "optional")
	if _, err := c.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("tampered bundle: %v", err)
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Fatal("tampered bundle was installed")
	}
	noStageLeftovers(t, c.Root)
	os.WriteFile(b1, orig, 0o644)

	// 2. Attacker edits the manifest inside the bundle AND fixes up the
	// index's sha256: the signature no longer matches the manifest.
	evil := rewriteZip(t, b1, "plugin.yaml", func(b []byte) []byte {
		return bytes.Replace(b, []byte("https://api.example.com/v1"), []byte("https://evil.example.net/v1"), -1)
	})
	raw, _ := os.ReadFile(evil)
	os.WriteFile(b1, raw, 0o644)
	forged, err := Build(BuildOptions{Dir: dir, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	writeIndexFile(t, dir, forged.Index, nil)
	if _, err := c.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
		t.Fatalf("edited manifest with a fixed-up index must fail verification: %v", err)
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Fatal("forged bundle was installed")
	}
	// A normal build refuses to list it at all.
	if _, err := Build(BuildOptions{Dir: dir, TrustStore: ts}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("build must refuse an invalid bundle: %v", err)
	}
	os.WriteFile(b1, orig, 0o644)

	// 3. Signed by a key that is not trusted.
	rogue := newPublisher(t, "rogue-2026")
	dir2 := t.TempDir()
	publish(t, dir2, "acme_one", "1.0.0", rogue)
	if _, err := Build(BuildOptions{Dir: dir2, TrustStore: ts}); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("build must refuse an untrusted signer: %v", err)
	}
	forged2, err := Build(BuildOptions{Dir: dir2, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	writeIndexFile(t, dir2, forged2.Index, nil)
	c2 := newClient(t, dir2, ts, "optional")
	if _, err := c2.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "signature is untrusted") {
		t.Fatalf("untrusted bundle signer: %v", err)
	}

	// 4. Index claims a different signature than the bundle carries (valid
	// bundle, lying index).
	lie := *good
	lie.Plugins = []Plugin{{Name: "acme_one", Versions: append([]Version(nil), good.Plugins[0].Versions...)}}
	lie.Plugins[0].Versions[0].Signature.Signature = "AAAA"
	writeIndexFile(t, dir, &lie, nil)
	if _, err := c.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "signature in the index") {
		t.Fatalf("index/bundle signature mismatch: %v", err)
	}

	// 5. Index understates capabilities.
	lie.Plugins = []Plugin{{Name: "acme_one", Versions: append([]Version(nil), good.Plugins[0].Versions...)}}
	lie.Plugins[0].Versions[0].Capabilities = Capabilities{Network: []string{}, Secrets: []string{}}
	writeIndexFile(t, dir, &lie, nil)
	if _, err := c.Install(context.Background(), "acme_one", false); err == nil || !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("index capabilities must match the signed manifest: %v", err)
	}

	// 6. Size understated or overstated.
	for _, size := range []int64{good.Plugins[0].Versions[0].Size - 1, good.Plugins[0].Versions[0].Size + 1} {
		lie.Plugins = []Plugin{{Name: "acme_one", Versions: append([]Version(nil), good.Plugins[0].Versions...)}}
		lie.Plugins[0].Versions[0].Size = size
		writeIndexFile(t, dir, &lie, nil)
		if _, err := c.Install(context.Background(), "acme_one", false); err == nil {
			t.Fatalf("size %d vs actual must fail", size)
		}
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Fatal("something was installed")
	}
}

func TestRollbackViaRelabelledBundle(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	old := publish(t, dir, "acme_one", "1.0.0", k)
	publish(t, dir, "acme_one", "2.0.0", k)
	ix := buildIndex(t, dir, ts, &k, "")

	// A malicious mirror re-points 2.0.0 at the (validly signed) 1.0.0 file,
	// fixing sha256 and size so only the contents give it away. The index is
	// re-signed by the test to model a compromised publisher key for the
	// index, which is the strongest variant of this attack.
	evil := *ix
	evil.Plugins = []Plugin{{Name: "acme_one", Versions: append([]Version(nil), ix.Plugins[0].Versions...)}}
	v1 := evil.Plugins[0].Versions[1]
	two := &evil.Plugins[0].Versions[0]
	two.URL, two.SHA256, two.Size, two.Signature = filepath.Base(old), v1.SHA256, v1.Size, v1.Signature
	two.Capabilities = v1.Capabilities
	writeIndexFile(t, dir, &evil, &k)
	c := newClient(t, dir, ts, "required")
	if _, err := c.Install(context.Background(), "acme_one@2.0.0", false); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("relabelled bundle: %v", err)
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Fatal("relabelled bundle was installed")
	}
}

func TestDowngradePolicy(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	publish(t, dir, "acme_one", "1.1.0", k)
	buildIndex(t, dir, ts, &k, "")
	ctx := context.Background()

	c := newClient(t, dir, ts, "optional")
	if _, err := c.Install(ctx, "acme_one@1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Install(ctx, "acme_one@1.0.0", true); err == nil || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Fatalf("default: %v", err)
	}
	c.AllowDowngrade = true
	res, err := c.Install(ctx, "acme_one@1.0.0", true)
	if err != nil || res.Version != "1.0.0" || res.Previous != "1.1.0" {
		t.Fatalf("explicit downgrade under optional: %+v %v", res, err)
	}

	// Under required the flag does not help, even through the hook that
	// re-checks under the install lock.
	if _, err := c.Install(ctx, "acme_one@1.1.0", true); err != nil {
		t.Fatal(err)
	}
	c.Policy = "required"
	if _, err := c.Install(ctx, "acme_one@1.0.0", true); err == nil || !strings.Contains(err.Error(), "required signature policy") {
		t.Fatalf("required policy: %v", err)
	}
	if v, _ := c.installedVersion("acme_one"); v != "1.1.0" {
		t.Fatalf("installed version changed to %s", v)
	}
	// The installer's own check, not just the pre-check, catches a race:
	// call the hook directly with a stale pre-check.
	err = c.verifyCandidate(bundle.Candidate{Name: "acme_one", Version: "1.0.0", Installed: true, InstalledVersion: "1.1.0"}, "acme_one", Version{Version: "1.0.0"}, &Result{})
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("hook must re-check downgrades: %v", err)
	}
}

func TestUpdateAndYank(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	publish(t, dir, "acme_two", "1.0.0", k)
	buildIndex(t, dir, ts, &k, "")
	ctx := context.Background()
	c := newClient(t, dir, ts, "required")
	for _, n := range []string{"acme_one", "acme_two"} {
		if _, err := c.Install(ctx, n, false); err != nil {
			t.Fatal(err)
		}
	}
	// Publish 1.1.0 of acme_one and 1.2.0 of acme_two, then yank 1.2.0.
	publish(t, dir, "acme_one", "1.1.0", k)
	publish(t, dir, "acme_two", "1.1.0", k)
	publish(t, dir, "acme_two", "1.2.0", k)
	res, err := Build(BuildOptions{Dir: dir, TrustStore: ts, Meta: map[string]Meta{
		"acme_two@1.2.0": {Yanked: true, YankedReason: "bad release"},
		"acme_one@1.1.0": {Certification: CertBeta},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeIndexFile(t, dir, res.Index, &k)

	st, err := c.Update(ctx, nil, true)
	if err != nil || len(st) != 2 || st[0].Status != "would-update" || st[0].To != "1.1.0" || st[1].To != "1.1.0" {
		t.Fatalf("dry run: %+v %v", st, err)
	}
	if v, _ := c.installedVersion("acme_one"); v != "1.0.0" {
		t.Fatal("dry run installed something")
	}
	st, err = c.Update(ctx, nil, false)
	if err != nil || st[0].Status != "updated" || st[1].Status != "updated" || st[1].Result.Previous != "1.0.0" {
		t.Fatalf("update: %+v %v", st, err)
	}
	if v, _ := c.installedVersion("acme_two"); v != "1.1.0" {
		t.Fatalf("yanked 1.2.0 must not be chosen, got %s", v)
	}
	st, _ = c.Update(ctx, []string{"acme_one"}, false)
	if st[0].Status != "up-to-date" {
		t.Fatalf("%+v", st)
	}
	if _, err := c.Update(ctx, []string{"nothere"}, false); err == nil {
		t.Fatal("updating a plugin that is not installed must fail")
	}
	// A yanked version is still installable on request.
	if r, err := c.Install(ctx, "acme_two@1.2.0", true); err != nil || r.Version != "1.2.0" {
		t.Fatalf("explicit yanked install: %+v %v", r, err)
	}
	if _, err := Build(BuildOptions{Dir: dir, TrustStore: ts, Meta: map[string]Meta{"acme_two@9.9.9": {Yanked: true}}}); err == nil {
		t.Fatal("metadata for a missing release is a typo and must fail the build")
	}
	hits, _ := c.Search(ctx, "")
	if len(hits) != 2 || hits[0].Latest != "1.1.0" || hits[0].Certification != CertBeta || hits[1].Latest != "1.1.0" {
		t.Fatalf("search: %+v", hits)
	}
}

func TestFailedInstallLeavesPreviousVersionIntact(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	b2 := publish(t, dir, "acme_one", "1.1.0", k)
	buildIndex(t, dir, ts, &k, "")
	c := newClient(t, dir, ts, "required")
	if _, err := c.Install(context.Background(), "acme_one@1.0.0", false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(c.Root, "acme_one", "plugin.yaml"))
	data, _ := os.ReadFile(b2)
	data[len(data)/3] ^= 0x55
	os.WriteFile(b2, data, 0o644)
	if _, err := c.Install(context.Background(), "acme_one@1.1.0", true); err == nil {
		t.Fatal("corrupt bundle must fail")
	}
	after, _ := os.ReadFile(filepath.Join(c.Root, "acme_one", "plugin.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("a failed install changed the installed plugin")
	}
	noStageLeftovers(t, c.Root)
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	k := newPublisher(t, "acme-2026")
	ts := trustStore(t, k)
	publish(t, dir, "acme_one", "1.0.0", k)
	buildIndex(t, dir, ts, &k, "")
	c := newClient(t, dir, ts, "required")
	if _, err := c.Install(context.Background(), "acme_one", false); err != nil {
		t.Fatal(err)
	}
	// Something that is not a plugin is never deleted.
	os.MkdirAll(filepath.Join(c.Root, "notes_dir"), 0o755)
	if err := c.Remove("notes_dir"); err == nil {
		t.Fatal("a non-plugin directory must not be removed")
	}
	for _, bad := range []string{"../plugins", "", "ACME", "a/b"} {
		if err := c.Remove(bad); err == nil {
			t.Fatalf("Remove(%q) accepted", bad)
		}
	}
	if err := c.Remove("acme_one"); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Fatalf("still listed: %+v", list)
	}
	noStageLeftovers(t, c.Root)
	if err := c.Remove("acme_one"); err == nil {
		t.Fatal("removing twice must fail")
	}
}

func TestSourceSafety(t *testing.T) {
	// A remote index can never read local files or leave https.
	remote, err := OpenSource("https://example.com/plugins/index.json", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"file:///etc/passwd", "ftp://example.com/x.ogc", "http://example.com/x.ogc", "https://user:pw@example.com/x.ogc", "//evil.example/x.ogc", ""} {
		if loc, err := remote.Resolve(ref); err == nil {
			t.Errorf("remote Resolve(%q) = %q, want error", ref, loc)
		}
	}
	if loc, err := remote.Resolve("a%20b.ogc"); err != nil || loc != "https://example.com/plugins/a%20b.ogc" {
		t.Fatalf("relative: %q %v", loc, err)
	}
	if loc, err := remote.Resolve("https://cdn.example.com/x.ogc"); err != nil || loc != "https://cdn.example.com/x.ogc" {
		t.Fatalf("absolute https: %q %v", loc, err)
	}
	if loc, err := remote.Resolve("http://127.0.0.1:9/x.ogc"); err != nil || loc == "" {
		t.Fatalf("loopback http: %q %v", loc, err)
	}
	if _, err := OpenSource("http://example.com/index.json", nil, ""); err == nil {
		t.Fatal("plain http to a non-loopback host must be refused")
	}
	if _, err := OpenSource("https://user:pw@example.com/index.json", nil, ""); err == nil {
		t.Fatal("credentials in the index URL must be refused")
	}
	if _, err := OpenSource("", nil, ""); err == nil {
		t.Fatal("empty source")
	}

	dir := t.TempDir()
	local, err := OpenSource(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"../x.ogc", "/etc/passwd", "a/../../x.ogc", "file:///etc/passwd"} {
		if loc, err := local.Resolve(ref); err == nil {
			t.Errorf("local Resolve(%q) = %q, want error", ref, loc)
		}
	}
	if loc, err := local.Resolve("sub/x.ogc"); err != nil || loc != filepath.Join(dir, "sub", "x.ogc") {
		t.Fatalf("local relative: %q %v", loc, err)
	}

	// Redirects may not downgrade to http.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tls.Close()
	client := tls.Client()
	client.CheckRedirect = NewHTTPClient().CheckRedirect
	s, _ := OpenSource(tls.URL+"/index.json", client, "")
	if _, _, err := s.FetchIndex(context.Background()); err == nil || !strings.Contains(err.Error(), "https to http") {
		t.Fatalf("https->http redirect: %v", err)
	}
}

func TestOversizedAndErrorResponses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("a"), MaxIndexBytes+10))
	})
	mux.HandleFunc("/err/index.json", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	mux.HandleFunc("/sigerr/index.json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("/sigerr/index.json.sig", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 503) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, path := range []string{"/big/index.json", "/err/index.json", "/sigerr/index.json", "/missing/index.json"} {
		c := newClient(t, srv.URL+path, "", "optional")
		if _, err := c.Load(context.Background()); err == nil {
			t.Errorf("%s: want an error", path)
		}
	}
}

func TestIndexURLIsNotLeakedInErrors(t *testing.T) {
	if got := redactURL("https://u:p@example.com/a.ogc?token=secret"); strings.Contains(got, "secret") || strings.Contains(got, "p@") {
		t.Fatalf("redactURL leaked: %s", got)
	}
}
