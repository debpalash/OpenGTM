package config

import "testing"

func TestLoadSourceWb(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	s, err := LoadSourceWb(get(nil))
	if err != nil || s.GoExecutor || s.PreviewEnabled || s.CRMImportEnabled || s.PeopleSearchEnabled {
		t.Fatalf("defaults must be off: %+v %v", s, err)
	}
	s, err = LoadSourceWb(get(map[string]string{"GO_EXECUTOR_SOURCE_WORKBOOK": "true", "GO_SOURCE_PREVIEW_ENABLED": "1",
		"CRM_IMPORT_ENABLED": "yes", "PEOPLE_SEARCH_SOURCE_ENABLED": "on"}))
	if err != nil || !s.GoExecutor || !s.PreviewEnabled || !s.CRMImportEnabled || !s.PeopleSearchEnabled {
		t.Fatalf("on: %+v %v", s, err)
	}
	if _, err := LoadSourceWb(get(map[string]string{"GO_EXECUTOR_SOURCE_WORKBOOK": "maybe"})); err == nil {
		t.Fatal("a malformed flag must fail startup")
	}
	// The per-request reader never fails: a malformed value is off.
	if s := SourceWbFrom(get(map[string]string{"GO_SOURCE_PREVIEW_ENABLED": "maybe"})); s.PreviewEnabled {
		t.Fatal("lenient read: malformed is off")
	}
}

func TestSourceWorkbookExecutor(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	on := map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws1, ws2", "GO_EXECUTOR_SOURCE_WORKBOOK": "true"}
	cases := []struct {
		name string
		env  map[string]string
		ws   string
		want string
	}{
		{"all_on_in_cohort", on, "ws2", "go"},
		{"wildcard_cohort", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "*", "GO_EXECUTOR_SOURCE_WORKBOOK": "true"}, "any", "go"},
		{"all_native_switch", map[string]string{"GO_ALL_NATIVE": "true"}, "any", "go"},
		{"not_in_cohort", on, "ws3", "python"},
		{"defaults", nil, "ws1", "python"},
		{"type_flag_off", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws1"}, "ws1", "python"},
		{"master_off", map[string]string{"GO_EXECUTOR_WORKSPACES": "ws1", "GO_EXECUTOR_SOURCE_WORKBOOK": "true"}, "ws1", "python"},
	}
	for _, c := range cases {
		if got := SourceWorkbookExecutor(AllNative(get(c.env)), c.ws); got != c.want {
			t.Errorf("%s: %q want %q", c.name, got, c.want)
		}
	}
}
