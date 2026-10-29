package main

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

func TestPoolConfigSessionLimits(t *testing.T) {
	p, err := poolConfig(config.Config{DatabaseURL: "postgres://u:p@localhost:5432/db"})
	if err != nil {
		t.Fatal(err)
	}
	rp := p.ConnConfig.RuntimeParams
	if rp["TimeZone"] != "UTC" || rp["lock_timeout"] == "" || rp["statement_timeout"] == "" {
		t.Fatalf("runtime params %v", rp)
	}
	if _, err := poolConfig(config.Config{DatabaseURL: "::bad::"}); err == nil {
		t.Fatal("bad DATABASE_URL must fail")
	}
}

func TestImapCheckerNeedsLocalCredentials(t *testing.T) {
	f, err := imapChecker(config.Config{CredentialsSource: "http"}, nil)
	if err != nil || f != nil {
		t.Fatalf("http credentials source has no secret store: nil func=%v err=%v", f == nil, err)
	}
}
