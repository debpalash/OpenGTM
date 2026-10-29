package instantly

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

func run(t *testing.T, tr *kittest.Transcript) {
	c := New(tr.Client(t), tr)
	var camp string
	skip := true
	tr.Arg(t, "campaign_id", &camp)
	tr.Arg(t, "skip_if_in_campaign", &skip)
	res, err := c.AddLeadToCampaign(context.Background(), kittest.WS, camp, tr.ObjArg(t, "lead"), skip)
	if err != nil {
		t.Fatalf("%s: %v", tr.File, err)
	}
	tr.CheckResult(t, res)
	tr.Done(t)
}

func runAll(t *testing.T, only func(*kittest.Transcript) bool, want int) {
	n := 0
	for _, tr := range kittest.Load(t, "instantly") {
		if only(tr) {
			n++
			t.Run(tr.Scenario, func(t *testing.T) { run(t, tr) })
		}
	}
	if n != want {
		t.Fatalf("ran %d want %d", n, want)
	}
}

func TestInstantlyTopLevelFieldsAndCustom(t *testing.T) {
	runAll(t, func(tr *kittest.Transcript) bool {
		return tr.Scenario == "add_created" || tr.Scenario == "add_custom_variables_merge" || tr.Scenario == "add_no_key" ||
			tr.Scenario == "add_no_campaign" || tr.Scenario == "add_no_email"
	}, 5)
}

func TestInstantlyDuplicateDetection(t *testing.T) {
	runAll(t, func(tr *kittest.Transcript) bool {
		switch tr.Scenario {
		case "add_duplicate_text_409", "add_skipped_status", "add_2xx_non_json", "add_401", "add_422", "add_429", "add_5xx":
			return true
		}
		return false
	}, 7)
	for text, want := range map[string]bool{"Lead ALREADY EXISTS": true, "already in campaign": true, "Duplicate!": true, "fine": false, "": false} {
		if got := looksLikeDuplicate(text); got != want {
			t.Errorf("looksLikeDuplicate(%q) = %v", text, got)
		}
	}
}

func TestPathSegmentsEscaped(t *testing.T) {
	// The campaign id is a JSON body field here, never a URL segment.
	var path string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.String()
		return kittest.Respond(r, 200, `{"id":"l1"}`), nil
	})}, kittest.Static{"w": {"INSTANTLY_API_KEY": "k"}})
	if _, err := c.AddLeadToCampaign(context.Background(), "w", "../x", lead(t, `{"email":"a@b.co"}`), true); err != nil {
		t.Fatal(err)
	}
	if path != "https://api.instantly.ai/api/v2/leads" {
		t.Fatalf("url %q", path)
	}
}

func TestNoSecretInErrors(t *testing.T) {
	const tok = "ctk_instantly_secret_9999"
	st := kittest.Static{"w": {"INSTANTLY_API_KEY": tok}}
	for name, rt := range map[string]kittest.RoundTripFunc{
		"transport":   func(r *http.Request) (*http.Response, error) { return nil, errors.New("failed Bearer " + tok) },
		"echoed body": func(r *http.Request) (*http.Response, error) { return kittest.Respond(r, 500, "x "+tok), nil },
	} {
		t.Run(name, func(t *testing.T) {
			res, err := New(&http.Client{Transport: rt}, st).AddLeadToCampaign(context.Background(), "w", "c", lead(t, `{"email":"a@b.co"}`), true)
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
		return kittest.Respond(r, 301, "", "Location", "https://evil.example/"), nil
	})}, kittest.Static{"w": {"INSTANTLY_API_KEY": "k"}})
	res, _ := c.AddLeadToCampaign(context.Background(), "w", "c", lead(t, `{"email":"a@b.co"}`), true)
	if e, _ := res.Get("error"); e != "HTTP 301: " || calls != 1 {
		t.Fatalf("%v %d", e, calls)
	}
}

func TestTenantTokenIsolation(t *testing.T) {
	var auth []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		return kittest.Respond(r, 200, `{"id":"l"}`), nil
	})}, kittest.Static{"a": {"INSTANTLY_API_KEY": "ka"}, "b": {"INSTANTLY_API_KEY": "kb"}})
	ctx := context.Background()
	c.AddLeadToCampaign(ctx, "a", "c", lead(t, `{"email":"a@b.co"}`), true)
	c.AddLeadToCampaign(ctx, "b", "c", lead(t, `{"email":"a@b.co"}`), true)
	res, _ := c.AddLeadToCampaign(ctx, "c", "c", lead(t, `{"email":"a@b.co"}`), true)
	if len(auth) != 2 || auth[0] != "Bearer ka" || auth[1] != "Bearer kb" {
		t.Fatalf("%v", auth)
	}
	if e, _ := res.Get("error"); e != "Instantly not connected (set INSTANTLY_API_KEY)" {
		t.Fatalf("%v", e)
	}
	if ok, _ := c.IsConnected(ctx, "a"); !ok {
		t.Fatal("a")
	}
	if ok, _ := c.IsConnected(ctx, "c"); ok {
		t.Fatal("c")
	}
}
