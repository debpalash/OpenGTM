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

func TestConfigRejectsInsecureSecretInProd(t *testing.T) {
	for _, e := range []string{"production", "staging", "prod", ""} {
		m := map[string]string{"APP_ENV": e}
		if e == "" {
			m = map[string]string{"APP_ENV": "  "}
		}
		_, err := loadAPI(apiEnv(m)) // default SECRET_KEY is the insecure key
		if err == nil || strings.Contains(err.Error(), insecureKey) {
			t.Errorf("APP_ENV=%q: want refusal without echoing key, got %v", e, err)
		}
	}
	// substring match, like Python
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "production", "SECRET_KEY": "x" + insecureKey})); err == nil {
		t.Error("substring of insecure key must be refused")
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "production", "SECRET_KEY": "a-real-secret"})); err != nil {
		t.Errorf("real key in prod: %v", err)
	}
}

func TestConfigAcceptsInsecureSecretInDev(t *testing.T) {
	for _, e := range []string{"dev", "development", "test", "testing", "local", " DEV "} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": e})); err != nil {
			t.Errorf("APP_ENV=%q: %v", e, err)
		}
	}
	// APP_ENV unset defaults to dev (Python default)
	if _, err := loadAPI(apiEnv(nil)); err != nil {
		t.Errorf("unset APP_ENV: %v", err)
	}
}

func TestConfigRejectsNonHS256(t *testing.T) {
	for _, a := range []string{"HS512", "none", "RS256"} {
		if _, err := loadAPI(apiEnv(map[string]string{"ALGORITHM": a})); err == nil {
			t.Errorf("ALGORITHM=%s accepted", a)
		}
	}
	if _, err := loadAPI(apiEnv(map[string]string{"ALGORITHM": "HS256"})); err != nil {
		t.Error(err)
	}
}

func TestScimTTLClamped(t *testing.T) {
	cases := map[string]int{"": 90, "0": 1, "-5": 1, "9999": 365, "30": 30}
	for in, want := range cases {
		c, err := loadAPI(apiEnv(map[string]string{"OPENGTM_SCIM_TOKEN_TTL_DAYS": in}))
		if err != nil || c.ScimTokenTTLDays != want {
			t.Errorf("%q: got %d, %v want %d", in, c.ScimTokenTTLDays, err, want)
		}
	}
}

func TestAPIDefaults(t *testing.T) {
	c, err := loadAPI(apiEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8000" || c.AccessTTL != 30*time.Minute || c.RefreshTTL != 20160*time.Minute ||
		c.CORSOrigins != "*" || c.MCPTokenTTLDays != 90 || c.TrustProxy || c.Algorithm != "HS256" {
		t.Fatalf("defaults: %+v", c)
	}
	c, _ = loadAPI(apiEnv(map[string]string{"TRUST_PROXY": "true", "API_ADDR": ":9", "ACCESS_TOKEN_EXPIRE_MINUTES": "5"}))
	if !c.TrustProxy || c.Addr != ":9" || c.AccessTTL != 5*time.Minute {
		t.Fatalf("overrides: %+v", c)
	}
	if _, err := loadAPI(env(nil)); err == nil {
		t.Error("DATABASE_URL required")
	}
}

func TestAPIConfigRedacted(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"SECRET_KEY": "super-SECRET-key", "DATABASE_URL": "postgres://u:dbPASS-9@h/d"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{c.String(), c.LogValue().String()} {
		if strings.Contains(s, "super-SECRET-key") || strings.Contains(s, "dbPASS-9") {
			t.Errorf("leak: %s", s)
		}
	}
}
