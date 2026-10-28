package destdeliver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

type liveRT struct {
	mu     sync.Mutex
	status []int
	bodies []string
	reqs   []*http.Request
	sent   [][]byte
}

func (f *liveRT) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs, f.sent = append(f.reqs, r), append(f.sent, b)
	st, body := 200, `{}`
	if len(f.status) > 0 {
		st, f.status = f.status[0], f.status[1:]
		body, f.bodies = f.bodies[0], f.bodies[1:]
	}
	return &http.Response{StatusCode: st, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

type liveStore struct{ ws, global map[string]string }

func (s liveStore) AppSetting(_ context.Context, k string) (string, error) { return s.global[k], nil }
func (s liveStore) WorkspaceSetting(_ context.Context, ws, k string) (string, bool, error) {
	if ws != "ws1" {
		return "", false, nil
	}
	v, ok := s.ws[k]
	return v, ok, nil
}

func livePorts(rt *liveRT, ws map[string]string, sleeps *[]time.Duration) Ports {
	return NewLivePorts(LiveConfig{
		Secrets: secrets.NewResolver(nil, liveStore{ws: ws}, func(string) string { return "env-value" }),
		HTTP:    &http.Client{Transport: rt},
		Sleep: func(ctx context.Context, d time.Duration) error {
			*sleeps = append(*sleeps, d)
			return ctx.Err()
		},
	})
}

func TestLivePortsSupportEveryType(t *testing.T) {
	p := NewLivePorts(LiveConfig{})
	p.Webhook = AutoWebhook{}
	for _, typ := range []string{"webhook", "hubspot", "salesforce", "warehouse_http", "meta_ads", "google_ads", "linkedin_ads",
		"instantly", "smartlead", "google_sheets", "airtable", "slack"} {
		if !p.Supports(typ) {
			t.Errorf("%s unsupported", typ)
		}
	}
}

// A 429 is retried with 1 s, 2 s, 4 s backoff (Python failed the member at once); the fourth 429 stands.
func TestLiveRateLimitBackoff(t *testing.T) {
	var sleeps []time.Duration
	rt := &liveRT{status: []int{429, 429, 200}, bodies: []string{`{"error":"slow down"}`, `{}`, `{"id":"L1"}`}}
	p := livePorts(rt, map[string]string{"INSTANTLY_API_KEY": "k-secret"}, &sleeps)
	res, err := p.Instantly.AddLead(context.Background(), CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "c", "skip_if_in_campaign": true},
		Payload: map[string]any{"email": "a@x.test"}})
	if err != nil || !res.Success || res.ExternalID != "L1" || len(rt.reqs) != 3 {
		t.Fatalf("res=%+v err=%v requests=%d", res, err, len(rt.reqs))
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Errorf("sleeps %v", sleeps)
	}

	sleeps = nil
	rt = &liveRT{status: []int{429, 429, 429, 429, 429}, bodies: []string{"a", "b", "c", "d k-secret", "e"}}
	p = livePorts(rt, map[string]string{"AIRTABLE_TOKEN": "k-secret"}, &sleeps)
	res, err = p.Airtable.Upsert(context.Background(), RowUpsert{WorkspaceID: "ws1", Payload: map[string]any{"Email": "a@x.test"},
		Config: map[string]any{"base_id": "b", "table": "t", "key": "k1", "idempotency_field": "OpenGTM ID", "typecast": true}})
	if err != nil || res.Success || len(rt.reqs) != 4 || len(sleeps) != 3 || sleeps[2] != 4*time.Second {
		t.Fatalf("res=%+v err=%v requests=%d sleeps=%v", res, err, len(rt.reqs), sleeps)
	}
	if strings.Contains(res.Error, "k-secret") || !strings.HasPrefix(res.Error, "HTTP 429:") {
		t.Errorf("error text %q", res.Error)
	}

	// A cancelled context stops the backoff with the 429 result.
	sleeps = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt = &liveRT{status: []int{429}, bodies: []string{"x"}}
	p = livePorts(rt, map[string]string{"GOOGLE_SHEETS_TOKEN": "t"}, &sleeps)
	res, _ = p.Sheets.Upsert(ctx, RowUpsert{WorkspaceID: "ws1", Config: map[string]any{"spreadsheet_id": "s", "key": "k", "range": "A:B"}})
	if res.Success || len(rt.reqs) > 1 {
		t.Errorf("cancelled: %+v after %d requests", res, len(rt.reqs))
	}
}

// Nested payload objects reach the clients as objects: custom_variables merge (Python merged a dict), Smartlead
// settings are sent as given, and a non-object settings value is not sent.
func TestLiveNestedPayloads(t *testing.T) {
	var sleeps []time.Duration
	rt := &liveRT{}
	p := livePorts(rt, map[string]string{"INSTANTLY_API_KEY": "k", "SMARTLEAD_API_KEY": "s"}, &sleeps)
	ctx := context.Background()
	if _, err := p.Instantly.AddLead(ctx, CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "c", "skip_if_in_campaign": false},
		Payload: map[string]any{"email": "a@x.test", "custom_variables": map[string]any{"tier": "gold"}, "city": "Austin"}}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(rt.sent[0], &body)
	cv, _ := body["custom_variables"].(map[string]any)
	if cv["tier"] != "gold" || cv["city"] != "Austin" || body["skip_if_in_campaign"] != false {
		t.Errorf("instantly body %s", rt.sent[0])
	}
	for _, settings := range []any{map[string]any{"ignore_global_block_list": true}, "yes"} {
		rt.sent = nil
		if _, err := p.Smartlead.AddLead(ctx, CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "7", "settings": settings},
			Payload: map[string]any{"email": "a@x.test"}}); err != nil {
			t.Fatal(err)
		}
		has := bytes.Contains(rt.sent[0], []byte(`"settings":{"ignore_global_block_list":true}`))
		if _, isMap := settings.(map[string]any); has != isMap {
			t.Errorf("settings %v: body %s", settings, rt.sent[0])
		}
	}
}

// A global or environment credential is never used (the resolver's env answers "env-value" for every key).
func TestLiveWorkspaceOnlyCredentials(t *testing.T) {
	var sleeps []time.Duration
	rt := &liveRT{}
	p := livePorts(rt, nil, &sleeps)
	ctx := context.Background()
	res := p.CRM.Push(ctx, "ws1", map[string]any{"type": "salesforce"}, map[string]any{"email": "a@x.test"}, "", 1)
	if res.Success || res.Error != "Salesforce not connected" {
		t.Errorf("crm %+v", res)
	}
	r, _ := p.Smartlead.AddLead(ctx, CampaignRequest{WorkspaceID: "ws1", Config: map[string]any{"campaign_id": "7"}, Payload: map[string]any{"email": "a@x.test"}})
	if r.Success || r.Error != "Smartlead not connected (set SMARTLEAD_API_KEY)" {
		t.Errorf("smartlead %+v", r)
	}
	if len(rt.reqs) != 0 {
		t.Errorf("%d requests", len(rt.reqs))
	}
}
