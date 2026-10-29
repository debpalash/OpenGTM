// Native provider cost metadata exported with the checked-in catalog vectors.
// These providers do not all have a declarative manifest. Paid catalog prices
// and declared manifest overrides still take precedence.
package vendorcatalog

var nativeFreeProviders = map[string]bool{
	"ats_hiring":           true,
	"companies_house":      true,
	"company_intel":        true,
	"crosslinked":          true,
	"ddg_company":          true,
	"ddg_email":            true,
	"decision_maker":       true,
	"deep_scraper":         true,
	"email_harvester":      true,
	"facebook_pages":       true,
	"gleif":                true,
	"holehe":               true,
	"ipinfo":               true,
	"jobspy":               true,
	"jsonld_firmographics": true,
	"lead_scorer":          true,
	"local_business":       true,
	"mailscout":            true,
	"mca_registry":         true,
	"sec_edgar":            true,
	"social_finder":        true,
	"staffspy":             true,
	"tech_stack":           true,
	"website_scraper":      true,
	"wikidata":             true,
}
