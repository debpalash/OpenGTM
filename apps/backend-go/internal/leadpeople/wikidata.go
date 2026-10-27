package leadpeople

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	wikidataAPI     = "https://www.wikidata.org/w/api.php"
	wikidataUA      = "Mozilla/5.0 (compatible; yupcha-enrichment/1.0; +https://yupcha.com)"
	wikidataMaxBody = 2 << 20
)

var (
	wdOrgHints = []string{"company", "corporation", "business", "enterprise", "firm", "organization",
		"organisation", "manufacturer", "consultancy", "consulting", "agency",
		"services", "technology", "software", "staffing", "recruitment", "holding",
		"startup", "subsidiary", "conglomerate", "vendor", "provider"}
	wdNegHints = []string{"award", "prize", "human settlement", "given name", "surname", "family name"}

	wdSuffixRe = regexp.MustCompile(`(?:^|[^` + pyW + `])(incorporated|inc|corp(?:oration)?|llc|ltd|limited|plc|pvt)\.?$`)
)

// wikidataIdentityName is _identity_name of wikidata_provider.py.
func wikidataIdentityName(v string) string {
	v = strings.ToLower(providers.PyStrip(v))
	if m := wdSuffixRe.FindStringSubmatchIndex(v); m != nil {
		v = v[:m[2]]
	}
	return providers.PyStrip(nonAlnumRe.ReplaceAllString(v, " "))
}

// selectExactOrgEntity is _select_exact_org_entity: one exact,
// organization-like search hit, or "ambiguous" / "not_found".
func selectExactOrgEntity(company string, hits []any) (qid, status string) {
	target := wikidataIdentityName(company)
	var exact []string
	for _, h := range hits {
		m, _ := h.(map[string]any)
		desc := strings.ToLower(getStr(m, "description"))
		if anySubstr(desc, wdNegHints) || !anySubstr(desc, wdOrgHints) {
			continue
		}
		if wikidataIdentityName(getStr(m, "label")) == target {
			exact = append(exact, getStr(m, "id"))
		}
	}
	var ids []string
	for _, id := range dedupStrings(exact) {
		if id != "" {
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], "resolved"
	case len(ids) > 1:
		return "", "ambiguous"
	}
	return "", "not_found"
}

// WikidataIdentity is IdentityResolver over the keyless Wikidata API
// (WikidataProvider.resolve_identity): an exact company name resolves to its
// official website and the Wikidata item as evidence. Client must be the
// egress-guarded client. Deviation: the industry / headquarters label lookup
// of the Python extraction is skipped (resolve_identity never read it).
type WikidataIdentity struct {
	Client *http.Client
	API    string // "" = the public API
	Now    func() time.Time
}

func (w WikidataIdentity) get(ctx context.Context, params url.Values) (map[string]any, error) {
	api := w.API
	if api == "" {
		api = wikidataAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", wikidataUA)
	c := w.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, wikidataMaxBody))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("wikidata: %w", err)
	}
	return out, nil
}

// ResolveIdentity implements IdentityResolver.
func (w WikidataIdentity) ResolveIdentity(ctx context.Context, company string) (map[string]any, error) {
	company = providers.PyStrip(company)
	if company == "" {
		return map[string]any{"status": "not_found", "canonical_domain": ""}, nil
	}
	search, err := w.get(ctx, url.Values{"action": {"wbsearchentities"}, "search": {company}, "language": {"en"},
		"type": {"item"}, "format": {"json"}, "limit": {"7"}})
	if err != nil {
		return nil, err
	}
	hits, _ := search["search"].([]any)
	qid, status := selectExactOrgEntity(company, hits)
	if qid == "" {
		return map[string]any{"status": status, "canonical_domain": ""}, nil
	}
	ent, err := w.get(ctx, url.Values{"action": {"wbgetentities"}, "ids": {qid}, "props": {"claims|descriptions"},
		"languages": {"en"}, "format": {"json"}})
	if err != nil {
		return nil, err
	}
	website := providers.PyStrip(firstClaimString(ent, qid, "P856"))
	canonical := NormalizeDomain(website)
	evidence := "https://www.wikidata.org/wiki/" + qid
	if canonical == "" {
		return map[string]any{"status": "website_missing", "canonical_domain": "", "entity_id": qid, "evidence_url": evidence}, nil
	}
	now := time.Now().UTC()
	if w.Now != nil {
		now = w.Now()
	}
	return map[string]any{
		"status": "resolved", "company": company, "canonical_domain": canonical, "website": website,
		"entity_id": qid, "confidence": 0.9, "observed_at": isoDate(now), "evidence_url": evidence,
		"source": "wikidata", "source_license": "CC0-1.0",
	}, nil
}

// firstClaimString is _first_string(claims.get(prop)) of the entity.
func firstClaimString(ent map[string]any, qid, prop string) string {
	entities, _ := ent["entities"].(map[string]any)
	e, _ := entities[qid].(map[string]any)
	claims, _ := e["claims"].(map[string]any)
	list, _ := claims[prop].([]any)
	if len(list) == 0 {
		return ""
	}
	first, _ := list[0].(map[string]any)
	snak, _ := first["mainsnak"].(map[string]any)
	dv, _ := snak["datavalue"].(map[string]any)
	s, _ := dv["value"].(string)
	return s
}
