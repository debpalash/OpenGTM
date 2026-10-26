package db_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// sqliteLeadColumns is PRAGMA table_info(leads) of a LeadDB-created SQLite file
// (SELECT * order), the key order of Python's job-leads JSON. search_tsv is PG-only.
const sqliteLeadColumns = "id,company,website,email,phone,contact_person,contact_title,city,state,specialization,company_size,description,linkedin_url,twitter_url,source,source_url,collection_job_id,score,score_tier,status,yupcha_value_prop,company_need,notes,created_at,updated_at,last_enriched_at,workspace_id,address,employee_count_exact,revenue_range,founded_year,industry_tags,technologies,technographics,funding_stage,company_size_basis,secondary_emails,secondary_phones,decision_makers,glassdoor_rating,email_confidence,email_provider,phone_provider,facebook_url,hiring_signals,enrichment_attempts,enrichment_waterfall,field_provenance,founding_year,last_funding_amount,investors,recent_news,google_rating,revenue_estimate,email_verify,email_presence"

func TestLeadsByCollectionJobColumnOrder(t *testing.T) {
	b, err := os.ReadFile("queries/collection_leads.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)-- name: LeadsByCollectionJob :many\s+SELECT (.*?)\s+FROM leads`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("LeadsByCollectionJob select list not found")
	}
	got := strings.Join(strings.Fields(strings.ReplaceAll(m[1], ", ", ",")), "")
	if got != sqliteLeadColumns {
		t.Errorf("column order mismatch:\n got %s\nwant %s", got, sqliteLeadColumns)
	}
}
