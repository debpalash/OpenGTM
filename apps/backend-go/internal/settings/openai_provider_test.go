package settings

import (
	"context"
	"strings"
	"testing"
)

// The openai provider is registered and its key is a secret that is stored
// encrypted at rest like other provider keys.
func TestOpenAIProviderKeyEncryptedAtRest(t *testing.T) {
	st := newFake()
	p := newProviders(t, st)
	prov, ok := p.Reg.Provider("openai")
	if !ok || prov.EnvKey != "OPENAI_API_KEY" || prov.EnvURL != "OPENAI_BASE_URL" || prov.EnvModel != "OPENAI_MODEL" || prov.DefaultURL != "https://api.openai.com/v1" {
		t.Fatalf("openai registry entry: %+v ok=%v", prov, ok)
	}
	if !prov.OpenAICompatible {
		t.Fatal("openai must be openai_compatible")
	}
	key := "sk-plain-secret-0123456789"
	if found, err := p.Update(context.Background(), "openai", ProviderUpdate{APIKey: &key}); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if v := st.m["OPENAI_API_KEY"]; !strings.HasPrefix(v, "enc:v1:") || strings.Contains(v, "plain-secret") {
		t.Fatalf("key not encrypted: %q", v)
	}
}
