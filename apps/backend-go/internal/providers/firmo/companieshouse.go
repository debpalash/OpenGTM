package firmo

import (
	"context"
	"encoding/base64"
	"net/http"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const chBase = "https://api.company-information.service.gov.uk"

var chDirectorRoles = map[string]bool{
	"director": true, "llp-member": true, "llp-designated-member": true,
	"member-of-management-organ": true, "member-of-supervisory-organ": true, "managing-officer": true,
}

var chRoleLabels = map[string]string{
	"director": "Director", "llp-member": "LLP Member", "llp-designated-member": "LLP Designated Member",
	"member-of-management-organ": "Management Board Member", "member-of-supervisory-organ": "Supervisory Board Member",
	"managing-officer": "Managing Officer", "secretary": "Company Secretary",
}

// cleanStr is _clean: "" for falsy values and the NA placeholders, otherwise
// the whitespace-collapsed string without surrounding commas.
func cleanStr(v any) string {
	if !providers.Truthy(v) {
		return ""
	}
	s := strings.Trim(pyStrSplitJoin(providers.Str(v)), ",")
	s = providers.PyStrip(s)
	switch strings.ToUpper(s) {
	case "", "NA", "N/A", "NIL", "-":
		return ""
	}
	return s
}

// companiesHouse ports providers/companies_house.py.
type companiesHouse struct{}

func (companiesHouse) Name() string     { return "companies_house" }
func (companiesHouse) KeyEnv() []string { return []string{"COMPANIES_HOUSE_API_KEY"} }

func (p companiesHouse) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	if k == "" {
		return fail("No Companies House API key configured"), nil
	}
	company := providers.PyStrip(lead(in, "company"))
	if company == "" {
		return fail("no_company"), nil
	}
	out, err := p.run(ctx, c, k, company, in)
	return finish(ctx, out, err, 200)
}

type chClient struct {
	c    *http.Client
	auth string
	key  string
}

func (h chClient) get(ctx context.Context, path string, q [][2]string) (providers.Response, error) {
	return providers.Do(ctx, h.c, providers.Req{Method: http.MethodGet, URL: chBase + path, Query: q,
		Header: [][2]string{{"Accept", "application/json"}, {"Authorization", h.auth}}}, h.key,
		strings.TrimPrefix(h.auth, "Basic "))
}

func (p companiesHouse) run(ctx context.Context, c *http.Client, k, company string, in providers.Input) (providers.Output, error) {
	h := chClient{c: c, key: k, auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(k+":"))}
	profile, err := p.resolve(ctx, h, company)
	if err != nil {
		return providers.Output{}, err
	}
	if !providers.Truthy(profile) {
		return fail("not_in_companies_house"), nil
	}
	pm, err := get(profile, "company_number")
	if err != nil {
		return providers.Output{}, err
	}
	number := cleanStr(pm)
	officers := p.items(ctx, h, number, "/officers")
	pscs := p.items(ctx, h, number, "/persons-with-significant-control")
	if cerr := ctx.Err(); cerr != nil {
		return providers.Output{}, cerr
	}
	f, err := chFields(profile, officers, pscs)
	if err != nil {
		return providers.Output{}, err
	}
	if f.Len() == 0 {
		return fail("no_fields"), nil
	}
	return success(in, f, 0.85), nil
}

func (p companiesHouse) resolve(ctx context.Context, h chClient, company string) (any, error) {
	resp, err := h.get(ctx, "/search/companies", [][2]string{{"q", company}, {"items_per_page", "20"}})
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return nil, err
	}
	itemsV, err := get(orEmpty(data), "items")
	if err != nil {
		return nil, err
	}
	if !providers.Truthy(itemsV) {
		return nil, nil
	}
	items, err := elems(itemsV)
	if err != nil {
		return nil, err
	}
	var chosen any
	for _, it := range items {
		st, err := get(it, "company_status")
		if err != nil {
			return nil, err
		}
		if cleanStr(st) == "active" {
			chosen = it
			break
		}
	}
	if chosen == nil {
		chosen = items[0]
	}
	nv, err := get(chosen, "company_number")
	if err != nil {
		return nil, err
	}
	number := cleanStr(nv)
	if number == "" {
		return nil, nil
	}
	pr, err := h.get(ctx, "/company/"+number, nil)
	if err != nil {
		return nil, err
	}
	if pr.Status != 200 {
		return chosen, nil
	}
	pd, err := providers.ParseJSON(pr.Body)
	if err != nil {
		return nil, err
	}
	if providers.Truthy(pd) {
		return pd, nil
	}
	return chosen, nil
}

// items is _fetch_officers / _fetch_psc: any failure is an empty list.
func (p companiesHouse) items(ctx context.Context, h chClient, number, suffix string) []any {
	if number == "" {
		return nil
	}
	resp, err := h.get(ctx, "/company/"+number+suffix, [][2]string{{"items_per_page", "35"}})
	if err != nil || resp.Status != 200 {
		return nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return nil
	}
	v, err := get(orEmpty(data), "items")
	if err != nil || !providers.Truthy(v) {
		return nil
	}
	l, _ := v.([]any)
	return l
}

func chFlattenAddress(addr any) string {
	m, ok := addr.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, k := range []string{"premises", "address_line_1", "address_line_2", "locality", "region", "postal_code", "country"} {
		if s := cleanStr(m[k]); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func chFields(profile any, officers, pscs []any) (*fields, error) {
	out := newFields()
	g := func(k string) any { v, _ := get(profile, k); return v }
	number := cleanStr(g("company_number"))
	status := cleanStr(g("company_status"))
	incorporated := cleanStr(g("date_of_creation"))
	addrSrc := g("registered_office_address")
	if !providers.Truthy(addrSrc) {
		addrSrc = g("address")
	}
	address := chFlattenAddress(addrSrc)
	if number != "" {
		out.Set("company_number", number)
	}
	if status != "" {
		out.Set("company_status", status)
	}
	if incorporated != "" {
		out.Set("incorporation_date", incorporated)
		if y := providers.TruncRunes(incorporated, 4); len([]rune(incorporated)) >= 4 && isDigits(y) {
			out.Set("founded_year", y)
		}
	}
	if address != "" {
		out.Set("registered_address", address)
		out.Set("address", address)
	}
	dms, err := chDecisionMakers(officers, pscs)
	if err != nil {
		return nil, err
	}
	if len(dms) > 0 {
		out.Set("decision_makers", dumps(dms))
		if n, _ := dms[0].get("name"); providers.Truthy(n) {
			out.Set("contact_person", n)
		}
		if t, _ := dms[0].get("title"); providers.Truthy(t) {
			out.Set("contact_title", t)
		}
	}
	return out, nil
}

func chRoleLabel(role string) string {
	if l, ok := chRoleLabels[role]; ok {
		return l
	}
	if role == "" {
		return "Director"
	}
	return pyTitle(strings.ReplaceAll(role, "-", " "))
}

func chDecisionMakers(officers, pscs []any) ([]jo, error) {
	var people []jo
	seen := map[string]bool{}
	add := func(nameV any, title, role, appointed, source string) {
		name := cleanStr(nameV)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		if title == "" {
			title = role
		}
		e := jo{{"name", name}, {"title", title}, {"role", role}, {"source", source}}
		if appointed != "" {
			e = append(e, kv{"appointed_on", appointed})
		}
		people = append(people, e)
	}
	type dir struct {
		m      map[string]any
		active bool
		appt   string
	}
	var dirs []dir
	for _, o := range officers {
		m, ok := o.(map[string]any)
		if !ok {
			return nil, attrErr(o)
		}
		if chDirectorRoles[cleanStr(m["officer_role"])] {
			dirs = append(dirs, dir{m, cleanStr(m["resigned_on"]) == "", cleanStr(m["appointed_on"])})
		}
	}
	// sort key (not active, appointed_on): active first, then by date; stable.
	sort.SliceStable(dirs, func(i, j int) bool {
		if dirs[i].active != dirs[j].active {
			return dirs[i].active
		}
		return dirs[i].appt < dirs[j].appt
	})
	for _, d := range dirs {
		role := cleanStr(d.m["officer_role"])
		if role == "" {
			role = "director"
		}
		add(d.m["name"], chRoleLabel(role), role, cleanStr(d.m["appointed_on"]), "officer")
	}
	for _, p := range pscs {
		m, ok := p.(map[string]any)
		if !ok {
			return nil, attrErr(p)
		}
		if cleanStr(m["ceased_on"]) != "" {
			continue
		}
		add(m["name"], "Beneficial Owner", "beneficial-owner", cleanStr(m["notified_on"]), "psc")
	}
	return people, nil
}

// pyTitle is str.title(): upper-case the first cased letter of each run of
// letters, lower-case the rest.
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		up, lo := strings.ToUpper(string(r)), strings.ToLower(string(r))
		cased := up != lo
		switch {
		case cased && !prevCased:
			b.WriteString(up)
		case cased:
			b.WriteString(lo)
		default:
			b.WriteRune(r)
		}
		prevCased = cased
	}
	return b.String()
}
