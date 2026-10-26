package settings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

func mustLoad(t *testing.T) *Registry {
	t.Helper()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestRegistryProviderOrderPreserved(t *testing.T) {
	r := mustLoad(t)
	fx := contract.Load(t, "settings/providers_list")
	var body struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(fx.Response.Body, &body); err != nil {
		t.Fatal(err)
	}
	ids := r.Providers.Keys()
	if len(ids) != len(body.Providers) {
		t.Fatalf("provider count %d, fixture %d", len(ids), len(body.Providers))
	}
	for i, p := range body.Providers {
		if ids[i] != p.ID {
			t.Fatalf("provider[%d] = %q, fixture %q", i, ids[i], p.ID)
		}
	}
}

func TestRegistryEveryProviderHasEnvKey(t *testing.T) {
	r := mustLoad(t)
	for _, id := range r.Providers.Keys() {
		p, _ := r.Provider(id)
		if p.EnvKey == "" {
			t.Errorf("provider %s has no env_key", id)
		}
	}
	for _, id := range r.EnrichmentProviders.Keys() {
		p, _ := r.EnrichmentProviders.Get(id)
		if p.EnvKey == "" {
			t.Errorf("enrichment provider %s has no env_key", id)
		}
	}
}

func TestRegistryIntegrationFieldKeysUnique(t *testing.T) {
	r := mustLoad(t)
	seen := map[string]string{}
	for _, in := range r.Integrations {
		for _, f := range in.Fields {
			if prev, dup := seen[f.Key]; dup {
				t.Errorf("field key %s in %s and %s", f.Key, prev, in.ID)
			}
			seen[f.Key] = in.ID
		}
	}
}

func TestRegistryMatchesRecordedProvidersFixture(t *testing.T) {
	r := mustLoad(t)
	fx := contract.Load(t, "settings/providers_list")
	const def = "openrouter" // nothing configured, LLM_DEFAULT_PROVIDER unset
	var list []map[string]any
	for _, id := range r.Providers.Keys() {
		p, _ := r.Provider(id)
		list = append(list, map[string]any{
			"id": id, "name": p.Name, "icon": p.Icon, "configured": false,
			"api_key_masked": "", "base_url": p.DefaultURL, "model": p.DefaultModel,
			"default_model": p.DefaultModel, "docs_url": p.Docs, "free_tier": p.FreeTier,
			"is_default": id == def, "openai_compatible": p.OpenAICompatible,
		})
	}
	body, err := json.Marshal(map[string]any{"providers": list, "default_provider": def})
	if err != nil {
		t.Fatal(err)
	}
	contract.AssertEqual(t, contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: body}, fx.Response)
}

func TestRegistrySecretAppKeys(t *testing.T) {
	r := mustLoad(t)
	have := map[string]bool{}
	for _, k := range r.SecretAppKeys {
		have[k] = true
		if strings.HasSuffix(k, "_BASE_URL") || strings.HasSuffix(k, "_MODEL") {
			t.Errorf("non-secret key %s in SecretAppKeys", k)
		}
	}
	if have["REACHER_URL"] {
		t.Error("REACHER_URL must not be secret")
	}
	for _, id := range r.Providers.Keys() {
		p, _ := r.Provider(id)
		if !have[p.EnvKey] {
			t.Errorf("provider %s env_key %s missing", id, p.EnvKey)
		}
	}
	for _, in := range r.Integrations {
		for _, f := range in.Fields {
			if f.Secret && !have[f.Key] {
				t.Errorf("secret integration field %s missing", f.Key)
			}
		}
	}
}
