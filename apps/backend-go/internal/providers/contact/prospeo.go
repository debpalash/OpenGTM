package contact

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const prospeoBase = "https://api.prospeo.io"

// prospeo ports providers/prospeo.py (ProspeoProvider).
type prospeo struct{}

func (prospeo) Name() string     { return "prospeo" }
func (prospeo) KeyEnv() []string { return []string{"PROSPEO_API_KEY"} }

func (p prospeo) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No Prospeo API key configured"), nil
	}
	out, err := p.run(ctx, c, k, in)
	return finish(ctx, out, err)
}

// response is `resp.json().get("response", {})` when the status is 200.
func prospeoResponse(ctx context.Context, c *http.Client, r providers.Req, k string) (data any, ok bool, err error) {
	ok, d, err := callOK(ctx, c, r, k)
	if err != nil || !ok {
		return nil, false, err
	}
	resp, err := getd(d, "response", map[string]any{})
	return resp, true, err
}

func (prospeo) run(ctx context.Context, c *http.Client, k string, in providers.Input) (providers.Output, error) {
	hdr := [2]string{"Authorization", "Bearer " + k}

	// Strategy 1: LinkedIn URL -> email.
	if li := lead(in, "linkedin_url"); li != "" && hasSub(li, "linkedin.com") {
		data, ok, err := prospeoResponse(ctx, c, jsonReq(prospeoBase+"/linkedin-email-finder", jo{{"url", li}}, hdr), k)
		if err != nil {
			return providers.Output{}, err
		}
		if ok {
			email, err := providers.PyGet(data, "email", nil)
			if err != nil {
				return providers.Output{}, err
			}
			if providers.Truthy(email) {
				f := newFields()
				f.Set("email", email)
				f.Set("email_match_method", "linkedin")
				verified, _ := providers.PyGet(data, "email_verified", nil)
				conf := 0.75
				if providers.Truthy(verified) {
					f.Set("email_verified", true)
					conf = 0.90
				}
				return success(in, f, conf), nil
			}
		}
	}

	// Strategy 2: domain + name email finder.
	domain := providers.ExtractDomain(lead(in, "website"))
	name := lead(in, "contact_person")
	if domain != "" && name != "" {
		parts := providers.PyFields(name)
		var first, last string
		if len(parts) > 0 {
			first = parts[0]
		}
		if len(parts) > 1 {
			last = parts[len(parts)-1]
		}
		if first != "" && last != "" {
			data, ok, err := prospeoResponse(ctx, c, jsonReq(prospeoBase+"/email-finder",
				jo{{"domain", domain}, {"first_name", first}, {"last_name", last}}, hdr), k)
			if err != nil {
				return providers.Output{}, err
			}
			if ok {
				email, err := providers.PyGet(data, "email", nil)
				if err != nil {
					return providers.Output{}, err
				}
				if providers.Truthy(email) {
					f := newFields()
					f.Set("email", email)
					f.Set("email_match_method", "exact_name")
					return success(in, f, 0.80), nil
				}
			}
		}
	}

	// Strategy 3: verify the existing email.
	if existing := lead(in, "email"); existing != "" && hasSub(existing, "@") {
		data, ok, err := prospeoResponse(ctx, c, jsonReq(prospeoBase+"/email-verifier", jo{{"email", existing}}, hdr), k)
		if err != nil {
			return providers.Output{}, err
		}
		if ok {
			valid, err := providers.PyGet(data, "is_valid", false)
			if err != nil {
				return providers.Output{}, err
			}
			f := newFields()
			f.Set("email", existing)
			if providers.Truthy(valid) {
				f.Set("email_verify", "valid")
				return success(in, f, 0.95), nil
			}
			f.Set("email_verify", "invalid")
			return success(in, f, 0.3), nil
		}
	}
	return fail("No LinkedIn URL, domain+name, or email available"), nil
}
