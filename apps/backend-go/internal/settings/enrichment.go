package settings

import (
	"context"
	"log/slog"
)

// EnrichmentProviders serves /api/settings/enrichment-providers.
type EnrichmentProviders struct {
	Reg *Registry
	App *App
	Log *slog.Logger
}

// EnrichmentView is one entry of GET /api/settings/enrichment-providers.
type EnrichmentView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Icon         string `json:"icon"`
	Capability   string `json:"capability"`
	Configured   bool   `json:"configured"`
	APIKeyMasked string `json:"api_key_masked"`
	FreeTier     string `json:"free_tier"`
	DocsURL      string `json:"docs_url"`
}

// List returns every enrichment provider with its masked key. A stored key that
// cannot be decrypted reports configured with a fixed mask (name logged, never
// the value) and does not fail the list.
func (e *EnrichmentProviders) List(ctx context.Context) ([]EnrichmentView, error) {
	ids := e.Reg.EnrichmentProviders.Keys()
	out := make([]EnrichmentView, 0, len(ids))
	for _, id := range ids {
		p, _ := e.Reg.EnrichmentProviders.Get(id)
		v := EnrichmentView{ID: id, Name: p.Name, Icon: p.Icon, Capability: p.Capability, FreeTier: p.FreeTier, DocsURL: p.Docs}
		key, err := e.App.Get(ctx, p.EnvKey, "")
		if err != nil {
			l := e.Log
			if l == nil {
				l = slog.Default()
			}
			l.Error("settings: stored enrichment key undecryptable", "key", p.EnvKey)
			v.Configured, v.APIKeyMasked = true, maskedUnknown
		} else {
			v.Configured, v.APIKeyMasked = key != "", MaskKey(key)
		}
		out = append(out, v)
	}
	return out, nil
}

// Update stores the provider's API key (encrypted via App.Set) when provided.
// found is false for an unknown provider.
func (e *EnrichmentProviders) Update(ctx context.Context, id string, u ProviderUpdate) (found bool, err error) {
	p, ok := e.Reg.EnrichmentProviders.Get(id)
	if !ok {
		return false, nil
	}
	if u.APIKey != nil {
		if err := e.App.Set(ctx, p.EnvKey, *u.APIKey); err != nil {
			return true, err
		}
	}
	return true, nil
}
