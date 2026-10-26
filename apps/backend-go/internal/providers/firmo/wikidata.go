package firmo

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	wikidataAPI = "https://www.wikidata.org/w/api.php"
	wikidataUA  = "Mozilla/5.0 (compatible; yupcha-enrichment/1.0; +https://yupcha.com)"
)

var (
	wdOrgHints = []string{"company", "corporation", "business", "enterprise", "firm", "organization",
		"organisation", "manufacturer", "consultancy", "consulting", "agency",
		"services", "technology", "software", "staffing", "recruitment", "holding",
		"startup", "subsidiary", "conglomerate", "vendor", "provider"}
	wdNegHints = []string{"award", "prize", "human settlement", "given name", "surname", "family name"}
)

// wikidata ports providers/wikidata_provider.py (WikidataProvider.enrich).
// resolve_identity, a separate Python entry point used by the domain resolver,
// is not an enrichment call and is not ported here.
type wikidata struct{}

func (wikidata) Name() string { return "wikidata" }

func (w wikidata) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := providers.PyStrip(lead(in, "company"))
	if company == "" {
		return fail("no_company"), nil
	}
	out, err := w.run(ctx, c, company, in)
	return finish(ctx, out, err, 120)
}

func (w wikidata) run(ctx context.Context, c *http.Client, company string, in providers.Input) (providers.Output, error) {
	qid, err := w.resolve(ctx, c, company)
	if err != nil {
		return providers.Output{}, err
	}
	if qid == "" {
		return fail("not_on_wikidata"), nil
	}
	all, err := w.extract(ctx, c, qid)
	if err != nil {
		return providers.Output{}, err
	}
	f := newFields()
	for _, k := range all.Keys() {
		v, _ := all.Get(k)
		if providers.Truthy(v) {
			f.Set(k, v)
		}
	}
	if f.Len() == 0 {
		return fail("no_fields"), nil
	}
	return success(in, f, 0.6), nil
}

func wdGet(ctx context.Context, c *http.Client, q [][2]string) (any, error) {
	_, data, err := fetchJSON(ctx, c, providers.Req{Method: http.MethodGet, URL: wikidataAPI, Query: q,
		Header: [][2]string{{"User-Agent", wikidataUA}}})
	return data, err
}

func containsAny(s string, hints []string) bool {
	for _, h := range hints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

// resolve is _resolve_entity: the first company-like hit, else the first
// hit that is not negative.
func (w wikidata) resolve(ctx context.Context, c *http.Client, company string) (string, error) {
	data, err := wdGet(ctx, c, [][2]string{{"action", "wbsearchentities"}, {"search", company}, {"language", "en"},
		{"type", "item"}, {"format", "json"}, {"limit", "7"}})
	if err != nil {
		return "", err
	}
	hitsV, err := providers.PyGet(data, "search", []any{})
	if err != nil {
		return "", err
	}
	if !providers.Truthy(hitsV) {
		return "", nil
	}
	hits, err := elems(hitsV)
	if err != nil {
		return "", err
	}
	best := ""
	haveBest := false
	for _, h := range hits {
		d, err := get(h, "description")
		if err != nil {
			return "", err
		}
		desc := ""
		if providers.Truthy(d) {
			desc = strings.ToLower(providers.Str(d))
		}
		if containsAny(desc, wdNegHints) {
			continue
		}
		if containsAny(desc, wdOrgHints) {
			return wdID(h)
		}
		if !haveBest {
			id, err := wdID(h)
			if err != nil {
				return "", err
			}
			best, haveBest = id, true
		}
	}
	return best, nil
}

// wdID is h["id"]; a missing key is KeyError('id').
func wdID(h any) (string, error) {
	m, ok := h.(map[string]any)
	if !ok {
		return "", attrErr(h)
	}
	v, ok := m["id"]
	if !ok {
		return "", &providers.PyException{Msg: "'id'"}
	}
	return providers.Str(v), nil
}

func (w wikidata) extract(ctx context.Context, c *http.Client, qid string) (*fields, error) {
	data, err := wdGet(ctx, c, [][2]string{{"action", "wbgetentities"}, {"ids", qid}, {"props", "claims|descriptions"},
		{"languages", "en"}, {"format", "json"}})
	if err != nil {
		return nil, err
	}
	ents, err := providers.PyGet(data, "entities", map[string]any{})
	if err != nil {
		return nil, err
	}
	ent, err := providers.PyGet(ents, qid, map[string]any{})
	if err != nil {
		return nil, err
	}
	claims, err := providers.PyGet(ent, "claims", map[string]any{})
	if err != nil {
		return nil, err
	}
	out := newFields()

	descs, err := providers.PyGet(ent, "descriptions", map[string]any{})
	if err != nil {
		return nil, err
	}
	en, err := providers.PyGet(descs, "en", map[string]any{})
	if err != nil {
		return nil, err
	}
	desc, err := providers.PyGet(orEmpty(en), "value", "")
	if err != nil {
		return nil, err
	}
	if providers.Truthy(desc) {
		out.Set("description", desc)
	}
	claim := func(p string) (any, error) { return providers.PyGet(claims, p, nil) }

	cl, err := claim("P856")
	if err != nil {
		return nil, err
	}
	out.Set("website", firstString(cl))

	if cl, err = claim("P2002"); err != nil {
		return nil, err
	}
	if tw := firstString(cl); tw != "" {
		out.Set("twitter_url", "https://twitter.com/"+strings.TrimLeft(tw, "@"))
	}
	if cl, err = claim("P2013"); err != nil {
		return nil, err
	}
	if fb := firstString(cl); fb != "" {
		out.Set("facebook_url", "https://www.facebook.com/"+fb)
	}
	if cl, err = claim("P4264"); err != nil {
		return nil, err
	}
	if li := firstString(cl); li != "" {
		out.Set("linkedin_url", "https://www.linkedin.com/company/"+li)
	}
	if cl, err = claim("P571"); err != nil {
		return nil, err
	}
	if y := firstTimeYear(cl); y != "" {
		out.Set("founded_year", y)
	}
	if cl, err = claim("P1128"); err != nil {
		return nil, err
	}
	emp, ok, err := firstQuantity(cl)
	if err != nil {
		return nil, err
	}
	if ok && emp != 0 {
		out.Set("company_size", bucketSize(emp))
	}
	if cl, err = claim("P452"); err != nil {
		return nil, err
	}
	indQ := firstEntityID(cl)
	if cl, err = claim("P159"); err != nil {
		return nil, err
	}
	hqQ := firstEntityID(cl)
	var refs []string
	for _, q := range []string{indQ, hqQ} {
		if q != "" {
			refs = append(refs, q)
		}
	}
	if len(refs) > 0 {
		labels, err := w.labels(ctx, c, refs)
		if err != nil {
			return nil, err
		}
		if indQ != "" && providers.Truthy(labels[indQ]) {
			out.Set("industry_tags", labels[indQ])
		}
		if hqQ != "" && providers.Truthy(labels[hqQ]) {
			out.Set("address", labels[hqQ])
		}
	}
	return out, nil
}

func (w wikidata) labels(ctx context.Context, c *http.Client, qids []string) (map[string]any, error) {
	data, err := wdGet(ctx, c, [][2]string{{"action", "wbgetentities"}, {"ids", strings.Join(qids, "|")}, {"props", "labels"},
		{"languages", "en"}, {"format", "json"}})
	if err != nil {
		return nil, err
	}
	ents, err := providers.PyGet(data, "entities", map[string]any{})
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, q := range qids {
		e, err := providers.PyGet(ents, q, map[string]any{})
		if err != nil {
			return nil, err
		}
		l, err := providers.PyGet(e, "labels", map[string]any{})
		if err != nil {
			return nil, err
		}
		en, err := providers.PyGet(l, "en", map[string]any{})
		if err != nil {
			return nil, err
		}
		v, err := providers.PyGet(orEmpty(en), "value", "")
		if err != nil {
			return nil, err
		}
		out[q] = v
	}
	return out, nil
}

// ── claim value extractors ────────────────────────────────────────────────

// datavalue is claim_list[0]["mainsnak"]["datavalue"]["value"] with every
// KeyError / IndexError / TypeError mapped to nil.
func datavalue(claimList any) any {
	if !providers.Truthy(claimList) {
		return nil
	}
	l, ok := claimList.([]any)
	if !ok || len(l) == 0 {
		return nil
	}
	v := l[0]
	for _, k := range []string{"mainsnak", "datavalue", "value"} {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = m[k]; !ok {
			return nil
		}
	}
	return v
}

func firstString(cl any) string {
	if s, ok := datavalue(cl).(string); ok {
		return s
	}
	return ""
}

func firstEntityID(cl any) string {
	if m, ok := datavalue(cl).(map[string]any); ok && providers.Truthy(m["id"]) {
		return providers.Str(m["id"])
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func firstTimeYear(cl any) string {
	m, ok := datavalue(cl).(map[string]any)
	if !ok {
		return ""
	}
	t, ok := m["time"].(string)
	if !ok {
		return ""
	}
	t = strings.TrimLeft(t, "+")
	if p := providers.TruncRunes(t, 4); isDigits(p) {
		return p
	}
	return ""
}

var pyFloatRE = regexp.MustCompile(`^[+-]?(\d+(_\d+)*(\.(\d+(_\d+)*)?)?|\.\d+(_\d+)*)([eE][+-]?\d+(_\d+)*)?$`)

// firstQuantity is _first_quantity: int(float(str(amount).lstrip("+"))), None
// on ValueError; infinity raises OverflowError (not caught by Python).
func firstQuantity(cl any) (int64, bool, error) {
	m, ok := datavalue(cl).(map[string]any)
	if !ok || !providers.Truthy(m["amount"]) {
		return 0, false, nil
	}
	s := providers.PyStrip(strings.TrimLeft(providers.Str(m["amount"]), "+"))
	low := strings.ToLower(strings.TrimLeft(s, "+-"))
	switch low {
	case "inf", "infinity":
		return 0, false, &providers.PyException{Msg: "cannot convert float infinity to integer"}
	case "nan":
		return 0, false, nil
	}
	if !pyFloatRE.MatchString(s) {
		return 0, false, nil
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false, nil
	}
	if math.IsInf(f, 0) {
		return 0, false, &providers.PyException{Msg: "cannot convert float infinity to integer"}
	}
	t := math.Trunc(f)
	if t >= math.MaxInt64 || t <= math.MinInt64 {
		return math.MaxInt64, true, nil
	}
	return int64(t), true, nil
}

func bucketSize(n int64) string {
	switch {
	case n <= 50:
		return "1-50"
	case n <= 200:
		return "51-200"
	case n <= 500:
		return "201-500"
	case n <= 1000:
		return "501-1000"
	}
	return "1000+"
}

var _ = fmt.Sprint
