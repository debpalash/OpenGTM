package contact

import (
	"context"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const snovBase = "https://api.snov.io/v1"

// snovio ports providers/snovio.py (SnovioProvider): OAuth2 client
// credentials, then a domain email search. The key callback returns
// JoinKeys(client id, client secret).
type snovio struct{}

func (snovio) Name() string     { return "snovio" }
func (snovio) KeyEnv() []string { return []string{"SNOVIO_CLIENT_ID", "SNOVIO_CLIENT_SECRET"} }

func (s snovio) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	packed, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	parts := providers.SplitKeys(packed)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fail("No Snov.io API credentials configured"), nil
	}
	domain := providers.ExtractDomain(lead(in, "website"))
	if domain == "" {
		return fail("No domain available for lookup"), nil
	}
	out, err := s.run(ctx, c, parts[0], parts[1], domain, in)
	return finish(ctx, out, err)
}

func (snovio) run(ctx context.Context, c *http.Client, id, secret, domain string, in providers.Input) (providers.Output, error) {
	ok, data, err := callOK(ctx, c, jsonReq(snovBase+"/oauth/access_token",
		jo{{"grant_type", "client_credentials"}, {"client_id", id}, {"client_secret", secret}}), id, secret)
	if err != nil {
		return providers.Output{}, err
	}
	var token string
	if ok {
		tv, err := providers.PyGet(data, "access_token", "")
		if err != nil {
			return providers.Output{}, err
		}
		token, _ = tv.(string)
		if tv != nil && !providers.Truthy(tv) {
			token = ""
		}
	}
	if token == "" {
		return fail("Failed to authenticate with Snov.io"), nil
	}

	f := newFields()
	ok, data, err = callOK(ctx, c, jsonReq(snovBase+"/get-domain-emails-with-info",
		jo{{"access_token", token}, {"domain", domain}, {"limit", 5}}), id, secret, token)
	if err != nil {
		return providers.Output{}, err
	}
	if ok {
		emailsV, err := providers.PyGet(data, "emails", []any{})
		if err != nil {
			return providers.Output{}, err
		}
		if emails, _ := emailsV.([]any); providers.Truthy(emailsV) && len(emails) > 0 {
			priority := []string{"ceo", "cto", "cfo", "founder", "director", "vp", "head", "owner", "manager"}
			var best any
			for _, e := range emails {
				pos, err := providers.PyGet(e, "position", nil)
				if err != nil {
					return providers.Output{}, err
				}
				title := ""
				if providers.Truthy(pos) {
					title = strings.ToLower(providers.Str(pos))
				}
				for _, t := range priority {
					if strings.Contains(title, t) {
						best = e
						break
					}
				}
				if best != nil {
					break
				}
			}
			if best == nil {
				best = emails[0]
			}
			g := func(name string) any { v, _ := providers.PyGet(best, name, nil); return v }
			if _, err := providers.PyGet(best, "x", nil); err != nil {
				return providers.Output{}, err
			}
			if v := g("email"); providers.Truthy(v) {
				f.Set("email", v)
			}
			var name []string
			if v := g("first_name"); providers.Truthy(v) {
				name = append(name, providers.Str(v))
			}
			if v := g("last_name"); providers.Truthy(v) {
				name = append(name, providers.Str(v))
			}
			if len(name) > 0 {
				f.Set("contact_person", strings.Join(name, " "))
			}
			if v := g("position"); providers.Truthy(v) {
				f.Set("contact_title", v)
			}
		}
	}
	if f.Len() > 0 {
		return success(in, f, 0.80), nil
	}
	return fail("No emails found for domain"), nil
}
