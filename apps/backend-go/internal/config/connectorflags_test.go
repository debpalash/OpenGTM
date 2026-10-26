package config

import (
	"strings"
	"testing"
)

func TestConnectorFlagsDefaultsAndEnv(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	f := c.Flags
	if f.GoConnectorRoutesEnabled || f.GoExecutorAmbitionBox || f.CRMImportEnabled ||
		f.CertificationPath != "" || f.CertificationKey != "" || f.BuildSHA != "" {
		t.Fatalf("defaults wrong: %+v", f)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test",
		"GO_CONNECTOR_ROUTES_ENABLED": "true", "GO_EXECUTOR_AMBITIONBOX": "1", "CRM_IMPORT_ENABLED": "true",
		"OPENGTM_INTEGRATION_CERTIFICATIONS": "/c.json", "OPENGTM_INTEGRATION_CERTIFICATION_KEY": " cert-key-secret ",
		"OPENGTM_BUILD_SHA": "abc123"}))
	if err != nil {
		t.Fatal(err)
	}
	f = c.Flags
	if !f.GoConnectorRoutesEnabled || !f.GoExecutorAmbitionBox || !f.CRMImportEnabled ||
		f.CertificationPath != "/c.json" || f.CertificationKey != "cert-key-secret" || f.BuildSHA != "abc123" {
		t.Fatalf("flags not read: %+v", f)
	}
	if strings.Contains(c.String(), "cert-key-secret") {
		t.Fatal("certification key leaked in String()")
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "GO_CONNECTOR_ROUTES_ENABLED": "maybe"})); err == nil {
		t.Fatal("invalid bool accepted")
	}
}
