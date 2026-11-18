package lemlist

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

func TestLemlistTranscripts(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "lemlist") {
		n++
		t.Run(tr.Scenario, func(t *testing.T) {
			c := New(tr.Client(t), tr)
			var camp string
			tr.Arg(t, "campaign_id", &camp)
			res, err := c.AddLeadToCampaign(context.Background(), kittest.WS, camp, tr.ObjArg(t, "lead"))
			if err != nil {
				t.Fatal(err)
			}
			tr.CheckResult(t, res)
			tr.Done(t)
		})
	}
	if n != 12 {
		t.Fatalf("ran %d", n)
	}
}

func TestPathSegmentsEscaped(t *testing.T) {
	var path string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.EscapedPath()
		return kittest.Respond(r, 200, `{}`), nil
	})}, kittest.Static{"w": {"LEMLIST_API_KEY": "k"}})
	if _, err := c.AddLeadToCampaign(context.Background(), "w", "1/../../x?y", lead(t, `{"email":"a@b.co"}`)); err != nil {
		t.Fatal(err)
	}
	if path != "/api/campaigns/1%2F..%2F..%2Fx%3Fy/leads/" {
		t.Fatalf("path %q", path)
	}
}

func TestNoSecretOrLeadInErrors(t *testing.T) {
	const tok = "ctk_lemlist_secret_9999"
	st := kittest.Static{"w": {"LEMLIST_API_KEY": tok}}
	for name, rt := range map[string]kittest.RoundTripFunc{
		"transport":   func(r *http.Request) (*http.Response, error) { return nil, errors.New("dial " + tok + ": refused") },
		"echoed body": func(r *http.Request) (*http.Response, error) { return kittest.Respond(r, 500, "x "+tok), nil },
	} {
		t.Run(name, func(t *testing.T) {
			res, err := New(&http.Client{Transport: rt}, st).AddLeadToCampaign(context.Background(), "w", "cam_1", lead(t, `{"email":"pii@b.co"}`))
			b, _ := res.MarshalJSON()
			if err != nil || strings.Contains(string(b), tok) || strings.Contains(string(b), "pii@b.co") || !strings.Contains(string(b), `"success":false`) {
				t.Fatalf("%v %s", err, b)
			}
		})
	}
}

func TestRedirectRefused(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return kittest.Respond(r, 302, "", "Location", "https://evil.example/"), nil
	})}, kittest.Static{"w": {"LEMLIST_API_KEY": "k"}})
	res, _ := c.AddLeadToCampaign(context.Background(), "w", "cam_1", lead(t, `{"email":"a@b.co"}`))
	if e, _ := res.Get("error"); e != "HTTP 302: " || calls != 1 {
		t.Fatalf("%v %d", e, calls)
	}
}

func TestTenantTokenIsolation(t *testing.T) {
	var auth []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		return kittest.Respond(r, 200, `{}`), nil
	})}, kittest.Static{"a": {"LEMLIST_API_KEY": "ka"}, "b": {"LEMLIST_API_KEY": "kb"}})
	ctx := context.Background()
	_, _ = c.AddLeadToCampaign(ctx, "a", "cam_1", lead(t, `{"email":"a@b.co"}`))
	_, _ = c.AddLeadToCampaign(ctx, "b", "cam_1", lead(t, `{"email":"a@b.co"}`))
	res, _ := c.AddLeadToCampaign(ctx, "c", "cam_1", lead(t, `{"email":"a@b.co"}`))
	if len(auth) != 2 || auth[0] == auth[1] {
		t.Fatalf("%v", auth)
	}
	if e, _ := res.Get("error"); e != "lemlist not connected (set LEMLIST_API_KEY)" {
		t.Fatalf("%v", e)
	}
	if ok, _ := c.IsConnected(ctx, "c"); ok {
		t.Fatal("c")
	}
}
