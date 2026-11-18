// Package heyreach adds a lead to a HeyReach (LinkedIn outreach) campaign with the workspace's own API key.
//
// Official API docs (confirmed 2026-11):
//   - https://docs.heyreach.io/hey-reach-api.md : base URL https://api.heyreach.io, header "X-API-KEY", limit of
//     300 requests per minute, a 429 status and an error body beyond it.
//   - https://docs.heyreach.io/hey-reach-api/campaigns/add-leads-to-campaign-v-2.md and
//     https://documenter.getpostman.com/view/23808049/2sA2xb5F75 (AddLeadsToCampaignV2):
//     POST /api/public/campaign/AddLeadsToCampaignV2, body {campaignId, accountLeadPairs:[{linkedInAccountId,
//     lead:{...}}], resumeFinishedCampaign, resumePausedCampaign}; 200 {addedLeadsCount, updatedLeadsCount,
//     failedLeadsCount}; 400 and 404 {errorMessage}; 401 and 429 an RFC 7807 style {type,title,status,detail,instance}.
//
// The API key rides in a header and is scrubbed from every error text. The lead's LinkedIn URL and email are PII and
// are never put in an error or log line. Fixed host, redirects refused, bounded body (intkit.Send).
package heyreach

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	secretName = "HEYREACH_API_KEY"
	baseURL    = "https://api.heyreach.io"
	addPath    = "/api/public/campaign/AddLeadsToCampaignV2"
)

// Obj is an insertion-ordered JSON object.
type Obj = intkit.Obj

// leadFields maps the accepted lead keys (snake_case or the vendor's camelCase) to the vendor lead fields. Every
// other key becomes a customUserFields entry.
var leadFields = map[string]string{
	"first_name": "firstName", "firstName": "firstName",
	"last_name": "lastName", "lastName": "lastName",
	"location": "location", "summary": "summary", "about": "about",
	"company_name": "companyName", "companyName": "companyName",
	"position": "position", "job_title": "position", "title": "position",
	"email": "emailAddress", "email_address": "emailAddress", "emailAddress": "emailAddress",
	"linkedin_url": "profileUrl", "linkedin_profile": "profileUrl", "profile_url": "profileUrl", "profileUrl": "profileUrl",
}

// customName rewrites a key to HeyReach's custom field rule: only alphanumerics and underscores.
var customName = regexp.MustCompile(`[^A-Za-z0-9_]`)

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

func num(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil { // a JSON number in the destination config reads as text like "235.0"
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil || f != math.Trunc(f) || f > 1<<53 {
			return 0, false
		}
		n = int64(f)
	}
	return n, n > 0
}

// AddLeadToCampaign adds one lead. campaignID and linkedInAccountID are the integers HeyReach issues (as text).
// Result: {"success", "duplicate"?, "error"?}; an updated (already known) lead reads as a duplicate.
func (c *Client) AddLeadToCampaign(ctx context.Context, ws, campaignID, linkedInAccountID string, lead *Obj) (*Obj, error) {
	key, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return intkit.Fail("HeyReach not connected (set HEYREACH_API_KEY)"), nil
	}
	cid, ok := num(campaignID)
	if !ok {
		return intkit.Fail("HeyReach campaign_id must be a number"), nil
	}
	aid, ok := num(linkedInAccountID)
	if !ok {
		return intkit.Fail("HeyReach linkedin_account_id must be a number"), nil
	}

	entry := intkit.NewObj()
	var custom []any
	for _, k := range lead.Keys() {
		v, _ := lead.Get(k)
		if intkit.Empty(v) {
			continue
		}
		if f, ok := leadFields[k]; ok {
			if _, dup := entry.Get(f); !dup {
				entry.Set(f, intkit.Str(v))
			}
			continue
		}
		cf := intkit.NewObj()
		cf.Set("name", customName.ReplaceAllString(k, "_"))
		cf.Set("value", intkit.Str(v))
		custom = append(custom, cf)
	}
	pu, _ := entry.Get("profileUrl")
	if s, _ := pu.(string); providers.PyStrip(s) == "" {
		return intkit.Fail("no_linkedin_url"), nil
	}
	if len(custom) > 0 {
		entry.Set("customUserFields", custom)
	}
	pair := intkit.NewObj()
	pair.Set("linkedInAccountId", aid)
	pair.Set("lead", entry)
	body := intkit.NewObj()
	body.Set("campaignId", cid)
	body.Set("accountLeadPairs", []any{pair})
	body.Set("resumeFinishedCampaign", false)
	body.Set("resumePausedCampaign", false)

	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPost,
		URL:    baseURL + addPath,
		Header: append(intkit.JSONHeaders(""), [2]string{"X-API-KEY", key}),
		Body:   intkit.Marshal(body),
	}, key)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, key))), nil
	}
	if resp.Status >= 200 && resp.Status < 300 {
		return outcome(resp.Body), nil
	}
	if resp.Status == 401 {
		return intkit.Fail("HeyReach auth failed (invalid HEYREACH_API_KEY)"), nil
	}
	return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, key)), nil
}

func count(data map[string]any, f string) int {
	switch n := data[f].(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// outcome reads the 200 counts. A lead the vendor failed (and neither added nor updated) is a failure; an updated
// lead was already in HeyReach and reads as a duplicate.
func outcome(body []byte) *Obj {
	v, err := intkit.ParseJSON(body)
	data, ok := v.(map[string]any)
	if err != nil || !ok {
		data = map[string]any{}
	}
	added, updated, failed := count(data, "addedLeadsCount"), count(data, "updatedLeadsCount"), count(data, "failedLeadsCount")
	if failed > 0 && added+updated == 0 {
		return intkit.Fail("HeyReach rejected lead")
	}
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("duplicate", updated > 0 && added == 0)
	return res
}
