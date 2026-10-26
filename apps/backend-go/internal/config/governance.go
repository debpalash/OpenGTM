package config

// GovernanceSettings are the plan 11 (governance and research playbooks) settings. The Go*
// flags are Go-only rollout switches, all default false. SSO_ALLOWED_ISSUER_HOSTS keeps
// Python's name and default (empty: no OIDC issuer host approved). The SCIM token TTL is
// APIConfig.ScimTokenTTLDays (OPENGTM_SCIM_TOKEN_TTL_DAYS, plan 02), not repeated here.
type GovernanceSettings struct {
	GoGovernanceRoutesEnabled bool   // GO_GOVERNANCE_ROUTES_ENABLED
	GoPlaybookRoutesEnabled   bool   // GO_PLAYBOOK_ROUTES_ENABLED
	GoExecutorRetention       bool   // GO_EXECUTOR_RETENTION (retention_enforce jobs claimed by Go)
	GoExecutorPlaybooks       bool   // GO_EXECUTOR_PLAYBOOKS (research_playbook_run / _schedule jobs claimed by Go)
	SSOAllowedIssuerHosts     string // SSO_ALLOWED_ISSUER_HOSTS (comma list, parsed by the OIDC code)
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
	return g, nil
}
