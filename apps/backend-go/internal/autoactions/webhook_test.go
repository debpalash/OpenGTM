package autoactions

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// ---- harness ---------------------------------------------------------------

type recorded struct {
	Method  string
	Host    string
	Header  http.Header
	Body    []byte
	SNI     string
	Dialed  string
	Present bool
}

type fakeRes struct {
	mu      sync.Mutex
	answers map[string][]string // host -> answers; missing -> public default
	errs    map[string]bool
	flip    map[string][]string // answers used from the 2nd lookup on
	calls   map[string]int
}

func (f *fakeRes) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[host]++
	if f.errs[host] {
		return nil, errors.New("lookup " + host + ": no such host")
	}
	ans, ok := f.answers[host]
	if f2, ok2 := f.flip[host]; ok2 && f.calls[host] > 1 {
		ans, ok = f2, true
	}
	if !ok {
		ans = []string{"93.184.216.34"}
	}
	out := []netip.Addr{}
	for _, a := range ans {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	res     *fakeRes
	rec     recorded
	mu      sync.Mutex
	hits    atomic.Int32
	dialErr error
	dials   []string
	handler http.HandlerFunc
	deps    WebhookDeps
	sec     *fakeSecrets
}

type fakeSecrets struct {
	byWS    map[string]map[string]string
	global  map[string]string
	failErr error
}

func (f *fakeSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	if f.failErr != nil {
		return "", "", f.failErr
	}
	if v, ok := f.byWS[ws][key]; ok && v != "" {
		return v, secrets.SourceWorkspace, nil
	}
	if v := f.global[key]; v != "" {
		return v, secrets.SourceGlobal, nil
	}
	return "", secrets.SourceDefault, nil
}

func newHarness(t *testing.T, h http.HandlerFunc) *harness {
	t.Helper()
	hh := &harness{t: t, res: &fakeRes{}, sec: &fakeSecrets{}}
	hh.handler = h
	hh.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hh.hits.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		hh.mu.Lock()
		hh.rec.Method, hh.rec.Host, hh.rec.Header, hh.rec.Body, hh.rec.Present = r.Method, r.Host, r.Header.Clone(), body, true
		hh.mu.Unlock()
		if hh.handler != nil {
			hh.handler(w, r)
			return
		}
		w.WriteHeader(200)
	}))
	hh.srv.TLS = &tls.Config{GetConfigForClient: func(ch *tls.ClientHelloInfo) (*tls.Config, error) {
		hh.mu.Lock()
		hh.rec.SNI = ch.ServerName
		hh.mu.Unlock()
		return nil, nil
	}}
	hh.srv.StartTLS()
	t.Cleanup(hh.srv.Close)
	srvAddr := hh.srv.Listener.Addr().String()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		hh.mu.Lock()
		hh.dials = append(hh.dials, addr)
		err := hh.dialErr
		hh.mu.Unlock()
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srvAddr)
	}
	client := newWebhookClient("production", nil, hh.res, dial)
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server cert
	hh.deps = WebhookDeps{Client: client, AppEnv: "production", Secrets: hh.sec, Timeout: 5 * time.Second}
	return hh
}

func (h *harness) last() recorded { h.mu.Lock(); defer h.mu.Unlock(); return h.rec }

func cols(kv ...string) []Column {
	var out []Column
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, colgraph.MakeCol("id", kv[i], "name", kv[i+1]))
	}
	return out
}

var (
	testCols = cols("company", "Company", "email", "Email")
	testLead = map[string]any{"company": "Hot Pursuit Labs", "email": "ceo@hotpursuit.example.com", "id": json.Number("14"), "lead_id": json.Number("14")}
)

func (h *harness) run(cfg map[string]any) Result {
	return Webhook(context.Background(), h.deps, "ws-1", cfg, testLead, testCols)
}

// ---- transcripts -------------------------------------------------------------

type tExchange struct {
	DNS *struct {
		Host    string   `json:"host"`
		Answers []string `json:"answers"`
		Error   string   `json:"error"`
	} `json:"dns"`
	Request *struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    *string           `json:"body"`
	} `json:"request"`
	Response *struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Raises  string            `json:"raises"`
		Message string            `json:"message"`
	} `json:"response"`
}

type transcript struct {
	Scenario string `json:"scenario"`
	Input    struct {
		WS       string           `json:"workspace_id"`
		Cfg      map[string]any   `json:"cfg"`
		Lead     map[string]any   `json:"lead_data"`
		Columns  []map[string]any `json:"columns"`
		Allowlst []string         `json:"allowlist"`
	} `json:"input"`
	Secrets   map[string]string `json:"secrets"`
	Exchanges []tExchange       `json:"http_exchanges"`
	Result    struct {
		Status     *string `json:"status"`
		Summary    string  `json:"summary"`
		Error      *string `json:"error"`
		SkipReason *string `json:"skip_reason"`
		Raised     *struct {
			Message string `json:"message"`
		} `json:"raised"`
	} `json:"result"`
}

func loadTranscripts(t *testing.T) []transcript {
	files, err := filepath.Glob("../../testdata/transcripts/webhook/*.json")
	if err != nil || len(files) < 20 {
		t.Fatalf("webhook transcripts missing: %v (%d files)", err, len(files))
	}
	var out []transcript
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		var tr transcript
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&tr); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, tr)
	}
	return out
}

func trColumns(in []map[string]any) []Column {
	var out []Column
	for _, c := range in {
		out = append(out, colgraph.MakeCol("id", c["id"], "name", c["name"]))
	}
	return out
}

// transcript deviations from Python (documented in testdata/contract/README.md).
var transcriptDeviation = map[string]bool{
	"allowlist_subdomain_ok": true, // bare allowlist entry is exact-host only (TestAllowlistEnforced)
}

func TestWebhookTranscripts(t *testing.T) {
	for _, tr := range loadTranscripts(t) {
		tr := tr
		t.Run(tr.Scenario, func(t *testing.T) {
			if transcriptDeviation[tr.Scenario] {
				t.Skip("documented deviation")
			}
			var resp *struct {
				Status  int               `json:"status"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
				Raises  string            `json:"raises"`
				Message string            `json:"message"`
			}
			var wantReq *tExchange
			h := newHarness(t, nil)
			h.res.answers, h.res.errs = map[string][]string{}, map[string]bool{}
			for i := range tr.Exchanges {
				e := tr.Exchanges[i]
				switch {
				case e.DNS != nil:
					if e.DNS.Error != "" {
						h.res.errs[e.DNS.Host] = true
					} else {
						h.res.answers[e.DNS.Host] = e.DNS.Answers
					}
				case e.Request != nil:
					wantReq, resp = &tExchange{Request: e.Request}, e.Response
				}
			}
			if resp != nil && resp.Raises != "" {
				switch resp.Raises {
				case "ConnectError":
					h.dialErr = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
				case "ReadTimeout":
					h.dialErr = &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
				}
			}
			h.handler = func(w http.ResponseWriter, r *http.Request) {
				for k, v := range resp.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(resp.Status)
				_, _ = io.WriteString(w, resp.Body)
			}
			h.deps.Allowlist = tr.Input.Allowlst
			h.sec.byWS = map[string]map[string]string{"ws-1": tr.Secrets}
			if tr.Scenario == "secret_other_workspace_not_injected" {
				h.sec.byWS = map[string]map[string]string{"ws-2": tr.Secrets}
			}
			got := Webhook(context.Background(), h.deps, tr.Input.WS, tr.Input.Cfg, tr.Input.Lead, trColumns(tr.Input.Columns))

			if tr.Result.Raised != nil {
				if got.Status != "failed" || got.Error != truncRunes(tr.Result.Raised.Message, 200) {
					t.Fatalf("raised %q -> %+v", tr.Result.Raised.Message, got)
				}
				return
			}
			if got.Status != *tr.Result.Status {
				t.Fatalf("status %q want %q (%+v)", got.Status, *tr.Result.Status, got)
			}
			if got.Summary != tr.Result.Summary {
				t.Fatalf("summary %q want %q", got.Summary, tr.Result.Summary)
			}
			wantSkip := ""
			if tr.Result.SkipReason != nil {
				wantSkip = *tr.Result.SkipReason
			}
			if got.SkipReason != wantSkip {
				t.Fatalf("skip_reason %q want %q", got.SkipReason, wantSkip)
			}
			if tr.Result.Error != nil {
				want := *tr.Result.Error
				if strings.HasPrefix(want, "blocked url: cannot resolve host: ") {
					if !strings.HasPrefix(got.Error, "blocked url: cannot resolve host: ") {
						t.Fatalf("error %q", got.Error)
					}
				} else if got.Error != want {
					t.Fatalf("error %q want %q", got.Error, want)
				}
			} else if got.Error != "" {
				t.Fatalf("unexpected error %q", got.Error)
			}
			if wantReq == nil || (resp != nil && resp.Raises != "") {
				return
			}
			rec := h.last()
			if !rec.Present {
				t.Fatal("no request reached the server")
			}
			pu := wantReq.Request.URL // https://<pinnedIP[:port]>/path
			wantHost := strings.TrimPrefix(strings.TrimPrefix(pu, "https://"), "http://")
			wantHost, _, _ = strings.Cut(wantHost, "/")
			if !strings.Contains(wantHost, ":") || strings.HasSuffix(wantHost, "]") {
				wantHost = strings.Trim(wantHost, "[]")
				wantHost = net.JoinHostPort(wantHost, "443")
			} else if strings.HasPrefix(wantHost, "[") && !strings.Contains(wantHost, "]:") {
				wantHost = net.JoinHostPort(strings.Trim(wantHost, "[]"), "443")
			}
			if len(h.dials) != 1 || h.dials[0] != wantHost {
				t.Fatalf("dialed %v, want only the pinned %s", h.dials, wantHost)
			}
			if rec.Method != wantReq.Request.Method {
				t.Fatalf("method %s", rec.Method)
			}
			if rec.Host != wantReq.Request.Headers["host"] {
				t.Fatalf("Host %q want %q", rec.Host, wantReq.Request.Headers["host"])
			}
			if !strings.HasPrefix(wantReq.Request.Headers["host"], rec.SNI) || rec.SNI == "" {
				t.Fatalf("SNI %q vs host %q", rec.SNI, wantReq.Request.Headers["host"])
			}
			for k, v := range wantReq.Request.Headers {
				if k == "host" {
					continue
				}
				for sk, sv := range tr.Secrets {
					v = strings.ReplaceAll(v, "{{secret:"+sk+"}}", sv)
				}
				if got := rec.Header.Get(k); got != v {
					t.Fatalf("header %s=%q want %q", k, got, v)
				}
			}
			if wantReq.Request.Body != nil {
				ct := wantReq.Request.Headers["content-type"]
				if strings.Contains(ct, "json") {
					var a, b any
					_ = json.Unmarshal(rec.Body, &a)
					_ = json.Unmarshal([]byte(*wantReq.Request.Body), &b)
					if !reflect.DeepEqual(a, b) {
						t.Fatalf("body %s want %s", rec.Body, *wantReq.Request.Body)
					}
				} else if string(rec.Body) != *wantReq.Request.Body {
					t.Fatalf("body %q want %q", rec.Body, *wantReq.Request.Body)
				}
			} else if len(rec.Body) != 0 {
				t.Fatalf("unexpected body %q", rec.Body)
			}
		})
	}
}

// ---- vectors -----------------------------------------------------------------

type vec struct {
	Name     string                       `json:"name"`
	Cfg      map[string]any               `json:"cfg"`
	Lead     map[string]any               `json:"lead_data"`
	Columns  []map[string]any             `json:"columns"`
	WS       string                       `json:"ws"`
	Secrets  map[string]map[string]string `json:"secrets"`
	DNS      map[string][]string          `json:"dns"`
	Allow    []string                     `json:"allowlist"`
	Response *struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
		Raise  string `json:"raise"`
	} `json:"response"`
	Result *struct {
		Status     string  `json:"status"`
		Summary    string  `json:"summary"`
		Error      *string `json:"error"`
		SkipReason *string `json:"skip_reason"`
	} `json:"result"`
	Raised *struct {
		Message string `json:"message"`
	} `json:"raised"`
	Request *struct {
		Method  string         `json:"method"`
		Headers map[string]any `json:"headers"`
		Kwargs  map[string]any `json:"kwargs"`
	} `json:"request"`
}

// vectorDeviation: vectors whose Python outcome the Go sender deliberately
// changes (README D4 webhook hardening). Each is asserted separately below.
var vectorDeviation = map[string]bool{
	"url_cgnat": true, "url_localhost_subdomain": true, "url_loopback_short": true,
	"allowlist_hit_subdomain": true,                                // bare entry is exact-host only
	"status_304":              true, "status_199_is_failure": true, // net/http servers cannot emit a 304 body or a 1xx final status
	"secret_does_not_override_explicit_header":          true, // setdefault is case-insensitive
	"secret_lowercase_explicit_header_both_sent":        true,
	"body_plain_text_keeps_user_content_type_lowercase": true,
}

func TestWebhookVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/webhook_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []vec `json:"vectors"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&file); err != nil || len(file.Vectors) < 100 {
		t.Fatalf("vectors: %v (%d)", err, len(file.Vectors))
	}
	for _, v := range file.Vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			if vectorDeviation[v.Name] {
				t.Skip("documented deviation")
			}
			h := newHarness(t, nil)
			h.res.answers, h.res.errs = map[string][]string{}, map[string]bool{}
			for host, ans := range v.DNS {
				if ans == nil {
					h.res.errs[host] = true
				} else {
					h.res.answers[host] = ans
				}
			}
			h.deps.Allowlist = v.Allow
			h.sec.byWS = v.Secrets
			status, text := 200, ""
			if v.Response != nil {
				if v.Response.Status != 0 {
					status = v.Response.Status
				}
				text = v.Response.Text
				switch v.Response.Raise {
				case "timeout":
					h.dialErr = &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
				case "blocked":
					h.res.answers["hooks.example.com"] = []string{"10.0.0.9"}
				case "connect":
					h.dialErr = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
				case "long":
					h.dialErr = errors.New(strings.Repeat("x", 300))
				}
			}
			h.handler = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, text)
			}
			// literal-IP and http:// vectors dial the plain scheme: serve both.
			if u, _ := v.Cfg["url"].(string); strings.HasPrefix(strings.TrimSpace(u), "http://") {
				plain := httptest.NewServer(h.srv.Config.Handler)
				t.Cleanup(plain.Close)
				h.deps.Client = newWebhookClient("production", nil, h.res, func(ctx context.Context, n, a string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, n, plain.Listener.Addr().String())
				})
			}
			got := Webhook(context.Background(), h.deps, v.WS, v.Cfg, v.Lead, trColumns(v.Columns))
			if v.Raised != nil {
				if got.Status != "failed" || got.Error != truncRunes(v.Raised.Message, 200) {
					t.Fatalf("raised %q -> %+v", v.Raised.Message, got)
				}
				return
			}
			want := v.Result
			if got.Status != want.Status || got.Summary != want.Summary {
				t.Fatalf("got %+v want %+v", got, *want)
			}
			wantErr := ""
			if want.Error != nil {
				wantErr = *want.Error
			}
			if strings.HasPrefix(wantErr, "blocked url: cannot resolve host: ") {
				if !strings.HasPrefix(got.Error, "blocked url: cannot resolve host: ") {
					t.Fatalf("error %q", got.Error)
				}
			} else if got.Error != wantErr {
				t.Fatalf("error %q want %q", got.Error, wantErr)
			}
			wantSkip := ""
			if want.SkipReason != nil {
				wantSkip = *want.SkipReason
			}
			if got.SkipReason != wantSkip {
				t.Fatalf("skip %q want %q", got.SkipReason, wantSkip)
			}
			if v.Request == nil || (v.Response != nil && v.Response.Raise != "") {
				return
			}
			rec := h.last()
			if !rec.Present || rec.Method != v.Request.Method {
				t.Fatalf("request %+v want method %s", rec.Method, v.Request.Method)
			}
			for k, hv := range v.Request.Headers {
				sv := fmt.Sprint(hv)
				for ws, m := range v.Secrets {
					if ws != v.WS {
						continue
					}
					for sk, sval := range m {
						sv = strings.ReplaceAll(sv, "{{secret:"+sk+"}}", sval)
					}
				}
				if got := rec.Header.Get(k); got != sv {
					t.Fatalf("header %s=%q want %q", k, got, sv)
				}
			}
			if j, ok := v.Request.Kwargs["json"]; ok {
				var a any
				d := json.NewDecoder(strings.NewReader(string(rec.Body)))
				d.UseNumber()
				if err := d.Decode(&a); err != nil || !reflect.DeepEqual(a, normNum(j)) {
					t.Fatalf("json body %s want %v", rec.Body, j)
				}
			} else if c, ok := v.Request.Kwargs["content"]; ok {
				if string(rec.Body) != c.(string) {
					t.Fatalf("content %q want %q", rec.Body, c)
				}
			} else if len(rec.Body) != 0 {
				t.Fatalf("unexpected body %q", rec.Body)
			}
		})
	}
}

func normNum(v any) any { return v }

// ---- security / behaviour tests ----------------------------------------------

func TestBlockedURLReasonsMatchPython(t *testing.T) {
	cases := map[string]string{
		"ftp://x.example.com/":          "blocked url: scheme 'ftp' not allowed",
		"https://u:p@hooks.example.com": "blocked url: credentials in url not allowed",
		"https:///x":                    "blocked url: no host",
		"http://localhost/x":            "blocked url: host 'localhost' is blocked",
		"http://127.0.0.1/x":            "blocked url: ip '127.0.0.1' is private/blocked",
		"http://169.254.169.254/x":      "blocked url: ip '169.254.169.254' is private/blocked",
		"http://10.1.2.3/x":             "blocked url: ip '10.1.2.3' is private/blocked",
		"http://[::1]/x":                "blocked url: ip '::1' is private/blocked",
	}
	for u, want := range cases {
		h := newHarness(t, nil)
		got := h.run(map[string]any{"url": u})
		if got.Status != "failed" || got.SkipReason != "blocked_url" || got.Error != want {
			t.Errorf("%s -> %+v, want %q", u, got, want)
		}
		if h.hits.Load() != 0 || len(h.dials) != 0 {
			t.Errorf("%s reached the network", u)
		}
	}
	h := newHarness(t, nil)
	h.res.answers = map[string][]string{"hooks.example.com": {"93.184.216.34", "10.0.0.5"}}
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got.Error != "blocked url: host resolves to private ip 10.0.0.5" || got.SkipReason != "blocked_url" || len(h.dials) != 0 {
		t.Errorf("private dns: %+v dials=%v", got, h.dials)
	}
	if got := h.run(map[string]any{}); got.Status != "failed" || got.Error != "webhook url not configured" {
		t.Errorf("empty url: %+v", got)
	}
}

func TestHardenedHostForms(t *testing.T) {
	for _, u := range []string{
		"http://a.localhost/x", "http://localhost./x", "http://x.y.LOCALHOST/x",
		"http://127.1/x", "http://0x7f.1/x", "http://2130706433/x", "http://0177.0.0.1/x", "http://127.0.1/x",
		"http://100.64.0.1/x", "http://100.127.255.254/x", "https://[::ffff:100.64.0.1]/x", "https://[::ffff:127.0.0.1]/x",
		"http://0/x", "http://0.0.0.0/x",
	} {
		h := newHarness(t, nil)
		got := h.run(map[string]any{"url": u})
		if got.Status != "failed" || got.SkipReason != "blocked_url" {
			t.Errorf("%s -> %+v", u, got)
		}
		if h.hits.Load() != 0 || len(h.dials) != 0 || len(h.res.calls) != 0 {
			t.Errorf("%s touched resolver/network: dials=%v calls=%v", u, h.dials, h.res.calls)
		}
	}
	// A public 100.x address outside 100.64/10 is fine.
	h := newHarness(t, nil)
	if got := h.run(map[string]any{"url": "https://100.128.0.1/x"}); got.Status != "success" {
		t.Errorf("100.128.0.1 -> %+v", got)
	}
}

func TestCGNATBlockedAtDialForResolvedHost(t *testing.T) {
	h := newHarness(t, nil)
	h.res.answers = map[string][]string{"hooks.example.com": {"100.64.1.1"}}
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got.Status != "failed" || got.SkipReason != "blocked_url" || got.Error != "blocked url: host resolves to private ip 100.64.1.1" || len(h.dials) != 0 {
		t.Fatalf("%+v dials=%v", got, h.dials)
	}
	h2 := newHarness(t, nil)
	h2.res.answers = map[string][]string{"hooks.example.com": {"::ffff:100.64.1.1"}}
	if got := h2.run(map[string]any{"url": "https://hooks.example.com/x"}); got.SkipReason != "blocked_url" || len(h2.dials) != 0 {
		t.Fatalf("mapped cgnat: %+v", got)
	}
}

func TestTemplatedURLResolvingToLoopbackBlocked(t *testing.T) {
	for _, val := range []string{"127.0.0.1", "127.1", "a.localhost", "100.64.0.9", "localhost", "169.254.169.254"} {
		h := newHarness(t, nil)
		lead := map[string]any{"company": "x", "email": val}
		got := Webhook(context.Background(), h.deps, "ws-1", map[string]any{"url": "http://{email}/x"}, lead, testCols)
		if got.Status != "failed" || got.SkipReason != "blocked_url" || h.hits.Load() != 0 || len(h.dials) != 0 {
			t.Errorf("%s -> %+v", val, got)
		}
	}
}

func TestIPv4MappedIPv6Blocked(t *testing.T) {
	for _, u := range []string{"http://[::ffff:127.0.0.1]/", "http://[::ffff:7f00:1]/", "http://[::ffff:10.0.0.1]/", "http://[::ffff:169.254.169.254]/"} {
		h := newHarness(t, nil)
		got := h.run(map[string]any{"url": u})
		if got.Status != "failed" || got.SkipReason != "blocked_url" || len(h.dials) != 0 {
			t.Errorf("%s -> %+v", u, got)
		}
	}
	h := newHarness(t, nil)
	h.res.answers = map[string][]string{"hooks.example.com": {"::ffff:10.0.0.1"}}
	if got := h.run(map[string]any{"url": "https://hooks.example.com/x"}); got.SkipReason != "blocked_url" || len(h.dials) != 0 {
		t.Errorf("mapped dns answer: %+v", got)
	}
}

func TestDNSRebindingBlockedAtDial(t *testing.T) {
	h := newHarness(t, nil)
	// lookup 1 public, lookup 2 (at dial) private: the dial must refuse it.
	h.res.flip = map[string][]string{"hooks.example.com": {"127.0.0.1"}}
	h.res.answers = map[string][]string{"hooks.example.com": {"93.184.216.34"}}
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	// Whether the sender looks up once or twice, no connection may reach a blocked IP.
	for _, d := range h.dials {
		if strings.HasPrefix(d, "127.") {
			t.Fatalf("dialed rebound address %s", d)
		}
	}
	if h.res.calls["hooks.example.com"] > 1 && (got.Status != "failed" || got.SkipReason != "blocked_url") {
		t.Fatalf("rebind not blocked: %+v", got)
	}
	// The dial itself must re-resolve and vet: drive the client directly.
	h2 := newHarness(t, nil)
	h2.res.answers = map[string][]string{"hooks.example.com": {"93.184.216.34"}}
	_ = h2.run(map[string]any{"url": "https://hooks.example.com/x"})
	h2.res.answers = map[string][]string{"hooks.example.com": {"127.0.0.1"}}
	h2.deps.Client.CloseIdleConnections()
	got2 := h2.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got2.Status != "failed" || got2.SkipReason != "blocked_url" || got2.Error != "blocked url: host resolves to private ip 127.0.0.1" {
		t.Fatalf("flip after check: %+v", got2)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var other atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			other.Add(1)
		}
		w.Header().Set("Location", "https://hooks.example.com/other")
		w.WriteHeader(302)
	})
	got := h.run(map[string]any{"url": "https://hooks.example.com/in"})
	if got.Status != "failed" || got.Summary != "POST 302" || got.Error != "HTTP 302: " || other.Load() != 0 || h.hits.Load() != 1 {
		t.Fatalf("%+v hits=%d", got, h.hits.Load())
	}
}

func TestAllowlistEnforced(t *testing.T) {
	type tc struct {
		allow []string
		url   string
		ok    bool
	}
	for _, c := range []tc{
		{[]string{"hooks.example.com"}, "https://hooks.example.com/x", true},
		{[]string{"hooks.example.com"}, "https://HOOKS.Example.com/x", true},
		{[]string{"hooks.example.com"}, "https://a.hooks.example.com/x", false}, // bare = exact
		{[]string{".example.com"}, "https://a.hooks.example.com/x", true},
		{[]string{".example.com"}, "https://example.com/x", false},
		{[]string{".example.com"}, "https://evilexample.com/x", false},
		{[]string{"example.com"}, "https://evilexample.com/x", false},
		{[]string{"*.example.com"}, "https://a.example.com/x", false}, // no globs
		{[]string{"*"}, "https://a.example.com/x", false},
		{[]string{"example.*"}, "https://example.com/x", false},
		{[]string{"hooks.example.com"}, "https://93.184.216.34/x", false},
		{[]string{"hooks.example.com"}, "https://evil.example.org/x", false},
		{[]string{"hooks.example.com"}, "https://hooks.example.com./x", true},
	} {
		h := newHarness(t, nil)
		h.deps.Allowlist = c.allow
		got := h.run(map[string]any{"url": c.url})
		if c.ok && got.Status != "success" {
			t.Errorf("%v %s -> %+v, want success", c.allow, c.url, got)
		}
		if !c.ok && (got.Status != "failed" || got.SkipReason != "blocked_url" || !strings.Contains(got.Error, "not in webhook domain allowlist") || len(h.dials) != 0) {
			t.Errorf("%v %s -> %+v, want allowlist refusal", c.allow, c.url, got)
		}
	}
}

func TestHeaderSecretOnlyFromOwnWorkspace(t *testing.T) {
	h := newHarness(t, nil)
	h.sec.byWS = map[string]map[string]string{"ws-1": {"HOOK": "ctk_transcript_hook_0001"}, "ws-2": {"HOOK": "ctk_other_ws"}}
	h.sec.global = map[string]string{"GLOBAL_KEY": "ctk_global_secret", "DATABASE_URL": "postgres://x"}
	cfg := map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": "HOOK"}
	if got := h.run(cfg); got.Status != "success" || h.last().Header.Get("Authorization") != "ctk_transcript_hook_0001" {
		t.Fatalf("own ws: %+v auth=%q", got, h.last().Header.Get("Authorization"))
	}
	// ws-3 has no such secret: nothing injected (never ws-1's, never global).
	_ = Webhook(context.Background(), h.deps, "ws-3", cfg, testLead, testCols)
	if a := h.last().Header.Get("Authorization"); a != "" {
		t.Fatalf("injected foreign secret %q", a)
	}
	// Global / env fallback is refused (hardening): only workspace secrets inject.
	for _, ref := range []string{"GLOBAL_KEY", "DATABASE_URL"} {
		_ = h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": ref})
		if a := h.last().Header.Get("Authorization"); a != "" {
			t.Fatalf("global secret %s injected", ref)
		}
	}
	// custom header name + explicit header wins (case-insensitive setdefault).
	_ = h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": "HOOK", "header_name": "X-Api-Key"})
	if h.last().Header.Get("X-Api-Key") != "ctk_transcript_hook_0001" {
		t.Fatal("custom header name not used")
	}
	_ = h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": "HOOK", "headers": map[string]any{"authorization": "Bearer mine"}})
	if h.last().Header.Get("Authorization") != "Bearer mine" {
		t.Fatalf("explicit header overridden: %q", h.last().Header.Get("Authorization"))
	}
}

func TestHeaderSecretNotLoggedOrInErrors(t *testing.T) {
	const sec = "ctk_transcript_hook_0001"
	h := newHarness(t, nil)
	h.sec.byWS = map[string]map[string]string{"ws-1": {"HOOK": sec}}
	var logbuf strings.Builder
	h.deps.Log = newCaptureLogger(&logbuf)
	h.dialErr = errors.New("boom " + sec + " Bearer " + sec + " https://hooks.example.com/x?token=QSECRET123&a=b")
	cfg := map[string]any{"url": "https://hooks.example.com/x?token=QSECRET123&a=b", "header_secret_ref": "HOOK", "headers": map[string]any{"X-Tok": "HDRVALUE99"}}
	got := h.run(cfg)
	if got.Status != "failed" {
		t.Fatalf("%+v", got)
	}
	for _, leak := range []string{sec, "QSECRET123", "token=", "HDRVALUE99"} {
		if strings.Contains(got.Error, leak) || strings.Contains(got.Summary, leak) || strings.Contains(logbuf.String(), leak) {
			t.Fatalf("leaked %q: err=%q log=%q", leak, got.Error, logbuf.String())
		}
	}
	// non-2xx path and success path log nothing sensitive either.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	h2.sec.byWS = h.sec.byWS
	var lb2 strings.Builder
	h2.deps.Log = newCaptureLogger(&lb2)
	_ = h2.run(cfg)
	if strings.Contains(lb2.String(), sec) || strings.Contains(lb2.String(), "QSECRET123") {
		t.Fatalf("log leak %q", lb2.String())
	}
	// secret store failure: generic error, no detail.
	h3 := newHarness(t, nil)
	h3.sec.failErr = errors.New("db password=" + sec)
	got3 := h3.run(cfg)
	if got3.Status != "failed" || strings.Contains(got3.Error, sec) || h3.hits.Load() != 0 {
		t.Fatalf("secret lookup failure: %+v", got3)
	}
}

func TestBodyShapes(t *testing.T) {
	type tc struct {
		name, ct string
		cfg      map[string]any
		check    func(t *testing.T, body []byte)
	}
	jsonEq := func(want string) func(*testing.T, []byte) {
		return func(t *testing.T, b []byte) {
			var x, y any
			if json.Unmarshal(b, &x) != nil || json.Unmarshal([]byte(want), &y) != nil || !reflect.DeepEqual(x, y) {
				t.Fatalf("body %s want %s", b, want)
			}
		}
	}
	cases := []tc{
		{"nil", "application/json", map[string]any{}, jsonEq(`{"company":"Hot Pursuit Labs","email":"ceo@hotpursuit.example.com","id":14,"lead_id":14}`)},
		{"dict", "application/json", map[string]any{"body": map[string]any{"c": "{company}", "n": 1, "l": []any{"{email}"}}}, jsonEq(`{"c":"Hot Pursuit Labs","n":1,"l":["ceo@hotpursuit.example.com"]}`)},
		{"list", "application/json", map[string]any{"body": []any{"{company}", float64(2)}}, jsonEq(`["Hot Pursuit Labs",2]`)},
		{"json string array", "application/json", map[string]any{"body": `["{company}"]`}, nil},
		{"json string number", "application/json", map[string]any{"body": "5"}, jsonEq(`5`)},
		{"plain string", "text/plain", map[string]any{"body": "hello {company}"}, func(t *testing.T, b []byte) {
			if string(b) != "hello Hot Pursuit Labs" {
				t.Fatalf("%q", b)
			}
		}},
	}
	for _, c := range cases {
		h := newHarness(t, nil)
		cfg := map[string]any{"url": "https://hooks.example.com/x", "method": "put"}
		for k, v := range c.cfg {
			cfg[k] = v
		}
		got := h.run(cfg)
		if c.name == "json string array" {
			// braces/brackets are plain here: a "[...]" string with a placeholder resolves then parses.
			if got.Status != "success" || h.last().Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%s: %+v ct=%s", c.name, got, h.last().Header.Get("Content-Type"))
			}
			jsonEq(`["Hot Pursuit Labs"]`)(t, h.last().Body)
			continue
		}
		if got.Status != "success" || got.Summary != "PUT 200" {
			t.Fatalf("%s: %+v", c.name, got)
		}
		if ct := h.last().Header.Get("Content-Type"); ct != c.ct {
			t.Fatalf("%s: content-type %q want %q", c.name, ct, c.ct)
		}
		c.check(t, h.last().Body)
	}
	// GET never sends a body.
	h := newHarness(t, nil)
	_ = h.run(map[string]any{"url": "https://hooks.example.com/x", "method": "GET", "body": map[string]any{"a": 1}})
	if len(h.last().Body) != 0 {
		t.Fatal("GET sent a body")
	}
	// unknown template field fails closed with the Python message.
	h = newHarness(t, nil)
	got := h.run(map[string]any{"url": "https://hooks.example.com/x", "body": map[string]any{"a": "{nope}"}})
	if got.Status != "failed" || !strings.Contains(got.Error, "Unknown column reference: nope") || h.hits.Load() != 0 {
		t.Fatalf("%+v", got)
	}
	// disallowed methods are refused before any connect.
	for _, m := range []string{"CONNECT", "TRACE", "BREW", "PO ST"} {
		h = newHarness(t, nil)
		got = h.run(map[string]any{"url": "https://hooks.example.com/x", "method": m})
		if got.Status != "failed" || h.hits.Load() != 0 || len(h.dials) != 0 {
			t.Errorf("%s: %+v", m, got)
		}
	}
}

func TestNon2xxCarriesBodyPrefix160(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, strings.Repeat("é", 400))
	})
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	want := "HTTP 503: " + strings.Repeat("é", 160)
	if got.Status != "failed" || got.Summary != "POST 503" || got.Error != want {
		t.Fatalf("%+v", got)
	}
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404); _, _ = io.WriteString(w, "nope") })
	if g := h2.run(map[string]any{"url": "https://hooks.example.com/x"}); g.Error != "HTTP 404: nope" {
		t.Fatalf("%+v", g)
	}
}

func TestOversizedResponseBounded(t *testing.T) {
	var written atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		chunk := []byte(strings.Repeat("y", 64<<10))
		for i := 0; i < 1024; i++ { // 64 MiB offered
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got.Status != "failed" || got.Error != "HTTP 500: "+strings.Repeat("y", 160) {
		t.Fatalf("%+v", got)
	}
	if written.Load() > 32<<20 {
		t.Fatalf("server wrote %d bytes: response not bounded", written.Load())
	}
	// request body bound: an absurd dict body is refused, not sent.
	h2 := newHarness(t, nil)
	big := strings.Repeat("z", 2<<20)
	g := Webhook(context.Background(), h2.deps, "ws-1", map[string]any{"url": "https://hooks.example.com/x"}, map[string]any{"company": big, "email": "e"}, testCols)
	if g.Status != "failed" || h2.hits.Load() != 0 {
		t.Fatalf("oversized request body sent: %+v", g)
	}
}

func TestContextCancelAborts(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- Webhook(ctx, h.deps, "ws-1", map[string]any{"url": "https://hooks.example.com/x"}, testLead, testCols)
	}()
	for h.hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case r := <-done:
		if r.Status != "failed" {
			t.Fatalf("%+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not abort the request")
	}
}

func TestTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	h.deps.Timeout = 150 * time.Millisecond
	start := time.Now()
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got.Status != "failed" || got.Error != "timed out" || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %v", got, time.Since(start))
	}
}

func TestPinnedDialOnlyToValidatedIP(t *testing.T) {
	h := newHarness(t, nil)
	h.res.answers = map[string][]string{"hooks.example.com": {"93.184.216.34"}}
	_ = h.run(map[string]any{"url": "https://hooks.example.com:8443/x"})
	if !reflect.DeepEqual(h.dials, []string{"93.184.216.34:8443"}) {
		t.Fatalf("dials %v", h.dials)
	}
	if r := h.last(); r.SNI != "hooks.example.com" || r.Host != "hooks.example.com:8443" {
		t.Fatalf("sni/host %q %q", r.SNI, r.Host)
	}
}

func TestHeaderCountAndHostOverride(t *testing.T) {
	h := newHarness(t, nil)
	hs := map[string]any{}
	for i := 0; i < 200; i++ {
		hs[fmt.Sprintf("X-H-%d", i)] = "v"
	}
	got := h.run(map[string]any{"url": "https://hooks.example.com/x", "headers": hs})
	if got.Status != "failed" || h.hits.Load() != 0 {
		t.Fatalf("too many headers sent: %+v", got)
	}
	h = newHarness(t, nil)
	_ = h.run(map[string]any{"url": "https://hooks.example.com/x", "headers": map[string]any{"Host": "evil.internal", "X-A": "1\r\nX-Injected: 1"}})
	if h.last().Host == "evil.internal" || h.last().Header.Get("X-Injected") != "" {
		t.Fatalf("host override/header injection: %+v", h.last())
	}
}

func newCaptureLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
