package contact

import (
	"context"
	"fmt"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// debounce ports providers/debounce.py (DebounceProvider).
type debounce struct{}

func (debounce) Name() string     { return "debounce" }
func (debounce) KeyEnv() []string { return []string{"DEBOUNCE_API_KEY"} }

func (d debounce) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No Debounce API key configured"), nil
	}
	email := lead(in, "email")
	if email == "" || !hasSub(email, "@") {
		return fail("No email to verify"), nil
	}
	out, err := d.run(ctx, c, k, email, in)
	return finish(ctx, out, err)
}

func (debounce) run(ctx context.Context, c *http.Client, k, email string, in providers.Input) (providers.Output, error) {
	resp, err := providers.Do(ctx, c, providers.Req{Method: http.MethodGet, URL: "https://api.debounce.io/v1/",
		Query: [][2]string{{"api", k}, {"email", email}}}, k)
	if err != nil {
		return providers.Output{}, err
	}
	if resp.Status != 200 {
		return fail(fmt.Sprintf("Debounce API error: %d", resp.Status)), nil
	}
	body, err := providers.ParseJSON(resp.Body)
	if err != nil {
		return providers.Output{}, err
	}
	data, err := getd(body, "debounce", map[string]any{})
	if err != nil {
		return providers.Output{}, err
	}
	g := func(name string, def any) any { v, _ := providers.PyGet(data, name, def); return v }
	if _, err := providers.PyGet(data, "x", nil); err != nil {
		return providers.Output{}, err
	}
	f := newFields()
	f.Set("email", email)

	// Python compares with str literals: a non-string code never matches.
	code, _ := g("code", "").(string)
	var conf float64
	switch code {
	case "5":
		f.Set("email_verify", "valid")
		conf = 0.95
	case "6", "7":
		f.Set("email_verify", "risky")
		conf = 0.60
	case "1", "2":
		f.Set("email_verify", "invalid")
		conf = 0.10
	default:
		f.Set("email_verify", "unknown")
		conf = 0.40
	}
	if g("free_email", "false") == "true" {
		f.Set("email_type", "free_provider")
	}
	if g("disposable_email", "false") == "true" {
		f.Set("email_type", "disposable")
	}
	if g("role", "false") == "true" {
		f.Set("email_role", "role_based")
	}
	return success(in, f, conf), nil
}
