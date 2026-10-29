package config

import "testing"

func TestScrapeRoutesFrom(t *testing.T) {
	for in, want := range map[string]bool{"": false, "true": true, "1": true, "off": false} {
		got, err := ScrapeRoutesFrom(func(string) string { return in })
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v", in, got, err)
		}
	}
	if _, err := ScrapeRoutesFrom(func(string) string { return "maybe" }); err == nil {
		t.Error("a malformed flag must be an error")
	}
}

func TestScrapeFromRedactsSecrets(t *testing.T) {
	env := map[string]string{"BROWSER_SERVICE_URL": " http://browser:8090 ", "BROWSER_SERVICE_TOKEN": "tok", "GOOGLE_API_KEY": "gk", "GOOGLE_CSE_ID": "cx",
		"GOOGLE_BOOKS_API_KEY": "bk"}
	s := ScrapeFrom(func(k string) string { return env[k] })
	if s.BrowserURL != "http://browser:8090" || s.BrowserToken.Reveal() != "tok" || s.GoogleAPIKey.Reveal() != "gk" || s.GoogleCSEID.Reveal() != "cx" ||
		s.GoogleBooksKey.Reveal() != "bk" {
		t.Errorf("unexpected settings")
	}
	if got := s.BrowserToken.String(); got != "[redacted]" {
		t.Errorf("secret prints %q", got)
	}
}
