package config

// CopilotNativeFrom reads GO_COPILOT_NATIVE (plan 15 WP-COPILOT-CORE): run the copilot's llm rounds, pre-routes,
// system prompt, action descriptions and the tools with a Go implementation in-process instead of over the Python
// worker's /internal/copilot/* RPCs (tools without a Go implementation still go to /internal/copilot/tool). Off by
// default (the Python path); WP-CUTOVER flips it. A value that is not a boolean is an error, like every GO_* flag.
func CopilotNativeFrom(get func(string) string) (bool, error) {
	return boolVar(get, "GO_COPILOT_NATIVE", false)
}
