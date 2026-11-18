package destdeliver

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Attio is a CRM destination: it delivers through the CRM port with the workspace's own token, upserts one person
// by email, and is "not connected" without a workspace token (a global or env value never resolves).
func TestLiveAttioCRM(t *testing.T) {
	var sleeps []time.Duration
	rt := &liveRT{status: []int{200}, bodies: []string{`{"data":{"id":{"record_id":"rec-9"}}}`}}
	p := livePorts(rt, map[string]string{"ATTIO_ACCESS_TOKEN": "at-secret"}, &sleeps)
	d := New(p, 5*time.Second)
	dest := Destination{ID: "d", WorkspaceID: "ws1", Type: "attio", Config: map[string]any{}}
	r := d.Deliver(context.Background(), dest, 1, map[string]any{"email": "a@x.test", "contact_person": "Ada Lovelace"}, "i")
	if !r.Success || r.Summary != "Attio: upserted rec-9" || len(rt.reqs) != 1 {
		t.Fatalf("%+v requests=%d", r, len(rt.reqs))
	}
	if rt.reqs[0].Method != "PUT" || rt.reqs[0].URL.Host != "api.attio.com" || rt.reqs[0].Header.Get("Authorization") != "Bearer at-secret" {
		t.Errorf("request %v %v", rt.reqs[0].Method, rt.reqs[0].URL)
	}
	if !strings.Contains(string(rt.sent[0]), `"full_name":"Ada Lovelace"`) {
		t.Errorf("body %s", rt.sent[0])
	}

	rt = &liveRT{}
	p = livePorts(rt, map[string]string{}, &sleeps)
	r = New(p, 5*time.Second).Deliver(context.Background(), dest, 1, map[string]any{"email": "a@x.test"}, "i")
	if r.Success || r.Error != "Attio not connected" || len(rt.reqs) != 0 {
		t.Fatalf("%+v requests=%d", r, len(rt.reqs))
	}
}
