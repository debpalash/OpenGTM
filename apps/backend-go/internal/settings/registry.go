// Package settings holds the settings registries (LLM providers, enrichment
// providers, integrations) exported from the Python source of truth by
// legacy/python/scripts/export_settings_registry.py.
package settings

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

//go:embed registry.json
var registryJSON []byte

// OrderedMap is a string-keyed map that remembers JSON object order.
type OrderedMap[T any] struct {
	keys []string
	m    map[string]T
}

// UnmarshalJSON decodes a JSON object preserving key order.
func (o *OrderedMap[T]) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("settings: ordered map: expected JSON object")
	}
	o.keys = nil
	o.m = map[string]T{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return errors.New("settings: ordered map: non-string key")
		}
		var v T
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("settings: ordered map key %q: %w", key, err)
		}
		if _, dup := o.m[key]; !dup {
			o.keys = append(o.keys, key)
		}
		o.m[key] = v
	}
	_, err = dec.Token() // closing brace
	return err
}

// Keys returns the keys in registry order (a copy).
func (o *OrderedMap[T]) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

// Get returns the value for key.
func (o *OrderedMap[T]) Get(key string) (T, bool) {
	if o == nil {
		var zero T
		return zero, false
	}
	v, ok := o.m[key]
	return v, ok
}

// Len is the number of entries.
func (o *OrderedMap[T]) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Provider is an LLM provider entry.
type Provider struct {
	Name             string `json:"name"`
	EnvKey           string `json:"env_key"`
	EnvURL           string `json:"env_url"`
	EnvModel         string `json:"env_model"`
	DefaultURL       string `json:"default_url"`
	DefaultModel     string `json:"default_model"`
	Docs             string `json:"docs"`
	FreeTier         string `json:"free_tier"`
	Icon             string `json:"icon"`
	OpenAICompatible bool   `json:"openai_compatible"`
}

// EnrichmentProvider is a data-enrichment provider entry.
type EnrichmentProvider struct {
	Name       string `json:"name"`
	EnvKey     string `json:"env_key"`
	Capability string `json:"capability"`
	FreeTier   string `json:"free_tier"`
	Docs       string `json:"docs"`
	Icon       string `json:"icon"`
}

// IntegrationField is one credential field of an integration.
type IntegrationField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Secret      bool   `json:"secret"`
	Placeholder string `json:"placeholder"`
}

// Integration is a BYOK output destination.
type Integration struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Icon        string             `json:"icon"`
	Description string             `json:"description"`
	Fields      []IntegrationField `json:"fields"`
}

// DailyLimit is the approximate free-tier daily limit of a provider.
type DailyLimit struct {
	Calls int    `json:"calls"`
	Label string `json:"label"`
}

// Registry is the decoded registry.json.
type Registry struct {
	Providers           *OrderedMap[Provider]           `json:"providers"`
	EnrichmentProviders *OrderedMap[EnrichmentProvider] `json:"enrichment_providers"`
	Integrations        []Integration                   `json:"integrations"`
	EnrichmentDefaults  *OrderedMap[int]                `json:"enrichment_defaults"`
	VerifierGlobalOnly  []string                        `json:"verifier_global_only"`
	DailyLimits         *OrderedMap[DailyLimit]         `json:"daily_limits"`
	SecretAppKeys       []string                        `json:"secret_app_keys"`
}

// Load decodes the embedded registry.
func Load() (*Registry, error) {
	var r Registry
	if err := json.Unmarshal(registryJSON, &r); err != nil {
		return nil, fmt.Errorf("settings: decode registry: %w", err)
	}
	if r.Providers == nil || r.EnrichmentProviders == nil {
		return nil, errors.New("settings: registry missing providers")
	}
	return &r, nil
}

// Provider returns the LLM provider with the given id.
func (r *Registry) Provider(id string) (Provider, bool) {
	return r.Providers.Get(id)
}
