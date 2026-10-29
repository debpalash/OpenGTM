//go:build integration

package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestProviderUpdateIsAtomicOnDatabaseFailure(t *testing.T) {
	e := newSettingsEnv(t)
	_, admin := e.seedAdmin(t, "atomic-provider-admin")
	path := "/api/settings/providers/openrouter"
	initial := `{"api_key":"synthetic-initial-key","base_url":"https://initial.example.test/v1","model":"initial-model"}`
	if rec, _ := e.do("PUT", path, "application/json", initial, admin); rec.Code != http.StatusOK {
		t.Fatalf("initial settings status: %d", rec.Code)
	}
	keys := []string{"OPENROUTER_API_KEY", "OPENROUTER_BASE_URL", "OPENROUTER_MODEL", "LLM_DEFAULT_PROVIDER"}
	before := map[string]string{}
	for _, key := range keys {
		before[key], _ = e.raw(t, key)
	}
	ownerExec(t, `CREATE FUNCTION qa_reject_provider_model() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.key = 'OPENROUTER_MODEL' AND NEW.value = 'synthetic-rejected-model' THEN
			RAISE EXCEPTION 'synthetic model write failure';
		END IF;
		RETURN NEW;
	END $$;
	CREATE TRIGGER qa_reject_provider_model BEFORE INSERT OR UPDATE ON app_settings
	FOR EACH ROW EXECUTE FUNCTION qa_reject_provider_model()`)
	t.Cleanup(func() {
		ownerExec(t, `DROP TRIGGER IF EXISTS qa_reject_provider_model ON app_settings; DROP FUNCTION IF EXISTS qa_reject_provider_model()`)
	})
	update := `{"api_key":"synthetic-replacement-key","base_url":"https://replacement.example.test/v1","model":"synthetic-rejected-model","set_default":true}`
	if rec, _ := e.do("PUT", path, "application/json", update, admin); rec.Code != http.StatusInternalServerError {
		t.Fatalf("refused multi-field update status: %d", rec.Code)
	}
	for _, key := range keys {
		after, _ := e.raw(t, key)
		if after != before[key] {
			t.Errorf("failed update changed %s; all fields must roll back", key)
		}
	}
	ownerExec(t, `DROP TRIGGER qa_reject_provider_model ON app_settings`)
	if rec, _ := e.do("PUT", path, "application/json", update, admin); rec.Code != http.StatusOK {
		t.Fatalf("explicit retry status: %d", rec.Code)
	}
	for key, want := range map[string]string{
		"OPENROUTER_BASE_URL":  "https://replacement.example.test/v1",
		"OPENROUTER_MODEL":     "synthetic-rejected-model",
		"LLM_DEFAULT_PROVIDER": "openrouter",
	} {
		if got, _ := e.raw(t, key); got != want {
			t.Errorf("confirmed update did not persist %s", key)
		}
	}
	if key, _ := e.raw(t, "OPENROUTER_API_KEY"); !strings.HasPrefix(key, "enc:v1:") || strings.Contains(key, "synthetic-replacement-key") {
		t.Fatal("confirmed update did not encrypt the provider key")
	}
}
