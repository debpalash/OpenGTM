package contact

import (
	"context"
	"fmt"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// abstractAPI ports providers/abstract_api.py (AbstractAPIProvider).
type abstractAPI struct{}

func (abstractAPI) Name() string     { return "abstract_api" }
func (abstractAPI) KeyEnv() []string { return []string{"ABSTRACT_API_KEY"} }

func (a abstractAPI) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No AbstractAPI key configured"), nil
	}
	email := lead(in, "email")
	if email == "" {
		return fail("No email to validate"), nil
	}
	out, err := a.run(ctx, c, k, email, in)
	return finish(ctx, out, err)
}

func (abstractAPI) run(ctx context.Context, c *http.Client, k, email string, in providers.Input) (providers.Output, error) {
	resp, err := providers.Do(ctx, c, providers.Req{Method: http.MethodGet, URL: "https://emailvalidation.abstractapi.com/v1",
		Query: [][2]string{{"api_key", k}, {"email", email}}}, k)
	if err != nil {
		return providers.Output{}, err
	}
	if resp.Status < 200 || resp.Status > 299 { // httpx raise_for_status: anything but 2xx
		return fail(fmt.Sprintf("AbstractAPI error: %d", resp.Status)), nil
	}
	data, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return providers.Output{}, err
	}
	deliv, err := providers.PyGet(data, "deliverability", "UNKNOWN")
	if err != nil {
		return providers.Output{}, err
	}
	val := func(name string) (any, error) {
		o, err := providers.PyGet(data, name, map[string]any{})
		if err != nil {
			return nil, err
		}
		return providers.PyGet(o, "value", false)
	}
	isValid, err := val("is_valid_format")
	if err != nil {
		return providers.Output{}, err
	}
	isSMTP, err := val("is_smtp_valid")
	if err != nil {
		return providers.Output{}, err
	}
	isDisp, err := val("is_disposable_email")
	if err != nil {
		return providers.Output{}, err
	}
	var label string
	var conf float64
	switch {
	case deliv == "DELIVERABLE" && providers.Truthy(isSMTP) && !providers.Truthy(isDisp):
		label, conf = "smtp_verified", 0.95
	case deliv == "DELIVERABLE":
		label, conf = "verified", 0.8
	case providers.Truthy(isValid):
		label, conf = "pattern", 0.5
	default:
		label, conf = "invalid", 0.0
	}
	f := newFields()
	f.Set("email_confidence", label)
	return success(in, f, conf), nil
}
