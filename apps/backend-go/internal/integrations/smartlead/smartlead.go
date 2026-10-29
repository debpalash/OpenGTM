// Package smartlead ports services/integrations/smartlead.py: add a workbook
// row as a lead in a Smartlead campaign. The API key rides in the query string
// (api_key), so every transport error is scrubbed of it. Fixed host, redirects
// refused, 10 s timeout, 1 MiB body bound. The campaign id is percent-encoded
// as a path segment (Python concatenated it raw; D4 hardening).
package smartlead

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	secretName = "SMARTLEAD_API_KEY"
	baseURL    = "https://server.smartlead.ai/api/v1"
)

// Obj is an insertion-ordered JSON object.
type Obj = intkit.Obj

// topLevel are the lead_list entry fields Smartlead accepts; every other lead
// key goes into custom_fields.
var topLevel = map[string]bool{
	"email": true, "first_name": true, "last_name": true, "company_name": true, "phone_number": true,
	"website": true, "location": true, "linkedin_profile": true, "company_url": true,
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

// duplicateCount is _duplicate_count: the sum of the three count fields that
// are numbers (a bool counts as an int, a float is truncated).
func duplicateCount(data map[string]any) int {
	total := 0
	for _, f := range []string{"already_added_to_campaign", "duplicate_count", "skipped_count"} {
		switch v := data[f].(type) {
		case bool:
			if v {
				total++
			}
		case json.Number:
			if n, ok := providers.Num(mustNum(v)); ok {
				total += int(n)
			}
		case int64, int, float64:
			n, _ := providers.Num(v)
			total += int(n)
		}
	}
	return total
}

func mustNum(n json.Number) any {
	v, _ := providers.ParseJSON([]byte(n))
	return v
}

func hasDup(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "already") || strings.Contains(s, "duplicate")
}

// AddLeadToCampaign adds one lead; unknown lead keys become custom_fields.
// settings (optional) is sent as-is. Result: {"success", "duplicate"?, "error"?}.
func (c *Client) AddLeadToCampaign(ctx context.Context, ws, campaignID string, lead, settings *Obj) (*Obj, error) {
	key, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return intkit.Fail("Smartlead not connected (set SMARTLEAD_API_KEY)"), nil
	}
	if campaignID == "" {
		return intkit.Fail("Smartlead campaign_id is required"), nil
	}
	ev, _ := lead.Get("email")
	email := ""
	if providers.Truthy(ev) {
		email = intkit.Str(ev)
	}
	if providers.PyStrip(email) == "" {
		return intkit.Fail("no_email"), nil
	}

	entry := intkit.NewObj()
	custom := intkit.NewObj()
	if cf, ok := lead.Get("custom_fields"); ok && providers.Truthy(cf) {
		if o, ok := cf.(*Obj); ok {
			for _, k := range o.Keys() {
				v, _ := o.Get(k)
				custom.Set(k, v)
			}
		}
	}
	for _, k := range lead.Keys() {
		v, _ := lead.Get(k)
		if k == "custom_fields" || intkit.Empty(v) {
			continue
		}
		if topLevel[k] {
			entry.Set(k, v)
		} else {
			custom.Set(k, v)
		}
	}
	if custom.Len() > 0 {
		entry.Set("custom_fields", custom)
	}
	body := intkit.NewObj()
	body.Set("lead_list", []any{entry})
	if settings != nil && settings.Len() > 0 {
		body.Set("settings", settings)
	}

	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPost,
		URL:    baseURL + "/campaigns/" + intkit.PyQuote(campaignID) + "/leads",
		Query:  [][2]string{{"api_key", key}},
		Header: intkit.JSONHeaders(""),
		Body:   intkit.Marshal(body),
	}, key)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, key))), nil
	}
	if resp.Status >= 200 && resp.Status < 300 {
		return outcome(resp.Body, key), nil
	}
	txt := intkit.Text(resp.Body, key)
	if hasDup(txt) {
		return success(true), nil
	}
	if resp.Status == 401 {
		return intkit.Fail("Smartlead auth failed (invalid SMARTLEAD_API_KEY)"), nil
	}
	return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, key)), nil
}

func success(dup bool) *Obj {
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("duplicate", dup)
	return res
}

func outcome(body []byte, key string) *Obj {
	v, err := intkit.ParseJSON(body)
	data, ok := v.(map[string]any)
	if err != nil || !ok {
		data = map[string]any{} // non-JSON or non-object 2xx bodies count as {}
	}
	// Smartlead reports duplicates inside a 200 body through counts.
	if duplicateCount(data) > 0 {
		return success(true)
	}
	if data["ok"] == false || data["success"] == false {
		var msg any = "smartlead rejected lead"
		for _, f := range []string{"message", "error"} {
			if x := data[f]; providers.Truthy(x) {
				msg = x
				break
			}
		}
		text := providers.Scrub(providers.Str(msg), key)
		if hasDup(text) {
			return success(true)
		}
		return intkit.Fail(providers.TruncRunes(text, 200))
	}
	return success(false)
}
