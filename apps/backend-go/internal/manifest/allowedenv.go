package manifest

// AllowedEnvNames is compiler.allowed_env_names: the variables a manifest may
// read (auth.env_var plus every ${env:NAME} written in its own templates).
func AllowedEnvNames(m Manifest) map[string]struct{} { return allowedEnvNames(m) }
