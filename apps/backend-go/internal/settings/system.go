package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SystemInfo is GET /api/settings/system. Everything but default_provider comes
// from the process environment (not app_settings), as in the Python route.
// An empty variable counts as unset; a non-integer API_PORT is an error (Python's
// int() raises, FastAPI answers 500).
func SystemInfo(ctx context.Context, app *App, getenv func(string) string) (map[string]any, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	port := 8000
	if v := getenv("API_PORT"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("settings: API_PORT is not an integer")
		}
		port = n
	}
	def, err := app.Get(ctx, "LLM_DEFAULT_PROVIDER", "openrouter")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"debug":                    getenv("DEBUG") == "true",
		"api_host":                 or("API_HOST", "0.0.0.0"),
		"api_port":                 port,
		"cors_origins":             or("CORS_ORIGINS", "*"),
		"default_provider":         def,
		"google_search_configured": getenv("GOOGLE_API_KEY") != "",
		"linkedin_configured":      getenv("LINKEDIN_LI_AT_COOKIE") != "",
	}, nil
}
