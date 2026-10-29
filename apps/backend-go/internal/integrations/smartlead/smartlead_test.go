package smartlead

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit/kittest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func lead(t *testing.T, raw string) *wbcrud.Obj {
	t.Helper()
	o, err := wbcrud.ParseObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestSmartleadTranscripts(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "smartlead") {
		n++
		t.Run(tr.Scenario, func(t *testing.T) {
			c := New(tr.Client(t), tr)
			var camp string
			tr.Arg(t, "campaign_id", &camp)
			res, err := c.AddLeadToCampaign(context.Background(), kittest.WS, camp, tr.ObjArg(t, "lead"), tr.ObjArg(t, "settings"))
			if err != nil {
				t.Fatal(err)
			}
			tr.CheckResult(t, res)
			tr.Done(t)
		})
	}
	if n != 15 {
		t.Fatalf("ran %d", n)
	}
}

func TestSmartleadDuplicateCount(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{}`, 0},
		{`{"already_added_to_campaign":2}`, 2},
		{`{"already_added_to_campaign":1,"duplicate_count":2.9,"skipped_count":3}`, 6},
		{`{"duplicate_count":"4"}`, 0},
		{`{"skipped_count":true}`, 1}, // bool is an int in Python
		{`{"skipped_count":null,"duplicate_count":-1}`, -1},
	}
	for _, c := range cases {
		o := lead(t, c.body)
		m := map[string]any{}
		for _, k := range o.Keys() {
			v, _ := o.Get(k)
			m[k] = v
		}
		if got := duplicateCount(m); got != c.want {
			t.Errorf("%s: %d want %d", c.body, got, c.want)
		}
	}
}

func TestPathSegmentsEscaped(t *testing.T) {
	var path string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.EscapedPath()
		return kittest.Respond(r, 200, `{"ok":true}`), nil
	})}, kittest.Static{"w": {"SMARTLEAD_API_KEY": "k"}})
	if _, err := c.AddLeadToCampaign(context.Background(), "w", "1/../../x?y", lead(t, `{"email":"a@b.co"}`), nil); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/campaigns/1%2F..%2F..%2Fx%3Fy/leads" {
		t.Fatalf("path %q", path)
	}
}

func TestNoSecretInErrors(t *testing.T) {
	const tok = "ctk_smartlead_secret_9999"
	st := kittest.Static{"w": {"SMARTLEAD_API_KEY": tok}}
	for name, rt := range map[string]kittest.RoundTripFunc{
		// the key rides in the query string, so a transport error carries it
		"transport": func(r *http.Request) (*http.Response, error) {
			return nil, errors.New(`Post "` + r.URL.String() + `": connection refused`)
		},
		"echoed body": func(r *http.Request) (*http.Response, error) { return kittest.Respond(r, 500, "x "+tok), nil },
		"echoed message": func(r *http.Request) (*http.Response, error) {
			return kittest.Respond(r, 200, `{"ok":false,"message":"bad key `+tok+`"}`), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := New(&http.Client{Transport: rt}, st).AddLeadToCampaign(context.Background(), "w", "7", lead(t, `{"email":"a@b.co"}`), nil)
			b, _ := res.MarshalJSON()
			if err != nil || strings.Contains(string(b), tok) || !strings.Contains(string(b), `"success":false`) {
				t.Fatalf("%v %s", err, b)
			}
		})
	}
}

func TestRedirectRefused(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return kittest.Respond(r, 302, "", "Location", "https://evil.example/?api_key=1"), nil
	})}, kittest.Static{"w": {"SMARTLEAD_API_KEY": "k"}})
	res, _ := c.AddLeadToCampaign(context.Background(), "w", "7", lead(t, `{"email":"a@b.co"}`), nil)
	if e, _ := res.Get("error"); e != "HTTP 302: " || calls != 1 {
		t.Fatalf("%v %d", e, calls)
	}
}

func TestTenantTokenIsolation(t *testing.T) {
	var keys []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		keys = append(keys, r.URL.Query().Get("api_key"))
		return kittest.Respond(r, 200, `{"ok":true}`), nil
	})}, kittest.Static{"a": {"SMARTLEAD_API_KEY": "ka"}, "b": {"SMARTLEAD_API_KEY": "kb"}})
	ctx := context.Background()
	c.AddLeadToCampaign(ctx, "a", "7", lead(t, `{"email":"a@b.co"}`), nil)
	c.AddLeadToCampaign(ctx, "b", "7", lead(t, `{"email":"a@b.co"}`), nil)
	res, _ := c.AddLeadToCampaign(ctx, "c", "7", lead(t, `{"email":"a@b.co"}`), nil)
	if len(keys) != 2 || keys[0] != "ka" || keys[1] != "kb" {
		t.Fatalf("%v", keys)
	}
	if e, _ := res.Get("error"); e != "Smartlead not connected (set SMARTLEAD_API_KEY)" {
		t.Fatalf("%v", e)
	}
	if ok, _ := c.IsConnected(ctx, "c"); ok {
		t.Fatal("c")
	}
}
