package db_test

import (
	"strings"
	"testing"
)

func TestSchemaHasAudienceDestinationTables(t *testing.T) {
	s := schemaSQL(t)
	rls := []string{"audiences", "audience_members", "audience_membership_events", "audience_destinations",
		"destination_runs", "destination_deliveries", "destination_inbound_receipts"}
	noRLS := []string{"audience_schedules", "destination_inbound_tokens"}
	for _, tbl := range append(append([]string{}, rls...), noRLS...) {
		if !strings.Contains(s, "CREATE TABLE public."+tbl+" (") {
			t.Errorf("%s: table missing", tbl)
		}
	}
	for _, tbl := range rls {
		for _, stmt := range []string{"ALTER TABLE public." + tbl + " ENABLE ROW LEVEL SECURITY;", "ALTER TABLE ONLY public." + tbl + " FORCE ROW LEVEL SECURITY;"} {
			if !strings.Contains(s, stmt) {
				t.Errorf("%s: missing %q", tbl, stmt)
			}
		}
	}
	for _, tbl := range noRLS {
		if strings.Contains(s, "ALTER TABLE public."+tbl+" ENABLE ROW LEVEL SECURITY") || strings.Contains(s, "ALTER TABLE ONLY public."+tbl+" FORCE ROW LEVEL SECURITY") {
			t.Errorf("%s must not have RLS (as Python)", tbl)
		}
	}
}
