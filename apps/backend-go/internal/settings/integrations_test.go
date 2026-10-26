package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// wsFake adds per-workspace rows to fakeStore.
type wsFake struct {
	*fakeStore
	ws map[string]string // "ws|key" -> raw
}

func (f *wsFake) WorkspaceSetting(_ context.Context, ws, k string) (string, bool, error) {
	v, ok := f.ws[ws+"|"+k]
	return v, ok, nil
}
func (f *wsFake) SetWorkspaceSetting(_ context.Context, ws, k, v string) error {
	f.ws[ws+"|"+k] = v
	return nil
}

func newInteg(t *testing.T, env map[string]string, list []Integration) (*Integrations, *wsFake) {
	t.Helper()
	st := &wsFake{fakeStore: newFake(), ws: map[string]string{}}
	v := pyVector(t, "valid_fernet_key")
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: v.MasterKeyRaw, AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return env[k] }
	secret := func(k string) bool { return strings.HasSuffix(k, "_TOKEN") }
	app := &App{Store: st, Env: e, Secret: secret, Getenv: getenv}
	if list == nil {
		reg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		list = reg.Integrations
		app.Secret = reg.SecretSet()
	}
	return &Integrations{List: list, App: app, WS: secrets.NewResolver(e, st, getenv)}, st
}

func sfdc(t *testing.T, g *Integrations) Integration {
	t.Helper()
	it, ok := g.Find("salesforce")
	if !ok {
		t.Fatal("salesforce missing")
	}
	return it
}

func TestIntegrationViewNeverEchoesSecrets(t *testing.T) {
	g, _ := newInteg(t, nil, nil)
	ctx := context.Background()
	it := sfdc(t, g)
	if _, err := g.SaveGlobal(ctx, it, []KV{{"SALESFORCE_INSTANCE_URL", "https://x.my.salesforce.com"}, {"SALESFORCE_ACCESS_TOKEN", "tok-secret-1234"}}); err != nil {
		t.Fatal(err)
	}
	v, err := g.GlobalView(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Connected || len(v.Fields) != 2 {
		t.Fatalf("view %+v", v)
	}
	url, tok := v.Fields[0], v.Fields[1]
	if url.Value != "https://x.my.salesforce.com" || url.Masked != "set" || !url.Set {
		t.Fatalf("url field %+v", url)
	}
	if tok.Value != "" || tok.Masked != "…1234" || !tok.Set {
		t.Fatalf("token field %+v", tok)
	}
	raw, _ := json.Marshal(v)
	if bytes.Contains(raw, []byte("tok-secret")) {
		t.Fatalf("secret echoed: %s", raw)
	}
	// Short secret (< 4 chars) masks as "set".
	g2, _ := newInteg(t, nil, nil)
	h, _ := g2.Find("hubspot")
	if _, err := g2.SaveGlobal(ctx, h, []KV{{"HUBSPOT_TOKEN", "abc"}}); err != nil {
		t.Fatal(err)
	}
	hv, _ := g2.GlobalView(ctx, h)
	if hv.Fields[0].Masked != "set" || hv.Fields[0].Value != "" {
		t.Fatalf("short secret %+v", hv.Fields[0])
	}
	// Unset: not connected, empty masked.
	g3, _ := newInteg(t, nil, nil)
	av, _ := g3.GlobalView(ctx, sfdc(t, g3))
	if av.Connected || av.Fields[0].Set || av.Fields[0].Masked != "" {
		t.Fatalf("unset %+v", av)
	}
}

func TestIntegrationViewEnvFallbackCountsAsSet(t *testing.T) {
	g, _ := newInteg(t, map[string]string{"HUBSPOT_TOKEN": "env-token-9999"}, nil)
	h, _ := g.Find("hubspot")
	v, err := g.GlobalView(context.Background(), h)
	if err != nil || !v.Connected || v.Fields[0].Masked != "…9999" || v.Fields[0].Value != "" {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestIntegrationPutIgnoresBlankAndForeignKeys(t *testing.T) {
	g, st := newInteg(t, nil, nil)
	ctx := context.Background()
	it := sfdc(t, g)
	saved, err := g.SaveGlobal(ctx, it, []KV{
		{"SALESFORCE_INSTANCE_URL", "  https://a.test  "},
		{"SALESFORCE_ACCESS_TOKEN", "   "},
		{"HUBSPOT_TOKEN", "foreign-key-value"},
		{"NOT_A_FIELD", "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0] != "SALESFORCE_INSTANCE_URL" {
		t.Fatalf("saved %v", saved)
	}
	if st.m["SALESFORCE_INSTANCE_URL"] != "https://a.test" {
		t.Fatalf("not trimmed: %q", st.m["SALESFORCE_INSTANCE_URL"])
	}
	for _, k := range []string{"SALESFORCE_ACCESS_TOKEN", "HUBSPOT_TOKEN", "NOT_A_FIELD"} {
		if _, ok := st.m[k]; ok {
			t.Fatalf("%s written", k)
		}
	}
	// A blank submission never clobbers a saved secret.
	if _, err := g.SaveGlobal(ctx, it, []KV{{"SALESFORCE_ACCESS_TOKEN", "keep-me-0001"}}); err != nil {
		t.Fatal(err)
	}
	before := st.m["SALESFORCE_ACCESS_TOKEN"]
	if saved, _ := g.SaveGlobal(ctx, it, []KV{{"SALESFORCE_ACCESS_TOKEN", ""}}); len(saved) != 0 || st.m["SALESFORCE_ACCESS_TOKEN"] != before {
		t.Fatalf("clobbered: %v", saved)
	}
}

func TestGlobalIntegrationSecretEncryptedAtRest(t *testing.T) {
	g, st := newInteg(t, nil, nil)
	it := sfdc(t, g)
	if _, err := g.SaveGlobal(context.Background(), it, []KV{{"SALESFORCE_ACCESS_TOKEN", "plain-secret-xyz"}, {"SALESFORCE_INSTANCE_URL", "https://i.test"}}); err != nil {
		t.Fatal(err)
	}
	if v := st.m["SALESFORCE_ACCESS_TOKEN"]; !strings.HasPrefix(v, "enc:v1:") || strings.Contains(v, "plain-secret") {
		t.Fatalf("secret not encrypted: %q", v)
	}
	if st.m["SALESFORCE_INSTANCE_URL"] != "https://i.test" {
		t.Fatalf("non-secret field must stay plaintext: %q", st.m["SALESFORCE_INSTANCE_URL"])
	}
}

func TestWorkspaceIntegrationSourcesWorkspaceGlobalUnset(t *testing.T) {
	g, st := newInteg(t, nil, nil)
	ctx := context.Background()
	it := sfdc(t, g)
	// Global URL only; workspace token only.
	st.m["SALESFORCE_INSTANCE_URL"] = "https://global.test"
	if _, err := g.SaveWorkspace(ctx, "ws1", it, []KV{{"SALESFORCE_ACCESS_TOKEN", "ws-secret-7777"}}); err != nil {
		t.Fatal(err)
	}
	v, err := g.WorkspaceView(ctx, "ws1", it)
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields[0].Source != "global" || v.Fields[1].Source != "workspace" || !v.Connected {
		t.Fatalf("%+v", v)
	}
	for _, f := range v.Fields {
		if f.Value != "" || f.Masked != "set" || !f.Set {
			t.Fatalf("field %+v", f)
		}
	}
	raw, _ := json.Marshal(v)
	if bytes.Contains(raw, []byte("ws-secret")) || bytes.Contains(raw, []byte("global.test")) {
		t.Fatalf("value echoed: %s", raw)
	}
	// Another workspace sees the global URL and an unset token.
	v2, _ := g.WorkspaceView(ctx, "ws2", it)
	if v2.Fields[0].Source != "global" || v2.Fields[1].Source != "unset" || v2.Fields[1].Set || v2.Fields[1].Masked != "" || v2.Connected {
		t.Fatalf("%+v", v2)
	}
	if v := st.ws["ws1|SALESFORCE_ACCESS_TOKEN"]; !strings.HasPrefix(v, "enc:v1:") {
		t.Fatalf("workspace row not encrypted: %q", v)
	}
}

func TestReacherURLNotWorkspaceSettable(t *testing.T) {
	list := []Integration{{ID: "verifier", Name: "V", Icon: "i", Description: "d", Fields: []IntegrationField{
		{Key: "REACHER_URL", Label: "Reacher URL"}, {Key: "OTHER_TOKEN", Label: "T", Secret: true},
	}}}
	g, st := newInteg(t, nil, list)
	ctx := context.Background()
	saved, err := g.SaveWorkspace(ctx, "ws1", list[0], []KV{{"REACHER_URL", "http://169.254.169.254/"}, {"OTHER_TOKEN", "ws-token-0001"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0] != "OTHER_TOKEN" {
		t.Fatalf("saved %v", saved)
	}
	if _, ok := st.ws["ws1|REACHER_URL"]; ok {
		t.Fatal("REACHER_URL written at workspace level")
	}
	// A stray workspace row for a global-only key never shows as workspace-sourced.
	st.ws["ws1|REACHER_URL"] = "stray"
	v, _ := g.WorkspaceView(ctx, "ws1", list[0])
	if v.Fields[0].Source != "unset" || v.Fields[0].Set {
		t.Fatalf("%+v", v.Fields[0])
	}
}

func TestPythonReadsGoWrittenSecret(t *testing.T) {
	g, st := newInteg(t, nil, nil)
	ctx := context.Background()
	h, _ := g.Find("hubspot")
	const pt = "go-written-hubspot-token"
	if _, err := g.SaveGlobal(ctx, h, []KV{{"HUBSPOT_TOKEN", pt}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SaveWorkspace(ctx, "ws1", h, []KV{{"HUBSPOT_TOKEN", pt}}); err != nil {
		t.Fatal(err)
	}
	v := pyVector(t, "valid_fernet_key")
	type vc struct {
		Master    string `json:"master_key_raw"`
		Plaintext string `json:"plaintext"`
		Token     string `json:"token"`
	}
	var cases []vc
	for _, raw := range []string{st.m["HUBSPOT_TOKEN"], st.ws["ws1|HUBSPOT_TOKEN"]} {
		if !strings.HasPrefix(raw, "enc:v1:") {
			t.Fatalf("not envelope ciphertext: %q", raw)
		}
		cases = append(cases, vc{v.MasterKeyRaw, pt, strings.TrimPrefix(raw, "enc:v1:")})
	}
	if os.Getenv("PYTHON_VERIFY") != "1" {
		t.Skip("PYTHON_VERIFY!=1: Go-side checks passed; Python decrypt not run")
	}
	in, _ := json.Marshal(cases)
	cmd := exec.Command("uv", "run", "python", "scripts/gen_secret_vectors.py", "--verify")
	cmd.Dir = "../../../../legacy/python"
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.CombinedOutput()
	t.Logf("python --verify output: %s", out)
	if err != nil {
		t.Fatalf("python could not decrypt Go-written integration secret: %v", err)
	}
}
