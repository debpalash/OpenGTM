// Package lemlist adds a lead to a lemlist (email sequencer) campaign with the workspace's own API key.
//
// Official API docs (confirmed 2026-11):
//   - https://developer.lemlist.com/api-reference/getting-started/authentication.md : HTTP Basic, empty username and
//     the API key as password ("Authorization: Basic base64(":<key>")"); base https://api.lemlist.com/api.
//   - https://developer.lemlist.com/api-reference/getting-started/rate-limits.md : 20 requests per 2 seconds per key,
//     Retry-After and X-RateLimit-* headers.
//   - https://developer.lemlist.com/api-reference/endpoints/leads/create-lead-in-campaign.md :
//     POST /api/campaigns/{campaignId}/leads/ with a flat JSON body (email, firstName, lastName, companyName,
//     jobTitle, linkedinUrl, phone, ...; any other key is a custom variable). 200 returns the lead; errors are
//     text/plain (400 "Lead already in the campaign", 401, 404 "Campaign not found") or JSON {"error": ...}.
//
// The API key is scrubbed from every error text. The lead's email and LinkedIn URL are PII and are never put in an
// error or log line. Fixed host, redirects refused, bounded body (intkit.Send).
package lemlist

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	secretName = "LEMLIST_API_KEY"
	baseURL    = "https://api.lemlist.com/api"
)

// Obj is an insertion-ordered JSON object.
type Obj = intkit.Obj

// topLevel maps accepted lead keys (snake_case or the vendor's camelCase) to the lemlist body fields; every other
// key is sent as is and becomes a custom variable.
var topLevel = map[string]string{
	"email": "email", "first_name": "firstName", "firstName": "firstName", "last_name": "lastName", "lastName": "lastName",
	"company_name": "companyName", "companyName": "companyName", "job_title": "jobTitle", "title": "jobTitle", "jobTitle": "jobTitle",
	"linkedin_url": "linkedinUrl", "linkedin_profile": "linkedinUrl", "linkedinUrl": "linkedinUrl",
	"phone": "phone", "phone_number": "phone", "company_domain": "companyDomain", "companyDomain": "companyDomain",
	"icebreaker": "icebreaker", "timezone": "timezone",
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

// AddLeadToCampaign adds one lead (campaignID is the "cam_..." id, percent-encoded as a path segment).
// Result: {"success", "duplicate"?, "error"?}.
func (c *Client) AddLeadToCampaign(ctx context.Context, ws, campaignID string, lead *Obj) (*Obj, error) {
	key, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return intkit.Fail("lemlist not connected (set LEMLIST_API_KEY)"), nil
	}
	if campaignID == "" {
		return intkit.Fail("lemlist campaign_id is required"), nil
	}
	ev, _ := lead.Get("email")
	if s, _ := ev.(string); providers.PyStrip(s) == "" {
		return intkit.Fail("no_email"), nil
	}

	body := intkit.NewObj()
	for _, k := range lead.Keys() {
		v, _ := lead.Get(k)
		if intkit.Empty(v) {
			continue
		}
		name := k
		if f, ok := topLevel[k]; ok {
			name = f
		}
		if _, dup := body.Get(name); dup {
			continue
		}
		body.Set(name, v)
	}

	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPost,
		URL:    baseURL + "/campaigns/" + intkit.PyQuote(campaignID) + "/leads/",
		Header: append(intkit.JSONHeaders(""), [2]string{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+key))}),
		Body:   intkit.Marshal(body),
	}, key)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, key))), nil
	}
	if resp.Status >= 200 && resp.Status < 300 {
		return success(false), nil
	}
	txt := strings.ToLower(intkit.Text(resp.Body, key))
	// "Lead already in the campaign" is 400 today and 409 from 2026-12-01; the other-campaign 409 is a failure.
	if (resp.Status == 400 || resp.Status == 409) && strings.Contains(txt, "already in the campaign") {
		return success(true), nil
	}
	if resp.Status == 401 {
		return intkit.Fail("lemlist auth failed (invalid LEMLIST_API_KEY)"), nil
	}
	return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, key)), nil
}

func success(dup bool) *Obj {
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("duplicate", dup)
	return res
}
