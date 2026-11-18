package heyreach

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

func TestHeyReachTranscripts(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "heyreach") {
		n++
		t.Run(tr.Scenario, func(t *testing.T) {
			c := New(tr.Client(t), tr)
			var camp, acct string
			tr.Arg(t, "campaign_id", &camp)
			tr.Arg(t, "linkedin_account_id", &acct)
			res, err := c.AddLeadToCampaign(context.Background(), kittest.WS, camp, acct, tr.ObjArg(t, "lead"))
			if err != nil {
				t.Fatal(err)
			}
			tr.CheckResult(t, res)
			tr.Done(t)
		})
	}
	if n != 11 {
		t.Fatalf("ran %d", n)
	}
}

func TestNoSecretOrLeadInErrors(t *testing.T) {
	const tok = "ctk_heyreach_secret_9999"
	st := kittest.Static{"w": {"HEYREACH_API_KEY": tok}}
	for name, rt := range map[string]kittest.RoundTripFunc{
		"transport":   func(r *http.Request) (*http.Response, error) { return nil, errors.New("dial " + tok + ": refused") },
		"echoed body": func(r *http.Request) (*http.Response, error) { return kittest.Respond(r, 500, "x "+tok), nil },
	} {
		t.Run(name, func(t *testing.T) {
			res, err := New(&http.Client{Transport: rt}, st).AddLeadToCampaign(context.Background(), "w", "1", "2",
				lead(t, `{"linkedin_url":"https://www.linkedin.com/in/pii-person"}`))
			b, _ := res.MarshalJSON()
			if err != nil || strings.Contains(string(b), tok) || strings.Contains(string(b), "pii-person") || !strings.Contains(string(b), `"success":false`) {
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
	})}, kittest.Static{"w": {"HEYREACH_API_KEY": "k"}})
	res, _ := c.AddLeadToCampaign(context.Background(), "w", "1", "2", lead(t, `{"linkedin_url":"https://www.linkedin.com/in/x"}`))
	if e, _ := res.Get("error"); e != "HTTP 302: " || calls != 1 {
		t.Fatalf("%v %d", e, calls)
	}
}

func TestTenantTokenIsolation(t *testing.T) {
	var keys []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		keys = append(keys, r.Header.Get("X-API-KEY"))
		return kittest.Respond(r, 200, `{"addedLeadsCount":1}`), nil
	})}, kittest.Static{"a": {"HEYREACH_API_KEY": "ka"}, "b": {"HEYREACH_API_KEY": "kb"}})
	ctx := context.Background()
	l := lead(t, `{"linkedin_url":"https://www.linkedin.com/in/x"}`)
	_, _ = c.AddLeadToCampaign(ctx, "a", "1", "2", l)
	_, _ = c.AddLeadToCampaign(ctx, "b", "1", "2", l)
	res, _ := c.AddLeadToCampaign(ctx, "c", "1", "2", l)
	if len(keys) != 2 || keys[0] != "ka" || keys[1] != "kb" {
		t.Fatalf("%v", keys)
	}
	if e, _ := res.Get("error"); e != "HeyReach not connected (set HEYREACH_API_KEY)" {
		t.Fatalf("%v", e)
	}
	if ok, _ := c.IsConnected(ctx, "c"); ok {
		t.Fatal("c")
	}
}

func TestNumericIDs(t *testing.T) {
	for in, want := range map[string]bool{"235": true, " 7 ": true, "235.0": true, "0": false, "-1": false, "1.5": false, "x": false, "": false} {
		if _, ok := num(in); ok != want {
			t.Errorf("num(%q) ok=%v want %v", in, ok, want)
		}
	}
}
