package firmo

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	gleifBase     = "https://api.gleif.org/api/v1"
	gleifFuzzy    = gleifBase + "/fuzzycompletions"
	gleifRecords  = gleifBase + "/lei-records"
	gleifSource   = "gleif"
	gleifLicense  = "CC0-1.0"
	gleifAccept   = "application/vnd.api+json"
	gleifAgent    = "OpenGTM Enrichment admin@yupcha.com"
	gleifInterval = time.Second
)

// limiter spaces calls at least interval apart across every caller in the
// process (the Python _RateLimiter), honouring context cancellation.
type limiter struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

func (l *limiter) acquire(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if wait := l.interval - time.Since(l.last); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	l.last = time.Now()
	return nil
}

var gleifLimiter = &limiter{interval: gleifInterval}

var (
	gleifSuffixRE   = regexp.MustCompile(`(?i)\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|ltd|limited|co|company|plc|lp|gmbh|ag|sa|nv|bv|oy|ab|holdings|group|the)\b`)
	gleifNonAlnumRE = regexp.MustCompile(`[^a-z0-9 ]+`)
	spacesRE        = regexp.MustCompile(`\s+`)
)

// gleifNorm is _norm_name.
func gleifNorm(name string) string {
	s := strings.ToLower(name)
	s = gleifNonAlnumRE.ReplaceAllString(s, " ")
	s = gleifSuffixRE.ReplaceAllString(s, " ")
	return providers.PyStrip(spacesRE.ReplaceAllString(s, " "))
}

// gleif ports providers/gleif.py (GleifProvider). Redirects are followed like
// httpx's follow_redirects=True (20 hops) but every hop is URL-checked and
// passes the rate limiter.
type gleif struct{}

func (gleif) Name() string { return "gleif" }

func (g gleif) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := providers.PyStrip(lead(in, "company"))
	if company == "" {
		return fail("no_company"), nil
	}
	out, err := g.run(ctx, c, company, in)
	return finish(ctx, out, err, 120)
}

func (g gleif) run(ctx context.Context, c *http.Client, company string, in providers.Input) (providers.Output, error) {
	record, err := g.resolve(ctx, c, company)
	if err != nil {
		return providers.Output{}, err
	}
	if !providers.Truthy(record) {
		return fail("no_lei_match"), nil
	}
	f, err := gleifRecordFields(record)
	if err != nil {
		return providers.Output{}, err
	}
	hier, err := g.hierarchy(ctx, c, record)
	if err != nil {
		return providers.Output{}, err
	}
	for _, k := range hier.Keys() {
		v, _ := hier.Get(k)
		f.Set(k, v)
	}
	out := newFields()
	for _, k := range f.Keys() {
		v, _ := f.Get(k)
		if s, ok := v.(string); v == nil || (ok && s == "") {
			continue
		}
		if l, ok := v.([]any); ok && len(l) == 0 {
			continue
		}
		out.Set(k, v)
	}
	if _, ok := out.Get("lei"); !ok {
		return fail("no_lei_match"), nil
	}
	out.Set("gleif_source", gleifSource)
	out.Set("gleif_license", gleifLicense)
	return success(in, out, 0.85), nil
}

func (g gleif) resolve(ctx context.Context, c *http.Client, company string) (any, error) {
	lei, err := g.fuzzyLEI(ctx, c, company)
	if err != nil {
		return nil, err
	}
	if providers.Truthy(lei) {
		data, err := gleifGet(ctx, c, gleifRecords+"/"+providers.Str(lei), nil)
		if err != nil {
			return nil, err
		}
		if rec := firstRecord(data); providers.Truthy(rec) {
			return rec, nil
		}
	}
	data, err := gleifGet(ctx, c, gleifRecords, [][2]string{{"filter[entity.legalName]", company}, {"page[size]", "1"}})
	if err != nil {
		return nil, err
	}
	return firstRecord(data), nil
}

func (g gleif) fuzzyLEI(ctx context.Context, c *http.Client, company string) (any, error) {
	data, err := gleifGet(ctx, c, gleifFuzzy, [][2]string{{"field", "entity.legalName"}, {"q", company}})
	if err != nil {
		return nil, err
	}
	rowsV, err := get(orEmpty(data), "data")
	if err != nil {
		return nil, err
	}
	rows := orEmptyList(rowsV)
	list, err := elems(rows)
	if err != nil {
		return nil, err
	}
	target := gleifNorm(company)
	var best any
	haveBest := false
	for _, row := range list {
		lei, err := chain(row, "relationships", "lei-records", "data", "id")
		if err != nil {
			return nil, err
		}
		if !providers.Truthy(lei) {
			continue
		}
		attrs, err := get(row, "attributes")
		if err != nil {
			return nil, err
		}
		value, err := get(orEmpty(attrs), "value")
		if err != nil {
			return nil, err
		}
		vs := ""
		if providers.Truthy(value) {
			vs = providers.Str(value)
		}
		if gleifNorm(vs) == target {
			return lei, nil
		}
		if !haveBest {
			best, haveBest = lei, true
		}
	}
	return best, nil
}

// orEmptyList is `v or []`.
func orEmptyList(v any) any {
	if providers.Truthy(v) {
		return v
	}
	return []any{}
}

// gleifGet is _get_json: any non-200, transport or decode failure is None.
func gleifGet(ctx context.Context, c *http.Client, url string, q [][2]string) (any, error) {
	resp, err := guardedGet(ctx, c, url, hopOpts{maxHops: httpxMaxRedirects, query: q,
		header: [][2]string{{"Accept", gleifAccept}, {"User-Agent", gleifAgent}}, pre: gleifLimiter.acquire})
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if err != nil || resp.Status != 200 {
		return nil, nil
	}
	d, perr := providers.ParseJSON(resp.Body)
	if perr != nil {
		return nil, nil
	}
	return d, nil
}

// firstRecord is _first_record.
func firstRecord(data any) any {
	m, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	switch node := m["data"].(type) {
	case []any:
		if len(node) > 0 {
			return node[0]
		}
	case map[string]any:
		return node
	}
	return nil
}

func gleifLegalName(rec any) (any, error) {
	v, err := chain(rec, "attributes", "entity", "legalName", "name")
	if err != nil {
		return nil, err
	}
	if !providers.Truthy(v) {
		return "", nil
	}
	return v, nil
}

// gleifRecordFields is _record_to_fields.
func gleifRecordFields(rec any) (*fields, error) {
	out := newFields()
	lei, err := get(rec, "id")
	if err != nil {
		return nil, err
	}
	if providers.Truthy(lei) {
		out.Set("lei", lei)
	}
	attrs, err := get(rec, "attributes")
	if err != nil {
		return nil, err
	}
	entity, err := get(orEmpty(attrs), "entity")
	if err != nil {
		return nil, err
	}
	entity = orEmpty(entity)

	name, err := chain(entity, "legalName", "name")
	if err != nil {
		return nil, err
	}
	if providers.Truthy(name) {
		out.Set("legal_name", name)
	}
	addrV, err := get(entity, "legalAddress")
	if err != nil {
		return nil, err
	}
	for _, p := range [][2]string{{"country", "country"}, {"city", "city"}, {"region", "region"}} {
		v, err := get(orEmpty(addrV), p[0])
		if err != nil {
			return nil, err
		}
		if providers.Truthy(v) {
			out.Set(p[1], v)
		}
	}
	status, err := get(entity, "status")
	if err != nil {
		return nil, err
	}
	if providers.Truthy(status) {
		out.Set("entity_status", status)
	}
	ra, err := chain(entity, "registeredAt", "id")
	if err != nil {
		return nil, err
	}
	if providers.Truthy(ra) {
		out.Set("registration_authority", ra)
	}
	jur, err := get(entity, "jurisdiction")
	if err != nil {
		return nil, err
	}
	if providers.Truthy(jur) {
		out.Set("jurisdiction", jur)
	}
	return out, nil
}

func (g gleif) hierarchy(ctx context.Context, c *http.Client, record any) (*fields, error) {
	rels, err := get(record, "relationships")
	if err != nil {
		return nil, err
	}
	rels = orEmpty(rels)
	out := newFields()
	var hierarchy jo

	direct, err := g.parent(ctx, c, rels, "direct-parent")
	if err != nil {
		return nil, err
	}
	if direct != nil {
		out.Set("parent_lei", direct.lei)
		hierarchy.set("direct_parent", jo{{"lei", direct.lei}, {"legal_name", direct.name}})
	}
	ult, err := g.parent(ctx, c, rels, "ultimate-parent")
	if err != nil {
		return nil, err
	}
	if ult != nil {
		out.Set("ultimate_parent_lei", ult.lei)
		if providers.Truthy(ult.name) {
			out.Set("ultimate_parent_name", ult.name)
		}
		hierarchy.set("ultimate_parent", jo{{"lei", ult.lei}, {"legal_name", ult.name}})
	}
	if len(hierarchy) > 0 {
		hierarchy.set("source", gleifSource)
		hierarchy.set("license", gleifLicense)
		out.Set("corporate_hierarchy", dumps(hierarchy))
	}
	return out, nil
}

type parentRec struct{ lei, name any }

func (g gleif) parent(ctx context.Context, c *http.Client, rels any, key string) (*parentRec, error) {
	link, err := chain(rels, key, "links", "lei-record")
	if err != nil {
		return nil, err
	}
	if !providers.Truthy(link) {
		return nil, nil
	}
	url, ok := link.(string)
	if !ok {
		return nil, &providers.PyException{Msg: "Invalid type for url.  Expected str or httpx.URL, got <class '" +
			providers.TypeName(link) + "'>"}
	}
	data, err := gleifGet(ctx, c, url, nil)
	if err != nil {
		return nil, err
	}
	rec := firstRecord(data)
	if !providers.Truthy(rec) {
		return nil, nil
	}
	lei, err := get(rec, "id")
	if err != nil {
		return nil, err
	}
	if !providers.Truthy(lei) {
		return nil, nil
	}
	name, err := gleifLegalName(rec)
	if err != nil {
		return nil, err
	}
	return &parentRec{lei: lei, name: name}, nil
}
