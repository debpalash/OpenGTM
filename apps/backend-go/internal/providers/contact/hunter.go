package contact

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// hunter ports providers/hunter_io.py (HunterProvider).
type hunter struct{ base string }

func (*hunter) Name() string     { return "hunter_io" }
func (*hunter) KeyEnv() []string { return []string{"HUNTER_API_KEY"} }

func (h *hunter) url(p string) string {
	b := h.base
	if b == "" {
		b = "https://api.hunter.io/v2"
	}
	return b + p
}

func (h *hunter) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No Hunter.io API key configured"), nil
	}
	domain := providers.ExtractDomain(firstTruthy(lead(in, "website"), lead(in, "company")))
	if domain == "" {
		return fail("No domain available"), nil
	}
	out, err := h.run(ctx, c, k, domain, in)
	return finish(ctx, out, err)
}

func firstTruthy(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (h *hunter) run(ctx context.Context, c *http.Client, k, domain string, in providers.Input) (providers.Output, error) {
	if person := lead(in, "contact_person"); person != "" {
		parts := providers.PySplit1(person)
		var first, last string
		if len(parts) > 0 {
			first = parts[0]
		}
		if len(parts) > 1 {
			last = parts[1]
		}
		_, data, err := call(ctx, c, providers.Req{Method: http.MethodGet, URL: h.url("/email-finder"),
			Query: [][2]string{{"domain", domain}, {"first_name", first}, {"last_name", last}, {"api_key", k}}}, k)
		if err != nil {
			return providers.Output{}, err
		}
		emailData, err := getd(data, "data", map[string]any{})
		if err != nil {
			return providers.Output{}, err
		}
		email, err := providers.PyGet(emailData, "email", nil)
		if err != nil {
			return providers.Output{}, err
		}
		if providers.Truthy(email) {
			conf, _ := providers.PyGet(emailData, "confidence", int64(0))
			f := newFields()
			f.Set("email", email)
			f.Set("email_confidence", providers.Str(conf))
			f.Set("email_provider", "hunter_io")
			f.Set("email_match_method", "exact_name")
			c100, err := div100(conf)
			if err != nil {
				return providers.Output{}, err
			}
			return success(in, f, c100), nil
		}
	}

	_, data, err := call(ctx, c, providers.Req{Method: http.MethodGet, URL: h.url("/domain-search"),
		Query: [][2]string{{"domain", domain}, {"api_key", k}, {"limit", "5"}}}, k)
	if err != nil {
		return providers.Output{}, err
	}
	emailsV, err := getd(data, "data", map[string]any{}, "emails", []any{})
	if err != nil {
		return providers.Output{}, err
	}
	emails, _ := emailsV.([]any)
	if !providers.Truthy(emailsV) {
		return fail("No emails found for domain"), nil
	}
	best, err := maxByConfidence(emails)
	if err != nil {
		return providers.Output{}, err
	}
	value, found := best["value"]
	if !found {
		return providers.Output{}, &providers.PyException{Msg: "'value'"}
	}
	conf := best["confidence"]
	if _, has := best["confidence"]; !has {
		conf = int64(0)
	}
	f := newFields()
	f.Set("email", value)
	f.Set("email_confidence", providers.Str(conf))
	f.Set("email_provider", "hunter_io")
	f.Set("email_match_method", "domain_search")
	if providers.Truthy(best["first_name"]) && providers.Truthy(best["last_name"]) {
		f.Set("contact_person", providers.Str(best["first_name"])+" "+providers.Str(best["last_name"]))
	}
	if providers.Truthy(best["position"]) {
		f.Set("contact_title", best["position"])
	}
	c100, err := div100(conf)
	if err != nil {
		return providers.Output{}, err
	}
	return success(in, f, c100), nil
}

// maxByConfidence is max(emails, key=lambda e: e.get("confidence", 0)): the
// first maximal item wins.
func maxByConfidence(emails []any) (map[string]any, error) {
	var best map[string]any
	var bestKey any
	for i, e := range emails {
		kv, err := providers.PyGet(e, "confidence", int64(0))
		if err != nil {
			return nil, err
		}
		if i == 0 {
			best, bestKey = e.(map[string]any), kv
			continue
		}
		a, aok := providers.Num(kv)
		b, bok := providers.Num(bestKey)
		if !aok || !bok {
			return nil, &providers.PyException{Msg: "'>' not supported between instances of '" +
				providers.TypeName(kv) + "' and '" + providers.TypeName(bestKey) + "'"}
		}
		if a > b {
			best, bestKey = e.(map[string]any), kv
		}
	}
	return best, nil
}
