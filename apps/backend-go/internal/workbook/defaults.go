package workbook

// defaultWaterfalls is DEFAULT_WATERFALLS from enrichment.py (the
// COMPANY_SIZE_HEURISTIC_ENABLED append is not reproduced: that flag is off by
// default and the heuristic provider is not declarative, so Python keeps such
// runs). Used only for columns without an explicit waterfall/provider.
var defaultWaterfalls = map[string][]string{
	"email": {
		"deep_scraper", "jsonld_firmographics", "website_scraper", "email_harvester",
		"ddg_email", "mailscout",
		"hunter_io", "apollo_io", "snovio", "prospeo",
	},
	"email_confidence": {"mailscout", "holehe", "abstract_api", "debounce"},
	"email_verify":     {"mailscout", "holehe", "abstract_api", "debounce"},
	"phone": {
		"deep_scraper", "jsonld_firmographics", "website_scraper", "local_business",
		"ddg_company", "facebook_pages",
		"apollo_io", "people_data_labs",
	},
	"mobile_phone": {"leadmagic_mobile", "prospeo_mobile"},
	"description": {
		"deep_scraper", "jsonld_firmographics", "website_scraper", "ddg_company",
		"company_intel", "wikidata", "mca_registry",
	},
	"decision_makers":     {"staffspy", "deep_scraper", "crosslinked", "decision_maker"},
	"contact_person":      {"deep_scraper", "crosslinked", "decision_maker", "staffspy", "people_data_labs"},
	"contact_title":       {"deep_scraper", "crosslinked", "decision_maker", "staffspy"},
	"linkedin_url":        {"jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder", "wikidata"},
	"twitter_url":         {"jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder", "wikidata"},
	"facebook_url":        {"jsonld_firmographics", "deep_scraper", "website_scraper", "social_finder", "wikidata"},
	"company_size":        {"deep_scraper", "website_scraper", "company_intel", "wikidata", "people_data_labs"},
	"industry_tags":       {"deep_scraper", "website_scraper", "local_business", "company_intel", "mca_registry", "wikidata"},
	"address":             {"jsonld_firmographics", "deep_scraper", "local_business", "mca_registry", "wikidata", "google_maps"},
	"founding_year":       {"jsonld_firmographics", "deep_scraper", "company_intel", "mca_registry", "wikidata"},
	"founded_year":        {"jsonld_firmographics", "deep_scraper", "company_intel", "mca_registry", "wikidata"},
	"funding_stage":       {"company_intel"},
	"last_funding_amount": {"company_intel"},
	"investors":           {"company_intel"},
	"recent_news":         {"company_intel"},
	"technologies":        {"tech_stack"},
	"hiring_signals":      {"ats_hiring", "jobspy"},
	"score":               {"lead_scorer"},
}

// structuredFields are write-back-only targets; Python keeps such runs.
var structuredFields = map[string]bool{
	"decision_makers": true, "hiring_signals": true, "secondary_emails": true,
	"secondary_phones": true, "technographics": true,
}
