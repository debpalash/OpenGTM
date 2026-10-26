package providerrun

// globalSettingKeys lists the operator settings a provider reads from
// Input.Settings. They are resolved only from global operator config
// (app_settings, then env), never from workspace settings: a tenant must not be
// able to aim REACHER_URL at an internal host or switch on website fetching.
func globalSettingKeys(provider string) []string {
	switch provider {
	case "reacher":
		return []string{"REACHER_URL", "REACHER_ENABLED", "REACHER_TIMEOUT"}
	case "tech_stack":
		return []string{"TECH_STACK_WEBSITE_FETCH_ENABLED", "TECH_STACK_RESPECT_ROBOTS"}
	case "sec_edgar":
		return []string{"SEC_EDGAR_USER_AGENT"}
	case "mca_registry":
		return []string{"MCA_RESOURCE_ID"}
	}
	return nil
}
