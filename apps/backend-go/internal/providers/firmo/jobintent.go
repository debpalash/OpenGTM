package firmo

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// A port of the parts of providers/job_tech_intent.py that ats_hiring uses
// (analyze_job_text). RE2 word boundaries are ASCII-only whereas Python's \b
// is Unicode-aware; text next to non-ASCII letters can therefore differ.

type techCat struct {
	cat     string
	entries []techEntry
}
type techEntry struct {
	canon   string
	aliases []string
}

var techDictionary = []techCat{
	{"CRM", []techEntry{{"Salesforce", []string{"salesforce", "sfdc", "sales cloud", "apex"}}, {"HubSpot", []string{"hubspot"}},
		{"Microsoft Dynamics", []string{"dynamics 365", "dynamics crm", "ms dynamics"}}, {"Zoho CRM", []string{"zoho crm", "zoho"}},
		{"Pipedrive", []string{"pipedrive"}}, {"Close", []string{"close.com", "close crm"}}}},
	{"MarTech", []techEntry{{"Marketo", []string{"marketo"}}, {"Pardot", []string{"pardot"}}, {"Mailchimp", []string{"mailchimp"}},
		{"Klaviyo", []string{"klaviyo"}}, {"Braze", []string{"braze"}}, {"Iterable", []string{"iterable"}},
		{"Marketing Cloud", []string{"marketing cloud", "sfmc"}}, {"Google Analytics", []string{"google analytics", "ga4", "gtag"}},
		{"Segment", []string{"segment.io", "twilio segment"}}}},
	{"Data & Warehouse", []techEntry{{"Snowflake", []string{"snowflake"}}, {"Databricks", []string{"databricks"}},
		{"BigQuery", []string{"bigquery", "big query"}}, {"Redshift", []string{"redshift"}}, {"dbt", []string{"dbt", "data build tool"}},
		{"Airflow", []string{"airflow"}}, {"Fivetran", []string{"fivetran"}}, {"Looker", []string{"looker"}}, {"Tableau", []string{"tableau"}},
		{"Power BI", []string{"power bi", "powerbi"}}, {"Kafka", []string{"kafka"}}, {"Spark", []string{"apache spark", "pyspark", "spark"}}}},
	{"Databases", []techEntry{{"PostgreSQL", []string{"postgresql", "postgres", "psql"}}, {"MySQL", []string{"mysql"}},
		{"MongoDB", []string{"mongodb", "mongo"}}, {"Redis", []string{"redis"}}, {"Elasticsearch", []string{"elasticsearch", "elastic search"}},
		{"Cassandra", []string{"cassandra"}}, {"DynamoDB", []string{"dynamodb"}}}},
	{"Cloud & Infra", []techEntry{{"AWS", []string{"amazon web services", "aws"}}, {"Azure", []string{"microsoft azure", "azure"}},
		{"GCP", []string{"google cloud", "gcp"}}, {"Kubernetes", []string{"kubernetes", "k8s"}}, {"Docker", []string{"docker"}},
		{"Terraform", []string{"terraform"}}, {"Ansible", []string{"ansible"}}}},
	{"Languages", []techEntry{{"Python", []string{"python"}}, {"JavaScript", []string{"javascript"}}, {"TypeScript", []string{"typescript"}},
		{"Java", []string{"java"}}, {"Go", []string{"golang", `\bgo\b`}}, {"Rust", []string{"rust"}}, {"Ruby", []string{"ruby"}},
		{"C#", []string{`c#`, `c♯`, `\bdotnet\b`, `\.net\b`}}, {"PHP", []string{"php"}}, {"Scala", []string{"scala"}}}},
	{"Web Frameworks", []techEntry{{"React", []string{"react", "react.js", "reactjs"}}, {"Vue.js", []string{"vue.js", "vuejs", "vue"}},
		{"Angular", []string{"angular"}}, {"Next.js", []string{"next.js", "nextjs"}}, {"Node.js", []string{"node.js", "nodejs", "node"}},
		{"Django", []string{"django"}}, {"Rails", []string{"ruby on rails", "rails"}}, {"Spring", []string{"spring boot", "spring framework"}}}},
	{"Competitor Tools", []techEntry{{"Clay", []string{"clay.com", `clay\b`}}, {"Apollo.io", []string{"apollo.io", "apollo"}},
		{"ZoomInfo", []string{"zoominfo", "zoom info"}}, {"Outreach", []string{"outreach.io", "outreach"}},
		{"Salesloft", []string{"salesloft", "sales loft"}}, {"Lemlist", []string{"lemlist"}}, {"Instantly", []string{"instantly.ai", "instantly"}},
		{"Clearbit", []string{"clearbit"}}, {"Lusha", []string{"lusha"}}, {"Gong", []string{"gong.io", "gong"}}}},
}

var adoptingCues = []string{
	"migrat", "implement", "roll out", "rolling out", "adopt", "introduc",
	"transition to", "moving to", "move to", "switch to", "switching to",
	"greenfield", "from scratch", "build out", "stand up", "standing up",
	"evaluate", "evaluating", "proof of concept", "poc ", "first ",
	"set up", "setting up",
}

const cueWindow = 80

var aliasMeta = regexp.MustCompile(`[\\^$.|?*+()\[\]{}#]`)

type compiledTech struct {
	canon, cat string
	pats       []*regexp.Regexp
}

var techCompiled = func() []compiledTech {
	var out []compiledTech
	for _, c := range techDictionary {
		for _, e := range c.entries {
			ct := compiledTech{canon: e.canon, cat: c.cat}
			for _, a := range e.aliases {
				if aliasMeta.MatchString(a) {
					ct.pats = append(ct.pats, regexp.MustCompile("(?i)"+a))
				} else {
					ct.pats = append(ct.pats, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(a)+`\b`))
				}
			}
			out = append(out, ct)
		}
	}
	return out
}()

type adoption struct{ tech, category, intent string }

// detectTechAdoption is detect_tech_adoption_signal: sorted by canonical name.
func detectTechAdoption(text string) []adoption {
	if pyBlank(text) {
		return nil
	}
	found := map[string]string{}
	for _, ct := range techCompiled {
		if _, ok := found[ct.canon]; ok {
			continue
		}
		for _, rx := range ct.pats {
			if rx.MatchString(text) {
				found[ct.canon] = ct.cat
				break
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	lower := strings.ToLower(text)
	out := make([]adoption, 0, len(names))
	for _, n := range names {
		var pats []*regexp.Regexp
		for _, ct := range techCompiled {
			if ct.canon == n {
				pats = ct.pats
				break
			}
		}
		occ := 0
		for _, rx := range pats {
			occ += len(rx.FindAllStringIndex(lower, -1))
		}
		out = append(out, adoption{n, found[n], intentFor(pats, lower, occ)})
	}
	return out
}

func pyBlank(s string) bool {
	for _, r := range s {
		if !isPySpaceRune(r) {
			return false
		}
	}
	return true
}

func isPySpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// intentFor is _intent_for.
func intentFor(pats []*regexp.Regexp, lower string, occ int) string {
	for _, rx := range pats {
		for _, m := range rx.FindAllStringIndex(lower, -1) {
			start := m[0]
			for i := 0; i < cueWindow && start > 0; i++ {
				_, w := utf8.DecodeLastRuneInString(lower[:start])
				start -= w
			}
			end := m[1]
			for i := 0; i < cueWindow && end < len(lower); i++ {
				_, w := utf8.DecodeRuneInString(lower[end:])
				end += w
			}
			window := lower[start:end]
			for _, cue := range adoptingCues {
				if strings.Contains(window, cue) {
					return "adopting"
				}
			}
		}
	}
	if occ >= 2 {
		return "expanding"
	}
	return "using"
}

// hiringVelocity is compute_hiring_velocity(open_roles) with no recency.
func hiringVelocity(open int) jo {
	level := "none"
	switch {
	case open >= 10:
		level = "hypergrowth"
	case open >= 5:
		level = "high"
	case open >= 2:
		level = "moderate"
	case open >= 1:
		level = "low"
	}
	return jo{{"open_roles", int64(open)}, {"recent_roles", int64(0)}, {"recency_days", nil}, {"level", level}}
}

// analyzeJobText is analyze_job_text(text, open_roles=n): technologies,
// tech_adoption_signal and hiring_velocity.
func analyzeJobText(text string, open int) (technologies []any, signal []jo, velocity jo) {
	ad := detectTechAdoption(text)
	technologies = []any{}
	signal = []jo{}
	for _, a := range ad {
		technologies = append(technologies, a.tech)
		signal = append(signal, jo{{"tech", a.tech}, {"category", a.category}, {"intent", a.intent}})
	}
	return technologies, signal, hiringVelocity(open)
}
