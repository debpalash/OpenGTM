package config

import (
	"reflect"
	"testing"
)

func TestGovernanceConfigDefaultsMatchPython(t *testing.T) {
	g, err := loadGovernance(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, GovernanceSettings{}) { // SSO_ALLOWED_ISSUER_HOSTS defaults to "" in core/config.py
		t.Fatalf("defaults %#v", g)
	}
}

func TestGovernanceSettingsParseAndReject(t *testing.T) {
	env := map[string]string{"GO_GOVERNANCE_ROUTES_ENABLED": "true", "GO_PLAYBOOK_ROUTES_ENABLED": "1",
		"GO_EXECUTOR_RETENTION": "yes", "GO_EXECUTOR_PLAYBOOKS": "on", "SSO_ALLOWED_ISSUER_HOSTS": " a.com,B.com ",
		"SSO_ALLOWED_ISSUERS": "https://a.com/t1"}
	g, err := loadGovernance(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := GovernanceSettings{true, true, true, true, " a.com,B.com ", "https://a.com/t1", "", ""}
	if g != want {
		t.Fatalf("got %#v want %#v", g, want)
	}
	for _, k := range []string{"GO_GOVERNANCE_ROUTES_ENABLED", "GO_PLAYBOOK_ROUTES_ENABLED", "GO_EXECUTOR_RETENTION", "GO_EXECUTOR_PLAYBOOKS"} {
		if _, err := loadGovernance(func(x string) string {
			if x == k {
				return "maybe"
			}
			return ""
		}); err == nil {
			t.Errorf("%s=maybe accepted", k)
		}
	}
}

func TestGovernanceSSORedirectBaseURL(t *testing.T) {
	for in, want := range map[string]string{"": "", " https://app.example.com/ ": "https://app.example.com", "http://localhost:5173": "http://localhost:5173"} {
		g, err := loadGovernance(func(k string) string { return map[string]string{"SSO_REDIRECT_BASE_URL": in}[k] })
		if err != nil || g.SSORedirectBaseURL != want {
			t.Errorf("%q: %q %v", in, g.SSORedirectBaseURL, err)
		}
	}
	for _, bad := range []string{"app.example.com", "ftp://x.com", "https://u@x.com", "https://x.com/app", "https://x.com?a=1"} {
		if _, err := loadGovernance(func(k string) string { return map[string]string{"SSO_REDIRECT_BASE_URL": bad}[k] }); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSSOEgressAllowHostsDefaultEmpty(t *testing.T) {
	g, err := loadGovernance(func(k string) string {
		if k == "SSO_EGRESS_ALLOW_HOSTS" {
			return "idp.corp.internal, 10.0.0.5:8443"
		}
		return ""
	})
	if err != nil || g.SSOEgressAllowHosts != "idp.corp.internal, 10.0.0.5:8443" {
		t.Fatalf("got %q %v", g.SSOEgressAllowHosts, err)
	}
	if g, _ := loadGovernance(func(string) string { return "" }); g.SSOEgressAllowHosts != "" {
		t.Fatalf("default %q", g.SSOEgressAllowHosts)
	}
}
