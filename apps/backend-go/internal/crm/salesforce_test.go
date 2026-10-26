package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const (
	sfInst  = "https://contract.my.salesforce.com"
	sfToken = "ctk_salesforce_alpha_0001"
)

// withSFSecrets stores the workspace's encrypted Salesforce credentials.
func withSFSecrets(t *testing.T, e *secrets.Envelope, st *fakeStore, ws, inst, token string) {
	t.Helper()
	for k, v := range map[string]string{"SALESFORCE_INSTANCE_URL": inst, "SALESFORCE_ACCESS_TOKEN": token} {
		if v == "" {
			continue
		}
		ct, err := e.Encrypt(context.Background(), v)
		if err != nil {
			t.Fatal(err)
		}
		st.ws[ws+"/"+k] = ct
	}
}

func loadSFTranscripts(t *testing.T) []*transcript {
	t.Helper()
	files, err := filepath.Glob("../../testdata/vendors/crm/salesforce__*.json")
	if err != nil || len(files) < 20 {
		t.Fatalf("salesforce transcripts missing: %d %v", len(files), err)
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

func TestSalesforceTranscripts(t *testing.T) {
	for _, tr := range loadSFTranscripts(t) {
		t.Run(strings.TrimSuffix(tr.file, ".json"), func(t *testing.T) {
			st := newStore()
			res, env := newResolver(t, st, nil)
			withSFSecrets(t, env, st, testWS, tr.Secrets["SALESFORCE_INSTANCE_URL"], tr.Secrets["SALESFORCE_ACCESS_TOKEN"])
			rp := &replay{t: t, tr: tr}
			sl := &sleepRec{}
			s := &Salesforce{HTTP: &http.Client{Transport: rp}, Secrets: res, Sleep: sl.fn}
			ctx := context.Background()

			var args struct {
				Lead     map[string]any  `json:"lead"`
				FieldMap json.RawMessage `json:"field_map"`
				Fields   json.RawMessage `json:"fields"`
				SObject  string          `json:"sobject"`
				RecordID string          `json:"record_id"`
				Limit    *int            `json:"limit"`
				Where    string          `json:"where"`
			}
			if len(tr.Call.Args) > 0 {
				if err := json.Unmarshal(tr.Call.Args, &args); err != nil {
					t.Fatal(err)
				}
			}
			var got any
			switch tr.Call.Function {
			case "push_lead_as_contact":
				b, _ := json.Marshal(args.Lead)
				v, err := providers.ParseJSON(b)
				if err != nil {
					t.Fatal(err)
				}
				got = s.PushLeadAsContact(ctx, testWS, mapLead(v.(map[string]any)), decodeFieldMap(t, args.FieldMap))
			case "fetch_contacts":
				limit := 500
				if args.Limit != nil {
					limit = *args.Limit
				}
				got = s.FetchContacts(ctx, testWS, limit, args.Where, decodeFieldMap(t, args.FieldMap))
			case "update_record":
				got = s.UpdateRecord(ctx, testWS, args.SObject, args.RecordID, decodeFieldMap(t, args.Fields))
			default:
				t.Fatalf("unhandled function %s", tr.Call.Function)
			}
			if g, w := canon(t, got), canon(t, json.RawMessage(tr.Result)); g != w {
				t.Errorf("result\n got %s\nwant %s", strings.ReplaceAll(g, sfToken, "***"), w)
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

type sfVectors struct {
	Salesforce struct {
		DefaultFieldMap     json.RawMessage `json:"DEFAULT_FIELD_MAP"`
		ImportDefaultFields json.RawMessage `json:"IMPORT_DEFAULT_FIELDS"`
		ContactUpdate       json.RawMessage `json:"CONTACT_UPDATE_FIELD_MAP"`
		APIVersion          string          `json:"API_VERSION"`
	} `json:"salesforce"`
	SOQLEscape []struct {
		Input string          `json:"input"`
		Value string          `json:"value"`
		Raw   json.RawMessage `json:"-"`
	} `json:"soql_escape"`
	RecordValue []struct {
		Record json.RawMessage `json:"record"`
		Path   string          `json:"path"`
		Value  json.RawMessage `json:"value"`
	} `json:"record_value"`
	MapImportRecord []struct {
		Name     string          `json:"name"`
		Record   json.RawMessage `json:"record"`
		FieldMap json.RawMessage `json:"field_map"`
		Value    json.RawMessage `json:"value"`
	} `json:"map_import_record"`
}

func loadSFVectors(t *testing.T) sfVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/crm_maps.json")
	if err != nil {
		t.Fatal(err)
	}
	var v sfVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSOQLEscapeVectors(t *testing.T) {
	v := loadSFVectors(t)
	if len(v.SOQLEscape) < 10 {
		t.Fatalf("only %d vectors", len(v.SOQLEscape))
	}
	for _, c := range v.SOQLEscape {
		if got := SOQLEscape(c.Input); got != c.Value {
			t.Errorf("SOQLEscape(%q) = %q want %q", c.Input, got, c.Value)
		}
	}
}

func TestFieldMapsMatchVectors(t *testing.T) {
	v := loadSFVectors(t).Salesforce
	for name, pair := range map[string]struct {
		got  FieldMap
		want json.RawMessage
	}{
		"DEFAULT_FIELD_MAP":        {SalesforceDefaultFieldMap, v.DefaultFieldMap},
		"IMPORT_DEFAULT_FIELDS":    {ImportDefaultFields, v.ImportDefaultFields},
		"CONTACT_UPDATE_FIELD_MAP": {ContactUpdateFieldMap, v.ContactUpdate},
	} {
		want := decodeFieldMap(t, pair.want)
		if len(want) == 0 || len(pair.got) != len(want) {
			t.Fatalf("%s len %d want %d", name, len(pair.got), len(want))
		}
		for i := range want { // order fixes the request body order
			if pair.got[i] != want[i] {
				t.Errorf("%s[%d] = %v want %v", name, i, pair.got[i], want[i])
			}
		}
	}
	if APIVersion != v.APIVersion || APIVersion != "v59.0" {
		t.Errorf("APIVersion = %q", APIVersion)
	}
}

func TestRecordValueAndMapImportRecordVectors(t *testing.T) {
	v := loadSFVectors(t)
	if len(v.RecordValue) < 10 || len(v.MapImportRecord) < 10 {
		t.Fatalf("vectors: %d %d", len(v.RecordValue), len(v.MapImportRecord))
	}
	parse := func(raw json.RawMessage) map[string]any {
		x, err := providers.ParseJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := x.(map[string]any)
		return m
	}
	for i, c := range v.RecordValue {
		if g, w := canon(t, recordValue(parse(c.Record), c.Path)), canon(t, c.Value); g != w {
			t.Errorf("recordValue case %d path %q: got %s want %s", i, c.Path, g, w)
		}
	}
	for _, c := range v.MapImportRecord {
		t.Run(c.Name, func(t *testing.T) {
			got := mapImportRecord(parse(c.Record), decodeFieldMap(t, c.FieldMap))
			if g, w := canon(t, got), canon(t, c.Value); g != w {
				t.Errorf("got %s want %s", g, w)
			}
		})
	}
}

// ── search escaping ─────────────────────────────────────────────────────

func TestSearchByEmailUsesEscape(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, sfToken)
	var q string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			q = r.URL.Query().Get("q")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader(`{"records":[]}`))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	evil := "x' OR Name != '\\\"\n"
	r := s.PushLeadAsContact(context.Background(), testWS, mapLead{"company": "Co", "email": evil, "contact_person": "A"}, nil)
	_ = r
	want := "SELECT Id FROM Lead WHERE Email = '" + SOQLEscape(evil) + "' LIMIT 1"
	if q != want {
		t.Fatalf("query %q want %q", q, want)
	}
	if strings.Contains(q, "x' OR") {
		t.Fatalf("unescaped quote reached SOQL: %q", q)
	}
}

// ── tenant and secrets ──────────────────────────────────────────────────

func TestTenantCredentialsIsolated(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, "ws-a", "https://a.my.salesforce.com", "token-of-A")
	withSFSecrets(t, env, st, "ws-b", "https://b.my.salesforce.com", "token-of-B")
	var seen []string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Host+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 204, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	ctx := context.Background()
	f := FieldMap{{"Phone", "1"}}
	s.UpdateRecord(ctx, "ws-a", "Contact", "1", f)
	s.UpdateRecord(ctx, "ws-b", "Contact", "1", f)
	withSFSecrets(t, env, st, "ws-a", "https://a.my.salesforce.com", "token-of-A2") // rotation, never cached
	s.UpdateRecord(ctx, "ws-a", "Contact", "1", f)
	r := s.UpdateRecord(ctx, "ws-c", "Contact", "1", f) // no secrets, no global: must not borrow
	want := "a.my.salesforce.com Bearer token-of-A|b.my.salesforce.com Bearer token-of-B|a.my.salesforce.com Bearer token-of-A2"
	if strings.Join(seen, "|") != want {
		t.Fatalf("requests %v", seen)
	}
	if r.Success || !strings.HasPrefix(r.Error, "Salesforce not connected") {
		t.Fatalf("ws-c = %+v", r)
	}
	if s.IsConnected(ctx, "ws-c") || !s.IsConnected(ctx, "ws-a") {
		t.Fatal("IsConnected mismatch")
	}
}

func TestSalesforceGlobalFallbackMatchesGetSecret(t *testing.T) {
	ctx := context.Background()
	st := newStore()
	res, env := newResolver(t, st, map[string]string{
		"SALESFORCE_INSTANCE_URL": "https://env.my.salesforce.com", "SALESFORCE_ACCESS_TOKEN": "env-token"})
	var seen []string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Host+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 204, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	step := func(ws, want string) {
		t.Helper()
		seen = nil
		s.UpdateRecord(ctx, ws, "Contact", "1", FieldMap{{"Phone", "1"}})
		if len(seen) != 1 || seen[0] != want {
			t.Errorf("%s: sent %v want %q", ws, seen, want)
		}
	}
	step("ws-x", "env.my.salesforce.com Bearer env-token")
	st.app["SALESFORCE_INSTANCE_URL"] = "https://global.my.salesforce.com"
	st.app["SALESFORCE_ACCESS_TOKEN"] = "global-token"
	step("ws-x", "global.my.salesforce.com Bearer global-token")
	withSFSecrets(t, env, st, "ws-x", "https://ws.my.salesforce.com", "ws-token")
	step("ws-x", "ws.my.salesforce.com Bearer ws-token")
	step("ws-other", "global.my.salesforce.com Bearer global-token")
}

func TestSalesforceNoSecretInErrors(t *testing.T) {
	const tok = "SUPER-SECRET-SF-TOKEN"
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, tok)
	leak := rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: proxy rejected Authorization: " + r.Header.Get("Authorization"))
	})
	s := &Salesforce{HTTP: &http.Client{Transport: leak}, Secrets: res, Sleep: (&sleepRec{}).fn}
	ctx := context.Background()
	lead := mapLead{"id": int64(1), "company": "Co", "email": "a@b.co"}
	all := []string{
		canon(t, s.PushLeadAsContact(ctx, testWS, lead, nil)),
		canon(t, s.UpdateRecord(ctx, testWS, "Contact", "1", FieldMap{{"a", "b"}})),
		canon(t, s.FetchContacts(ctx, testWS, 5, "", nil)),
	}
	for i, x := range all {
		if strings.Contains(x, tok) {
			t.Errorf("result %d leaks the token: %s", i, x)
		}
		if !strings.Contains(x, "dial tcp") {
			t.Errorf("result %d lost the transport error: %s", i, x)
		}
	}
	echo := rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader("oops " + tok))}, nil
	})
	s.HTTP = &http.Client{Transport: echo}
	for _, x := range []string{
		canon(t, s.PushLeadAsContact(ctx, testWS, lead, nil)),
		canon(t, s.UpdateRecord(ctx, testWS, "Contact", "1", FieldMap{{"a", "b"}})),
		canon(t, s.FetchContacts(ctx, testWS, 5, "", nil)),
	} {
		if strings.Contains(x, tok) {
			t.Errorf("echoed body leaks the token: %s", x)
		}
	}
}

// ── SSRF guard ──────────────────────────────────────────────────────────

func TestInstanceURLPrivateAddressBlocked(t *testing.T) {
	bad := []string{
		"https://127.0.0.1",
		"https://169.254.169.254",
		"https://[::ffff:127.0.0.1]",
		"https://[::1]",
		"https://localhost",
		"https://user:pw@contract.my.salesforce.com",
		"http://contract.my.salesforce.com",
		"https://contract.my.salesforce.com@evil.example.com",
		"https://evil.example.com",
		"https://salesforce.com.evil.example.com",
		"https://evilsalesforce.com",
		"https://contract.my.salesforce.com.evil.example.com",
		"https://contract.my.salesforce.com:8443",
		"https://contract.my.salesforce.com/some/path",
		"https://contract.my.salesforce.com?x=1",
		"ftp://contract.my.salesforce.com",
		"//contract.my.salesforce.com",
		"contract.my.salesforce.com",
	}
	st := newStore()
	res, env := newResolver(t, st, nil)
	hits := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	ctx := context.Background()
	for _, inst := range bad {
		withSFSecrets(t, env, st, testWS, inst, sfToken)
		results := map[string]any{
			"fetch":  s.FetchContacts(ctx, testWS, 5, "", nil),
			"update": s.UpdateRecord(ctx, testWS, "Contact", "1", FieldMap{{"Phone", "1"}}),
			"push":   s.PushLeadAsContact(ctx, testWS, mapLead{"company": "Co", "email": "a@b.co"}, nil),
		}
		for name, r := range results {
			b, _ := json.Marshal(r)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			e, _ := m["error"].(string)
			if m["success"] != false || !strings.HasPrefix(e, "blocked url: ") {
				t.Errorf("%s %q: %s", name, inst, b)
			}
		}
	}
	if hits != 0 {
		t.Fatalf("%d requests left the process for blocked instance URLs", hits)
	}
}

func TestInstanceURLAllowedSuffixes(t *testing.T) {
	for _, ok := range []string{
		"https://contract.my.salesforce.com", "https://acme.my.salesforce.com/", "https://acme.lightning.force.com",
		"https://na1.salesforce.com", "https://Acme.My.Salesforce.COM",
	} {
		if err := checkInstanceURL(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://salesforce.com", "https://force.com", "https://x.salesforce.com."} {
		if err := checkInstanceURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNextRecordsURLMustStayOnInstance(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, sfToken)
	for _, next := range []string{"@evil.example.com/x", "//evil.example.com/x", "https://evil.example.com/x", "/other/path"} {
		var hosts []string
		rt := rtFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.String())
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
				Body: io.NopCloser(strings.NewReader(`{"done":false,"records":[],"nextRecordsUrl":"` + next + `"}`))}, nil
		})
		s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
		r := s.FetchContacts(context.Background(), testWS, 5, "", nil)
		if r.Success || len(hosts) != 1 {
			t.Errorf("next %q: result %+v requests %v", next, r, hosts)
		}
	}
}

func TestSalesforceRedirectRefused(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, sfToken)
	hits := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		h := http.Header{}
		h.Set("Location", "http://169.254.169.254/latest/meta-data")
		return &http.Response{StatusCode: 302, Header: h, Request: r, Body: io.NopCloser(strings.NewReader("moved"))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	r := s.UpdateRecord(context.Background(), testWS, "Contact", "1", FieldMap{{"Phone", "1"}})
	if hits != 1 || r.Success || r.Error != "HTTP 302: moved" {
		t.Fatalf("hits %d result %+v", hits, r)
	}
}

func TestSalesforceBodyCap(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, sfToken)
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
			Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), providers.MaxBody+10)))}, nil
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res}
	r := s.FetchContacts(context.Background(), testWS, 5, "", nil)
	if r.Success || !strings.Contains(r.Error, "exceeds") {
		t.Fatalf("result %+v", r)
	}
}

func TestSalesforceContextCancelStopsRetry(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, sfToken)
	hits := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		h := http.Header{}
		h.Set("Retry-After", "5")
		return &http.Response{StatusCode: 429, Header: h, Request: r, Body: io.NopCloser(strings.NewReader("slow"))}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res,
		Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }}
	s.UpdateRecord(ctx, testWS, "Contact", "1", FieldMap{{"Phone", "1"}})
	if hits != 1 {
		t.Fatalf("hits %d, a cancel must stop retries", hits)
	}
}

// fakeDNS answers every A query with 127.0.0.1 (a DNS-rebinding attacker).
func fakeDNS(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no udp: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			if n < 12 {
				continue
			}
			i := 12
			for i < n && q[i] != 0 {
				i += int(q[i]) + 1
			}
			qEnd := i + 5 // root label + qtype + qclass
			if qEnd > n {
				continue
			}
			qtype := uint16(q[qEnd-4])<<8 | uint16(q[qEnd-3])
			resp := append([]byte{}, q[:qEnd]...)
			resp[2], resp[3] = 0x81, 0x80
			resp[6], resp[7], resp[8], resp[9], resp[10], resp[11] = 0, 0, 0, 0, 0, 0
			if qtype == 1 {
				resp[7] = 1
				resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4, 127, 0, 0, 1)
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

var dnsMu sync.Mutex

func TestInstanceURLRebindingBlockedAtDial(t *testing.T) {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	old := net.DefaultResolver
	net.DefaultResolver = fakeDNS(t)
	defer func() { net.DefaultResolver = old }()

	st := newStore()
	res, env := newResolver(t, st, nil)
	// an allowed-suffix hostname whose DNS answer is loopback
	withSFSecrets(t, env, st, testWS, "https://rebind.my.salesforce.com", sfToken)
	s := &Salesforce{Secrets: res} // nil HTTP: the default must be the egress dial-time guard
	ctx := context.Background()
	for name, r := range map[string]any{
		"fetch":  s.FetchContacts(ctx, testWS, 5, "", nil),
		"update": s.UpdateRecord(ctx, testWS, "Contact", "1", FieldMap{{"Phone", "1"}}),
		"push":   s.PushLeadAsContact(ctx, testWS, mapLead{"company": "Co", "email": "a@b.co"}, nil),
	} {
		b, _ := json.Marshal(r)
		if !strings.Contains(string(b), `"success":false`) || !strings.Contains(string(b), "blocked url: ") ||
			!strings.Contains(string(b), "private ip") {
			t.Errorf("%s: %s", name, b)
		}
		if strings.Contains(string(b), sfToken) {
			t.Errorf("%s leaks token", name)
		}
	}
}
