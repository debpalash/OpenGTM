package collectintent

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is the insertion-ordered JSON object used for to_dict output.
type Obj = wbcrud.Obj

// IntentOption mirrors IntentOption (field order = asdict order).
type IntentOption struct {
	Key         string
	Intent      string
	Label       string
	Description string
	Route       string
	Draft       string
}

// ToDict mirrors IntentOption.to_dict.
func (o IntentOption) ToDict() *Obj {
	d := wbcrud.NewObj()
	d.Set("key", o.Key)
	d.Set("intent", o.Intent)
	d.Set("label", o.Label)
	d.Set("description", o.Description)
	d.Set("route", o.Route)
	d.Set("draft", o.Draft)
	return d
}

// ClarificationOptions mirrors clarification_options.
func ClarificationOptions(domain string) []IntentOption {
	brand := brandOf(domain)
	return []IntentOption{
		{
			Key:         "company_research",
			Intent:      "company_research",
			Label:       "Research " + brand,
			Description: "Build one evidence-backed company profile; do not create page-title leads.",
			Route:       "/chat",
			Draft:       "Research the company at " + domain + ". Build one evidence-backed company profile with products, market, size, leadership, customers, recent signals, and cited sources.",
		},
		{
			Key:         "people_at_company",
			Intent:      "people_at_company",
			Label:       "Find people at " + brand,
			Description: "Find named decision-makers who are demonstrably associated with this company.",
			Route:       "/chat",
			Draft:       "Find decision-makers at the company " + domain + ". Return named people with title, LinkedIn evidence, and only company-attributable contact data.",
		},
		{
			Key:         "technology_users",
			Intent:      "technology_users",
			Label:       "Find " + brand + " users",
			Description: "Find actual companies using the product; keep list vendors and articles as evidence only.",
			Route:       "/chat",
			Draft:       "Find companies that use the technology or product represented by " + domain + ". Return actual customer companies, not directories or list vendors, and include evidence for each relationship.",
		},
		{
			Key:         "signal_monitor",
			Intent:      "signal_monitor",
			Label:       "Monitor " + brand,
			Description: "Create a watch for company, hiring, product, pricing, and leadership changes.",
			Route:       "/watches",
			Draft:       domain,
		},
	}
}

// specializedRouteOption mirrors _specialized_route_option.
func specializedRouteOption(intent, query string) []IntentOption {
	if intent == "signal_monitor" {
		return []IntentOption{{
			Key:         "signal_monitor",
			Intent:      intent,
			Label:       "Create a signal watch",
			Description: "Monitor the target instead of running a one-time lead search.",
			Route:       "/watches",
			Draft:       query,
		}}
	}
	labels := map[string]string{
		"company_research":  "Open company research",
		"people_at_company": "Find people in Chat",
		"technology_users":  "Research technology users",
	}
	descs := map[string]string{
		"company_research":  "Build one evidence-backed account profile.",
		"people_at_company": "Find named, attributable decision-makers.",
		"technology_users":  "Find companies with evidence of actual product usage.",
	}
	label, ok := labels[intent]
	if !ok {
		return nil
	}
	return []IntentOption{{
		Key:         intent,
		Intent:      intent,
		Label:       label,
		Description: descs[intent],
		Route:       "/chat",
		Draft:       query,
	}}
}

// CompanyTeamOptions mirrors company_team_options.
func CompanyTeamOptions(r CompanyTeamRequest) []IntentOption {
	entity, function := r.Entity, r.Function
	labelFunction := pyTitle(function)
	return []IntentOption{
		{
			Key:         "team_people",
			Intent:      "people_at_company",
			Label:       "People on " + labelFunction,
			Description: "Find named people at " + entity + " and require public evidence of both current employment and a " + function + " remit.",
			Route:       "/chat",
			Draft: "Find named people currently working on " + entity + "'s " + function + " team. " +
				"Require evidence for current employment and their " + function + " remit. " +
				"Return name, title, location, LinkedIn profile, evidence URL, retrieval " +
				"date, and confidence. Reject similarly named companies and do not guess emails.",
		},
		{
			Key:    "partner_ecosystem",
			Intent: "company_research",
			Label:  entity + " partner ecosystem",
			Description: "Find organizations with documented " + entity + " partner relationships, " +
				"not people or lookalike companies.",
			Route: "/chat",
			Draft: "Research " + entity + "'s partner ecosystem. Return organizations with a " +
				"documented partnership with " + entity + ", the relationship type, evidence URL, " +
				"evidence date, and confidence. Reject directories and unsupported claims.",
		},
		{
			Key:         "team_overview",
			Intent:      "company_research",
			Label:       "Overview of " + labelFunction,
			Description: "Explain how " + entity + "'s " + function + " organization and programs appear to work.",
			Route:       "/chat",
			Draft: "Build an evidence-backed overview of " + entity + "'s " + function + " organization " +
				"and programs. Separate verified facts from inference and cite every material claim.",
		},
	}
}
