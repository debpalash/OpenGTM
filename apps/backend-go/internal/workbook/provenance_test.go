package workbook

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

type licenseVectors struct {
	Now      string `json:"now"`
	Licenses struct {
		Resolve []struct {
			Provider *string `json:"provider"`
			Declared *string `json:"declared"`
			License  string  `json:"license"`
		} `json:"resolve"`
		ProvenanceFor []map[string]any `json:"provenance_for"`
		Merge         []struct {
			Current *string                   `json:"current"`
			Updates map[string]json.RawMessage `json:"updates"`
			Result  string                    `json:"result"`
		} `json:"merge"`
	} `json:"licenses"`
}

func loadLicenseVectors(t *testing.T) licenseVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/wbexec_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v licenseVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestResolveLicenseVectors replays licenses.resolve_license.
func TestResolveLicenseVectors(t *testing.T) {
	v := loadLicenseVectors(t)
	for _, c := range v.Licenses.Resolve {
		if got := ResolveLicense(deref(c.Provider), deref(c.Declared)); got != c.License {
			t.Errorf("resolve(%v, %v) = %q, want %q", deref(c.Provider), deref(c.Declared), got, c.License)
		}
	}
}

// TestProvenanceForVectors replays licenses.provenance_for (fixed clock).
func TestProvenanceForVectors(t *testing.T) {
	v := loadLicenseVectors(t)
	now, err := time.Parse(time.RFC3339, v.Now)
	if err != nil {
		t.Fatal(err)
	}
	conf := func(f float64) *float64 { return &f }
	got := []Provenance{
		ProvenanceFor("gleif", ProvenanceOpts{Confidence: conf(0.123456)}, now),
		ProvenanceFor("", ProvenanceOpts{}, now),
		ProvenanceFor("x", ProvenanceOpts{License: "CC-BY-4.0"}, now),
		ProvenanceFor("x", ProvenanceOpts{License: "nonsense", Declared: "scraped"}, now),
		ProvenanceFor("hunter_io", ProvenanceOpts{Confidence: conf(1)}, now),
	}
	for i, want := range v.Licenses.ProvenanceFor {
		g, _ := json.Marshal(got[i])
		var back map[string]any
		_ = json.Unmarshal(g, &back)
		w, _ := json.Marshal(want)
		if gb, _ := json.Marshal(back); string(gb) != string(w) {
			t.Errorf("provenance_for #%d = %s, want %s", i, g, w)
		}
	}
}

// TestMergeFieldProvenanceVectors replays licenses.merge_field_provenance byte for byte.
func TestMergeFieldProvenanceVectors(t *testing.T) {
	v := loadLicenseVectors(t)
	for _, c := range v.Licenses.Merge {
		upd := map[string]any{}
		for k, raw := range c.Updates {
			var x any
			if err := json.Unmarshal(raw, &x); err != nil {
				t.Fatal(err)
			}
			upd[k] = x
		}
		if got := MergeFieldProvenance(deref(c.Current), upd); got != c.Result {
			t.Errorf("merge(%q) = %s, want %s", deref(c.Current), got, c.Result)
		}
	}
}

// TestPyIsoformat pins datetime.isoformat() for aware UTC times: microseconds only when non-zero.
func TestPyIsoformat(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-28T12:00:00Z":           "2026-10-28T12:00:00+00:00",
		"2026-10-28T12:00:00.5Z":         "2026-10-28T12:00:00.500000+00:00",
		"2026-10-28T12:00:00.123456789Z": "2026-10-28T12:00:00.123456+00:00",
	} {
		ts, err := time.Parse(time.RFC3339Nano, in)
		if err != nil {
			t.Fatal(err)
		}
		if got := pyIsoformatUTC(ts); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
}
