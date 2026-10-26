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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const (
	testToken = "ctk_hubspot_alpha_0001"
	testWS    = "ws-alpha"
)

// ── fakes ────────────────────────────────────────────────────────────────

type fakeStore struct {
	mu  sync.Mutex
	app map[string]string
	ws  map[string]string // "ws/key" -> ciphertext
}

func (s *fakeStore) AppSetting(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.app[key], nil
}
func (s *fakeStore) WorkspaceSetting(_ context.Context, ws, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ws[ws+"/"+key]
	return v, ok, nil
}

func newResolver(t *testing.T, st *fakeStore, env map[string]string) (*secrets.Resolver, *secrets.Envelope) {
	t.Helper()
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "crm-test-master-key", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.NewResolver(e, st, func(k string) string { return env[k] }), e
}

// withWorkspaceToken stores token as ws's encrypted HUBSPOT_TOKEN.
func withWorkspaceToken(t *testing.T, e *secrets.Envelope, st *fakeStore, ws, token string) {
	t.Helper()
	ct, err := e.Encrypt(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	st.ws[ws+"/HUBSPOT_TOKEN"] = ct
}

func newStore() *fakeStore { return &fakeStore{app: map[string]string{}, ws: map[string]string{}} }

type mapLead map[string]any

func (m mapLead) Get(key string) (any, bool) { v, ok := m[key]; return v, ok }

type sleepRec struct {
	mu sync.Mutex
	d  []float64
}

func (s *sleepRec) fn(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d.Seconds())
	s.mu.Unlock()
	return nil
}

// ── ordered JSON helpers (field maps are order significant) ─────────────

func decodeFieldMap(t *testing.T, raw json.RawMessage) FieldMap {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("field map is not an object: %s", raw)
	}
	var fm FieldMap
	for dec.More() {
		k, _ := dec.Token()
		v, _ := dec.Token()
		fm = append(fm, FieldPair{Key: k.(string), Value: v.(string)})
	}
	return fm
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(x)
	return string(out)
}

// ── transcript replay ────────────────────────────────────────────────────

type exchange struct {
	Request struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

type transcript struct {
	Scenario string `json:"scenario"`
	Call     struct {
		Function string          `json:"function"`
		Args     json.RawMessage `json:"args"`
	} `json:"call"`
	Secrets   map[string]string `json:"secrets"`
	Exchanges []exchange        `json:"http_exchanges"`
	Sleeps    []float64         `json:"sleeps"`
	Result    json.RawMessage   `json:"result"`
	file      string
}

func (tr *transcript) fresh(s string) string {
	for k, v := range tr.Secrets {
		s = strings.ReplaceAll(s, "{{secret:"+k+"}}", v)
	}
	return s
}

func redact(s string) string { return strings.ReplaceAll(s, testToken, "***") }

type replay struct {
	t    *testing.T
	tr   *transcript
	next int
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.next >= len(r.tr.Exchanges) {
		r.t.Errorf("%s: unexpected extra request %s %s", r.tr.file, req.Method, req.URL)
		return nil, errors.New("unrouted request")
	}
	ex := r.tr.Exchanges[r.next]
	r.next++
	if req.Method != ex.Request.Method {
		r.t.Errorf("%s: method %s want %s", r.tr.file, req.Method, ex.Request.Method)
	}
	if got := req.URL.String(); got != ex.Request.URL {
		r.t.Errorf("%s: url\n got %s\nwant %s", r.tr.file, got, ex.Request.URL)
	}
	gotH := map[string]string{}
	for k, v := range req.Header {
		gotH[strings.ToLower(k)] = strings.Join(v, ",")
	}
	wantH := map[string]string{}
	for k, v := range ex.Request.Headers {
		wantH[strings.ToLower(k)] = r.tr.fresh(v)
	}
	var gn, wn []string
	for k := range gotH {
		gn = append(gn, k)
	}
	for k := range wantH {
		wn = append(wn, k)
	}
	sort.Strings(gn)
	sort.Strings(wn)
	if strings.Join(gn, ",") != strings.Join(wn, ",") {
		r.t.Errorf("%s: header names %v want %v", r.tr.file, gn, wn)
	}
	for k, v := range wantH {
		if gotH[k] != v {
			r.t.Errorf("%s: header %s differs", r.tr.file, k)
		}
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if string(body) != r.tr.fresh(ex.Request.Body) {
		r.t.Errorf("%s: body\n got %s\nwant %s", r.tr.file, redact(string(body)), ex.Request.Body)
	}
	h := http.Header{}
	for k, v := range ex.Response.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: ex.Response.Status, Header: h, Request: req,
		Body: io.NopCloser(strings.NewReader(ex.Response.Body))}, nil
}

func loadTranscripts(t *testing.T) []*transcript {
	t.Helper()
	files, err := filepath.Glob("../../testdata/vendors/crm/hubspot__*.json")
	if err != nil || len(files) < 40 {
		t.Fatalf("hubspot transcripts missing: %d %v", len(files), err)
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

func TestHubSpotTranscripts(t *testing.T) {
	for _, tr := range loadTranscripts(t) {
		t.Run(strings.TrimSuffix(tr.file, ".json"), func(t *testing.T) {
			st := newStore()
			res, env := newResolver(t, st, nil)
			if tok := tr.Secrets["HUBSPOT_TOKEN"]; tok != "" {
				withWorkspaceToken(t, env, st, testWS, tok)
			}
			rp := &replay{t: t, tr: tr}
			sl := &sleepRec{}
			h := &HubSpot{HTTP: &http.Client{Transport: rp}, Secrets: res, Sleep: sl.fn}
			ctx := context.Background()

			var args struct {
				Lead       map[string]any   `json:"lead"`
				Leads      []map[string]any `json:"leads"`
				FieldMap   json.RawMessage  `json:"field_map"`
				ContactID  string           `json:"contact_id"`
				Properties map[string]any   `json:"properties"`
				Limit      *int             `json:"limit"`
				ListID     string           `json:"list_id"`
			}
			if len(tr.Call.Args) > 0 {
				dec := json.NewDecoder(bytes.NewReader(tr.Call.Args))
				if err := dec.Decode(&args); err != nil {
					t.Fatal(err)
				}
			}
			// ints in lead dicts must be Python ints, not float64
			normLead := func(m map[string]any) mapLead {
				b, _ := json.Marshal(m)
				v, err := providers.ParseJSON(b)
				if err != nil {
					t.Fatal(err)
				}
				return mapLead(v.(map[string]any))
			}
			fm := decodeFieldMap(t, args.FieldMap)

			var got any
			switch tr.Call.Function {
			case "test_connection":
				got = h.TestConnection(ctx, testWS)
			case "push_lead_as_contact":
				got = h.PushLeadAsContact(ctx, testWS, normLead(args.Lead), fm)
			case "push_leads_batch":
				var leads []Lead
				for _, l := range args.Leads {
					leads = append(leads, normLead(l))
				}
				got = h.PushLeadsBatch(ctx, testWS, leads, fm)
			case "update_contact_by_id":
				props := map[string]string{}
				for k, v := range args.Properties {
					props[k] = v.(string)
				}
				got = h.UpdateContactByID(ctx, testWS, args.ContactID, props)
			case "fetch_contacts":
				limit := 500
				if args.Limit != nil {
					limit = *args.Limit
				}
				got = h.FetchContacts(ctx, testWS, limit, args.ListID, fm)
			case "get_contacts":
				limit := 20
				if args.Limit != nil {
					limit = *args.Limit
				}
				c, err := h.GetContacts(ctx, testWS, limit)
				if err != nil {
					t.Fatalf("GetContacts: %v", err)
				}
				got = c
			default:
				t.Fatalf("unhandled function %s", tr.Call.Function)
			}

			if g, w := canon(t, got), canon(t, json.RawMessage(tr.Result)); g != w {
				t.Errorf("result\n got %s\nwant %s", redact(g), w)
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

// ── vectors ──────────────────────────────────────────────────────────────

type mapsVector struct {
	Hubspot struct {
		DefaultFieldMap json.RawMessage `json:"DEFAULT_FIELD_MAP"`
		ImportProps     []string        `json:"IMPORT_DEFAULT_PROPERTIES"`
		BaseURL         string          `json:"BASE_URL"`
	} `json:"hubspot"`
	MapImport []struct {
		Name     string          `json:"name"`
		Props    json.RawMessage `json:"props"`
		FieldMap json.RawMessage `json:"field_map"`
		Value    json.RawMessage `json:"value"`
	} `json:"hubspot_map_import_properties"`
}

func loadMaps(t *testing.T) mapsVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/crm_maps.json")
	if err != nil {
		t.Fatal(err)
	}
	var v mapsVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDefaultFieldMapMatchesVector(t *testing.T) {
	v := loadMaps(t)
	want := decodeFieldMap(t, v.Hubspot.DefaultFieldMap)
	if len(want) == 0 || len(DefaultFieldMap) != len(want) {
		t.Fatalf("DefaultFieldMap len %d want %d", len(DefaultFieldMap), len(want))
	}
	for i := range want { // order matters: it fixes the request body property order
		if DefaultFieldMap[i] != want[i] {
			t.Errorf("DefaultFieldMap[%d] = %v want %v", i, DefaultFieldMap[i], want[i])
		}
	}
	if strings.Join(ImportDefaultProperties, ",") != strings.Join(v.Hubspot.ImportProps, ",") {
		t.Errorf("ImportDefaultProperties = %v want %v", ImportDefaultProperties, v.Hubspot.ImportProps)
	}
	if DefaultBaseURL != v.Hubspot.BaseURL || DefaultBaseURL != "https://api.hubapi.com" {
		t.Errorf("DefaultBaseURL = %q", DefaultBaseURL)
	}
}

func TestMapImportPropertiesMatchesVector(t *testing.T) {
	v := loadMaps(t)
	if len(v.MapImport) < 5 {
		t.Fatalf("only %d vector cases", len(v.MapImport))
	}
	for _, c := range v.MapImport {
		t.Run(c.Name, func(t *testing.T) {
			var props map[string]any
			if string(c.Props) != "null" {
				p, err := providers.ParseJSON(c.Props)
				if err != nil {
					t.Fatal(err)
				}
				props = p.(map[string]any)
			}
			got := MapImportProperties(props, decodeFieldMap(t, c.FieldMap))
			if g, w := canon(t, got), canon(t, c.Value); g != w {
				t.Errorf("got %s want %s", g, w)
			}
		})
	}
}

// ── security / behaviour ────────────────────────────────────────────────

func authHeaderRT(seen *[]string, status int, body string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: status, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})
}

func TestTokenResolvedPerWorkspace(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withWorkspaceToken(t, env, st, "ws-a", "token-of-A")
	withWorkspaceToken(t, env, st, "ws-b", "token-of-B")
	var seen []string
	h := &HubSpot{HTTP: &http.Client{Transport: authHeaderRT(&seen, 200, `{"portalId":1,"timeZone":"UTC"}`)}, Secrets: res}
	ctx := context.Background()
	h.TestConnection(ctx, "ws-a")
	h.TestConnection(ctx, "ws-b")
	// a rotated token is picked up on the very next call (never cached)
	withWorkspaceToken(t, env, st, "ws-a", "token-of-A2")
	h.TestConnection(ctx, "ws-a")
	// a workspace without a secret and no global must not borrow another's
	r := h.TestConnection(ctx, "ws-c")
	want := []string{"Bearer token-of-A", "Bearer token-of-B", "Bearer token-of-A2"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("authorization headers %v want %v", seen, want)
	}
	if r["connected"] != false || r["error"] != "No HubSpot token configured" {
		t.Fatalf("ws-c = %v", r)
	}
	if h.IsConnected(ctx, "ws-c") || !h.IsConnected(ctx, "ws-a") {
		t.Fatal("IsConnected mismatch")
	}
}

func TestGlobalFallbackMatchesGetSecret(t *testing.T) {
	// workspace secret > global app setting > environment, exactly secrets.Resolver.Get
	ctx := context.Background()
	st := newStore()
	res, env := newResolver(t, st, map[string]string{"HUBSPOT_TOKEN": "env-token"})
	var seen []string
	h := &HubSpot{HTTP: &http.Client{Transport: authHeaderRT(&seen, 200, `{}`)}, Secrets: res}

	h.TestConnection(ctx, "ws-x") // env only
	st.app["HUBSPOT_TOKEN"] = "global-token"
	h.TestConnection(ctx, "ws-x") // global beats env
	withWorkspaceToken(t, env, st, "ws-x", "ws-token")
	h.TestConnection(ctx, "ws-x")     // workspace beats global
	h.TestConnection(ctx, "ws-other") // other workspace still on global
	for i, ws := range []string{"ws-x", "ws-x", "ws-x", "ws-other"} {
		_ = i
		want, _, err := res.Get(ctx, ws, "HUBSPOT_TOKEN")
		if err != nil {
			t.Fatal(err)
		}
		if seen[i] != "Bearer "+want {
			t.Errorf("call %d sent %q, GetSecret says %q", i, seen[i], want)
		}
	}
	if seen[0] != "Bearer env-token" || seen[1] != "Bearer global-token" || seen[2] != "Bearer ws-token" || seen[3] != "Bearer global-token" {
		t.Fatalf("order wrong: %v", seen)
	}
}

func TestNoSecretInErrors(t *testing.T) {
	const tok = "SUPER-SECRET-TOKEN-VALUE"
	st := newStore()
	res, env := newResolver(t, st, nil)
	withWorkspaceToken(t, env, st, testWS, tok)
	leak := rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: proxy rejected Authorization: " + r.Header.Get("Authorization"))
	})
	h := &HubSpot{HTTP: &http.Client{Transport: leak}, Secrets: res, Sleep: (&sleepRec{}).fn}
	ctx := context.Background()
	var all []string
	all = append(all, canon(t, h.TestConnection(ctx, testWS)))
	all = append(all, canon(t, h.PushLeadAsContact(ctx, testWS, mapLead{"id": int64(1), "email": "a@b.co"}, nil)))
	all = append(all, canon(t, h.PushLeadsBatch(ctx, testWS, []Lead{mapLead{"id": int64(1), "email": "a@b.co"}}, nil)))
	all = append(all, canon(t, h.UpdateContactByID(ctx, testWS, "5", map[string]string{"a": "b"})))
	all = append(all, canon(t, h.FetchContacts(ctx, testWS, 5, "", nil)))
	all = append(all, canon(t, h.FetchContacts(ctx, testWS, 5, "9", nil)))
	for i, s := range all {
		if strings.Contains(s, tok) {
			t.Errorf("result %d leaks the token: %s", i, s)
		}
		if !strings.Contains(s, "dial tcp") {
			t.Errorf("result %d lost the transport error: %s", i, s)
		}
	}
	// an upstream body echoing the token is scrubbed as well
	echo := rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader("bad credentials " + tok))}, nil
	})
	h.HTTP = &http.Client{Transport: echo}
	if s := canon(t, h.TestConnection(ctx, testWS)); strings.Contains(s, tok) {
		t.Errorf("echoed body leaks the token: %s", s)
	}
}

func TestRedirectRefused(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withWorkspaceToken(t, env, st, testWS, testToken)
	hits := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		if r.URL.Host != "api.hubapi.com" {
			t.Errorf("followed a redirect to %s", r.URL)
		}
		h := http.Header{}
		h.Set("Location", "http://169.254.169.254/latest/meta-data")
		return &http.Response{StatusCode: 302, Header: h, Request: r, Body: io.NopCloser(strings.NewReader("moved"))}, nil
	})
	h := &HubSpot{HTTP: &http.Client{Transport: rt}, Secrets: res}
	r := h.TestConnection(context.Background(), testWS)
	if hits != 1 || r["connected"] != false || r["error"] != "HTTP 302: moved" {
		t.Fatalf("hits %d result %v", hits, r)
	}
}

func TestBatchStopsOnContextCancel(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withWorkspaceToken(t, env, st, testWS, testToken)
	posts := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{"results":[]}`
		if strings.HasSuffix(r.URL.Path, "/contacts") {
			posts++
			status, body = 201, `{"id":"1"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	h := &HubSpot{HTTP: &http.Client{Transport: rt}, Secrets: res,
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleeps++
			if d != 150*time.Millisecond {
				t.Errorf("spacing %v want 150ms", d)
			}
			cancel()
			return ctx.Err()
		}}
	var leads []Lead
	for i := int64(1); i <= 5; i++ {
		leads = append(leads, mapLead{"id": i, "email": "x@y.co"})
	}
	r := h.PushLeadsBatch(ctx, testWS, leads, nil)
	if r.Created != 1 || posts != 1 || sleeps != 1 || r.Failed != 0 {
		t.Fatalf("result %+v posts %d sleeps %d", r, posts, sleeps)
	}
}

func TestBatchRealSleepIsContextAware(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withWorkspaceToken(t, env, st, testWS, testToken)
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`{"id":"1","results":[]}`))}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &HubSpot{HTTP: &http.Client{Transport: rt}, Secrets: res}
	start := time.Now()
	r := h.PushLeadsBatch(ctx, testWS, []Lead{mapLead{"id": int64(1), "email": "x@y.co"}, mapLead{"id": int64(2), "email": "z@y.co"}}, nil)
	if time.Since(start) > time.Second || r.Created+r.Updated+r.Failed > 0 {
		t.Fatalf("cancelled batch did work: %+v", r)
	}
}
