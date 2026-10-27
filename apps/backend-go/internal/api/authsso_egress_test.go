package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// The SSO OIDC client reaches a private-address IdP only when SSO_EGRESS_ALLOW_HOSTS names it;
// the general EGRESS_ALLOW_HOSTS (Deps.EgressAllowHosts) does not widen it and nothing is allowed
// by default.
func TestSSOHTTPClientEgressAllowHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")

	get := func(d Deps) error {
		resp, err := ssoHTTPClient(d).Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(Deps{}); err == nil {
		t.Fatal("private IdP reached with no allow-list")
	}
	if err := get(Deps{EgressAllowHosts: []string{hostPort}}); err == nil {
		t.Fatal("EGRESS_ALLOW_HOSTS must not widen the SSO client")
	}
	var f config.Flags
	f.Governance.SSOEgressAllowHosts = " , other.internal, " + hostPort
	if err := get(Deps{Flags: f}); err != nil {
		t.Fatalf("SSO_EGRESS_ALLOW_HOSTS entry refused: %v", err)
	}
}
