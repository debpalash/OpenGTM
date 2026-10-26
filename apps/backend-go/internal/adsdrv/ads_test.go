package adsdrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func decodeNum(t testing.TB, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t testing.TB, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decodeNum(t, raw, v)
}

func TestHashesMatchVectors(t *testing.T) {
	var v struct {
		HashEmail []struct {
			Input any    `json:"input"`
			Hash  string `json:"hash"`
		} `json:"hash_email"`
		HashPhone []struct {
			Input any    `json:"input"`
			Hash  string `json:"hash"`
		} `json:"hash_phone"`
		Hashed []struct {
			Snapshot    map[string]any    `json:"snapshot"`
			Identifiers map[string]string `json:"identifiers"`
		} `json:"hashed_identifiers"`
	}
	readJSON(t, "testdata/hashing.json", &v)
	if len(v.HashEmail) < 20 || len(v.HashPhone) < 20 || len(v.Hashed) < 10 {
		t.Fatalf("vectors missing: %d %d %d", len(v.HashEmail), len(v.HashPhone), len(v.Hashed))
	}
	for i, c := range v.HashEmail {
		if got := hashEmailAny(c.Input); got != c.Hash {
			t.Errorf("hash_email[%d] %#v: got %q want %q", i, c.Input, got, c.Hash)
		}
	}
	for i, c := range v.HashPhone {
		if got := hashPhoneAny(c.Input); got != c.Hash {
			t.Errorf("hash_phone[%d] %#v: got %q want %q", i, c.Input, got, c.Hash)
		}
	}
	for i, c := range v.Hashed {
		if got := HashedIdentifiers(c.Snapshot); !reflect.DeepEqual(got, c.Identifiers) {
			t.Errorf("hashed_identifiers[%d] %#v: got %v want %v", i, c.Snapshot, got, c.Identifiers)
		}
	}
	if HashEmail("A@B.CO") != "80305c9bb1bb2480e03894350e0a8a366dcbdeb302e69e0817aa0743abd77054" || HashPhone("+1 (512) 555-0014") == "" {
		t.Fatal("string helpers")
	}
}

func TestIdentifiersSupportedByPlatform(t *testing.T) {
	var v struct {
		Cases []struct {
			Type        string            `json:"type"`
			Identifiers map[string]string `json:"identifiers"`
			Supported   bool              `json:"supported"`
		} `json:"identifiers_supported"`
	}
	readJSON(t, "testdata/hashing.json", &v)
	if len(v.Cases) < 30 {
		t.Fatal("vectors missing")
	}
	for i, c := range v.Cases {
		if got := IdentifiersSupported(c.Type, c.Identifiers); got != c.Supported {
			t.Errorf("case %d %s %v: got %v", i, c.Type, c.Identifiers, got)
		}
	}
}

func TestFailureSummariesMatchVectors(t *testing.T) {
	var v struct {
		Google []struct {
			Body   any     `json:"body"`
			Result *string `json:"result"`
		} `json:"google"`
		LinkedIn []struct {
			Body     any     `json:"body"`
			Expected int     `json:"expected"`
			Result   *string `json:"result"`
		} `json:"linkedin"`
	}
	readJSON(t, "testdata/failures.json", &v)
	for i, c := range v.Google {
		got, ok := googlePartialFailure(c.Body)
		if c.Result == nil && ok || c.Result != nil && (!ok || got != *c.Result) {
			t.Errorf("google[%d] %#v: got %q,%v want %v", i, c.Body, got, ok, c.Result)
		}
	}
	for i, c := range v.LinkedIn {
		got, ok := linkedinBatchFailure(c.Body, c.Expected)
		if c.Result == nil && ok || c.Result != nil && (!ok || got != *c.Result) {
			t.Errorf("linkedin[%d] %#v: got %q,%v want %v", i, c.Body, got, ok, c.Result)
		}
	}
}

// --- transcript replay --------------------------------------------------------------------

type transcript struct {
	Platform  string            `json:"platform"`
	Operation string            `json:"operation"`
	Scenario  string            `json:"scenario"`
	Config    map[string]any    `json:"config"`
	Secrets   map[string]string `json:"secrets"`
	Snapshots []map[string]any  `json:"input_snapshots"`
	Exchanges []struct {
		Request struct {
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Form    map[string]any    `json:"form"`
			JSON    any               `json:"json"`
		} `json:"request"`
		Response struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    any               `json:"body"`
		} `json:"response"`
	} `json:"http_exchanges"`
	Result *struct {
		Success    bool    `json:"success"`
		Summary    string  `json:"summary"`
		Error      *string `json:"error"`
		ExternalID *string `json:"external_id"`
	} `json:"result"`
	Raises string `json:"raises"`
}

func loadTranscripts(t testing.TB, prefix string) map[string]*transcript {
	t.Helper()
	files, err := filepath.Glob("../../testdata/transcripts/ads/" + prefix + "*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no transcripts for %q: %v", prefix, err)
	}
	out := map[string]*transcript{}
	for _, f := range files {
		var tr transcript
		readJSON(t, f, &tr)
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = &tr
	}
	return out
}

func secretsOf(m map[string]string) SecretGetter {
	return func(_ context.Context, _, name string) (string, error) { return m[name], nil }
}

func subSecret(s string, secrets map[string]string) string {
	for k, v := range secrets {
		s = strings.ReplaceAll(s, "{{secret:"+k+"}}", v)
	}
	return s
}

func pathOf(t testing.TB, raw string) string {
	t.Helper()
	i := strings.Index(raw, "://")
	rest := raw[i+3:]
	return rest[strings.Index(rest, "/"):]
}

// replay serves the transcript's exchanges in order and records every deviation.
func replay(t *testing.T, tr *transcript) (*httptest.Server, *int32) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(tr.Exchanges) {
			t.Errorf("unrouted request %s %s", r.Method, r.URL)
			w.WriteHeader(599)
			return
		}
		ex := tr.Exchanges[i]
		if r.Method != ex.Request.Method {
			t.Errorf("req %d: method %s want %s", i, r.Method, ex.Request.Method)
		}
		if got, want := r.URL.RequestURI(), pathOf(t, ex.Request.URL); got != want {
			t.Errorf("req %d: path %s want %s", i, got, want)
		}
		for k, v := range ex.Request.Headers {
			if got := r.Header.Get(k); got != subSecret(v, tr.Secrets) {
				t.Errorf("req %d: header %s = %q want %q", i, k, got, subSecret(v, tr.Secrets))
			}
		}
		raw, _ := io.ReadAll(r.Body)
		switch {
		case ex.Request.Form != nil:
			form := map[string]string{}
			vals, _ := parseForm(string(raw))
			for k, v := range vals {
				form[k] = v
			}
			if len(form) != len(ex.Request.Form) {
				t.Errorf("req %d: form keys %v", i, form)
			}
			for k, want := range ex.Request.Form {
				got, ok := form[k]
				if !ok {
					t.Errorf("req %d: form field %s missing", i, k)
					continue
				}
				if s, isStr := want.(string); isStr {
					if got != subSecret(s, tr.Secrets) {
						t.Errorf("req %d: form %s mismatch", i, k)
					}
					continue
				}
				var gotV any
				decodeNum(t, []byte(got), &gotV)
				if !reflect.DeepEqual(gotV, want) {
					t.Errorf("req %d: form %s = %s want %v", i, k, got, want)
				}
			}
		case ex.Request.JSON != nil:
			var gotV any
			decodeNum(t, raw, &gotV)
			if !reflect.DeepEqual(gotV, ex.Request.JSON) {
				t.Errorf("req %d: json body %s want %v", i, raw, ex.Request.JSON)
			}
		}
		for k, v := range ex.Response.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(ex.Response.Status)
		switch b := ex.Response.Body.(type) {
		case string:
			_, _ = w.Write([]byte(b))
		case nil:
		default:
			_ = json.NewEncoder(w).Encode(b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func parseForm(raw string) (map[string]string, error) {
	vals, err := url.ParseQuery(raw)
	out := map[string]string{}
	for k, v := range vals {
		out[k] = v[0]
	}
	return out, err
}

func clientFor(srv *httptest.Server, secrets map[string]string) Client {
	c := NewClient(secretsOf(secrets), "test", []string{"127.0.0.1"})
	c.Endpoints = Endpoints{Meta: srv.URL, LinkedIn: srv.URL, Google: srv.URL}
	return c
}

func runTranscript(t *testing.T, name string, tr *transcript) {
	t.Run(name, func(t *testing.T) {
		srv, n := replay(t, tr)
		var logs bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(old)

		res, err := SyncBatch(context.Background(), clientFor(srv, tr.Secrets), "ws-1", tr.Platform, tr.Config, tr.Snapshots, tr.Operation)
		if int(*n) != len(tr.Exchanges) {
			t.Errorf("%d requests, transcript has %d", *n, len(tr.Exchanges))
		}
		for _, secret := range tr.Secrets {
			if secret != "" && (strings.Contains(logs.String(), secret) || (err != nil && strings.Contains(err.Error(), secret))) {
				t.Errorf("secret leaked into log or error")
			}
		}
		if tr.Raises != "" {
			if err == nil {
				t.Fatalf("want error for %q, got %+v", tr.Raises, res)
			}
			want := raisesText(tr.Raises)
			if want == "" {
				t.Fatalf("unmapped raises %q", tr.Raises)
			}
			if err.Error() != want && !(strings.HasPrefix(want, "platform request failed") && strings.HasPrefix(err.Error(), want)) {
				t.Errorf("error %q want %q (python: %q)", err, want, tr.Raises)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		want := tr.Result
		str := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		if res.Success != want.Success || res.Summary != want.Summary || res.Error != str(want.Error) || res.ExternalID != str(want.ExternalID) {
			t.Errorf("result %+v want %+v (err=%q ext=%q)", res, *want, str(want.Error), str(want.ExternalID))
		}
	})
}

// raisesText maps a Python exception to the Go error text.
func raisesText(raises string) string {
	switch {
	case strings.HasPrefix(raises, "RuntimeError: "):
		return strings.TrimPrefix(raises, "RuntimeError: ")
	case strings.HasPrefix(raises, "ValueError: "):
		return strings.TrimPrefix(raises, "ValueError: ")
	case strings.HasPrefix(raises, "JSONDecodeError"):
		return "invalid JSON response from platform"
	case strings.HasPrefix(raises, "KeyError: 'resourceName'"):
		return "Google Ads response missing resourceName"
	case strings.HasPrefix(raises, "HTTPStatusError"):
		for _, code := range []string{"302", "400", "429", "500"} {
			if strings.Contains(raises, "'"+code+" ") {
				return "platform request failed (status " + code + ")"
			}
		}
	}
	return ""
}

func TestMetaRequestsMatchTranscripts(t *testing.T) {
	for name, tr := range loadTranscripts(t, "meta_ads__") {
		runTranscript(t, name, tr)
	}
}

func TestLinkedInRequestsAndBatchValidationMatchTranscripts(t *testing.T) {
	for name, tr := range loadTranscripts(t, "linkedin_ads__") {
		runTranscript(t, name, tr)
	}
}

func TestGoogleThreeStepFlowMatchTranscripts(t *testing.T) {
	for name, tr := range loadTranscripts(t, "google_ads__") {
		if strings.Contains(name, "partial_failure") {
			continue
		}
		runTranscript(t, name, tr)
	}
}

func TestGooglePartialFailureSanitizedAndNotRun(t *testing.T) {
	for name, tr := range loadTranscripts(t, "google_ads__") {
		if !strings.Contains(name, "partial_failure") {
			continue
		}
		runTranscript(t, name, tr) // exactly create + addOperations: :run is never sent
		if len(tr.Exchanges) != 2 {
			t.Errorf("%s: want 2 exchanges, got %d", name, len(tr.Exchanges))
		}
		if tr.Result == nil || tr.Result.ExternalID == nil || !strings.HasPrefix(*tr.Result.ExternalID, "customers/") {
			t.Errorf("%s: job name is the external id", name)
		}
		if tr.Result != nil && tr.Result.Error != nil && strings.Contains(*tr.Result.Error, "@") {
			t.Errorf("%s: provider text leaked", name)
		}
	}
}

func TestAuthErrorsMapToPythonText(t *testing.T) {
	for _, code := range []int{401, 403} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"bad token tok-SECRET-1"}`))
		}))
		defer srv.Close()
		for _, typ := range []string{"meta_ads", "linkedin_ads", "google_ads"} {
			cfg := map[string]any{"custom_audience_id": "1", "segment_id": "2", "customer_id": "3", "user_list_id": "4"}
			_, err := SyncBatch(context.Background(), clientFor(srv, map[string]string{
				"META_ACCESS_TOKEN": "tok-SECRET-1", "LINKEDIN_ACCESS_TOKEN": "tok-SECRET-1",
				"GOOGLE_ADS_ACCESS_TOKEN": "tok-SECRET-1", "GOOGLE_ADS_DEVELOPER_TOKEN": "dev-SECRET-2",
			}), "ws", typ, cfg, []map[string]any{{"email": "a@b.co"}}, "add")
			want := fmt.Sprintf("platform authorization or API approval required (%d)", code)
			if err == nil || err.Error() != want {
				t.Errorf("%s %d: %v", typ, code, err)
			}
		}
	}
}

func TestMissingSecretError(t *testing.T) {
	c := NewClient(secretsOf(nil), "test", nil)
	for typ, name := range map[string]string{"meta_ads": "META_ACCESS_TOKEN", "linkedin_ads": "LINKEDIN_ACCESS_TOKEN", "google_ads": "GOOGLE_ADS_ACCESS_TOKEN"} {
		_, err := SyncBatch(context.Background(), c, "ws", typ, map[string]any{"custom_audience_id": "1", "segment_id": "2", "customer_id": "3", "user_list_id": "4"},
			[]map[string]any{{"email": "a@b.co"}}, "add")
		if err == nil || err.Error() != "missing workspace secret "+name {
			t.Errorf("%s: %v", typ, err)
		}
	}
	// A getter failure is reported without its text (it could carry anything).
	c.Secrets = func(context.Context, string, string) (string, error) { return "", errors.New("db down tok-SECRET") }
	_, err := SyncBatch(context.Background(), c, "ws", "meta_ads", map[string]any{"custom_audience_id": "1"}, []map[string]any{{"email": "a@b.co"}}, "add")
	if err == nil || strings.Contains(err.Error(), "tok-SECRET") {
		t.Errorf("getter error: %v", err)
	}
}

func TestRemoveKeepsPrehashedValues(t *testing.T) {
	h := "b5fc85e55755f9e0d030a10ab4429b6b2944855f9a0d60077fe832becbc41d72"
	rows := batchRows("remove", []map[string]any{
		{"email": h}, {"email": strings.ToUpper(h)}, {"phone": h}, {"email": "RAW@Example.com"}, {"company": "x"},
	})
	want := []map[string]string{
		{"email": h}, {"email": h}, {"phone": h}, {"email": HashEmail("RAW@Example.com")},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("remove rows %v want %v", rows, want)
	}
	// add never trusts a pre-hashed value: it is a non-email and is dropped.
	if rows := batchRows("add", []map[string]any{{"email": h}}); len(rows) != 0 {
		t.Errorf("add rows %v", rows)
	}
}

func TestInvalidOperationErrors(t *testing.T) {
	for _, op := range []string{"upsert", "", "ADD"} {
		_, err := SyncBatch(context.Background(), NewClient(secretsOf(nil), "test", nil), "ws", "meta_ads", nil, nil, op)
		if err == nil || err.Error() != "ad sync operation must be add or remove" {
			t.Errorf("op %q: %v", op, err)
		}
	}
}

func TestUnsupportedDestinationType(t *testing.T) {
	res, err := SyncBatch(context.Background(), NewClient(secretsOf(nil), "test", nil), "ws", "tiktok_ads", nil, []map[string]any{{"email": "a@b.co"}}, "add")
	if err != nil || res.Success || res.Error != "unsupported ad destination 'tiktok_ads'" {
		t.Errorf("%+v %v", res, err)
	}
}

func okSecrets() map[string]string {
	return map[string]string{"META_ACCESS_TOKEN": "tok-SECRET-1", "LINKEDIN_ACCESS_TOKEN": "tok-SECRET-1", "GOOGLE_ADS_ACCESS_TOKEN": "tok-SECRET-1", "GOOGLE_ADS_DEVELOPER_TOKEN": "dev-SECRET-2"}
}

func TestBlockedHostRefused(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	// Production posture: no allow-listed hosts, so the loopback endpoint is refused at dial time.
	c := NewClient(secretsOf(okSecrets()), "production", nil)
	c.Endpoints = Endpoints{Meta: srv.URL, LinkedIn: srv.URL, Google: srv.URL}
	_, err := SyncBatch(context.Background(), c, "ws", "meta_ads", map[string]any{"custom_audience_id": "1"}, []map[string]any{{"email": "a@b.co"}}, "add")
	if err == nil || atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
	if strings.Contains(err.Error(), "tok-SECRET-1") {
		t.Error("token in error")
	}
	if p := ProductionEndpoints; p.Meta != "https://graph.facebook.com" || p.LinkedIn != "https://api.linkedin.com" || p.Google != "https://googleads.googleapis.com" {
		t.Errorf("production endpoints %+v", p)
	}
	if NewClient(secretsOf(nil), "production", nil).Endpoints != ProductionEndpoints {
		t.Error("NewClient must default to production endpoints")
	}
}

func TestNoRedirectFollowed(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	_, err := SyncBatch(context.Background(), clientFor(redir, okSecrets()), "ws", "meta_ads", map[string]any{"custom_audience_id": "1"}, []map[string]any{{"email": "a@b.co"}}, "add")
	if err == nil || !strings.Contains(err.Error(), "307") || atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestResponseBodyBounded(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"session_id":"1","pad":"` + big + `"}`))
	}))
	defer srv.Close()
	_, err := SyncBatch(context.Background(), clientFor(srv, okSecrets()), "ws", "meta_ads", map[string]any{"custom_audience_id": "1"}, []map[string]any{{"email": "a@b.co"}}, "add")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err=%v", err)
	}
	if len(err.Error()) > 200 {
		t.Error("error carries body")
	}
}

func TestSecretsNeverInErrorsOrLogs(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile or buggy platform echoes credentials and PII back.
		w.WriteHeader(500)
		_, _ = w.Write([]byte("echo tok-SECRET-1 dev-SECRET-2 a@b.co " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	cfgs := map[string]map[string]any{
		"meta_ads": {"custom_audience_id": "1"}, "linkedin_ads": {"segment_id": "2"}, "google_ads": {"customer_id": "3", "user_list_id": "4"},
	}
	for typ, cfg := range cfgs {
		_, err := SyncBatch(context.Background(), clientFor(srv, okSecrets()), "ws", typ, cfg, []map[string]any{{"email": "a@b.co"}}, "add")
		if err == nil {
			t.Fatalf("%s: want error", typ)
		}
		for _, bad := range []string{"tok-SECRET-1", "dev-SECRET-2", "a@b.co", "Bearer"} {
			if strings.Contains(err.Error(), bad) || strings.Contains(logs.String(), bad) {
				t.Errorf("%s: %q leaked", typ, bad)
			}
		}
	}
}
