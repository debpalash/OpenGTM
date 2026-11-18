package audiencevalid

import "testing"

// Issue 114 follow-up ("GET /accounts 500 on invalid IPv6 website"): a malformed bracketed host makes Python's
// urlparse raise ValueError (a 500 there). The Go rollup must instead key the account by company, and a valid IPv6
// literal must still key by host.
func TestAccountKeyInvalidIPv6FallsBackToCompany(t *testing.T) {
	for _, w := range []string{
		"http://[bad]/", "http://[::zz]/", "http://[:::1]/", "http://[1:2:3:4:5:6:7:8:9]/", "https://[12345::1]:443/x",
		"http://[]/", "http://user@[gggg::1]:80", "//[::1", "HTTP://[BAD]",
	} {
		key, company := AccountKey(map[string]any{"website": w, "company": "Acme  Corp"})
		if key != "company:acme corp" || company != "Acme  Corp" {
			t.Errorf("%q -> (%q,%q), want the company key", w, key, company)
		}
	}
	if key, _ := AccountKey(map[string]any{"website": "http://[::1]:8080/x", "company": "Acme"}); key != "::1" {
		t.Errorf("valid IPv6 literal keyed %q, want ::1", key)
	}
}
