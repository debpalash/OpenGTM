package collect

import (
	"context"
	"fmt"
	"strings"
)

// Geo-relevance gate: when the query/ICP explicitly names a market (Netherlands,
// Germany, ...), leads with concrete evidence from ANOTHER market are dropped
// before scoring. This is the fix for narrow-geo ICPs (e.g. "Netherlands ...
// cybersecurity") returning India/US companies: the legacy pipeline had no
// geographic relevance check at all, and the India-biased strategies actively
// produced India leads. Evidence rules (conservative — unknown stays):
//
//   - the lead's domain ccTLD belongs to a different market -> drop
//     (m.rbi.org.in for an NL ICP),
//   - the lead's city belongs to a different market or is the legacy "India"
//     default -> drop,
//   - no geo evidence -> keep (scoring decides).

type geoVerdict struct {
	keep   bool
	reason string
}

func (m *marketSpec) judge(l *Lead) geoVerdict {
	if l.Email != "" {
		// email domains of free providers are not company-geo evidence
		if !emailProviderFree(l.Email) {
			if cc := domainCC(l.Email[strings.LastIndex(l.Email, "@")+1:]); cc != "" && cc != m.cc && cc != "com" {
				if otherMarket(cc) != nil {
					return geoVerdict{false, "email domain ." + cc}
				}
			}
		}
	}
	if cc := domainCC(DomainFromURL(l.Website)); cc != "" && cc != m.cc && cc != "com" {
		if otherMarket(cc) != nil {
			return geoVerdict{false, "website ." + cc}
		}
	}
	if l.LinkedinURL != "" {
		if cc := domainCC(hostOf(l.LinkedinURL)); cc != "" && cc != m.cc && cc != "com" && otherMarket(cc) != nil {
			return geoVerdict{false, "linkedin ." + cc}
		}
	}
	if m.contradictsCity(l.City) {
		return geoVerdict{false, "city " + l.City}
	}
	if m.contradictsCity(l.State) {
		return geoVerdict{false, "state " + l.State}
	}
	return geoVerdict{true, ""}
}

// otherMarket maps a ccTLD to a known market ("in" -> the legacy India market).
func otherMarket(cc string) *marketSpec {
	for i := range marketSpecs {
		if marketSpecs[i].cc == cc {
			return &marketSpecs[i]
		}
	}
	if cc == "in" {
		return &legacyIndiaMarket
	}
	return nil
}

// legacyIndiaMarket lets contradictsCity reason about India as a market even
// though it is the (unspecifiable) legacy default.
var legacyIndiaMarket = marketSpec{label: "India", key: "in", cc: "in", region: "india"}

// emailProviderFree reports free mail providers (gmail etc.) whose domain says
// nothing about the company's geography.
func emailProviderFree(email string) bool {
	d := strings.ToLower(email[strings.LastIndex(email, "@")+1:])
	return knownEmailProviders[d]
}

// geoStage is the geo_relevance pipeline stage: a no-op (all pass) unless the
// query explicitly names a market.
func (j *job) geoStage(ctx context.Context, leads []*Lead) ([]*Lead, error) {
	m := geoNamed(j.query)
	if m == nil {
		return leads, nil
	}
	id, err := j.createStage(ctx, "geo_relevance")
	if err != nil {
		return nil, err
	}
	var keep []*Lead
	var reasons counter
	for _, l := range leads {
		if v := m.judge(l); v.keep {
			keep = append(keep, l)
		} else {
			reasons.add(v.reason)
		}
	}
	if len(keep) == 0 {
		keep = []*Lead{}
	}
	if err := j.completeStage(ctx, id, StageResult{InputCount: len(leads), OutputCount: len(keep),
		RejectedCount: len(leads) - len(keep),
		Details:   pyDumps(dict{{"market", m.label}, {"reasons", reasons.dict()}})}); err != nil {
		return nil, err
	}
	if dropped := len(leads) - len(keep); dropped > 0 {
		j.progress("geo_relevance", fmt.Sprintf("🌍 Geo gate (%s): dropped %d leads with foreign-market evidence", m.label, dropped))
	}
	return keep, nil
}
