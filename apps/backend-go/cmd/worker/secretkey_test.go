package main

import (
	"strings"
	"testing"
)

// R-secret: the worker refuses to start without a strong SECRET_KEY unless APP_ENV=test, before it touches the database.
func TestRunRefusesWeakSecretKey(t *testing.T) {
	for _, tc := range []struct{ env, key string }{{"production", ""}, {"dev", ""}, {"dev", "short"}, {"", ""}, {"local", "short"}} {
		t.Setenv("DATABASE_URL", "postgres://u:pw@127.0.0.1:1/d")
		t.Setenv("INTERNAL_SERVICE_TOKEN", strings.Repeat("t", 40))
		t.Setenv("APP_ENV", tc.env)
		t.Setenv("SECRET_KEY", tc.key)
		err := run()
		if err == nil || !strings.Contains(err.Error(), "SECRET_KEY") {
			t.Fatalf("APP_ENV=%q key=%q: want SECRET_KEY refusal, got %v", tc.env, tc.key, err)
		}
	}
}
