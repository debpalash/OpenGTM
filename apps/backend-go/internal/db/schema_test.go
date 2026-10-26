package db_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func schemaSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSchemaHasEnrichmentCacheTables(t *testing.T) {
	s := schemaSQL(t)
	cases := []struct{ table, pk string }{
		{"enrichment_cache", "PRIMARY KEY (workspace_id, cache_key, field_name)"},
		{"email_verify_cache", "PRIMARY KEY (workspace_id, email_hash)"},
	}
	for _, c := range cases {
		if !strings.Contains(s, "CREATE TABLE public."+c.table+" (") {
			t.Errorf("%s: table missing", c.table)
			continue
		}
		if !regexp.MustCompile(`ADD CONSTRAINT \w+ ` + regexp.QuoteMeta(c.pk)).MatchString(s) {
			t.Errorf("%s: primary key %q missing", c.table, c.pk)
		}
		for _, stmt := range []string{
			"ALTER TABLE public." + c.table + " ENABLE ROW LEVEL SECURITY;",
			"ALTER TABLE ONLY public." + c.table + " FORCE ROW LEVEL SECURITY;",
		} {
			if !strings.Contains(s, stmt) {
				t.Errorf("%s: missing %q", c.table, stmt)
			}
		}
		if !strings.Contains(s, "CREATE POLICY workspace_isolation ON public."+c.table+" ") {
			t.Errorf("%s: workspace_isolation policy missing", c.table)
		}
	}
}
