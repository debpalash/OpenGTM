package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type vaultFake struct {
	srv     *httptest.Server
	caPath  string
	lastReq *http.Request
	rawPath string
	body    map[string]string
	respond func(w http.ResponseWriter, action string, body map[string]string)
}

func newVaultFake(t *testing.T) *vaultFake {
	t.Helper()
	f := &vaultFake{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastReq, f.rawPath = r, r.URL.EscapedPath()
		f.body = map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&f.body)
		action := "decrypt"
		if strings.Contains(r.URL.Path, "/encrypt/") {
			action = "encrypt"
		}
		if f.respond != nil {
			f.respond(w, action, f.body)
			return
		}
		if action == "encrypt" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"ciphertext": "vault:v1:" + f.body["plaintext"]}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": strings.TrimPrefix(f.body["ciphertext"], "vault:v1:")}})
		}
	}))
	t.Cleanup(f.srv.Close)
	f.caPath = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(f.caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *vaultFake) cfg() Config {
	return Config{Provider: "vault_transit", VaultAddr: f.srv.URL + "/", VaultToken: " tok-1 ",
		VaultTransitKey: "my key/1", VaultNamespace: "team-a", VaultCACert: f.caPath}
}

func TestVaultRoundTripAgainstFakeServer(t *testing.T) {
	f := newVaultFake(t)
	e, err := NewEnvelope(f.cfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	enc, err := e.Encrypt(ctx, "héllo secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, VaultPrefix+"vault:v1:") {
		t.Fatalf("enc = %q", enc)
	}
	if f.rawPath != "/v1/transit/encrypt/my%20key%2F1" {
		t.Errorf("path = %q", f.rawPath)
	}
	if f.lastReq.Header.Get("X-Vault-Token") != "tok-1" || f.lastReq.Header.Get("X-Vault-Namespace") != "team-a" {
		t.Errorf("headers = %v", f.lastReq.Header)
	}
	if b, _ := base64.StdEncoding.DecodeString(f.body["plaintext"]); string(b) != "héllo secret" {
		t.Errorf("body = %v", f.body)
	}
	got, err := e.Decrypt(ctx, enc)
	if err != nil || got != "héllo secret" {
		t.Fatalf("got %q, %v", got, err)
	}
	if f.rawPath != "/v1/transit/decrypt/my%20key%2F1" || f.body["ciphertext"] != "vault:v1:"+f.body["ciphertext"][len("vault:v1:"):] {
		t.Errorf("decrypt req path=%q body=%v", f.rawPath, f.body)
	}
}

func TestVaultNoNamespaceHeaderWhenUnset(t *testing.T) {
	f := newVaultFake(t)
	c := f.cfg()
	c.VaultNamespace = ""
	e, _ := NewEnvelope(c, nil)
	if _, err := e.Encrypt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.lastReq.Header["X-Vault-Namespace"]; ok {
		t.Error("namespace header sent")
	}
}

func TestVaultRejectsHTTPAndUserinfoAddr(t *testing.T) {
	for _, addr := range []string{"http://vault.example:8200", "https://u:p@vault.example", "https://u@vault.example",
		"https://vault.example/?x=1", "https://vault.example/#f", "https://", "", "vault.example"} {
		e, err := NewEnvelope(Config{Provider: "vault_transit", VaultAddr: addr, VaultToken: "t", VaultTransitKey: "k"}, nil)
		if err == nil {
			_, err = e.Encrypt(context.Background(), "x")
		}
		if err == nil || err.Error() != "VAULT_ADDR must be a clean HTTPS URL" {
			t.Errorf("addr %q: err = %v", addr, err)
		}
	}
}

func TestVaultTokenFromFile(t *testing.T) {
	f := newVaultFake(t)
	p := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(p, []byte("file-token\n"), 0o600)
	c := f.cfg()
	c.VaultToken, c.VaultTokenFile = "env-token", p
	e, _ := NewEnvelope(c, nil)
	if _, err := e.Encrypt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if f.lastReq.Header.Get("X-Vault-Token") != "file-token" {
		t.Errorf("token = %q", f.lastReq.Header.Get("X-Vault-Token"))
	}
	c.VaultTokenFile = filepath.Join(t.TempDir(), "missing")
	e, _ = NewEnvelope(c, nil)
	if _, err := e.Encrypt(context.Background(), "x"); err == nil || err.Error() != "Could not read VAULT_TOKEN_FILE" {
		t.Errorf("err = %v", err)
	}
}

func TestVaultRequiresTokenAndKey(t *testing.T) {
	f := newVaultFake(t)
	c := f.cfg()
	c.VaultToken = " "
	e, _ := NewEnvelope(c, nil)
	if _, err := e.Encrypt(context.Background(), "x"); err == nil || err.Error() != "Vault Transit requires a token and VAULT_TRANSIT_KEY" {
		t.Errorf("err = %v", err)
	}
}

func TestVaultInvalidBase64PlaintextErrors(t *testing.T) {
	f := newVaultFake(t)
	f.respond = func(w http.ResponseWriter, a string, b map[string]string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": "not base64!!"}})
	}
	e, _ := NewEnvelope(f.cfg(), nil)
	_, err := e.Decrypt(context.Background(), VaultPrefix+"vault:v1:abc")
	if err == nil || err.Error() != "Vault Transit returned invalid plaintext" {
		t.Fatalf("err = %v", err)
	}
}

func TestVaultFailureModes(t *testing.T) {
	f := newVaultFake(t)
	e, _ := NewEnvelope(f.cfg(), nil)
	cases := map[string]func(w http.ResponseWriter, a string, b map[string]string){
		"http 500": func(w http.ResponseWriter, a string, b map[string]string) { http.Error(w, "boom secret", 500) },
		"not json": func(w http.ResponseWriter, a string, b map[string]string) { _, _ = w.Write([]byte("<html>")) },
		"no data":  func(w http.ResponseWriter, a string, b map[string]string) { _, _ = w.Write([]byte(`{}`)) },
		"empty": func(w http.ResponseWriter, a string, b map[string]string) {
			_, _ = w.Write([]byte(`{"data":{"ciphertext":""}}`))
		},
		"non-str": func(w http.ResponseWriter, a string, b map[string]string) {
			_, _ = w.Write([]byte(`{"data":{"ciphertext":5}}`))
		},
	}
	for name, fn := range cases {
		f.respond = fn
		_, err := e.Encrypt(context.Background(), "x")
		if err == nil || strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestVaultUntrustedCertRejected(t *testing.T) {
	f := newVaultFake(t)
	c := f.cfg()
	c.VaultCACert = ""
	e, _ := NewEnvelope(c, nil)
	if _, err := e.Encrypt(context.Background(), "x"); err == nil {
		t.Fatal("expected TLS verification failure")
	}
}

func TestVaultClientReusedAcrossRequests(t *testing.T) {
	f := newVaultFake(t)
	e, _ := NewEnvelope(f.cfg(), nil)
	for i := 0; i < 2; i++ {
		if _, err := e.Encrypt(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
	}
	a, err1 := e.vault.client()
	b, err2 := e.vault.client()
	if err1 != nil || err2 != nil || a != b || a.Transport != b.Transport || a.Timeout != vaultTimeout {
		t.Fatal("client not reused")
	}
	bad := f.cfg()
	bad.VaultCACert = filepath.Join(t.TempDir(), "missing")
	e2, _ := NewEnvelope(bad, nil)
	if _, err := e2.Encrypt(context.Background(), "x"); err == nil || err.Error() != "Could not read VAULT_CACERT" {
		t.Fatalf("err = %v", err)
	}
}
