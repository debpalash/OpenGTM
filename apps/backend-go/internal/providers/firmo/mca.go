package firmo

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// mcaDefaultKey is the data.gov.in public sample key Python falls back to when
// DATA_GOV_IN_KEY is unset (published in the Python source, rate-limited).
var mcaDefaultKey = "579b464db66ec23bdd000001cdd3946e44ce4aad7209ff7b23ac571b"

const mcaDefaultResource = "4dbe5667-7b6b-41d7-82af-211562424d9a"

// mcaBudget is the time budget of the exact-name variant loop (Python: 7 s
// deadline under the 10 s worker kill).
var mcaBudget = 7 * time.Second

var (
	mcaSuffixRE = regexp.MustCompile(`\s+(PVT|PRIVATE)?\s*(LTD|LIMITED|LLP|INC|CORP)\.?$`)
	mcaPunctRE  = regexp.MustCompile(`[^\w\s&]`)
	mcaYearRE   = regexp.MustCompile(`(19|20)\d{2}`)
	mcaSpacesRE = regexp.MustCompile(`\s+`)
	mcaWordRE   = regexp.MustCompile(`[^\p{L}\p{N}_\s&]`)
	_           = mcaPunctRE
)

// mcaRegistry ports providers/mca_registry.py. The opt-in CIN-via-web-search
// path (MCA_CIN_SEARCH) needs the Python DDGS shim and stays on the Python
// worker; the Go provider always uses the exact-name variants.
type mcaRegistry struct{}

func (mcaRegistry) Name() string     { return "mca_registry" }
func (mcaRegistry) KeyEnv() []string { return []string{"DATA_GOV_IN_KEY"} }

func (m mcaRegistry) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	company := providers.PyStrip(lead(in, "company"))
	if company == "" {
		return fail("no_company"), nil
	}
	k, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	if k == "" {
		k = mcaDefaultKey
	}
	out, err := m.run(ctx, c, k, company, in)
	return finish(ctx, out, err, 120)
}

func (m mcaRegistry) run(ctx context.Context, c *http.Client, key, company string, in providers.Input) (providers.Output, error) {
	resource := mcaDefaultResource
	if s, ok := in.Settings["MCA_RESOURCE_ID"].(string); ok && s != "" {
		resource = s
	}
	deadline := time.Now().Add(mcaBudget)
	var record any
	for _, variant := range mcaVariants(company) {
		if !time.Now().Before(deadline) {
			break
		}
		rec, err := m.query(ctx, c, key, resource, "CompanyName", variant)
		if err != nil {
			return providers.Output{}, err
		}
		if providers.Truthy(rec) {
			record = rec
			break
		}
	}
	if record == nil {
		return fail("not_in_mca"), nil
	}
	f, err := mcaFields(record)
	if err != nil {
		return providers.Output{}, err
	}
	if f.Len() == 0 {
		return fail("no_fields"), nil
	}
	return success(in, f, 0.8), nil
}

// query is _mca_query: transport failures and non-200 answers are None; a
// body that is not JSON is an exception.
func (m mcaRegistry) query(ctx context.Context, c *http.Client, key, resource, field, value string) (any, error) {
	resp, err := providers.Do(ctx, c, providers.Req{Method: http.MethodGet,
		URL:    "https://api.data.gov.in/resource/" + resource,
		Query:  [][2]string{{"api-key", key}, {"format", "json"}, {"limit", "1"}, {"filters[" + field + "]", value}},
		Header: [][2]string{{"User-Agent", "curl/8.4.0"}, {"Accept", "application/json"}}}, key)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, nil
	}
	if resp.Status != 200 {
		return nil, nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return nil, err
	}
	recs, err := get(orEmpty(data), "records")
	if err != nil {
		return nil, err
	}
	if !providers.Truthy(recs) {
		return nil, nil
	}
	l, err := indexFirst(recs)
	return l, err
}

// indexFirst is recs[0] for a truthy decoded value.
func indexFirst(v any) (any, error) {
	switch t := v.(type) {
	case []any:
		return t[0], nil
	case string:
		return providers.TruncRunes(t, 1), nil
	case map[string]any:
		return nil, &providers.PyException{Msg: "0"}
	}
	return nil, &providers.PyException{Msg: "'" + providers.TypeName(v) + "' object is not subscriptable"}
}

func mcaClean(v any) string {
	if !providers.Truthy(v) {
		return ""
	}
	s := providers.PyStrip(mcaSpacesRE.ReplaceAllString(providers.Str(v), " "))
	s = strings.Trim(s, ",")
	switch strings.ToUpper(s) {
	case "", "NA", "N/A", "NIL", "-":
		return ""
	}
	return s
}

func mcaYear(v any) string {
	if !providers.Truthy(v) {
		return ""
	}
	return mcaYearRE.FindString(providers.Str(v))
}

func mcaFields(rv any) (*fields, error) {
	if _, err := dict(rv); err != nil {
		return nil, err
	}
	r := rv.(map[string]any)
	out := newFields()
	if a := mcaClean(r["Registered_Office_Address"]); a != "" {
		out.Set("address", a)
	}
	year := mcaYear(r["CompanyRegistrationdate_date"])
	if year != "" {
		out.Set("founded_year", year)
	}
	ind := mcaClean(r["CompanyIndustrialClassification"])
	if ind != "" {
		out.Set("industry_tags", ind)
	}
	status := mcaClean(r["CompanyStatus"])
	klass := mcaClean(r["CompanyClass"])
	cin := mcaClean(r["CIN"])
	roc := mcaClean(r["CompanyROCcode"])
	name := mcaClean(r["CompanyName"])
	if name != "" {
		summary := pyTitle(name)
		var parts []string
		switch {
		case klass != "" && status != "":
			parts = append(parts, status+" "+strings.ToLower(klass)+" company")
		case status != "":
			parts = append(parts, status+" company")
		}
		if year != "" {
			parts = append(parts, "incorporated "+year)
		}
		if roc != "" {
			parts = append(parts, "registered under "+roc)
		}
		if ind != "" {
			parts = append(parts, "industry: "+strings.ToLower(ind))
		}
		if len(parts) > 0 {
			summary += " — " + strings.Join(parts, ", ")
		}
		if cin != "" {
			summary += " (CIN " + cin + ")"
		}
		out.Set("description", summary)
	}
	return out, nil
}

// mcaVariants is _name_variants.
func mcaVariants(company string) []string {
	base := strings.ToUpper(mcaWordRE.ReplaceAllString(company, " "))
	base = providers.PyStrip(mcaSpacesRE.ReplaceAllString(base, " "))
	base = providers.PyStrip(mcaSuffixRE.ReplaceAllString(base, ""))
	if base == "" {
		return nil
	}
	cands := []string{
		base + " PRIVATE LIMITED", base + " LIMITED", base + " SERVICES PRIVATE LIMITED",
		base + " TECHNOLOGIES PRIVATE LIMITED", base + " SOLUTIONS PRIVATE LIMITED",
		base + " CONSULTING PRIVATE LIMITED", base + " LLP", base,
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range cands {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
