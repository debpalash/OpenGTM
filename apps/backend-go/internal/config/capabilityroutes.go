package config

// CapabilityRoutesFrom reads GO_CAPABILITY_ROUTES_ENABLED (plan 15 WP-ROUTES-A): serve the four certification
// capability routes, /health and /api from Go. Off by default (the Python path); WP-CUTOVER flips it. A value that
// is not a boolean is an error, like every other GO_* flag.
func CapabilityRoutesFrom(get func(string) string) (bool, error) {
	return boolVar(get, "GO_CAPABILITY_ROUTES_ENABLED", false)
}

// TrailingSlashCompatFrom reads GO_TRAILING_SLASH_COMPAT (plan 15 review wave 2a L14): answer FastAPI's
// redirect_slashes forms for every registered route from Go. Off by default and independent of
// GO_CAPABILITY_ROUTES_ENABLED. A value that is not a boolean is an error.
func TrailingSlashCompatFrom(get func(string) string) (bool, error) {
	return boolVar(get, "GO_TRAILING_SLASH_COMPAT", false)
}
