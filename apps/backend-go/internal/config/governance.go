package config

import (
	"fmt"
	"net/url"
	"strings"
)

// GovernanceSettings are the plan 11 (governance and research playbooks) settings. The Go*
// flags are Go-only rollout switches, all default false. SSO_ALLOWED_ISSUERS (S-M5) lists the
// approved full issuer URLs; SSO_ALLOWED_ISSUER_HOSTS keeps Python's name and default but is
// deprecated: when set it only narrows, it never approves an issuer alone. Both are read per
// call by internal/govidp (empty: nothing approved). The SCIM token TTL is
// APIConfig.ScimTokenTTLDays (OPENGTM_SCIM_TOKEN_TTL_DAYS, plan 02), not repeated here.
type GovernanceSettings struct {
	GoGovernanceRoutesEnabled bool   // GO_GOVERNANCE_ROUTES_ENABLED
	GoPlaybookRoutesEnabled   bool   // GO_PLAYBOOK_ROUTES_ENABLED
	GoExecutorRetention       bool   // GO_EXECUTOR_RETENTION (retention_enforce jobs claimed by Go)
	GoExecutorPlaybooks       bool   // GO_EXECUTOR_PLAYBOOKS (research_playbook_run / _schedule jobs claimed by Go)
	SSOAllowedIssuerHosts     string // SSO_ALLOWED_ISSUER_HOSTS (deprecated comma list, parsed by the OIDC code)
	SSOAllowedIssuers         string // SSO_ALLOWED_ISSUERS (comma list of full issuer URLs, parsed by the OIDC code)
	// SSORedirectBaseURL (SSO_REDIRECT_BASE_URL) is the public origin the /auth/sso callback URL is built on,
	// e.g. https://app.example.com (no trailing slash). Empty: derived from the request (Host, X-Forwarded-Proto).
	SSORedirectBaseURL string
	// SSOEgressAllowHosts (SSO_EGRESS_ALLOW_HOSTS) is a comma list of IdP hosts ("host" or "host:port")
	// the SSO OIDC client may reach although they resolve to a private address (self-hosted IdP).
	// Empty (default): every IdP must be public. EGRESS_ALLOW_HOSTS does not apply to SSO.
	SSOEgressAllowHosts string
}

func loadGovernance(get func(string) string) (GovernanceSettings, error) {
	var g GovernanceSettings
	var err error
	for _, b := range []struct {
		dst *bool
		key string
	}{
		{&g.GoGovernanceRoutesEnabled, "GO_GOVERNANCE_ROUTES_ENABLED"},
		{&g.GoPlaybookRoutesEnabled, "GO_PLAYBOOK_ROUTES_ENABLED"},
		{&g.GoExecutorRetention, "GO_EXECUTOR_RETENTION"},
		{&g.GoExecutorPlaybooks, "GO_EXECUTOR_PLAYBOOKS"},
	} {
		if *b.dst, err = boolVar(get, b.key, false); err != nil {
			return g, err
		}
	}
	g.SSOAllowedIssuerHosts = get("SSO_ALLOWED_ISSUER_HOSTS")
	g.SSOAllowedIssuers = get("SSO_ALLOWED_ISSUERS")
	g.SSOEgressAllowHosts = get("SSO_EGRESS_ALLOW_HOSTS")
	g.SSORedirectBaseURL = strings.TrimRight(strings.TrimSpace(get("SSO_REDIRECT_BASE_URL")), "/")
	if b := g.SSORedirectBaseURL; b != "" {
		if u, err := url.Parse(b); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return g, fmt.Errorf("config: SSO_REDIRECT_BASE_URL must be an http(s) origin such as https://app.example.com")
		}
	}
	return g, nil
}
