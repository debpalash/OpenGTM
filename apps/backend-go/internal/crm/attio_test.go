package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const attioToken = "ctk_attio_alpha_0001"

func loadAttioTranscripts(t *testing.T) []*transcript {
	t.Helper()
	files, err := filepath.Glob("../../testdata/vendors/crm/attio__*.json")
	if err != nil || len(files) < 15 {
		t.Fatalf("attio transcripts missing: %d %v", len(files), err)
	}
	var out []*transcript
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tr transcript
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		tr.file = filepath.Base(f)
		out = append(out, &tr)
	}
	return out
}

func withAttioToken(t *testing.T, st *fakeStore, ws, token string) *Attio {
	t.Helper()
	res, env := newResolver(t, st, nil)
	ct, err := env.Encrypt(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	st.ws[ws+"/ATTIO_ACCESS_TOKEN"] = ct
	return &Attio{Secrets: res}
}

func TestAttioTranscripts(t *testing.T) {
	for _, tr := range loadAttioTranscripts(t) {
		t.Run(strings.TrimSuffix(tr.file, ".json"), func(t *testing.T) {
			st := newStore()
			res, env := newResolver(t, st, nil)
			if tok := tr.Secrets["ATTIO_ACCESS_TOKEN"]; tok != "" {
				ct, err := env.Encrypt(context.Background(), tok)
				if err != nil {
					t.Fatal(err)
				}
				st.ws[testWS+"/ATTIO_ACCESS_TOKEN"] = ct
			}
			rp := &replay{t: t, tr: tr}
			sl := &sleepRec{}
			a := &Attio{HTTP: &http.Client{Transport: rp}, Secrets: res, Sleep: sl.fn}

			var args struct {
				Lead     map[string]any  `json:"lead"`
				FieldMap json.RawMessage `json:"field_map"`
			}
			if err := json.NewDecoder(bytes.NewReader(tr.Call.Args)).Decode(&args); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(args.Lead)
			v, err := providers.ParseJSON(b)
			if err != nil {
				t.Fatal(err)
			}
			got := a.PushLeadAsPerson(context.Background(), testWS, mapLead(v.(map[string]any)), decodeFieldMap(t, args.FieldMap))
			if g, w := canon(t, got), canon(t, json.RawMessage(tr.Result)); g != w {
				t.Errorf("result\n got %s\nwant %s", strings.ReplaceAll(g, attioToken, "***"), w)
			}
			if rp.next != len(tr.Exchanges) {
				t.Errorf("served %d of %d recorded exchanges", rp.next, len(tr.Exchanges))
			}
			if len(sl.d) != len(tr.Sleeps) {
				t.Fatalf("sleeps %v want %v", sl.d, tr.Sleeps)
			}
			for i := range sl.d {
				if math.Abs(sl.d[i]-tr.Sleeps[i]) > 1e-9 {
					t.Errorf("sleep %d = %v want %v", i, sl.d[i], tr.Sleeps[i])
				}
			}
		})
	}
}

// Every error text is free of the token and of the lead's email, whatever the failure.
func TestAttioNoSecretOrPIIInErrors(t *testing.T) {
	const email = "pii.person@example.com"
	lead := mapLead{"email": email, "contact_person": "Pia Person"}
	for name, rt := range map[string]attioRT{
		"transport": func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial " + attioToken + " " + email + ": refused")
		},
		"echo": func(r *http.Request) (*http.Response, error) {
			return attioRespond(r, 400, `{"status_code":400,"type":"invalid_request_error","code":"validation_type","message":"bad `+email+` Pia Person `+attioToken+`"}`), nil
		},
		"plain echo": func(r *http.Request) (*http.Response, error) {
			return attioRespond(r, 500, "oops "+email+" "+attioToken), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := withAttioToken(t, newStore(), testWS, attioToken)
			a.HTTP = &http.Client{Transport: rt}
			a.Sleep = (&sleepRec{}).fn
			res := a.PushLeadAsPerson(context.Background(), testWS, lead, nil)
			if res.Success || strings.Contains(res.Error, attioToken) || strings.Contains(res.Error, email) || strings.Contains(res.Error, "Pia Person") {
				t.Fatalf("%+v", res)
			}
		})
	}
}

type attioRT func(*http.Request) (*http.Response, error)

func (f attioRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func attioRespond(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
		Body: io.NopCloser(strings.NewReader(body))}
}

func TestAttioRedirectRefusedAndTenantIsolation(t *testing.T) {
	calls := 0
	st := newStore()
	a := withAttioToken(t, st, "wa", "token-a")
	_ = withAttioToken(t, st, "wb", "token-b")
	var auth []string
	a.HTTP = &http.Client{Transport: attioRT(func(r *http.Request) (*http.Response, error) {
		calls++
		auth = append(auth, r.Header.Get("Authorization"))
		resp := attioRespond(r, 302, "")
		resp.Header.Set("Location", "https://evil.example/")
		return resp, nil
	})}
	lead := mapLead{"email": "a@b.co"}
	r1 := a.PushLeadAsPerson(context.Background(), "wa", lead, nil)
	_ = a.PushLeadAsPerson(context.Background(), "wb", lead, nil)
	r3 := a.PushLeadAsPerson(context.Background(), "wc", lead, nil)
	if r1.Success || calls != 2 || auth[0] != "Bearer token-a" || auth[1] != "Bearer token-b" {
		t.Fatalf("calls=%d auth=%v r1=%+v", calls, auth, r1)
	}
	if r3.Error != "Attio not connected (set ATTIO_ACCESS_TOKEN)" || a.IsConnected(context.Background(), "wc") {
		t.Fatalf("%+v", r3)
	}
}

// The upsert is one PUT matched on email, so a redelivery cannot create a second person.
func TestAttioUpsertIsAMatchedPut(t *testing.T) {
	var method, query, path string
	a := withAttioToken(t, newStore(), testWS, attioToken)
	a.HTTP = &http.Client{Transport: attioRT(func(r *http.Request) (*http.Response, error) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		return attioRespond(r, 200, `{"data":{"id":{"record_id":"r1"}}}`), nil
	})}
	for i := 0; i < 2; i++ {
		if res := a.PushLeadAsPerson(context.Background(), testWS, mapLead{"email": "a@b.co"}, nil); !res.Success || res.AttioID != "r1" {
			t.Fatalf("%+v", res)
		}
	}
	if method != "PUT" || path != "/v2/objects/people/records" || query != "matching_attribute=email_addresses" {
		t.Fatalf("%s %s?%s", method, path, query)
	}
}

func TestAttioHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.Acme.com/x?y=1": "acme.com", "acme.com": "acme.com", "WWW.Foo.io": "foo.io", "sub.acme.co.uk:8080/p": "sub.acme.co.uk",
		"": "", "localhost": "", "not a domain": "", "http://": "", ".com": "",
	} {
		if got := attioDomain(in); got != want {
			t.Errorf("attioDomain(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"+14155550100": "+14155550100", " +44 7777 777777 ": "+44 7777 777777", "4155550100": "", "+1": "", "+1 (555) 010-0101": "+1 (555) 010-0101", "+44abc1234567": "",
	} {
		if got := attioPhone(in); got != want {
			t.Errorf("attioPhone(%q)=%q want %q", in, got, want)
		}
	}
	if attioName("  ") != nil || attioName("Cher") != "Cher" || attioName("Smith, John") == nil {
		t.Error("attioName")
	}
}

// A Retry-After HTTP-date (Attio) is honoured; a past or unparsable value never waits longer than the cap.
func TestRetryDelayHTTPDate(t *testing.T) {
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if d := retryDelay(future, 0); d < 1500*time.Millisecond || d > 3*time.Second {
		t.Errorf("future date delay %v", d)
	}
	if d := retryDelay("Tue, 23 May 2023 14:42:01 GMT", 0); d != 0 {
		t.Errorf("past date delay %v", d)
	}
	if d := retryDelay(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), 0); d != MaxRetrySleep {
		t.Errorf("far date delay %v", d)
	}
	if d := retryDelay("garbage", 2); d != 4*time.Second {
		t.Errorf("fallback %v", d)
	}
}
