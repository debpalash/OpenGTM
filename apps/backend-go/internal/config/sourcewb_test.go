package config

import "testing"

func TestLoadSourceWb(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	s, err := LoadSourceWb(get(nil))
	if err != nil || s.PreviewEnabled || s.CRMImportEnabled || s.PeopleSearchEnabled {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s, err = LoadSourceWb(get(map[string]string{"GO_SOURCE_PREVIEW_ENABLED": "1",
		"CRM_IMPORT_ENABLED": "yes", "PEOPLE_SEARCH_SOURCE_ENABLED": "on"}))
	if err != nil || !s.PreviewEnabled || !s.CRMImportEnabled || !s.PeopleSearchEnabled {
		t.Fatalf("on: %+v %v", s, err)
	}
	if _, err := LoadSourceWb(get(map[string]string{"CRM_IMPORT_ENABLED": "maybe"})); err == nil {
		t.Fatal("a malformed flag must fail startup")
	}
	// The per-request reader never fails: a malformed value is off.
	if s := SourceWbFrom(get(map[string]string{"GO_SOURCE_PREVIEW_ENABLED": "maybe"})); s.PreviewEnabled {
		t.Fatal("lenient read: malformed is off")
	}
}
