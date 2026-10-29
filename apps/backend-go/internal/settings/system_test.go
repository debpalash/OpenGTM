package settings

import (
	"context"
	"testing"
)

func TestSystemReadsEnvNotSettings(t *testing.T) {
	st := newFake()
	// Settings rows must be ignored for env-backed keys; LLM_DEFAULT_PROVIDER is the one DB-backed key.
	st.m["DEBUG"], st.m["API_PORT"], st.m["GOOGLE_API_KEY"] = "true", "1", "k"
	st.m["LLM_DEFAULT_PROVIDER"] = "groq"
	app, _ := testApp(t, st, nil)

	env := map[string]string{}
	got, err := SystemInfo(context.Background(), app, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"debug": false, "api_host": "0.0.0.0", "api_port": 8000, "cors_origins": "*",
		"default_provider": "groq", "google_search_configured": false, "linkedin_configured": false,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("default %s = %v, want %v", k, got[k], v)
		}
	}

	env = map[string]string{"DEBUG": "true", "API_HOST": "127.0.0.1", "API_PORT": "9001", "CORS_ORIGINS": "https://x.test",
		"GOOGLE_API_KEY": "g", "LINKEDIN_LI_AT_COOKIE": "c"}
	got, err = SystemInfo(context.Background(), app, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{
		"debug": true, "api_host": "127.0.0.1", "api_port": 9001, "cors_origins": "https://x.test",
		"default_provider": "groq", "google_search_configured": true, "linkedin_configured": true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %v, want %v", k, got[k], v)
		}
	}
	env = map[string]string{"DEBUG": "TRUE"}
	got, _ = SystemInfo(context.Background(), app, func(k string) string { return env[k] })
	if got["debug"] != false {
		t.Error("DEBUG must equal exactly \"true\"")
	}
}

func TestSystemBadPortIsError(t *testing.T) {
	app, _ := testApp(t, newFake(), nil)
	_, err := SystemInfo(context.Background(), app, func(k string) string {
		if k == "API_PORT" {
			return "abc"
		}
		return ""
	})
	if err == nil {
		t.Fatal("non-integer API_PORT must fail like int() does in Python")
	}
}
