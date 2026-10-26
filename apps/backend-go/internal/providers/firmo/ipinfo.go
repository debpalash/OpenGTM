package firmo

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// lookupIPv4 is socket.gethostbyname: the first IPv4 address of host. Tests
// replace it with the transcript DNS exchanges.
var lookupIPv4 = func(ctx context.Context, host string) (string, error) {
	if host == "" {
		return "0.0.0.0", nil // gethostbyname("") is INADDR_ANY
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return ip.String(), nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", &net.DNSError{Err: "no such host", Name: host}
	}
	return ips[0].String(), nil
}

// ipinfo ports providers/ipinfo.py (IPInfoProvider).
type ipinfo struct{}

func (ipinfo) Name() string     { return "ipinfo" }
func (ipinfo) KeyEnv() []string { return []string{"IPINFO_TOKEN"} }

func (ipinfo) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	token, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	if token == "" {
		return fail("No IPInfo token configured"), nil
	}
	website := lead(in, "website")
	if website == "" {
		return fail("No website to resolve"), nil
	}
	clean := strings.ReplaceAll(strings.ReplaceAll(website, "https://", ""), "http://", "")
	clean, _, _ = strings.Cut(clean, "/")
	ip, derr := lookupIPv4(ctx, providers.PyStrip(clean))
	if cerr := ctx.Err(); cerr != nil {
		return providers.Output{}, cerr
	}
	if derr != nil || ip == "" {
		return fail("Could not resolve " + website), nil
	}
	out, err := ipinfoQuery(ctx, c, token, ip, in)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return providers.Output{}, cerr
		}
		if providers.IsTimeout(err) {
			return fail("IPInfo API timeout"), nil
		}
		return fail(err.Error()), nil
	}
	return out, nil
}

func ipinfoQuery(ctx context.Context, c *http.Client, token, ip string, in providers.Input) (providers.Output, error) {
	_, data, err := fetchJSON(ctx, c, providers.Req{Method: http.MethodGet, URL: "https://ipinfo.io/" + ip,
		Header: [][2]string{{"Authorization", "Bearer " + token}}}, token)
	if err != nil {
		return providers.Output{}, err
	}
	org, err := get(data, "org")
	if err != nil {
		return providers.Output{}, err
	}
	company, _ := get(data, "company")
	if !providers.Truthy(org) && !providers.Truthy(company) {
		return fail("No org data for IP"), nil
	}
	// company_info = data.get("company", {}); company_info.get("name", ...)
	ci := any(map[string]any{})
	if m, ok := data.(map[string]any); ok {
		if v, has := m["company"]; has {
			ci = v
		}
	}
	if _, err := dict(ci); err != nil {
		return providers.Output{}, err
	}
	city, _ := get(data, "city")
	region, _ := get(data, "region")
	f := newFields()
	if providers.Truthy(orStr(city)) {
		f.Set("city", city)
	}
	if providers.Truthy(orStr(region)) {
		f.Set("state", region)
	}
	if f.Len() == 0 {
		return fail("No usable geo data for IP"), nil
	}
	return success(in, f, 0.70), nil
}

// orStr is `v` where a missing key defaulted to "" (nil stands in for it).
func orStr(v any) any {
	if v == nil {
		return ""
	}
	return v
}
