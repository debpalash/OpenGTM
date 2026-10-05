package audiencerefresh

import (
	"fmt"
	"strings"
)

// leadFields are the fields of the Lead dataclass, in dataclass order. A
// member snapshot is `Lead.from_dict(row).to_dict()`: exactly these keys, with
// NULL columns kept as null, so columns of the leads table that the dataclass
// does not know (founding_year, investors, ...) never reach a snapshot.
var leadFields = []string{
	"id", "company", "website", "email", "email_confidence", "email_provider", "phone", "phone_provider",
	"contact_person", "contact_title", "city", "state", "address", "specialization", "company_size",
	"employee_count_exact", "description", "revenue_range", "founded_year", "industry_tags", "technologies",
	"technographics", "funding_stage", "company_size_basis", "linkedin_url", "twitter_url", "facebook_url",
	"secondary_emails", "secondary_phones", "decision_makers", "glassdoor_rating", "hiring_signals",
	"enrichment_attempts", "enrichment_waterfall", "field_provenance", "source", "source_url",
	"collection_job_id", "workspace_id", "score", "score_tier", "status", "yupcha_value_prop", "company_need",
	"notes", "created_at", "updated_at", "last_enriched_at",
}

// snapshotSQL builds the snapshot of a lead row (alias l) as a JSON object.
// Lead.__post_init__ replaces a falsy created_at/updated_at (NULL or ”) with
// the construction time's isoformat, which nowParam supplies.
func snapshotSQL(nowParam string) string {
	pairs := make([]string, 0, len(leadFields))
	for _, f := range leadFields {
		expr := "l." + f
		if f == "created_at" || f == "updated_at" {
			expr = fmt.Sprintf("COALESCE(NULLIF(l.%s, ''), %s::text)", f, nowParam)
		}
		pairs = append(pairs, fmt.Sprintf("'%s', %s", f, expr))
	}
	return "json_build_object(" + strings.Join(pairs, ", ") + ")::text"
}
