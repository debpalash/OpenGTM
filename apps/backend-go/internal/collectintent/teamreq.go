package collectintent

import (
	"regexp"
	"strings"
)

// ci widens every literal "i" to the class Python's re.IGNORECASE matches
// (i, I, dotted capital I U+0130 and dotless i U+0131); Go's (?i) folds
// neither of the latter two. Only literal fragments are passed through it.
func ci(s string) string { return strings.ReplaceAll(s, "i", "[i\\x{130}\\x{131}]") }

// tokenChar is [a-z0-9&.'-] under Python IGNORECASE.
const tokenChar = `[a-z0-9&.'\x{130}\x{131}-]`
const tokenStart = `[a-z0-9\x{130}\x{131}]`

var teamFunctions = []string{
	`partnerships?`, `alliances?`, `business development`, `biz dev`,
	`channel(?: partnerships?)?`, `partner ecosystem`, `leadership`,
	`executive`, `sales`, `marketing`, `revenue`, `growth`,
	`engineering`, `product`, `customer success`,
}

var teamFunctionRE = "(?:" + ci(strings.Join(teamFunctions, "|")) + ")"

var companyTeamRE = regexp.MustCompile(`(?i)\A` +
	`(?:` + ci(`find|show|map|research|identify`) + `)?\s*` +
	`(?P<entity>` + tokenStart + tokenChar + `*(?:\s+` + tokenStart + tokenChar + `*){0,4}?)\s+` +
	`(?P<function>` + teamFunctionRE + `)\s+` +
	ci(`(?:teams?|org(?:anization|anisation)?|leaders?|leadership|people|contacts?|executives?)`) +
	ci(`(?:\s+(?:in|across)\s+.+)?`) + `[?.!]*\z`)

var genericMarketEntities = map[string]bool{
	"b2b": true, "companies": true, "company": true, "fintech": true, "saas": true,
	"software": true, "startups": true, "technology": true, "tech": true, "it": true,
	"enterprise": true, "businesses": true,
}

// CompanyTeamRequest mirrors CompanyTeamRequest.
type CompanyTeamRequest struct {
	Entity   string
	Function string
}

// ExtractCompanyTeamRequest mirrors extract_company_team_request.
func ExtractCompanyTeamRequest(query string) *CompanyTeamRequest {
	clean := collapseWS(pyStrip(query))
	// A match needs entity, function and noun separated by spaces.
	if strings.Count(clean, " ") < 2 {
		return nil
	}
	m := companyTeamRE.FindStringSubmatch(clean)
	if m == nil {
		return nil
	}
	entity := strings.Trim(m[companyTeamRE.SubexpIndex("entity")], " .")
	if genericMarketEntities[pyLower(entity)] {
		return nil
	}
	return &CompanyTeamRequest{
		Entity:   entity,
		Function: pyLower(pyStrip(m[companyTeamRE.SubexpIndex("function")])),
	}
}

var explicitPeopleRE = regexp.MustCompile(`(?i)\A` +
	ci(`(?:find|show|identify)`) + `\s+(?:` + ci(`named`) + `\s+)?` + ci(`people`) + `\s+` +
	ci(`currently`) + `\s+` + ci(`working`) + `\s+` + `on\s+` +
	`(?P<entity>.+?)(?:'s|’s)\s+(?P<function>.+?)\s+` + `team(?:\.|\s|\z)`)

// ExtractExplicitPeopleRequest mirrors extract_explicit_people_request.
func ExtractExplicitPeopleRequest(query string) *CompanyTeamRequest {
	clean := collapseWS(pyStrip(query))
	m := explicitPeopleRE.FindStringSubmatch(clean)
	if m == nil {
		return nil
	}
	entity := strings.Trim(m[explicitPeopleRE.SubexpIndex("entity")], " .")
	function := pyLower(strings.Trim(m[explicitPeopleRE.SubexpIndex("function")], " ."))
	if entity == "" || function == "" {
		return nil
	}
	return &CompanyTeamRequest{Entity: entity, Function: function}
}
