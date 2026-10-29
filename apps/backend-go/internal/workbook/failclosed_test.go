package workbook

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Final security Low 9: a runState without an injected client still sends webhooks through the SSRF-guarded
// egress client (never http.DefaultClient, which dials loopback and honours HTTP_PROXY).
func TestWebhookClientWithoutDepsIsGuarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	rs := &runState{}
	resp, err := rs.webhookClient().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("unguarded client reached a loopback server")
	}
}
