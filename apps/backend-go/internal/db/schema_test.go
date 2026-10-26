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

func TestSchemaHasOutreachMessageLookup(t *testing.T) {
	s := schemaSQL(t)
	fn := regexp.MustCompile(`(?s)CREATE FUNCTION public\.outreach_send_by_message_id\(p_message_id text\)\s+RETURNS TABLE\(workspace_id text, send_id integer, sequence_id text, enrollment_id integer, to_email text\)(.*?)\$\$;`)
	m := fn.FindStringSubmatch(s)
	if m == nil {
		t.Fatal("outreach_send_by_message_id function missing or wrong return columns")
	}
	for _, want := range []string{"SECURITY DEFINER", "SET search_path TO 'pg_catalog', 'public', 'pg_temp'"} {
		if !strings.Contains(m[1], want) {
			t.Errorf("function missing %q", want)
		}
	}

	rls := []string{"outreach_sequences", "outreach_enrollments", "outreach_sends", "outreach_suppressions",
		"outreach_inbound_messages", "outreach_drafts"}
	noRLS := []string{"outreach_schedules", "outreach_inbound_schedules"}
	for _, tbl := range append(append([]string{}, rls...), noRLS...) {
		if !strings.Contains(s, "CREATE TABLE public."+tbl+" (") {
			t.Errorf("%s: table missing", tbl)
		}
	}
	forced := 0
	for _, tbl := range rls {
		if strings.Contains(s, "ALTER TABLE ONLY public."+tbl+" FORCE ROW LEVEL SECURITY;") {
			forced++
		}
	}
	for _, tbl := range noRLS {
		if strings.Contains(s, "ALTER TABLE ONLY public."+tbl+" FORCE ROW LEVEL SECURITY;") {
			t.Errorf("%s: unexpected RLS", tbl)
		}
	}
	if forced != len(rls) {
		t.Errorf("forced RLS on %d outreach tables, want %d", forced, len(rls))
	}
}

func TestSchemaHasChatAndPersonTables(t *testing.T) {
	s := schemaSQL(t)
	for _, tbl := range []string{"chat_conversations", "chat_messages", "chat_tool_approvals", "chat_memories", "person_intel"} {
		if !strings.Contains(s, "CREATE TABLE public."+tbl+" (") {
			t.Errorf("%s: table missing", tbl)
		}
	}
	// FK cascade: deleting a conversation removes its messages.
	if !regexp.MustCompile(`ADD CONSTRAINT chat_messages_conversation_id_fkey FOREIGN KEY \(conversation_id\) REFERENCES public\.chat_conversations\(id\) ON DELETE CASCADE`).MatchString(s) {
		t.Error("chat_messages -> chat_conversations ON DELETE CASCADE missing")
	}
	// Tenant tables: RLS enabled + forced + workspace_isolation policy.
	for _, tbl := range []string{"chat_conversations", "chat_messages", "chat_tool_approvals"} {
		for _, stmt := range []string{"ALTER TABLE public." + tbl + " ENABLE ROW LEVEL SECURITY;", "ALTER TABLE ONLY public." + tbl + " FORCE ROW LEVEL SECURITY;"} {
			if !strings.Contains(s, stmt) {
				t.Errorf("%s: missing %q", tbl, stmt)
			}
		}
		if !strings.Contains(s, "CREATE POLICY workspace_isolation ON public."+tbl+" ") {
			t.Errorf("%s: workspace_isolation policy missing", tbl)
		}
	}
	// Global tables (as Python): no RLS, no workspace column.
	for _, tbl := range []string{"chat_memories", "person_intel"} {
		if strings.Contains(s, "ALTER TABLE public."+tbl+" ENABLE ROW LEVEL SECURITY") || strings.Contains(s, "ALTER TABLE ONLY public."+tbl+" FORCE ROW LEVEL SECURITY") {
			t.Errorf("%s must not have RLS", tbl)
		}
		m := regexp.MustCompile(`(?s)CREATE TABLE public\.` + tbl + ` \((.*?)\n\);`).FindStringSubmatch(s)
		if m == nil || strings.Contains(m[1], "workspace_id") {
			t.Errorf("%s must exist without a workspace_id column", tbl)
		}
	}
}
