package secrets

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func derived(s string) string {
	d := sha256.Sum256([]byte(s))
	return base64.URLEncoding.EncodeToString(d[:])
}

// Vector pairs recorded from Python (gen_outreach_vectors.py): the fernet keys
// are used verbatim and the passphrase derives to the recorded key.
func TestMasterKeyResolutionMatchesPython(t *testing.T) {
	fernetA := "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="
	cases := []struct{ name, raw, secret, want string }{
		{"valid fernet verbatim", fernetA, "", fernetA},
		{"fernet stripped", "  " + fernetA + "\n", "", fernetA},
		{"passphrase derived", "correct horse battery staple (test only)", "", "FeEQa29cHbE6T0zEZ5tQTa5kaCwDV9ro-0qM5nA_xTc="},
		{"short garbage derived", "abc", "", derived("abc")},
		{"unset falls back to SECRET_KEY", "", "s3cret", derived("s3cret")},
		{"blank falls back to SECRET_KEY", "   ", "s3cret", derived("s3cret")},
	}
	for _, c := range cases {
		got, err := MasterKeyBytes(c.raw, c.secret, false, false)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q err=%v want %q", c.name, got, err, c.want)
		}
	}
}

func TestMasterKeyInsecureDefaultRefused(t *testing.T) {
	got, err := MasterKeyBytes("", "x", true, false)
	if err == nil || got != nil {
		t.Fatalf("expected refusal, got %q %v", got, err)
	}
	if strings.Contains(err.Error(), "x") && strings.Contains(err.Error(), "SECRET_KEY=") {
		t.Fatal("error leaks key")
	}
	// dev env permits the insecure default; an explicit master key always wins.
	if _, err := MasterKeyBytes("", InsecureSecretKey, true, true); err != nil {
		t.Fatalf("dev: %v", err)
	}
	if _, err := MasterKeyBytes("explicit", InsecureSecretKey, true, false); err != nil {
		t.Fatalf("explicit: %v", err)
	}
}
