package contact

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const pdlBase = "https://api.peopledatalabs.com/v5"

// pdl ports providers/people_data_labs.py (PeopleDataLabsProvider).
type pdl struct{}

func (pdl) Name() string     { return "people_data_labs" }
func (pdl) KeyEnv() []string { return []string{"PDL_API_KEY"} }

func (p pdl) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No People Data Labs API key configured"), nil
	}
	domain := providers.ExtractDomain(lead(in, "website"))
	company := lead(in, "company")
	if domain == "" && company == "" {
		return fail("No domain or company name available"), nil
	}
	out, err := p.run(ctx, c, k, domain, company, in)
	return finish(ctx, out, err)
}

func (pdl) run(ctx context.Context, c *http.Client, k, domain, company string, in providers.Input) (providers.Output, error) {
	f := newFields()

	// 1. Company enrichment.
	if domain != "" {
		ok, data, err := callOK(ctx, c, providers.Req{Method: http.MethodGet, URL: pdlBase + "/company/enrich",
			Query: [][2]string{{"website", domain}}, Header: [][2]string{{"X-Api-Key", k}}}, k)
		if err != nil {
			return providers.Output{}, err
		}
		if ok {
			g := func(name string) (any, error) { return providers.PyGet(data, name, nil) }
			v, err := g("size")
			if err != nil {
				return providers.Output{}, err
			}
			if providers.Truthy(v) {
				f.Set("company_size", v)
			}
			if v, _ = g("industry"); providers.Truthy(v) {
				f.Set("industry_tags", v)
			}
			if v, _ = g("summary"); providers.Truthy(v) {
				s, err := slice500(v)
				if err != nil {
					return providers.Output{}, err
				}
				f.Set("description", s)
			}
			if v, _ = g("linkedin_url"); providers.Truthy(v) {
				f.Set("linkedin_url", v)
			}
			if v, _ = g("employee_count"); providers.Truthy(v) {
				f.Set("employee_count_exact", v)
			}
		}
	}

	// 2. Person search.
	var must any
	if domain != "" {
		must = jo{{"term", jo{{"job_company_website", domain}}}}
	} else {
		must = jo{{"match", jo{{"job_company_name", company}}}}
	}
	level := func(l string) any { return jo{{"match", jo{{"job_title_levels", l}}}} }
	body := jo{{"size", 3}, {"query", jo{{"bool", jo{
		{"must", []any{must}},
		{"should", []any{level("cxo"), level("vp"), level("director"), level("owner")}},
	}}}}}
	ok, data, err := callOK(ctx, c, jsonReq(pdlBase+"/person/search", body, [2]string{"X-Api-Key", k}), k)
	if err != nil {
		return providers.Output{}, err
	}
	if ok {
		resV, err := providers.PyGet(data, "data", []any{})
		if err != nil {
			return providers.Output{}, err
		}
		if results, _ := resV.([]any); providers.Truthy(resV) && len(results) > 0 {
			person := results[0]
			pg := func(name string, def any) (any, error) { return providers.PyGet(person, name, def) }
			name, err := pg("full_name", "")
			if err != nil {
				return providers.Output{}, err
			}
			if providers.Truthy(name) {
				f.Set("contact_person", name)
			}
			if v, _ := pg("job_title", nil); providers.Truthy(v) {
				f.Set("contact_title", v)
			}
			work, _ := pg("work_email", nil)
			personal, _ := pg("emails", []any{})
			if providers.Truthy(work) {
				f.Set("email", work)
			} else if providers.Truthy(personal) {
				list, _ := personal.([]any)
				for _, em := range list {
					if m, isMap := em.(map[string]any); isMap {
						addr, found := m["address"]
						if !found {
							addr = ""
						}
						f.Set("email", addr)
						break
					} else if s, isStr := em.(string); isStr {
						f.Set("email", s)
						break
					}
				}
			}
			if v, _ := pg("linkedin_url", nil); providers.Truthy(v) {
				f.Set("linkedin_url", v)
			}
			if phonesV, _ := pg("phone_numbers", []any{}); providers.Truthy(phonesV) {
				if phones, _ := phonesV.([]any); len(phones) > 0 {
					if s, isStr := phones[0].(string); isStr {
						f.Set("phone", s)
					} else {
						f.Set("phone", providers.Str(phones[0]))
					}
				}
			}
		}
	}

	if f.Len() > 0 {
		return success(in, f, 0.88), nil
	}
	return fail("No data found"), nil
}
