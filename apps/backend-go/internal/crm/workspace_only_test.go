package crm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Final security Low 5: CRM credentials are the workspace's own by default. A workspace without its own token
// never uses the operator's (global app setting or environment): it is not connected and sends nothing.
// ALLOW_GLOBAL_CREDENTIAL_FALLBACK=true opts a single-tenant self-host back into the global chain.
func TestHubSpotCredentialsWorkspaceOnlyByDefault(t *testing.T) {
	ctx := context.Background()
	st := newStore()
	res, env := newResolver(t, st, map[string]string{"HUBSPOT_TOKEN": "env-token"})
	st.app["HUBSPOT_TOKEN"] = "global-token"
	var seen []string
	h := &HubSpot{HTTP: &http.Client{Transport: authHeaderRT(&seen, 200, `{}`)}, Secrets: res}
	if h.IsConnected(ctx, "ws-x") {
		t.Fatal("connected on the operator's token")
	}
	h.TestConnection(ctx, "ws-x")
	if len(seen) != 0 {
		t.Fatalf("sent with the operator's token: %v", seen)
	}
	withWorkspaceToken(t, env, st, "ws-x", "ws-token")
	if !h.IsConnected(ctx, "ws-x") {
		t.Fatal("workspace token not used")
	}
	if (&HubSpot{}).IsConnected(ctx, "ws-x") {
		t.Fatal("resolver-less client fell back to the environment")
	}
	t.Setenv("ALLOW_GLOBAL_CREDENTIAL_FALLBACK", "true")
	if !h.IsConnected(ctx, "ws-other") {
		t.Fatal("opt-in fallback not honoured")
	}
}

func TestSalesforceCredentialsWorkspaceOnlyByDefault(t *testing.T) {
	ctx := context.Background()
	st := newStore()
	res, _ := newResolver(t, st, map[string]string{
		"SALESFORCE_INSTANCE_URL": "https://env.my.salesforce.com", "SALESFORCE_ACCESS_TOKEN": "env-token"})
	var seen []string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Host)
		return &http.Response{StatusCode: 204, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	if s.IsConnected(ctx, "ws-x") {
		t.Fatal("connected on the operator's credentials")
	}
	s.UpdateRecord(ctx, "ws-x", "Contact", "1", FieldMap{{"Phone", "1"}})
	if len(seen) != 0 {
		t.Fatalf("sent with the operator's credentials: %v", seen)
	}
	t.Setenv("ALLOW_GLOBAL_CREDENTIAL_FALLBACK", "true")
	if !s.IsConnected(ctx, "ws-x") {
		t.Fatal("opt-in fallback not honoured")
	}
}
