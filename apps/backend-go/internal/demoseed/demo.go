// Package demoseed is the opt-in first-run demo (apps/api/scripts/seed_demo.py): a workbook of 25 well-known
// companies wired to zero-key enrichment columns, and a first run so the grid fills while the user watches.
//
// Unlike the Python seed it never creates a user: the first admin comes from first-run setup in the UI
// (POST /auth/setup), never from a default admin/admin. With no workspace yet, Seed returns ErrNoWorkspace and
// writes nothing; the demo attaches to the workspace setup created.
package demoseed

import "encoding/json"

// Demo workbook identity. The id is stable so a rerun never creates a second copy.
const (
	WorkbookID          = "demo-zero-key-firstrun"
	WorkbookName        = "Demo — Zero-Key Enrichment"
	WorkbookDescription = "Auto-seeded first-run demo. Every column uses FREE, zero-key providers — no API keys required. Click any cell to see the provider waterfall."
)

// Company is one demo row: the lead's company name and website.
type Company struct{ Name, Domain string }

var companies = []Company{
	{"Stripe", "stripe.com"}, {"Notion", "notion.so"}, {"Figma", "figma.com"}, {"Vercel", "vercel.com"},
	{"Linear", "linear.app"}, {"Supabase", "supabase.com"}, {"HashiCorp", "hashicorp.com"}, {"GitLab", "gitlab.com"},
	{"Cloudflare", "cloudflare.com"}, {"MongoDB", "mongodb.com"}, {"Datadog", "datadoghq.com"}, {"Snowflake", "snowflake.com"},
	{"Twilio", "twilio.com"}, {"Atlassian", "atlassian.com"}, {"Shopify", "shopify.com"}, {"DigitalOcean", "digitalocean.com"},
	{"Elastic", "elastic.co"}, {"Confluent", "confluent.io"}, {"PagerDuty", "pagerduty.com"}, {"Auth0", "auth0.com"},
	{"Segment", "segment.com"}, {"Algolia", "algolia.com"}, {"Sentry", "sentry.io"}, {"Postman", "postman.com"},
	{"Asana", "asana.com"},
}

// Companies returns the demo rows in position order.
func Companies() []Company { return append([]Company(nil), companies...) }

// columnsJSON is _demo_columns(): enrichment columns leave `waterfall` empty so the engine's default
// waterfalls (free OSS scrapers first) drive each target. Key order follows Python.
const columnsJSON = `[
 {"id":"company","name":"Company","type":"lead_field","lead_field":"company","width":160},
 {"id":"website","name":"Website","type":"lead_field","lead_field":"website","width":180},
 {"id":"find_email","name":"Email","type":"waterfall","target_field":"email","width":220},
 {"id":"verify_email","name":"Email Verify","type":"waterfall","target_field":"email_verify","width":140},
 {"id":"find_phone","name":"Phone","type":"waterfall","target_field":"phone","width":150},
 {"id":"company_desc","name":"Description","type":"waterfall","target_field":"description","width":280},
 {"id":"company_size","name":"Size","type":"waterfall","target_field":"company_size","width":120},
 {"id":"linkedin","name":"LinkedIn","type":"waterfall","target_field":"linkedin_url","width":200},
 {"id":"hiring","name":"Hiring Signals","type":"waterfall","target_field":"hiring_signals","width":180},
 {"id":"research_hq","name":"HQ Location (research)","type":"research","prompt":"What city and country is {company} ({website}) headquartered in? Answer with just the city and country.","output_format":"text","max_steps":3,"width":220}
]`

// ColumnsJSON is the workbook's columns_config.
func ColumnsJSON() []byte {
	var v any
	if err := json.Unmarshal([]byte(columnsJSON), &v); err != nil {
		panic(err)
	}
	b, _ := json.Marshal(v)
	return b
}

// runColumnTypes are the column types a run computes (seed_demo.trigger_first_run).
var runColumnTypes = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true, "output": true,
	"research": true, "agent": true, "http": true, "formula": true}

// EnrichmentColumnIDs are the columns the first run computes, in column order.
func EnrichmentColumnIDs() []string {
	var cols []struct{ ID, Type string }
	if err := json.Unmarshal([]byte(columnsJSON), &cols); err != nil {
		panic(err)
	}
	var ids []string
	for _, c := range cols {
		if runColumnTypes[c.Type] {
			ids = append(ids, c.ID)
		}
	}
	return ids
}
