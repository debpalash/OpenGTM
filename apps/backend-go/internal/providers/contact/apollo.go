package contact

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const apolloBase = "https://api.apollo.io/api/v1"

// apollo ports providers/apollo_io.py (ApolloProvider).
type apollo struct{}

func (apollo) Name() string     { return "apollo_io" }
func (apollo) KeyEnv() []string { return []string{"APOLLO_API_KEY"} }

func (a apollo) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No Apollo.io API key configured"), nil
	}
	out, err := a.run(ctx, c, k, in)
	return finish(ctx, out, err)
}

func (apollo) run(ctx context.Context, c *http.Client, k string, in providers.Input) (providers.Output, error) {
	domain := providers.ExtractDomain(firstTruthy(lead(in, "website"), lead(in, "company")))
	hdr := [2]string{"X-Api-Key", k}
	f := newFields()

	if domain != "" {
		ok, data, err := callOK(ctx, c, jsonReq(apolloBase+"/organizations/enrich", jo{{"domain", domain}}, hdr), k)
		if err != nil {
			return providers.Output{}, err
		}
		if ok {
			org, err := getd(data, "organization", map[string]any{})
			if err != nil {
				return providers.Output{}, err
			}
			g := func(name string) any { v, _ := providers.PyGet(org, name, nil); return v }
			if _, err := providers.PyGet(org, "x", nil); err != nil { // org.get(...) on a non-dict
				return providers.Output{}, err
			}
			if v := g("estimated_num_employees"); providers.Truthy(v) {
				f.Set("employee_count_exact", v)
			}
			if v := g("industry"); providers.Truthy(v) {
				f.Set("industry_tags", v)
			}
			if v := g("linkedin_url"); providers.Truthy(v) {
				f.Set("linkedin_url", v)
			}
			if v := g("short_description"); providers.Truthy(v) {
				s, err := slice500(v)
				if err != nil {
					return providers.Output{}, err
				}
				f.Set("description", s)
			}
		}
	}

	search := jo{}
	if domain != "" {
		search = append(search, kv{"q_organization_domains", domain})
	} else if company := lead(in, "company"); company != "" {
		search = append(search, kv{"q_organization_name", company})
	}
	if len(search) > 0 {
		search = append(search, kv{"page", 1}, kv{"per_page", 3},
			kv{"person_seniorities", []string{"owner", "c_suite", "vp", "director"}})
		ok, data, err := callOK(ctx, c, jsonReq(apolloBase+"/mixed_people/search", search, hdr), k)
		if err != nil {
			return providers.Output{}, err
		}
		if ok {
			peopleV, err := getd(data, "people", []any{})
			if err != nil {
				return providers.Output{}, err
			}
			if people, _ := peopleV.([]any); providers.Truthy(peopleV) && len(people) > 0 {
				person := people[0]
				pg := func(name string, def any) (any, error) { return providers.PyGet(person, name, def) }
				name, err := pg("name", "")
				if err != nil {
					return providers.Output{}, err
				}
				if providers.Truthy(name) {
					f.Set("contact_person", name)
				}
				if v, _ := pg("title", nil); providers.Truthy(v) {
					f.Set("contact_title", v)
				}
				if v, _ := pg("email", nil); providers.Truthy(v) {
					f.Set("email", v)
					f.Set("email_provider", "apollo_io")
				}
				if pn, _ := pg("phone_numbers", nil); providers.Truthy(pn) {
					phones, _ := pn.([]any)
					if len(phones) > 0 {
						raw, err := providers.PyGet(phones[0], "raw_number", "")
						if err != nil {
							return providers.Output{}, err
						}
						san, _ := providers.PyGet(phones[0], "sanitized_number", raw)
						f.Set("phone", san)
						f.Set("phone_provider", "apollo_io")
					}
				}
				if v, _ := pg("linkedin_url", nil); providers.Truthy(v) {
					f.Set("linkedin_url", v)
				}
			}
		}
	}

	if f.Len() > 0 {
		return success(in, f, 0.85), nil
	}
	return fail("No data found"), nil
}
