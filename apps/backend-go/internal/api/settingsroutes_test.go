//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const settingsTestMaster = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

type settingsEnv struct {
	env
	pool *pgxpool.Pool
}

func newSettingsEnv(t *testing.T, allowHosts ...string) settingsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		keys := []string{"LLM_DEFAULT_PROVIDER"}
		for _, id := range reg.Providers.Keys() {
			p, _ := reg.Provider(id)
			keys = append(keys, p.EnvKey, p.EnvURL, p.EnvModel)
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key = ANY($1)`, keys)
	}
	clean()
	t.Cleanup(clean)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: settingsTestMaster, AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterSettings(r, Deps{
		Pool: pool, Signer: testSigner, Identity: st,
		Registry: reg, Envelope: envl, Egress: egress.NewClientForEnv("test", allowHosts),
		Getenv: func(string) string { return "" },
	})
	return settingsEnv{env: env{h: r.Handler(), st: st}, pool: pool}
}

func (e settingsEnv) raw(t *testing.T, key string) (string, bool) {
	t.Helper()
	var v string
	err := e.pool.QueryRow(context.Background(), `SELECT value FROM app_settings WHERE key=$1`, key).Scan(&v)
	return v, err == nil
}

func (e settingsEnv) put(t *testing.T, hdr map[string]string, path, body string) (*httptest.ResponseRecorder, contract.Resp) {
	return e.do("PUT", path, "application/json", body, hdr)
}

func TestProvidersListMatchesFixture(t *testing.T) {
	e := newSettingsEnv(t)
	e.seed(t, "t5-user", pythonHash, true, "user", false)
	fx := contract.Load(t, "settings/providers_list")
	_, got := e.do("GET", "/api/settings/providers", "", "", bearer("t5-user"))
	contract.AssertEqual(t, got, fx.Response)
	_, got = e.do("GET", "/api/settings/providers", "", "", nil)
	if got.Status != 401 {
		t.Fatalf("no token: %d", got.Status)
	}
}

func TestUpdateUnknownProvider404Detail(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm")
	rec, _ := e.put(t, hdr, "/api/settings/providers/nope", `{"api_key":"x"}`)
	if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Provider 'nope' not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.do("POST", "/api/settings/providers/nope/test", "", "", hdr)
	if rec.Code != 404 {
		t.Fatalf("test unknown: %d", rec.Code)
	}
}

func TestUpdateRequiresGlobalAdmin(t *testing.T) {
	e := newSettingsEnv(t)
	e.seed(t, "t5-plain", pythonHash, true, "user", false)
	h := bearer("t5-plain")
	for _, c := range [][2]string{{"PUT", "/api/settings/providers/openrouter"}, {"POST", "/api/settings/providers/openrouter/test"}} {
		rec, _ := e.do(c[0], c[1], "application/json", `{"api_key":"k"}`, h)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not authorized"}` {
			t.Fatalf("%s: %d %s", c[0], rec.Code, rec.Body.String())
		}
	}
	if _, ok := e.raw(t, "OPENROUTER_API_KEY"); ok {
		t.Fatal("non-admin wrote a key")
	}
}

func listProviders(t *testing.T, e settingsEnv, hdr map[string]string) (string, map[string]map[string]any) {
	t.Helper()
	_, got := e.do("GET", "/api/settings/providers", "", "", hdr)
	var body struct {
		Providers []map[string]any `json:"providers"`
		Default   string           `json:"default_provider"`
	}
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, p := range body.Providers {
		out[p["id"].(string)] = p
	}
	return body.Default, out
}

func TestUpdatePartialFields(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm2")
	rec, _ := e.put(t, hdr, "/api/settings/providers/groq", `{"model":"m-1"}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"provider":"groq","status":"ok"}` && strings.TrimSpace(rec.Body.String()) != `{"status":"ok","provider":"groq"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := e.raw(t, "GROQ_API_KEY"); ok {
		t.Fatal("api key written though not provided")
	}
	if v, _ := e.raw(t, "GROQ_MODEL"); v != "m-1" {
		t.Fatalf("model %q", v)
	}
	if _, ok := e.raw(t, "LLM_DEFAULT_PROVIDER"); ok {
		t.Fatal("default written")
	}
	_, ps := listProviders(t, e, hdr)
	if ps["groq"]["model"] != "m-1" || ps["groq"]["configured"] != false {
		t.Fatalf("%v", ps["groq"])
	}
}

func TestSetDefaultWritesKey(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm3")
	e.put(t, hdr, "/api/settings/providers/groq", `{"set_default":false}`)
	if _, ok := e.raw(t, "LLM_DEFAULT_PROVIDER"); ok {
		t.Fatal("set_default=false wrote key")
	}
	e.put(t, hdr, "/api/settings/providers/groq", `{"set_default":true}`)
	if v, _ := e.raw(t, "LLM_DEFAULT_PROVIDER"); v != "groq" {
		t.Fatalf("default %q", v)
	}
	def, ps := listProviders(t, e, hdr)
	if def != "groq" || ps["groq"]["is_default"] != true || ps["openrouter"]["is_default"] != false {
		t.Fatalf("%s %v", def, ps["groq"])
	}
}

func TestProviderKeyEncryptedAtRest(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm4")
	const key = "sk-or-abcdefghijklmnop1234"
	e.put(t, hdr, "/api/settings/providers/openrouter", `{"api_key":"`+key+`","base_url":"https://proxy.example/v1"}`)
	raw, _ := e.raw(t, "OPENROUTER_API_KEY")
	if !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, key) {
		t.Fatalf("raw row %q", raw)
	}
	if u, _ := e.raw(t, "OPENROUTER_BASE_URL"); u != "https://proxy.example/v1" {
		t.Fatalf("base url row %q", u)
	}
	rec, _ := e.do("GET", "/api/settings/providers", "", "", hdr)
	if strings.Contains(rec.Body.String(), key) {
		t.Fatal("list leaked the key")
	}
	_, ps := listProviders(t, e, hdr)
	if ps["openrouter"]["configured"] != true || ps["openrouter"]["api_key_masked"] != "sk-or-ab••••1234" {
		t.Fatalf("%v", ps["openrouter"])
	}
}

func TestProviderKeyWrittenByPythonDecrypts(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm5")
	// Python vector valid_fernet_key: plaintext sk-test-valid-key.
	const tok = "gAAAAABq3vJAKM8utY4ASYcX7smE5-ruRjQa383zDnWsm4WXUvElvkumYAvGVEkPLwDeSey97XgsjDY6G5hptviqWuu8lVdbd4MANS7nQhkm9G8ibCEi8BI="
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO app_settings (key,value) VALUES ('GROQ_API_KEY',$1)`, "enc:v1:"+tok); err != nil {
		t.Fatal(err)
	}
	_, ps := listProviders(t, e, hdr)
	if ps["groq"]["configured"] != true || ps["groq"]["api_key_masked"] != "sk-test-••••-key" {
		t.Fatalf("%v", ps["groq"])
	}
}

func TestLegacyPlaintextProviderKeyStillReads(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm6")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO app_settings (key,value) VALUES ('GROQ_API_KEY','gsk_legacyplaintext_9999')`); err != nil {
		t.Fatal(err)
	}
	_, ps := listProviders(t, e, hdr)
	if ps["groq"]["api_key_masked"] != "gsk_lega••••9999" {
		t.Fatalf("%v", ps["groq"])
	}
}

func TestEmptyStoredProviderValueFallsToDefault(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm7")
	e.put(t, hdr, "/api/settings/providers/groq", `{"api_key":"gsk_abcdefghijklmnop1234"}`)
	e.put(t, hdr, "/api/settings/providers/groq", `{"api_key":""}`)
	_, ps := listProviders(t, e, hdr)
	if ps["groq"]["configured"] != false || ps["groq"]["api_key_masked"] != "" {
		t.Fatalf("%v", ps["groq"])
	}
}

func TestUndecryptableKeyListStillServes(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm8")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO app_settings (key,value) VALUES ('GROQ_API_KEY','enc:v1:gAAAAAB-garbage')`); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.do("GET", "/api/settings/providers", "", "", hdr)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "garbage") {
		t.Fatalf("%d", rec.Code)
	}
	_, ps := listProviders(t, e, hdr)
	if ps["groq"]["configured"] != true || ps["groq"]["api_key_masked"] != "••••••••" {
		t.Fatalf("%v", ps["groq"])
	}
}

func TestTestProviderUnconfigured200(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm9")
	rec, _ := e.do("POST", "/api/settings/providers/groq/test", "", "", hdr)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"error":"API key not configured","status":"error"}` && strings.TrimSpace(rec.Body.String()) != `{"status":"error","error":"API key not configured"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestTestProviderBlocksPrivateBaseURL(t *testing.T) {
	e := newSettingsEnv(t)
	_, hdr := e.seedAdmin(t, "t5-adm10")
	e.put(t, hdr, "/api/settings/providers/groq", `{"api_key":"gsk_abcdefghijklmnop1234","base_url":"http://127.0.0.1:1"}`)
	rec, _ := e.do("POST", "/api/settings/providers/groq/test", "", "", hdr)
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	msg, _ := res["error"].(string)
	if rec.Code != 200 || res["status"] != "error" || !strings.Contains(msg, "blocked") || strings.Contains(rec.Body.String(), "gsk_abc") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestTestProviderAnthropicShape(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"model":"claude-x","content":[{"type":"text","text":"hello"}]}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	e := newSettingsEnv(t, host)
	_, hdr := e.seedAdmin(t, "t5-adm11")
	e.put(t, hdr, "/api/settings/providers/anthropic", `{"api_key":"sk-ant-abcdefghijklmnop","base_url":"`+srv.URL+`"}`)
	rec, _ := e.do("POST", "/api/settings/providers/anthropic/test", "", "", hdr)
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res["status"] != "ok" || res["response"] != "hello" || res["model"] != "claude-x" || gotKey != "sk-ant-abcdefghijklmnop" {
		t.Fatalf("%d %s key=%q", rec.Code, rec.Body.String(), gotKey)
	}
}
