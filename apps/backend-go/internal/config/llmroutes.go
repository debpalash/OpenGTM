package config

// LLMRoutesFrom reads GO_LLM_ROUTES_ENABLED (plan 15 WP-LLMROUTES): serve the four LLM-backed routes from Go
// (POST /api/campaigns/generate, POST /api/v2/workbooks/{id}/generate-column, GET /api/workbooks/meta/provider-accuracy,
// POST /api/lead/{id}/enrich). Off by default (the Python path); WP-CUTOVER flips it. A value that is not a boolean
// is an error, like every other GO_* flag.
func LLMRoutesFrom(get func(string) string) (bool, error) {
	return boolVar(get, "GO_LLM_ROUTES_ENABLED", false)
}
