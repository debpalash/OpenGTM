package airtable

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit/kittest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func obj(t *testing.T, raw string) *wbcrud.Obj {
	t.Helper()
	o, err := wbcrud.ParseObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func str(t *testing.T, tr *kittest.Transcript, name string) string {
	var s string
	tr.Arg(t, name, &s)
	return s
}

func runTranscript(t *testing.T, tr *kittest.Transcript) {
	c := New(tr.Client(t), tr)
	ctx := context.Background()
	typecast := true
	tr.Arg(t, "typecast", &typecast)
	var (
		res *wbcrud.Obj
		err error
	)
	switch tr.Call.Function {
	case "push_record":
		res, err = c.PushRecord(ctx, kittest.WS, tr.ObjArg(t, "fields"), str(t, tr, "base_id"), str(t, tr, "table"), typecast)
	case "upsert_record":
		field := DefaultIdempotencyField
		tr.Arg(t, "idempotency_field", &field)
		res, err = c.UpsertRecord(ctx, kittest.WS, tr.ObjArg(t, "fields"), str(t, tr, "base_id"), str(t, tr, "table"), str(t, tr, "idempotency_key"), field, typecast)
	default:
		t.Fatalf("%s: unknown function %s", tr.File, tr.Call.Function)
	}
	if err != nil {
		t.Fatalf("%s: %v", tr.File, err)
	}
	tr.CheckResult(t, res)
	tr.Done(t)
}

func TestAirtablePushTranscripts(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "airtable") {
		if tr.Call.Function != "push_record" {
			continue
		}
		n++
		t.Run(tr.Scenario, func(t *testing.T) { runTranscript(t, tr) })
	}
	if n != 10 {
		t.Fatalf("expected 10 push transcripts, ran %d", n)
	}
}

func TestAirtableUpsertCreatesOrUpdates(t *testing.T) {
	n := 0
	for _, tr := range kittest.Load(t, "airtable") {
		if tr.Call.Function != "upsert_record" {
			continue
		}
		n++
		t.Run(tr.Scenario, func(t *testing.T) { runTranscript(t, tr) })
	}
	if n != 5 {
		t.Fatalf("expected 5 upsert transcripts, ran %d", n)
	}
}

func TestPathSegmentsEscaped(t *testing.T) {
	var seen string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.URL.EscapedPath()
		return kittest.Respond(r, 200, `{"id":"rec1"}`), nil
	})}, kittest.Static{"w": {"AIRTABLE_TOKEN": "tok"}})
	_, err := c.PushRecord(context.Background(), "w", obj(t, `{"a":"b"}`), "app/../x", "t?x#y", true)
	if err != nil {
		t.Fatal(err)
	}
	if seen != "/v0/app%2F..%2Fx/t%3Fx%23y" {
		t.Fatalf("path %q", seen)
	}
}

func TestNoSecretInErrors(t *testing.T) {
	const tok = "ctk_airtable_secret_9999"
	st := kittest.Static{"w": {"AIRTABLE_TOKEN": tok}}
	for name, rt := range map[string]kittest.RoundTripFunc{
		"transport": func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed for Bearer " + tok)
		},
		"echoed body": func(r *http.Request) (*http.Response, error) {
			return kittest.Respond(r, 401, "bad token "+tok), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := New(&http.Client{Transport: rt}, st)
			for _, call := range []func() (*wbcrud.Obj, error){
				func() (*wbcrud.Obj, error) {
					return c.PushRecord(context.Background(), "w", obj(t, `{"a":"b"}`), "app", "tbl", true)
				},
				func() (*wbcrud.Obj, error) {
					return c.UpsertRecord(context.Background(), "w", obj(t, `{"a":"b"}`), "app", "tbl", "k", DefaultIdempotencyField, true)
				},
			} {
				res, err := call()
				if err != nil {
					t.Fatal(err)
				}
				b, _ := res.MarshalJSON()
				if strings.Contains(string(b), tok) {
					t.Fatalf("secret leaked: %s", b)
				}
				if ok, _ := res.Get("success"); ok != false {
					t.Fatalf("want failure, got %s", b)
				}
			}
		})
	}
}

func TestRedirectRefused(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.airtable.com" {
			t.Errorf("followed redirect to %s", r.URL.Host)
		}
		return kittest.Respond(r, 302, "", "Location", "https://evil.example/steal"), nil
	})}, kittest.Static{"w": {"AIRTABLE_TOKEN": "tok"}})
	res, err := c.PushRecord(context.Background(), "w", obj(t, `{"a":"b"}`), "app", "tbl", true)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := res.Get("error"); e != "HTTP 302: " || calls != 1 {
		t.Fatalf("error %v calls %d", e, calls)
	}
}

func TestTenantTokenIsolation(t *testing.T) {
	var auth []string
	c := New(&http.Client{Transport: kittest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		return kittest.Respond(r, 200, `{"id":"rec1"}`), nil
	})}, kittest.Static{"a": {"AIRTABLE_TOKEN": "tok-a"}, "b": {"AIRTABLE_TOKEN": "tok-b"}, "": {}})
	ctx := context.Background()
	for _, ws := range []string{"a", "b"} {
		if _, err := c.PushRecord(ctx, ws, obj(t, `{"x":1}`), "app", "tbl", true); err != nil {
			t.Fatal(err)
		}
	}
	if len(auth) != 2 || auth[0] != "Bearer tok-a" || auth[1] != "Bearer tok-b" {
		t.Fatalf("auth %v", auth)
	}
	// A workspace with no secret never borrows another tenant's token.
	res, _ := c.PushRecord(ctx, "c", obj(t, `{"x":1}`), "app", "tbl", true)
	if e, _ := res.Get("error"); e != "Airtable not connected (set AIRTABLE_TOKEN)" || len(auth) != 2 {
		t.Fatalf("workspace c: %v calls %d", e, len(auth))
	}
	if ok, err := c.IsConnected(ctx, "c"); err != nil || ok {
		t.Fatalf("IsConnected c = %v %v", ok, err)
	}
	if ok, _ := c.IsConnected(ctx, "a"); !ok {
		t.Fatal("IsConnected a false")
	}
	// The global fallback (single-tenant installs) is visible, as in Python.
	g := New(c.http, kittest.Static{"": {"AIRTABLE_TOKEN": "tok-g"}})
	if ok, _ := g.IsConnected(ctx, "c"); !ok {
		t.Fatal("global fallback not used")
	}
}
