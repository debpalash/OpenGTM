package secrets

import "testing"

func TestAllowGlobalCredentialFallback(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "0": false, "junk": false, "true": true, "1": true, "YES": true, " on ": true} {
		t.Setenv("ALLOW_GLOBAL_CREDENTIAL_FALLBACK", v)
		if got := AllowGlobalCredentialFallback(); got != want {
			t.Errorf("%q: %v want %v", v, got, want)
		}
	}
}
