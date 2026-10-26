package sourcecatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

// sourcesJSONSHA256 is the sha256 of sources.json as committed by Task 1 (the dump of Python DATA_SOURCES and
// get_source_summary()). A change means the dump was regenerated: re-verify against Python, then update.
const sourcesJSONSHA256 = "ff5418e3e77a68ecf54a6b1026b149f8089e4075f5a257b04ea4fc1f113d220b"

type dump struct {
	DataSources     []Source        `json:"data_sources"`
	RegistrySummary json.RawMessage `json:"registry_summary"`
}

func loadDump(t *testing.T) (dump, []byte) {
	t.Helper()
	raw, err := os.ReadFile("sources.json")
	if err != nil {
		t.Fatal(err)
	}
	var d dump
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d, raw
}

func TestCatalogDriftGuard(t *testing.T) {
	_, raw := loadDump(t)
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != sourcesJSONSHA256 {
		t.Fatalf("sources.json changed (sha256 %s); re-verify against Python DATA_SOURCES and update the guard", got)
	}
}

func TestSourcesMatchPython(t *testing.T) {
	d, _ := loadDump(t)
	got := Sources()
	if len(got) != 96 {
		t.Fatalf("want 96 sources, got %d", len(got))
	}
	if !reflect.DeepEqual(got, d.DataSources) {
		t.Fatal("Sources() differs from the Python DATA_SOURCES dump (content or order)")
	}
	// The returned slice is a copy: mutating it must not change the catalog.
	got[0].Name = "mutated"
	if Sources()[0].Name == "mutated" {
		t.Fatal("Sources() leaks its backing array")
	}
}

func TestRegistrySummaryMatchesPython(t *testing.T) {
	d, _ := loadDump(t)
	var want bytes.Buffer
	if err := json.Compact(&want, d.RegistrySummary); err != nil {
		t.Fatal(err)
	}
	b, err := Registry().MarshalJSON() // the route writes these bytes; json.Marshal would HTML-escape them
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := json.Compact(&got, b); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("Registry() bytes differ from get_source_summary()\n got: %.300s\nwant: %.300s", got.String(), want.String())
	}
	if Registry() == Registry() {
		t.Fatal("Registry() must return a fresh object per call")
	}
}

type fakeSettings struct {
	stored, env map[string]string
	err         error
	set         map[string]string
	setErr      error
	keys        []string
}

func (f *fakeSettings) Get(_ context.Context, key, def string) (string, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return "", f.err
	}
	if v := f.stored[key]; v != "" {
		return v, nil
	}
	if v := f.env[key]; v != "" {
		return v, nil
	}
	return def, nil
}

func (f *fakeSettings) Set(_ context.Context, key, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.set == nil {
		f.set = map[string]string{}
	}
	f.set[key] = value
	return nil
}

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestEnabledResolution(t *testing.T) {
	raw, err := os.ReadFile("testdata/enabled_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			SourceID string  `json:"source_id"`
			Stored   *string `json:"stored"`
			Env      *string `json:"env"`
			Enabled  bool    `json:"enabled"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 200 {
		t.Fatalf("only %d vectors", len(v.Cases))
	}
	for i, c := range v.Cases {
		key := "SOURCE_" + pyUpper(c.SourceID) + "_ENABLED"
		f := &fakeSettings{stored: map[string]string{key: ptr(c.Stored)}, env: map[string]string{key: ptr(c.Env)}}
		if got := Enabled(context.Background(), f, c.SourceID); got != c.Enabled {
			t.Fatalf("case %d (%q stored=%v env=%v): got %v want %v", i, c.SourceID, c.Stored, c.Env, got, c.Enabled)
		}
	}
}

func TestEnabledKeyAndDefaults(t *testing.T) {
	f := &fakeSettings{}
	if !Enabled(context.Background(), f, "duckduckgo") {
		t.Fatal("default_enabled source must be on when unset")
	}
	if len(f.keys) != 1 || f.keys[0] != "SOURCE_DUCKDUCKGO_ENABLED" {
		t.Fatalf("key = %v", f.keys)
	}
	if Enabled(context.Background(), f, "no_such_source") {
		t.Fatal("unknown source defaults to false")
	}
	// A settings read error falls back to the catalog default rather than panicking.
	bad := &fakeSettings{err: errors.New("db down")}
	if !Enabled(context.Background(), bad, "duckduckgo") {
		t.Fatal("read error -> default_enabled")
	}
	if Enabled(context.Background(), bad, "no_such_source") {
		t.Fatal("read error on unknown -> false")
	}
	if Enabled(context.Background(), nil, "no_such_source") {
		t.Fatal("nil getter -> default")
	}
}

func TestSetEnabled(t *testing.T) {
	f := &fakeSettings{}
	if err := SetEnabled(context.Background(), f, "indiamart", false); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled(context.Background(), f, "linkedin", true); err != nil {
		t.Fatal(err)
	}
	if f.set["SOURCE_INDIAMART_ENABLED"] != "0" || f.set["SOURCE_LINKEDIN_ENABLED"] != "1" {
		t.Fatalf("set = %v", f.set)
	}
	boom := errors.New("boom")
	if err := SetEnabled(context.Background(), &fakeSettings{setErr: boom}, "indiamart", true); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestPyUpper(t *testing.T) {
	for in, want := range map[string]string{"abc_1": "ABC_1", "straße": "STRASSE", "ǆ": "Ǆ", "": ""} {
		if got := pyUpper(in); got != want {
			t.Errorf("pyUpper(%q) = %q want %q", in, got, want)
		}
	}
}
