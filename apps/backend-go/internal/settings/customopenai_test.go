package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// custom_openai is registered, appears in the provider list, and its API key
// is treated as a secret (encrypted at rest, masked in responses).
func TestCustomOpenAIInRegistry(t *testing.T) {
	r := mustLoad(t)
	prov, ok := r.Provider("custom_openai")
	if !ok {
		t.Fatal("custom_openai missing from registry")
	}
	if prov.EnvKey != "CUSTOM_OPENAI_API_KEY" || prov.EnvURL != "CUSTOM_OPENAI_BASE_URL" || prov.EnvModel != "CUSTOM_OPENAI_MODEL" {
		t.Fatalf("unexpected env keys: %+v", prov)
	}
	if !prov.OpenAICompatible {
		t.Fatal("custom_openai must be openai_compatible")
	}
	if !r.SecretSet()("CUSTOM_OPENAI_API_KEY") {
		t.Fatal("CUSTOM_OPENAI_API_KEY must be in secret_app_keys")
	}
	if r.SecretSet()("CUSTOM_OPENAI_BASE_URL") || r.SecretSet()("CUSTOM_OPENAI_MODEL") {
		t.Fatal("non-secret keys must not be in secret_app_keys")
	}
}

func TestCustomOpenAIListView(t *testing.T) {
	st := newFake()
	st.m["CUSTOM_OPENAI_API_KEY"] = "vllm-secret-key-0123456789"
	st.m["CUSTOM_OPENAI_BASE_URL"] = "http://10.0.0.7:8008/v1"
	st.m["CUSTOM_OPENAI_MODEL"] = "qwen-local"
	p := newProviders(t, st)
	views, _, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range views {
		if v.ID != "custom_openai" {
			continue
		}
		found = true
		if !v.Configured || v.APIKeyMasked == "" || v.APIKeyMasked == "vllm-secret-key-0123456789" {
			t.Fatalf("key must be masked: %+v", v)
		}
		if v.BaseURL != "http://10.0.0.7:8008/v1" || v.Model != "qwen-local" {
			t.Fatalf("URL/model: %+v", v)
		}
	}
	if !found {
		t.Fatal("custom_openai missing from list")
	}
}

// Update stores the base URL and model, and the default flag routes the
// default provider to custom_openai.
func TestCustomOpenAIUpdateAndDefault(t *testing.T) {
	st := newFake()
	p := newProviders(t, st)
	base := "http://10.0.0.7:8008/v1"
	if found, err := p.Update(context.Background(), "custom_openai", ProviderUpdate{BaseURL: &base}); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if st.m["CUSTOM_OPENAI_BASE_URL"] != base {
		t.Fatalf("stored: %v", st.m)
	}
	if found, err := p.Update(context.Background(), "custom_openai", ProviderUpdate{Model: ptr("qwen"), SetDefault: true}); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if st.m["CUSTOM_OPENAI_MODEL"] != "qwen" || st.m["LLM_DEFAULT_PROVIDER"] != "custom_openai" {
		t.Fatalf("stored: %v", st.m)
	}
}

// The Test round-trip against a mock OpenAI-compatible server: with a key the
// Authorization header is sent; without a key none is sent; a missing base URL
// or model reports a meaningful error instead of dialing nowhere.
func TestCustomOpenAITestFlow(t *testing.T) {
	var gotAuth string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, `{"error":{"message":"bad path"}}`, 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "qwen",
			"choices": []map[string]any{{"message": map[string]any{"content": "hello"}}},
		})
	}))
	defer srv.Close()

	// keyed
	st := newFake()
	st.m["CUSTOM_OPENAI_API_KEY"] = "vllm-key-0123456789"
	st.m["CUSTOM_OPENAI_BASE_URL"] = srv.URL + "/v1"
	st.m["CUSTOM_OPENAI_MODEL"] = "qwen"
	p := newProviders(t, st, strings.TrimPrefix(srv.URL, "http://"))
	res, ok, err := p.Test(context.Background(), "custom_openai")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if res["status"] != "ok" || res["response"] != "hello" || gotAuth != "Bearer vllm-key-0123456789" {
		t.Fatalf("keyed test: %v auth=%q", res, gotAuth)
	}

	// keyless: allowed, no Authorization header
	st2 := newFake()
	st2.m["CUSTOM_OPENAI_BASE_URL"] = srv.URL + "/v1"
	st2.m["CUSTOM_OPENAI_MODEL"] = "qwen"
	p2 := newProviders(t, st2, strings.TrimPrefix(srv.URL, "http://"))
	res2, ok, err := p2.Test(context.Background(), "custom_openai")
	if err != nil || !ok {
		t.Fatalf("keyless: ok=%v err=%v", ok, err)
	}
	if res2["status"] != "ok" {
		t.Fatalf("keyless test result: %v", res2)
	}
	if gotAuth != "" {
		t.Fatalf("keyless must send no Authorization, got %q", gotAuth)
	}

	// missing model: meaningful error, no request
	before := hits
	st3 := newFake()
	st3.m["CUSTOM_OPENAI_BASE_URL"] = srv.URL + "/v1"
	p3 := newProviders(t, st3)
	res3, _, err := p3.Test(context.Background(), "custom_openai")
	if err != nil {
		t.Fatal(err)
	}
	if res3["status"] != "error" || !strings.Contains(res3["error"].(string), "Base URL and model") {
		t.Fatalf("missing model: %v", res3)
	}
	if hits != before {
		t.Fatal("must not call the server when the model is unset")
	}
}

func ptr(s string) *string { return &s }
