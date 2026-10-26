package config

import (
	"strings"
	"testing"
	"time"
)

const insecureKey = "INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY"

func apiEnv(extra map[string]string) func(string) string {
	m := map[string]string{"DATABASE_URL": "postgres://u:pw@h/d"}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

const strongKey = "0123456789abcdef0123456789abcdef-strong"

func TestConfigRejectsWeakSecretUnlessExplicitDev(t *testing.T) {
	cases := map[string]map[string]string{
		"prod default":   {"APP_ENV": "production"},
		"staging":        {"APP_ENV": "staging"},
		"blank env":      {"APP_ENV": "  "},
		"unset env":      {},
		"unset env weak": {"SECRET_KEY": "short"},
		"prod short":     {"APP_ENV": "production", "SECRET_KEY": "a-real-secret"},
		"prod 31 bytes":  {"APP_ENV": "production", "SECRET_KEY": strings.Repeat("a", 31)},
		"dev short":      {"APP_ENV": "dev", "SECRET_KEY": "short"},
		"prod substring": {"APP_ENV": "production", "SECRET_KEY": "x" + insecureKey},
		"unset env dflt": {"SECRET_KEY": insecureKey},
	}
	for name, m := range cases {
		if name == "dev short" {
			continue // covered by the dev acceptance test
		}
		_, err := loadAPI(apiEnv(m))
		if err == nil {
			t.Errorf("%s: want refusal", name)
			continue
		}
		if strings.Contains(err.Error(), insecureKey) || (m["SECRET_KEY"] != "" && strings.Contains(err.Error(), m["SECRET_KEY"])) {
			t.Errorf("%s: error echoes the key: %v", name, err)
		}
	}
	for _, k := range []string{strongKey, strings.Repeat("a", 32)} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "production", "SECRET_KEY": k})); err != nil {
			t.Errorf("strong key in prod: %v", err)
		}
		if _, err := loadAPI(apiEnv(map[string]string{"SECRET_KEY": k})); err != nil {
			t.Errorf("strong key, unset env: %v", err)
		}
	}
}

func TestConfigAcceptsWeakSecretInExplicitDev(t *testing.T) {
	for _, e := range []string{"dev", "development", "test", "testing", "local", " DEV "} {
		for _, k := range []string{"", "short", insecureKey} {
			if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": e, "SECRET_KEY": k})); err != nil {
				t.Errorf("APP_ENV=%q key=%q: %v", e, k, err)
			}
		}
	}
}

func TestMCPTokenTTLDaysAcceptsZeroAndNegative(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "-1": -1, "-30": -30, "365": 365, "100000": 100000, "": 90} {
		c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "MCP_TOKEN_TTL_DAYS": in}))
		if err != nil || c.MCPTokenTTLDays != want {
			t.Errorf("%q: got %d, %v want %d", in, c.MCPTokenTTLDays, err, want)
		}
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "MCP_TOKEN_TTL_DAYS": "abc"})); err == nil {
		t.Error("non-numeric must be refused")
	}
}

func TestConfigRejectsNonHS256(t *testing.T) {
	for _, a := range []string{"HS512", "none", "RS256"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "ALGORITHM": a})); err == nil {
			t.Errorf("ALGORITHM=%s accepted", a)
		}
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "ALGORITHM": "HS256"})); err != nil {
		t.Error(err)
	}
}

func TestScimTTLClamped(t *testing.T) {
	cases := map[string]int{"": 90, "0": 1, "-5": 1, "9999": 365, "30": 30}
	for in, want := range cases {
		c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "OPENGTM_SCIM_TOKEN_TTL_DAYS": in}))
		if err != nil || c.ScimTokenTTLDays != want {
			t.Errorf("%q: got %d, %v want %d", in, c.ScimTokenTTLDays, err, want)
		}
	}
}

func TestAPIDefaults(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8000" || c.AccessTTL != 30*time.Minute || c.RefreshTTL != 20160*time.Minute ||
		c.CORSOrigins != "*" || c.MCPTokenTTLDays != 90 || c.TrustProxy || c.Algorithm != "HS256" {
		t.Fatalf("defaults: %+v", c)
	}
	c, _ = loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "TRUST_PROXY": "true", "API_ADDR": ":9", "ACCESS_TOKEN_EXPIRE_MINUTES": "5"}))
	if !c.TrustProxy || c.Addr != ":9" || c.AccessTTL != 5*time.Minute {
		t.Fatalf("overrides: %+v", c)
	}
	if _, err := loadAPI(env(nil)); err == nil {
		t.Error("DATABASE_URL required")
	}
}

func TestAPIConfigRedacted(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "SECRET_KEY": "super-SECRET-key", "DATABASE_URL": "postgres://u:dbPASS-9@h/d"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{c.String(), c.LogValue().String()} {
		if strings.Contains(s, "super-SECRET-key") || strings.Contains(s, "dbPASS-9") {
			t.Errorf("leak: %s", s)
		}
	}
}
func TestRLSRequireSafeRoleDefaultsTrue(t *testing.T) {
	cases := map[string]bool{"": true, "true": true, "1": true, "false": false, "FALSE": false, "0": false, "no": false, "off": false, "yes": true}
	for in, want := range cases {
		c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "PG_RLS_REQUIRE_SAFE_ROLE": in}))
		if err != nil || c.RLSRequireSafeRole != want {
			t.Errorf("%q: got %v, %v want %v", in, c.RLSRequireSafeRole, err, want)
		}
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "dev", "PG_RLS_REQUIRE_SAFE_ROLE": "maybe"})); err == nil {
		t.Error("garbage must be refused")
	}
}
