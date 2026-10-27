package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestConnectorFlagsDefaultsAndEnv(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	f := c.Flags
	if f.GoConnectorRoutesEnabled || f.CRMImportEnabled ||
		f.CertificationPath != "" || f.CertificationKey.Reveal() != "" || f.BuildSHA != "" {
		t.Fatalf("defaults wrong: %+v", f)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test",
		"GO_CONNECTOR_ROUTES_ENABLED": "true", "CRM_IMPORT_ENABLED": "true",
		"OPENGTM_INTEGRATION_CERTIFICATIONS": "/c.json", "OPENGTM_INTEGRATION_CERTIFICATION_KEY": " cert-key-secret ",
		"OPENGTM_BUILD_SHA": "abc123"}))
	if err != nil {
		t.Fatal(err)
	}
	f = c.Flags
	if !f.GoConnectorRoutesEnabled || !f.CRMImportEnabled ||
		f.CertificationPath != "/c.json" || f.CertificationKey.Reveal() != "cert-key-secret" || f.BuildSHA != "abc123" {
		t.Fatalf("flags not read: %+v", f)
	}
	if strings.Contains(c.String(), "cert-key-secret") {
		t.Fatal("certification key leaked in String()")
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "GO_CONNECTOR_ROUTES_ENABLED": "maybe"})); err == nil {
		t.Fatal("invalid bool accepted")
	}
}

func TestCertificationKeyNeverRenders(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "OPENGTM_INTEGRATION_CERTIFICATION_KEY": "cert-key-secret"}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "flags", c.Flags, "key", c.Flags.CertificationKey, "cfg", c)
	j, _ := json.Marshal(c.Flags)
	for name, out := range map[string]string{
		"slog": buf.String(), "json": string(j),
		"%+v": fmt.Sprintf("%+v", c.Flags), "%v": fmt.Sprintf("%v", c.Flags), "%#v": fmt.Sprintf("%#v", c.Flags),
		"%s": fmt.Sprintf("%s", c.Flags.CertificationKey), "%q": fmt.Sprintf("%q", c.Flags.CertificationKey),
	} {
		if strings.Contains(out, "cert-key-secret") {
			t.Errorf("%s leaks the key: %s", name, out)
		}
	}
	if c.Flags.CertificationKey.Reveal() != "cert-key-secret" {
		t.Fatal("Reveal lost the value")
	}
}
