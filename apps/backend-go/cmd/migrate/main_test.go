package main

import (
	"bytes"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"postgresql+psycopg://u:p@h:5432/d":  "postgresql://u:p@h:5432/d",
		"postgresql+psycopg2://u:p@h/d":      "postgresql://u:p@h/d",
		"postgres+asyncpg://h/d":             "postgres://h/d",
		"postgres://u:p@h/d?sslmode=require": "postgres://u:p@h/d?sslmode=require",
		"host=h dbname=d":                    "host=h dbname=d",
	}
	for in, want := range cases {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOptionsFromEnv(t *testing.T) {
	o, err := optionsFromEnv(envOf(map[string]string{}))
	if err != nil || o.AppRole != "yupcha_app" || o.RuntimeLogin != nil {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	o, err = optionsFromEnv(envOf(map[string]string{
		"YUPCHA_APP_DB_ROLE": "grp", "YUPCHA_PROVISION_RUNTIME_ROLE": " Yes ",
		"YUPCHA_RUNTIME_DB_PASSWORD": "pw",
	}))
	if err != nil || o.AppRole != "grp" || o.RuntimeLogin == nil || o.RuntimeLogin.User != "yupcha_runtime" || o.RuntimeLogin.Password != "pw" {
		t.Fatalf("provisioning: %+v %v", o, err)
	}
	if _, err := optionsFromEnv(envOf(map[string]string{"YUPCHA_PROVISION_RUNTIME_ROLE": "1"})); err == nil {
		t.Fatal("provisioning without a password accepted")
	}
	o, _ = optionsFromEnv(envOf(map[string]string{"YUPCHA_PROVISION_RUNTIME_ROLE": "0", "YUPCHA_RUNTIME_DB_PASSWORD": "pw"}))
	if o.RuntimeLogin != nil {
		t.Fatal("provisioning enabled by 0")
	}
}

func TestRunRequiresDatabaseURL(t *testing.T) {
	err := run([]string{"up"}, envOf(map[string]string{}), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	err := run([]string{"down"}, envOf(map[string]string{"DATABASE_URL": "postgres://x/y"}), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v", err)
	}
}

func TestVersionCommandNeedsNoDatabase(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, envOf(map[string]string{}), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "latest=1") || !strings.Contains(out.String(), "alembic_baseline=e7b2c4d6f8a0") {
		t.Fatalf("out = %q", out.String())
	}
}
