package destdeliver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type capAdder struct {
	req []CampaignRequest
	res Result
}

func (c *capAdder) AddLead(_ context.Context, q CampaignRequest) (Result, error) {
	c.req = append(c.req, q)
	return c.res, nil
}

// HeyReach and lemlist deliver through the campaign path: the config is narrowed to the keys each client reads, the
// summaries match the other sequencers and neither returns an external id.
func TestDeliverHeyReachLemlist(t *testing.T) {
	hr, ll := &capAdder{res: Result{Success: true}}, &capAdder{res: Result{Success: true, Duplicate: true}}
	d := New(Ports{HeyReach: hr, Lemlist: ll}, 5*time.Second)
	snap := map[string]any{"linkedin_url": "https://www.linkedin.com/in/x", "email": "a@x.test"}

	r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "heyreach",
		Config: map[string]any{"campaign_id": float64(235), "linkedin_account_id": "123", "ignored": 1}}, 1, snap, "i")
	if !r.Success || r.Summary != "Lead added to campaign" || r.ExternalID != "" || r.Error != "" {
		t.Fatalf("heyreach %+v", r)
	}
	if len(hr.req) != 1 || hr.req[0].Config["linkedin_account_id"] != "123" || hr.req[0].Config["campaign_id"] != "235.0" || len(hr.req[0].Config) != 2 {
		t.Fatalf("heyreach request %+v", hr.req)
	}

	r = d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "lemlist",
		Config: map[string]any{"campaign_id": "cam_1"}}, 1, snap, "i")
	if !r.Success || r.Summary != "Lead already in campaign" || r.ExternalID != "" {
		t.Fatalf("lemlist %+v", r)
	}
	if len(ll.req) != 1 || ll.req[0].Config["campaign_id"] != "cam_1" || len(ll.req[0].Config) != 1 {
		t.Fatalf("lemlist request %+v", ll.req)
	}

	// An unwired port is "not configured", never a panic or a fallback.
	for _, typ := range []string{"heyreach", "lemlist"} {
		r = New(Ports{}, time.Second).Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: typ, Config: map[string]any{}}, 1, snap, "i")
		if r.Success || !strings.Contains(r.Error, "is not configured") {
			t.Errorf("%s unwired: %+v", typ, r)
		}
	}
}

func TestPortsSupportHeyReachLemlist(t *testing.T) {
	p := Ports{HeyReach: &capAdder{}}
	if !p.Supports("heyreach") || p.Supports("lemlist") {
		t.Fatal("Supports must follow the wired port")
	}
	if err := (Ports{Lemlist: &capAdder{}}).ValidateTypes([]string{"lemlist"}); err != nil {
		t.Fatal(err)
	}
	if err := (Ports{}).ValidateTypes([]string{"heyreach"}); err == nil {
		t.Fatal("heyreach without a port must be rejected")
	}
}

// The live adapters use the workspace key, retry a vendor 429 with the shared backoff and never leak the key.
func TestLiveHeyReachLemlist(t *testing.T) {
	var sleeps []time.Duration
	rt := &liveRT{status: []int{429, 200}, bodies: []string{`{"title":"Too Many Requests"}`, `{"addedLeadsCount":1,"updatedLeadsCount":0,"failedLeadsCount":0}`}}
	p := livePorts(rt, map[string]string{"HEYREACH_API_KEY": "hr-secret", "LEMLIST_API_KEY": "ll-secret"}, &sleeps)
	ctx := context.Background()
	res, err := p.HeyReach.AddLead(ctx, CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "235", "linkedin_account_id": "123"},
		Payload: map[string]any{"linkedin_url": "https://www.linkedin.com/in/x", "plan": "gold"}})
	if err != nil || !res.Success || len(rt.reqs) != 2 || len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("heyreach res=%+v err=%v requests=%d sleeps=%v", res, err, len(rt.reqs), sleeps)
	}
	if rt.reqs[1].Header.Get("X-API-KEY") != "hr-secret" || rt.reqs[1].URL.Host != "api.heyreach.io" {
		t.Errorf("heyreach request %v %v", rt.reqs[1].URL, rt.reqs[1].Header)
	}
	var body map[string]any
	_ = json.Unmarshal(rt.sent[1], &body)
	if body["campaignId"] != float64(235) {
		t.Errorf("body %v", body)
	}

	rt = &liveRT{status: []int{429, 429, 429, 429}, bodies: []string{"a", "b", "c", "slow ll-secret"}}
	p = livePorts(rt, map[string]string{"LEMLIST_API_KEY": "ll-secret"}, &sleeps)
	res, err = p.Lemlist.AddLead(ctx, CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "cam_1"}, Payload: map[string]any{"email": "a@x.test"}})
	if err != nil || res.Success || len(rt.reqs) != 4 || strings.Contains(res.Error, "ll-secret") || !strings.HasPrefix(res.Error, "HTTP 429:") {
		t.Fatalf("lemlist res=%+v err=%v requests=%d", res, err, len(rt.reqs))
	}
	if rt.reqs[0].URL.Host != "api.lemlist.com" || !strings.HasPrefix(rt.reqs[0].Header.Get("Authorization"), "Basic ") {
		t.Errorf("lemlist request %v", rt.reqs[0].URL)
	}

	// No workspace key: both fail "not connected" without a request.
	rt = &liveRT{}
	p = livePorts(rt, map[string]string{}, &sleeps)
	for name, ad := range map[string]CampaignAdder{"HeyReach": p.HeyReach, "lemlist": p.Lemlist} {
		r, _ := ad.AddLead(ctx, CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "1", "linkedin_account_id": "2"}, Payload: map[string]any{"email": "a@x.test"}})
		if r.Success || !strings.HasPrefix(r.Error, name+" not connected") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if len(rt.reqs) != 0 {
		t.Errorf("requests without a key: %d", len(rt.reqs))
	}
}
