package db_test

import (
	"strings"
	"testing"
)

func TestSchemaHasGovernanceAndPlaybookTables(t *testing.T) {
	s := schemaSQL(t)
	for _, tbl := range []string{"governance_audit_events", "retention_policies", "retention_schedules", "retention_runs",
		"research_playbooks", "playbook_schedules", "playbook_runs", "playbook_results",
		"workspace_members", "workspace_member_permissions", "workspace_scim_tokens"} {
		if !strings.Contains(s, "CREATE TABLE public."+tbl+" (") {
			t.Errorf("%s: table missing", tbl)
		}
	}
	for _, c := range []string{"uq_research_playbook_name", "uq_playbook_result_run_lead"} {
		if !strings.Contains(s, "ADD CONSTRAINT "+c+" UNIQUE") {
			t.Errorf("constraint %s missing", c)
		}
	}
	for _, tbl := range []string{"retention_schedules", "playbook_schedules"} { // identifier-only mirrors, no RLS
		if strings.Contains(s, "ALTER TABLE ONLY public."+tbl+" FORCE ROW LEVEL SECURITY") {
			t.Errorf("%s must not have RLS", tbl)
		}
	}
}
