package rpclocal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// recRT records every outgoing request and answers 404.
type recRT struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (r *recRT) RoundTrip(q *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, q)
	r.mu.Unlock()
	return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")),
		Header: http.Header{}, Request: q}, nil
}

func (r *recRT) seen() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*http.Request(nil), r.reqs...)
}

// opSecrets is global operator config only: a workspace lookup of an operator setting finds nothing.
type opSecrets struct {
	global map[string]string
	mu     sync.Mutex
	wsAsk  []string
}

func (s *opSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	if ws != "" {
		s.mu.Lock()
		s.wsAsk = append(s.wsAsk, key)
		s.mu.Unlock()
		return "", "", nil
	}
	return s.global[key], "env", nil
}

// M3: Go providers served by rpclocal run through providerrun, so the operator settings that
// providerrun fills (TECH_STACK_*, SEC_EDGAR_USER_AGENT, MCA_RESOURCE_ID) are honoured.
func TestLocalGoProvidersGetOperatorSettings(t *testing.T) {
	rt := &recRT{}
	sec := &opSecrets{global: map[string]string{
		"TECH_STACK_WEBSITE_FETCH_ENABLED": "true", "TECH_STACK_RESPECT_ROBOTS": "false",
		"SEC_EDGAR_USER_AGENT": "OpsTeam ops@operator.test", "MCA_RESOURCE_ID": "operator-resource-id",
	}}
	c := New(Deps{Local: only("tech_stack", "sec_edgar", "mca_registry"), HTTP: &http.Client{Transport: rt}, Secrets: sec})
	ctx := context.Background()

	res, err := c.Call(ctx, call("tech_stack", "t1", `{"company":"Acme","website":"https://93.184.216.34"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Output)+res.Error, "website_fetch_disabled") {
		t.Errorf("tech_stack ignored TECH_STACK_WEBSITE_FETCH_ENABLED: %+v %s", res, res.Output)
	}

	if _, err := c.Call(ctx, call("sec_edgar", "s1", `{"company":"Acme Robotics Inc"}`)); err != nil {
		t.Fatal(err)
	}
	var ua string
	for _, q := range rt.seen() {
		if strings.Contains(q.URL.Host, "sec.gov") {
			ua = q.Header.Get("User-Agent")
		}
	}
	if ua != "OpsTeam ops@operator.test" {
		t.Errorf("sec_edgar User-Agent = %q, want the operator's SEC_EDGAR_USER_AGENT", ua)
	}

	if _, err := c.Call(ctx, call("mca_registry", "m1", `{"company":"Acme Robotics Private Limited"}`)); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range rt.seen() {
		if strings.Contains(q.URL.String(), "operator-resource-id") {
			found = true
		}
	}
	if !found {
		t.Errorf("mca_registry never used MCA_RESOURCE_ID")
	}
	for _, k := range sec.wsAsk {
		if strings.HasPrefix(k, "TECH_STACK_") || k == "SEC_EDGAR_USER_AGENT" || k == "MCA_RESOURCE_ID" {
			t.Errorf("operator setting %s resolved from workspace config", k)
		}
	}
}

type fakeRunner struct {
	calls []string
	out   providers.Output
}

func (f *fakeRunner) RunLocal(_ context.Context, ws, provider string, in providers.Input, timeout time.Duration) (providers.Output, error) {
	f.calls = append(f.calls, ws+"/"+provider)
	return f.out, nil
}

// M3: the Go-provider path is providerrun.Runner.RunLocal (stats, cache, semaphore, settings).
func TestLocalGoProvidersUseRunner(t *testing.T) {
	obj := providers.NewObj()
	obj.Set("company_size", "11-50")
	fr := &fakeRunner{out: providers.Output{Extra: obj, Confidence: 0.6}}
	c := New(Deps{Local: only("tech_stack"), Runner: fr})
	res, err := c.Call(context.Background(), call("tech_stack", "r1", `{"company":"Acme"}`))
	if err != nil || res.Status != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "ws1/tech_stack" {
		t.Fatalf("runner calls %v", fr.calls)
	}
	var m map[string]any
	_ = json.Unmarshal(res.Output, &m)
	if m["success"] != true {
		t.Fatalf("output %s", res.Output)
	}
	// A runner timeout is the RPC's timeout status.
	fr.out = providers.Output{Error: "timeout"}
	res, err = c.Call(context.Background(), call("tech_stack", "r2", `{"company":"Acme"}`))
	if err != nil || res.Status != "timeout" {
		t.Fatalf("timeout: %+v %v", res, err)
	}
}
