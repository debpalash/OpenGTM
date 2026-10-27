package config

import "testing"

func TestLocalProvidersFrom(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	// GO_LOCAL_PROVIDERS is retired (WP-CUTOVER): every provider runs in process, whatever the value.
	s, err := LocalProvidersFrom(env(nil))
	if err != nil || !s.Local.Any() || !s.Local.Enabled("jobspy") || s.StaffSpyEnabled || s.BrowserURL != "" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s, err = LocalProvidersFrom(env(map[string]string{"GO_LOCAL_PROVIDERS": "jobspy;rm",
		"BROWSER_SERVICE_URL": " http://browser:8090 ", "BROWSER_SERVICE_TOKEN": "t", "STAFFSPY_ENABLED": "yes"}))
	if err != nil || !s.Local.Enabled("crosslinked") || s.BrowserURL != "http://browser:8090" || s.BrowserToken.Reveal() != "t" || !s.StaffSpyEnabled {
		t.Fatalf("list: %+v %v", s, err)
	}
}
