package config

import "testing"

func TestLocalProvidersFrom(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	s, err := LocalProvidersFrom(env(nil))
	if err != nil || s.Local.Any() || s.Local.Enabled("jobspy") || s.StaffSpyEnabled || s.BrowserURL != "" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s, err = LocalProvidersFrom(env(map[string]string{"GO_LOCAL_PROVIDERS": " jobspy, ddgs_search ,,Website_Scraper",
		"BROWSER_SERVICE_URL": " http://browser:8090 ", "BROWSER_SERVICE_TOKEN": "t", "STAFFSPY_ENABLED": "yes"}))
	if err != nil || !s.Local.Enabled("jobspy") || !s.Local.Enabled("ddgs_search") || !s.Local.Enabled("website_scraper") ||
		s.Local.Enabled("crosslinked") || !s.Local.Any() || s.BrowserURL != "http://browser:8090" || s.BrowserToken.Reveal() != "t" || !s.StaffSpyEnabled {
		t.Fatalf("list: %+v %v", s, err)
	}
	s, _ = LocalProvidersFrom(env(map[string]string{"GO_LOCAL_PROVIDERS": "all"}))
	if !s.Local.Enabled("anything") {
		t.Fatal("all")
	}
	if _, err := LocalProvidersFrom(env(map[string]string{"GO_LOCAL_PROVIDERS": "jobspy;rm"})); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := LocalProvidersFrom(env(map[string]string{"GO_LOCAL_PROVIDERS": "all,jobspy"})); err == nil {
		t.Fatal("all mixed with names accepted")
	}
}
