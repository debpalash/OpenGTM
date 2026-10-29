package contact

import (
	"context"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// numverify ports providers/numverify.py (NumVerifyProvider). The vendor
// endpoint is plain http://apilayer.net (as in Python); the egress client still
// vets the resolved address at dial time.
type numverify struct{}

func (numverify) Name() string     { return "numverify" }
func (numverify) KeyEnv() []string { return []string{"NUMVERIFY_API_KEY"} }

func (n numverify) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	k, ok, err := resolveKey(key)
	if err != nil {
		return providers.Output{}, err
	}
	if !ok {
		return fail("No NumVerify API key configured"), nil
	}
	phone := lead(in, "phone")
	if phone == "" {
		return fail("No phone number to validate"), nil
	}
	clean := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(providers.PyStrip(phone))
	out, err := n.run(ctx, c, k, clean, in)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return providers.Output{}, cerr
		}
		if providers.IsTimeout(err) {
			return fail("NumVerify API timeout"), nil
		}
		return fail(err.Error()), nil // str(e), not truncated in this module
	}
	return out, nil
}

func (numverify) run(ctx context.Context, c *http.Client, k, clean string, in providers.Input) (providers.Output, error) {
	_, data, err := call(ctx, c, providers.Req{Method: http.MethodGet, URL: "http://apilayer.net/api/validate",
		Query: [][2]string{{"access_key", k}, {"number", clean}, {"format", "1"}}}, k)
	if err != nil {
		return providers.Output{}, err
	}
	valid, err := providers.PyGet(data, "valid", nil)
	if err != nil {
		return providers.Output{}, err
	}
	if !providers.Truthy(valid) {
		return fail("Number not valid"), nil
	}
	num, _ := providers.PyGet(data, "international_format", clean)
	carrier, _ := providers.PyGet(data, "carrier", "")
	line, _ := providers.PyGet(data, "line_type", "")
	f := newFields()
	f.Set("phone", num)
	f.Set("phone_carrier", carrier)
	f.Set("phone_line_type", line)
	return success(in, f, 0.90), nil
}
