package builtin_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// A manifest only works from the UI when four places agree: the manifest itself, the
// vendor catalog (cost, capabilities), the workbook provider registry (the waterfall
// picker) and the settings registry (the key field in Settings, stored encrypted).
// This test fails when a manifest added after the first four is missing from one.
func TestPackManifestsAreWiredIntoTheApp(t *testing.T) {
	wb, err := wbregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	wbByName := map[string]wbregistry.Provider{}
	for _, p := range wb.Providers() {
		wbByName[p.Name] = p
	}
	st, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	cardKeys := map[string]bool{}
	for _, id := range st.EnrichmentProviders.Keys() {
		p, _ := st.EnrichmentProviders.Get(id)
		cardKeys[p.EnvKey] = true
	}
	secret := st.SecretSet()
	type vendorRow struct {
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	}
	catalog := map[string]vendorRow{}
	for _, o := range vendorcatalog.ListVendors() {
		b, _ := o.MarshalJSON()
		var r vendorRow
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		catalog[r.Name] = r
	}

	for name, e := range builtinIndex(t) {
		m := e.Manifest
		// Every manifest needs its key field in Settings, the first four included.
		if !cardKeys[m.Auth.EnvVar] {
			t.Errorf("%s: %s has no enrichment_providers entry in settings/registry.json", name, m.Auth.EnvVar)
		}
		if !secret(m.Auth.EnvVar) {
			t.Errorf("%s: %s is not in settings/registry.json secret_app_keys, so it would be stored in plain text", name, m.Auth.EnvVar)
		}
		if legacyManifests[name] {
			continue // their catalog and registry entries predate this test
		}
		caps := []string{m.Capability}
		for _, c := range m.Capabilities {
			if c != m.Capability {
				caps = append(caps, c)
			}
		}
		if row, ok := catalog[name]; !ok {
			t.Errorf("%s: missing from vendorcatalog/catalog.go", name)
		} else if !reflect.DeepEqual(row.Capabilities, caps) {
			t.Errorf("%s: catalog capabilities %v, manifest %v", name, row.Capabilities, caps)
		}
		if got := vendorcatalog.BaseCost(name); got != m.CostPerLookup {
			t.Errorf("%s: catalog cost %v, manifest cost_per_lookup %v", name, got, m.CostPerLookup)
		}
		if p, ok := wbByName[name]; !ok {
			t.Errorf("%s: missing from wbregistry/registry.json (and the two testdata/contract/meta/providers*.json fixtures)", name)
		} else {
			if !reflect.DeepEqual(p.Capabilities, caps) || p.Confidence != m.DefaultConfidence {
				t.Errorf("%s: wbregistry %v/%v, manifest %v/%v", name, p.Capabilities, p.Confidence, caps, m.DefaultConfidence)
			}
		}
	}
}
