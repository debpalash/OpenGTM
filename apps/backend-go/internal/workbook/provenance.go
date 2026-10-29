package workbook

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

// Per-fact provenance (leadgen/enrichment/licenses.py): the {source, license, confidence,
// fetched_at} tuple PROVENANCE_TRACKING_ENABLED records on produced cells and lead write-backs.

// licenseVocab is LICENSE_VOCAB.
var licenseVocab = map[string]bool{"CC0-1.0": true, "CC-BY-4.0": true, "public-record": true, "scraped": true,
	"proprietary-api": true, "user-provided": true, "unknown": true}

// providerLicense is PROVIDER_LICENSE, the central provider -> license fallback map.
var providerLicense = map[string]string{
	"gleif": "CC0-1.0", "wikidata": "CC0-1.0",
	"mca_registry": "public-record", "companies_house": "public-record", "sec": "public-record", "sec_edgar": "public-record",
	"hunter_io": "proprietary-api", "apollo_io": "proprietary-api", "snovio": "proprietary-api", "prospeo": "proprietary-api",
	"people_data_labs": "proprietary-api", "clearbit": "proprietary-api", "abstract_api": "proprietary-api", "debounce": "proprietary-api",
	"deep_scraper": "scraped", "website_scraper": "scraped", "jsonld_firmographics": "scraped", "email_harvester": "scraped",
	"ddg_email": "scraped", "ddg_company": "scraped", "mailscout": "scraped", "holehe": "scraped", "social_finder": "scraped",
	"crosslinked": "scraped", "staffspy": "scraped", "decision_maker": "scraped", "local_business": "scraped",
	"facebook_pages": "scraped", "google_maps": "scraped", "tech_stack": "scraped", "company_intel": "scraped",
	"jobspy": "scraped", "ats_hiring": "scraped", "company_size_heuristic": "scraped", "lead_scorer": "scraped",
	"user-provided": "user-provided", "csv": "user-provided", "manual": "user-provided", "csv_import": "user-provided",
}

// ResolveLicense is resolve_license: a real declared vocabulary token wins, then the central
// map by the full name, then by the name after a "cache:"-style prefix; "unknown" otherwise.
func ResolveLicense(provider, declared string) string {
	if declared != "" && declared != "unknown" && licenseVocab[declared] {
		return declared
	}
	if provider == "" {
		return "unknown"
	}
	name := provider
	if i := strings.IndexByte(provider, ':'); i >= 0 {
		name = provider[i+1:]
	}
	lic := providerLicense[provider]
	if lic == "" {
		lic = providerLicense[name]
	}
	if licenseVocab[lic] {
		return lic
	}
	return "unknown"
}

// Provenance is the canonical per-fact provenance dict, keys in Python's order.
type Provenance struct {
	Source     string   `json:"source"`
	License    string   `json:"license"`
	Confidence *float64 `json:"confidence"`
	FetchedAt  string   `json:"fetched_at"`
}

// ProvenanceOpts are provenance_for's keyword arguments ("" and nil are Python's None).
type ProvenanceOpts struct {
	License    string
	Confidence *float64
	Declared   string
}

// ProvenanceFor is provenance_for(source, ...) at now.
func ProvenanceFor(source string, o ProvenanceOpts, now time.Time) Provenance {
	lic := o.License
	if !licenseVocab[lic] {
		lic = ResolveLicense(source, o.Declared)
	}
	p := Provenance{Source: source, License: lic, FetchedAt: pyIsoformatUTC(now)}
	if p.Source == "" {
		p.Source = "unknown"
	}
	if o.Confidence != nil {
		c := round4(*o.Confidence)
		p.Confidence = &c
	}
	return p
}

// round4 is round(float(x), 4): correctly rounded to 4 decimals.
func round4(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 4, 64), 64)
	if err != nil {
		return x
	}
	return r
}

// pyIsoformatUTC is datetime.now(timezone.utc).isoformat(): microseconds only when non-zero.
func pyIsoformatUTC(t time.Time) string {
	t = t.UTC().Truncate(time.Microsecond)
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// MergeFieldProvenance is merge_field_provenance: the stored JSON object (anything else is
// treated as empty) updated with updates, rendered like json.dumps(d, default=str). Existing
// keys keep their position; new keys are appended in sorted order.
func MergeFieldProvenance(current string, updates map[string]any) string {
	d := colgraph.Col{Vals: map[string]any{}}
	if current != "" {
		if v, err := rowtmpl.DecodeOrdered([]byte(current)); err == nil {
			if c, ok := v.(colgraph.Col); ok {
				d = c
			}
		}
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.Set(k, updates[k])
	}
	var b bytes.Buffer
	pyDumps(&b, d)
	return b.String()
}

// pyDumps renders v like Python's json.dumps(v) with default separators (", ", ": "),
// ensure_ascii and allow_nan. Maps without an order are sorted.
func pyDumps(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		s, _ := canonjson.Marshal(x, false)
		b.Write(s)
	case json.Number:
		b.WriteString(pyNumberText(string(x)))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(pyFloatText(x))
	case *float64:
		if x == nil {
			b.WriteString("null")
		} else {
			b.WriteString(pyFloatText(*x))
		}
	case Provenance:
		pyDumpsPairs(b, []string{"source", "license", "confidence", "fetched_at"},
			map[string]any{"source": x.Source, "license": x.License, "confidence": x.Confidence, "fetched_at": x.FetchedAt})
	case colgraph.Col:
		pyDumpsPairs(b, x.Keys, x.Vals)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pyDumpsPairs(b, keys, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			pyDumps(b, e)
		}
		b.WriteByte(']')
	default: // default=str
		s, _ := canonjson.Marshal(rowtmpl.PyStr(x), false)
		b.Write(s)
	}
}

func pyDumpsPairs(b *bytes.Buffer, keys []string, vals map[string]any) {
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		pyDumps(b, k)
		b.WriteString(": ")
		pyDumps(b, vals[k])
	}
	b.WriteByte('}')
}

// pyNumberText is json.dumps(json.loads(text)) for a JSON number: an int stays as written,
// anything with a fraction or exponent is a float.
func pyNumberText(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return s
	}
	return pyFloatText(f)
}

func pyFloatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return s
}
