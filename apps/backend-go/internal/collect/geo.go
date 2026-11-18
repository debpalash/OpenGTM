package collect

import (
	"os"
	"strings"
)

// Market defaults for query building and geo filtering. The legacy pipeline
// hard-coded India into city-less queries and LinkedIn leads (the original
// deployment's market). An operator can declare a primary market with
// DEFAULT_REGION (or PRIMARY_MARKET), e.g. "nl", "Netherlands", "de", "berlin",
// so non-India collections stop pulling India-biased search queries and stop
// stamping leads with City="India". With no env set every helper returns the
// legacy India behaviour, which the recorded vectors pin.

// geoCity is a (match, display) pair for a city token in query text.
type geoCity struct{ match, display string }

// marketSpec is one primary-market setting.
type marketSpec struct {
	label    string // display name used in site-scoped queries
	key      string // canonical key matched against DEFAULT_REGION/PRIMARY_MARKET
	cc       string // ccTLD of the market ("nl"); "" = no ccTLD evidence
	city     string // default city for query building
	region   string // DetectRegion's answer when text matches
	aliases  []string // lower tokens matching the key/env value
	keywords []string // lower tokens in query text that imply the market
	cities   []geoCity
}

var marketSpecs = []marketSpec{
	{
		label: "Netherlands", key: "nl", cc: "nl", city: "Amsterdam", region: "eu",
		aliases:  []string{"nl", "netherlands", "holland", "nederland", "dutch", "amsterdam", "rotterdam"},
		keywords: []string{"netherlands", "holland", "nederland", "dutch", ".nl"},
		cities: []geoCity{
			{"amsterdam", "Amsterdam"}, {"rotterdam", "Rotterdam"}, {"the hague", "The Hague"},
			{"den haag", "The Hague"}, {"utrecht", "Utrecht"}, {"eindhoven", "Eindhoven"},
			{"tilburg", "Tilburg"}, {"breda", "Breda"}, {"groningen", "Groningen"},
			{"enschede", "Enschede"}, {"haarlem", "Haarlem"}, {"almere", "Almere"},
			{"zoetermeer", "Zoetermeer"}, {"leeuwarden", "Leeuwarden"}, {"nijmegen", "Nijmegen"},
			{"leiden", "Leiden"}, {"delft", "Delft"}, {"schiphol", "Schiphol"},
		},
	},
	{
		label: "Germany", key: "de", cc: "de", city: "Berlin", region: "eu",
		aliases:  []string{"de", "germany", "deutschland", "berlin", "munich", "muenchen", "hamburg"},
		keywords: []string{"germany", "deutschland", "german", ".de"},
		cities: []geoCity{
			{"berlin", "Berlin"}, {"munich", "Munich"}, {"muenchen", "Munich"}, {"hamburg", "Hamburg"},
			{"frankfurt", "Frankfurt"}, {"cologne", "Cologne"}, {"koeln", "Cologne"}, {"stuttgart", "Stuttgart"},
			{"duesseldorf", "Duesseldorf"}, {"dusseldorf", "Duesseldorf"},
		},
	},
	{
		label: "United Kingdom", key: "uk", cc: "uk", city: "London", region: "eu",
		aliases:  []string{"uk", "united kingdom", "great britain", "britain", "england", "london", "manchester"},
		keywords: []string{"united kingdom", "britain", "england", "british", ".uk"},
		cities: []geoCity{
			{"london", "London"}, {"manchester", "Manchester"}, {"birmingham", "Birmingham"},
			{"leeds", "Leeds"}, {"bristol", "Bristol"}, {"edinburgh", "Edinburgh"},
		},
	},
	{
		label: "France", key: "fr", cc: "fr", city: "Paris", region: "eu",
		aliases:  []string{"fr", "france", "french", "paris", "lyon"},
		keywords: []string{"france", "french", ".fr"},
		cities: []geoCity{{"paris", "Paris"}, {"lyon", "Lyon"}, {"toulouse", "Toulouse"}, {"lille", "Lille"}},
	},
}

// getenv is the process env seam (overridable in tests).
var getenv = os.Getenv

// primaryMarket resolves DEFAULT_REGION / PRIMARY_MARKET to a market spec,
// nil when unset or unknown (legacy India default preserved).
func primaryMarket() *marketSpec {
	v := strings.ToLower(strings.TrimSpace(getenv("DEFAULT_REGION")))
	if v == "" {
		v = strings.TrimSpace(getenv("PRIMARY_MARKET"))
	}
	if v == "" {
		return nil
	}
	for i := range marketSpecs {
		for _, a := range marketSpecs[i].aliases {
			if v == a {
				return &marketSpecs[i]
			}
		}
	}
	return nil
}

// cityDefault is the legacy "India" fallback for query building, replaced by the
// primary market's city when DEFAULT_REGION/PRIMARY_MARKET names a known market.
func cityDefault() string {
	if m := primaryMarket(); m != nil {
		return m.city
	}
	return "India"
}

// marketDefault is the legacy "India" country token used in site-scoped queries
// ("site:linkedin.com/company \"q\" India"), replaced by the primary market's
// country name when configured.
func marketDefault() string {
	if m := primaryMarket(); m != nil {
		return m.label
	}
	return "India"
}

// GeoCity is ExtractCity extended with primary-market cities: the India cities
// first (legacy vectors), then the market's cities, else "".
func GeoCity(query string) string {
	if c := ExtractCity(query); c != "" {
		return c
	}
	low := pyLower(query)
	for _, m := range marketSpecs {
		for _, c := range m.cities {
			if strings.Contains(low, c.match) {
				return c.display
			}
		}
	}
	return ""
}

// geoNamed reports the market explicitly named in the query text (ICP), used to
// gate geo-relevance. Market keywords first; a city token alone does not name
// a market (a city is geography, but "Mumbai" alone is legacy-neutral).
func geoNamed(query string) *marketSpec {
	low := pyLower(query)
	for i := range marketSpecs {
		for _, k := range marketSpecs[i].keywords {
			if strings.Contains(low, k) {
				return &marketSpecs[i]
			}
		}
	}
	return nil
}

// geoHasCity reports whether city text belongs to the market.
func (m *marketSpec) hasCity(city string) bool {
	low := pyLower(city)
	for _, c := range m.cities {
		if low == c.match || low == pyLower(c.display) || strings.Contains(low, c.match) {
			return true
		}
	}
	return false
}

// geoContradicts reports whether a lead's known city belongs to a DIFFERENT
// market than m (explicit wrong-country evidence, e.g. City="India" for an NL
// ICP). Empty/unknown cities never contradict.
func (m *marketSpec) contradictsCity(city string) bool {
	if city == "" {
		return false
	}
	for i := range marketSpecs {
		other := &marketSpecs[i]
		if other.key == m.key {
			continue
		}
		if other.hasCity(city) {
			return true
		}
	}
	low := pyLower(city)
	// the legacy India default (and any India/US city) contradicts a foreign market
	if m.key != "in" {
		if low == "india" || anyIn(low, indianKeywords) || inList(indianCities, low) || inList(usCities, low) {
			return true
		}
	}
	if m.key != "us" && inList(usCities, low) {
		return true
	}
	return false
}

// domainCC is the ccTLD of a domain ("" for .com/.net/.org/etc. or no dot).
func domainCC(domain string) string {
	domain = strings.TrimPrefix(pyLower(strings.TrimSpace(domain)), "www.")
	i := strings.LastIndex(domain, ".")
	if i <= 0 || i == len(domain)-1 {
		return ""
	}
	tld := domain[i+1:]
	if len(tld) == 2 {
		return tld
	}
	if strings.HasSuffix(domain, ".co.in") {
		return "in"
	}
	return ""
}

// inList reports whether list contains the lower-cased value exactly.
func inList(list []string, low string) bool {
	for _, s := range list {
		if s == low {
			return true
		}
	}
	return false
}
