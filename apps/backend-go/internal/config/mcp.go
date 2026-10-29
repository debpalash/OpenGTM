package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MCP is the configuration of the MCP server (cmd/mcp, internal/mcp). Names and
// defaults mirror the Python settings; the New* fields are Go-port hardening
// (plan 13). Token is a secret and never renders.
type MCP struct {
	RequireAuth                    bool   // MCP_REQUIRE_AUTH (default true: Go is Postgres-only)
	WriteEnabled                   bool   // MCP_WRITE_ENABLED (false)
	TokenTTLDays                   int    // MCP_TOKEN_TTL_DAYS (90; 0 = never expires)
	MaxWritesPerDay                int    // MCP_MAX_WRITES_PER_DAY (0 = unlimited)
	AutomationsEnabled             bool   // AUTOMATIONS_ENABLED (false)
	AutomationsAllowLegacyOutreach bool   // AUTOMATIONS_ALLOW_LEGACY_OUTREACH (false)
	AutomationsMaxRulesPerWS       int    // AUTOMATIONS_MAX_RULES_PER_WS (50)
	Token                          Secret // OPENGTM_MCP_TOKEN, fallback YUPCHA_MCP_TOKEN
	SSEHost                        string // OPENGTM_MCP_SSE_HOST, fallback YUPCHA_MCP_SSE_HOST (127.0.0.1)
	MaxEnrichPerDay                int    // MCP_MAX_ENRICH_PER_DAY (0 = unlimited)
	ToolTimeoutSeconds             int    // MCP_TOOL_TIMEOUT_SECONDS (120)
	StdioRevalidateSeconds         int    // MCP_STDIO_REVALIDATE_SECONDS (30)
	SSEAllowNonLoopback            bool   // OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK (false)
	DatabaseURL                    Secret // DATABASE_URL (carries a password)
}

// LoadMCP reads the MCP configuration from the process environment.
func LoadMCP() (MCP, error) { return LoadMCPFrom(os.Getenv) }

// LoadMCPFrom reads the MCP configuration through get (tests inject a map).
func LoadMCPFrom(get func(string) string) (MCP, error) {
	var c MCP
	var err error
	b := func(dst *bool, key string, def bool) {
		if err != nil {
			return
		}
		*dst, err = boolVar(get, key, def)
	}
	n := func(dst *int, key string, def, min int) {
		if err != nil {
			return
		}
		s := strings.TrimSpace(get(key))
		if s == "" {
			*dst = def
			return
		}
		v, perr := strconv.Atoi(s)
		if perr != nil || v < min {
			err = fmt.Errorf("config: %s: want an integer >= %d", key, min)
			return
		}
		*dst = v
	}
	b(&c.RequireAuth, "MCP_REQUIRE_AUTH", true)
	b(&c.WriteEnabled, "MCP_WRITE_ENABLED", false)
	b(&c.AutomationsEnabled, "AUTOMATIONS_ENABLED", false)
	b(&c.AutomationsAllowLegacyOutreach, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", false)
	b(&c.SSEAllowNonLoopback, "OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK", false)
	n(&c.TokenTTLDays, "MCP_TOKEN_TTL_DAYS", 90, 0)
	n(&c.MaxWritesPerDay, "MCP_MAX_WRITES_PER_DAY", 0, 0)
	n(&c.AutomationsMaxRulesPerWS, "AUTOMATIONS_MAX_RULES_PER_WS", 50, 0)
	n(&c.MaxEnrichPerDay, "MCP_MAX_ENRICH_PER_DAY", 0, 0)
	n(&c.ToolTimeoutSeconds, "MCP_TOOL_TIMEOUT_SECONDS", 120, 1)
	n(&c.StdioRevalidateSeconds, "MCP_STDIO_REVALIDATE_SECONDS", 30, 1)
	if err != nil {
		return MCP{}, err
	}
	c.Token = Secret(firstNonEmpty(get("OPENGTM_MCP_TOKEN"), get("YUPCHA_MCP_TOKEN")))
	c.SSEHost = firstNonEmpty(get("OPENGTM_MCP_SSE_HOST"), get("YUPCHA_MCP_SSE_HOST"))
	if c.SSEHost == "" {
		c.SSEHost = "127.0.0.1"
	}
	c.DatabaseURL = Secret(get("DATABASE_URL"))
	return c, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
