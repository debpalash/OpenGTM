package govidp

import "testing"

// S-M5: the allow-list pins the full issuer (scheme, host, port and path), not only the host:
// on a shared IdP host another tenant's issuer path is refused. SSO_ALLOWED_ISSUER_HOSTS is a
// deprecated extra constraint and never approves an issuer on its own.
func TestIssuerExactAllowList(t *testing.T) {
	const notApproved = "OIDC issuer is not approved by SSO_ALLOWED_ISSUERS"
	cases := []struct {
		hosts, issuers, in, out, err string
	}{
		{"", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a", ""},
		{"", "https://login.example.com/tenant-a/", " https://LOGIN.Example.com/tenant-a/ ", "https://LOGIN.Example.com/tenant-a", ""},
		{"", "https://login.example.com/tenant-a", "https://login.example.com/tenant-b", "", notApproved},
		{"", "https://login.example.com/tenant-a", "https://login.example.com/Tenant-A", "", notApproved},
		{"", "https://login.example.com/tenant-a", "https://login.example.com", "", notApproved},
		{"", "https://login.example.com/tenant-a", "https://login.example.com:8443/tenant-a", "", notApproved},
		{"", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a/x", "", notApproved},
		{"", "https://a.example.com, https://login.example.com/tenant-a", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a", ""},
		// the host list alone approves nothing
		{"login.example.com", "", "https://login.example.com/tenant-a", "", notApproved},
		// the host list still constrains when set
		{"other.example.com", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a", "", "OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS"},
		{"login.example.com", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a", "https://login.example.com/tenant-a", ""},
		// malformed allow-list entries are ignored, never matched
		{"", "http://login.example.com/tenant-a,https://u:p@login.example.com/tenant-a", "https://login.example.com/tenant-a", "", notApproved},
	}
	for _, c := range cases {
		t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", c.hosts)
		t.Setenv("SSO_ALLOWED_ISSUERS", c.issuers)
		got, err := ValidateIssuer(c.in)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%q (hosts %q issuers %q): want %q, got %q %v", c.in, c.hosts, c.issuers, c.err, got, err)
			}
			continue
		}
		if err != nil || got != c.out {
			t.Errorf("%q (hosts %q issuers %q): want %q, got %q %v", c.in, c.hosts, c.issuers, c.out, got, err)
		}
	}
}
