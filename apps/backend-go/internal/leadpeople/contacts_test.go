package leadpeople

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
)

type discCall struct {
	Provider      string  `json:"provider"`
	Company       string  `json:"company"`
	Website       string  `json:"website"`
	ContactPerson string  `json:"contact_person"`
	ContactTitle  string  `json:"contact_title"`
	LinkedinURL   string  `json:"linkedin_url"`
	WorkspaceID   string  `json:"workspace_id"`
	Timeout       float64 `json:"timeout"`
}

type fakeDisc struct {
	script map[string]any
	mu     sync.Mutex
	calls  []discCall
}

func (f *fakeDisc) Discover(_ context.Context, ws, provider string, lead map[string]any, timeout time.Duration) (*DiscoveryResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, discCall{provider, lead["company"].(string), lead["website"].(string), lead["contact_person"].(string),
		lead["contact_title"].(string), lead["linkedin_url"].(string), lead["workspace_id"].(string), timeout.Seconds()})
	f.mu.Unlock()
	key := provider + "|" + lead["contact_person"].(string)
	v, ok := f.script[key]
	if !ok {
		v = f.script[provider]
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	if kind, ok := m["raise"].(string); ok {
		switch kind {
		case "timeout":
			return nil, context.DeadlineExceeded
		case "value":
			return nil, classErr{"ValueError"}
		}
		return nil, classErr{"RuntimeError"}
	}
	r := &DiscoveryResult{}
	r.Success, _ = m["success"].(bool)
	r.Fields, _ = m["fields"].(map[string]any)
	r.Confidence, _ = m["confidence"].(float64)
	r.DurationMS, _ = m["duration_ms"].(float64)
	r.Error, _ = m["error"].(string)
	r.License, _ = m["license"].(string)
	return r, nil
}

type verifyCall struct {
	Email       string `json:"email"`
	WorkspaceID string `json:"workspace_id"`
}

type fakeVerify struct {
	script map[string]any
	calls  []verifyCall
}

func (f *fakeVerify) Verify(_ context.Context, ws, email string) (emailverify.DeliverabilityResult, error) {
	f.calls = append(f.calls, verifyCall{email, ws})
	v, ok := f.script[email]
	if !ok {
		v = f.script["*"]
	}
	m := v.(map[string]any)
	if _, boom := m["raise"]; boom {
		return emailverify.DeliverabilityResult{}, classErr{"RuntimeError"}
	}
	r := emailverify.DeliverabilityResult{Email: email}
	r.Confidence, _ = m["confidence"].(string)
	r.Status, _ = m["status"].(string)
	r.Source, _ = m["source"].(string)
	r.VerificationConf, _ = m["verification_confidence"].(float64)
	r.IsRole, _ = m["is_role"].(bool)
	r.IsFree, _ = m["is_free"].(bool)
	r.IsDisposable, _ = m["is_disposable"].(bool)
	for _, a := range asMaps(anyList(m["attempts"])) { // non-dict entries are dropped, as in Python
		at := emailverify.Attempt{}
		at.Provider, _ = a["provider"].(string)
		at.Status, _ = a["status"].(string)
		at.Detail, _ = a["detail"].(string)
		r.VerificationAttempts = append(r.VerificationAttempts, at)
	}
	return r, nil
}

// stepClock moves 250ms per read, like the recorder's fake time.monotonic.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(250 * time.Millisecond)
	return c.t
}

func TestContactsVectors(t *testing.T) {
	var cases []struct {
		Name            string         `json:"name"`
		Company         string         `json:"company"`
		Function        string         `json:"function"`
		People          []any          `json:"people"`
		Resolution      map[string]any `json:"resolution"`
		PersonIDs       []string       `json:"person_ids"`
		WorkspaceID     string         `json:"workspace_id"`
		ActionID        string         `json:"action_id"`
		ProviderTimeout float64        `json:"provider_timeout"`
		Discovery       map[string]any `json:"discovery"`
		Verify          map[string]any `json:"verify"`
		Calls           []discCall     `json:"calls"`
		VerifyCalls     []verifyCall   `json:"verify_calls"`
		Result          any            `json:"result"`
	}
	loadVec(t, "contacts.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			disc, ver := &fakeDisc{script: c.Discovery, calls: []discCall{}}, &fakeVerify{script: c.Verify, calls: []verifyCall{}}
			clk := &stepClock{t: time.Unix(100, 0)}
			s := &Service{Discover: disc, Verify: ver, Spend: &fakeContactSpend{}, Now: func() time.Time { return fixedNow }, Mono: clk.now, Concurrency: 1}
			got, err := s.EnrichPeopleContacts(context.Background(), c.Company, c.Function, asMaps(c.People), ContactOptions{
				CompanyResolution: c.Resolution, PersonIDs: c.PersonIDs, WorkspaceID: c.WorkspaceID, ActionID: c.ActionID,
				ProviderTimeout: time.Duration(c.ProviderTimeout * float64(time.Second)),
			})
			if err != nil {
				t.Fatal(err)
			}
			sameJSON(t, "result", got, c.Result)
			sameJSON(t, "discovery calls", disc.calls, c.Calls)
			sameJSON(t, "verify calls", ver.calls, c.VerifyCalls)
		})
	}
}

func TestContactsCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{Discover: cancelDisc{cancel}, Verify: &fakeVerify{}, Spend: &fakeContactSpend{}}
	_, err := s.EnrichPeopleContacts(ctx, "Stripe", "Sales", []map[string]any{{"name": "Jane Doe"}},
		ContactOptions{CompanyResolution: map[string]any{"canonical_domain": "stripe.com"}, WorkspaceID: "w"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

type cancelDisc struct{ cancel func() }

func (c cancelDisc) Discover(context.Context, string, string, map[string]any, time.Duration) (*DiscoveryResult, error) {
	c.cancel()
	return nil, context.Canceled
}
