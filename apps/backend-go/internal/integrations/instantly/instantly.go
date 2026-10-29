// Package instantly ports services/integrations/instantly.py: add a workbook
// row as a lead in an Instantly campaign (API v2, Bearer key). Fixed host,
// redirects refused, 10 s timeout, 1 MiB body bound; the key never appears in a
// result or error. The campaign id travels in the JSON body, not the URL.
package instantly

import (
	"context"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	secretName = "INSTANTLY_API_KEY"
	apiURL     = "https://api.instantly.ai/api/v2/leads"
)

// Obj is an insertion-ordered JSON object.
type Obj = intkit.Obj

// topLevel are the body fields Instantly accepts at the top level; every other
// lead key goes into custom_variables.
var topLevel = map[string]bool{
	"email": true, "first_name": true, "last_name": true, "company_name": true,
	"personalization": true, "website": true, "phone": true, "job_title": true,
}

// Client holds the egress client and the credential source.
type Client struct {
	http    *http.Client
	secrets intkit.Secrets
}

// New returns a Client; httpc must be the egress (SSRF-safe) client.
func New(httpc *http.Client, s intkit.Secrets) *Client { return &Client{http: httpc, secrets: s} }

// IsConnected reports whether a key resolves for the workspace.
func (c *Client) IsConnected(ctx context.Context, ws string) (bool, error) {
	k, err := intkit.Key(ctx, c.secrets, ws, secretName)
	return k != "", err
}

// looksLikeDuplicate is _looks_like_duplicate.
func looksLikeDuplicate(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "already exists") || strings.Contains(t, "already in") || strings.Contains(t, "duplicate")
}

// AddLeadToCampaign creates one lead; unknown lead keys become
// custom_variables. Result: {"success", "lead_id"?, "duplicate"?, "error"?}.
func (c *Client) AddLeadToCampaign(ctx context.Context, ws, campaignID string, lead *Obj, skipIfInCampaign bool) (*Obj, error) {
	key, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return intkit.Fail("Instantly not connected (set INSTANTLY_API_KEY)"), nil
	}
	if campaignID == "" {
		return intkit.Fail("Instantly campaign_id is required"), nil
	}
	ev, _ := lead.Get("email")
	if providers.Truthy(ev) {
		ev = intkit.Str(ev)
	} else {
		ev = ""
	}
	if providers.PyStrip(ev.(string)) == "" {
		return intkit.Fail("no_email"), nil
	}

	body := intkit.NewObj()
	body.Set("campaign", campaignID)
	body.Set("skip_if_in_campaign", skipIfInCampaign)
	custom := intkit.NewObj()
	if cv, ok := lead.Get("custom_variables"); ok && providers.Truthy(cv) {
		if o, ok := cv.(*Obj); ok {
			for _, k := range o.Keys() {
				v, _ := o.Get(k)
				custom.Set(k, v)
			}
		}
	}
	for _, k := range lead.Keys() {
		v, _ := lead.Get(k)
		if k == "custom_variables" || intkit.Empty(v) {
			continue
		}
		if topLevel[k] {
			body.Set(k, v)
		} else {
			custom.Set(k, v)
		}
	}
	if custom.Len() > 0 {
		body.Set("custom_variables", custom)
	}

	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPost, URL: apiURL, Header: intkit.JSONHeaders(key), Body: intkit.Marshal(body),
	}, key)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, key))), nil
	}
	if resp.Status >= 200 && resp.Status < 300 {
		res, err := created(resp.Body)
		if err != nil {
			return intkit.Fail(providers.Trunc200(intkit.ErrText(err, key))), nil
		}
		return res, nil
	}
	// A "lead already exists / in campaign" rejection means the desired end
	// state holds: success.
	if looksLikeDuplicate(intkit.Text(resp.Body, key)) {
		res := intkit.NewObj()
		res.Set("success", true)
		res.Set("duplicate", true)
		return res, nil
	}
	if resp.Status == 401 {
		return intkit.Fail("Instantly auth failed (invalid INSTANTLY_API_KEY)"), nil
	}
	return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, key)), nil
}

func created(body []byte) (*Obj, error) {
	data, err := intkit.ParseJSON(body)
	if err != nil {
		data = nil // `except Exception: data = {}`
	}
	m, isMap := data.(map[string]any)
	if !isMap && providers.Truthy(data) {
		// (data or {}).get(...) on a non-empty list/str/number
		_, err := providers.PyGet(data, "id", nil)
		return nil, err
	}
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("lead_id", m["id"])
	// Lead status -3 = Skipped (skip_if_in_campaign hit).
	if n, ok := providers.Num(m["status"]); ok && isMap && n == -3 {
		if _, isBool := m["status"].(bool); !isBool {
			res.Set("duplicate", true)
			return res, nil
		}
	}
	res.Set("duplicate", false)
	return res, nil
}
