package govidp

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type issuerVec struct {
	In    string `json:"in"`
	Out   string `json:"out"`
	Error string `json:"error"`
}

type vectors struct {
	AllowedHosts string      `json:"allowed_hosts"`
	Issuers      []issuerVec `json:"issuers"`
	Saves        []saveVec   `json:"saves"`
}

type saveVec struct {
	Name           string         `json:"name"`
	Input          map[string]any `json:"input"`
	ExistingSecret bool           `json:"existing_secret"`
	Config         *OIDCConfig    `json:"config"`
	Stored         *string        `json:"stored"`
	Error          string         `json:"error"`
	SecretWritten  bool           `json:"secret_written"`
}

func loadVectors(t testing.TB) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/oidc_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Inputs where Python (urlparse) accepts or crashes but Go hardens: control
// characters survive into the stored issuer, a non-numeric port is accepted,
// and an unbalanced IPv6 bracket raises a non-contract ValueError. Go answers the
// clean-origin error for all of them (documented deviation).
var issuerDeviations = map[string]bool{
	"https://login.example.com\t/a":     true,
	"https://lo\ngin.example.com":       true,
	"https://login.example.com:badport": true,
	"https://[::1":                      true,
}

func TestIssuerHostAllowList(t *testing.T) {
	v := loadVectors(t)
	t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", v.AllowedHosts)
	for _, c := range v.Issuers {
		got, err := ValidateIssuer(c.In)
		if issuerDeviations[c.In] {
			if err == nil || err.Error() != "OIDC issuer must be a clean HTTPS origin or path" {
				t.Errorf("%q: deviation must be rejected as clean-origin, got %q %v", c.In, got, err)
			}
			continue
		}
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("%q: want error %q, got %q %v", c.In, c.Error, got, err)
			}
			continue
		}
		if err != nil || got != c.Out {
			t.Errorf("%q: want %q, got %q %v", c.In, c.Out, got, err)
		}
	}
}

func TestIssuerEmptyAllowListRejectsEverything(t *testing.T) {
	t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", " , ,")
	_, err := ValidateIssuer("https://login.example.com")
	if err == nil || err.Error() != "OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS" {
		t.Fatalf("got %v", err)
	}
}

func TestIssuerErrorIsValueError(t *testing.T) {
	t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", "a.com")
	_, err := ValidateIssuer("http://a.com")
	var ve *ValueError
	if !errors.As(err, &ve) || !strings.Contains(ve.Error(), "HTTPS") {
		t.Fatalf("want *ValueError, got %T %v", err, err)
	}
}
