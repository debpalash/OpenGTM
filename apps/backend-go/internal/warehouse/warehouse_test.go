package warehouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
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

type scenario struct {
	Name        string `json:"name"`
	RunID       string `json:"run_id"`
	WorkspaceID string `json:"workspace_id"`
	Destination struct {
		ID         string         `json:"id"`
		AudienceID string         `json:"audience_id"`
		Config     map[string]any `json:"config"`
	} `json:"destination"`
	Rows []struct {
		LeadID  int            `json:"lead_id"`
		Payload map[string]any `json:"payload"`
	} `json:"rows"`
	File           string         `json:"file"`
	FileSHA        string         `json:"file_sha256"`
	Manifest       map[string]any `json:"manifest"`
	RowCount       int            `json:"row_count"`
	RolledToDisk   bool           `json:"rolled_to_disk"`
	MaxMemoryBytes int            `json:"max_memory_bytes"`
}

func loadScenarios(t testing.TB) []scenario {
	t.Helper()
	var v struct {
		Scenarios []scenario `json:"scenarios"`
	}
	readJSON(t, "testdata/manifest.json", &v)
	if len(v.Scenarios) < 10 {
		t.Fatalf("vectors missing: %d scenarios", len(v.Scenarios))
	}
	return v.Scenarios
}

func (s scenario) dest() DestRef {
	return DestRef{ID: s.Destination.ID, AudienceID: s.Destination.AudienceID, Config: s.Destination.Config}
}

func (s scenario) build(t testing.TB) *Buffer {
	t.Helper()
	b := NewBuffer(s.RunID, s.WorkspaceID, s.dest(), s.MaxMemoryBytes)
	for _, r := range s.Rows {
		if err := b.Append(r.LeadID, r.Payload); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestExportBytesMatchPythonGoldens(t *testing.T) {
	for _, s := range loadScenarios(t) {
		t.Run(s.Name, func(t *testing.T) {
			want, err := os.ReadFile("../../" + s.File)
			if err != nil {
				t.Fatal(err)
			}
			if sum := sha256.Sum256(want); hex.EncodeToString(sum[:]) != s.FileSHA {
				t.Fatalf("golden file corrupt")
			}
			b := s.build(t)
			defer b.Close()
			if b.RolledToDisk() != s.RolledToDisk {
				t.Fatalf("RolledToDisk = %v want %v", b.RolledToDisk(), s.RolledToDisk)
			}
			got, err := io.ReadAll(b.Reader())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("bytes differ from Python golden:\n got %q\nwant %q", clip(got), clip(want))
			}
			// The reader is repeatable (Python re-seeks).
			again, _ := io.ReadAll(b.Reader())
			if !bytes.Equal(again, want) {
				t.Fatal("second read differs")
			}
		})
	}
}

func clip(b []byte) string {
	if len(b) > 600 {
		return string(b[:600]) + "..."
	}
	return string(b)
}

func TestManifestMatchesVectors(t *testing.T) {
	for _, s := range loadScenarios(t) {
		t.Run(s.Name, func(t *testing.T) {
			b := s.build(t)
			defer b.Close()
			if b.RowCount() != s.RowCount {
				t.Fatalf("RowCount %d want %d", b.RowCount(), s.RowCount)
			}
			got := b.Manifest()
			gj, err := pyJSON(got)
			if err != nil {
				t.Fatal(err)
			}
			wj, err := pyJSON(s.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gj, wj) {
				t.Fatalf("manifest\n got %s\nwant %s", gj, wj)
			}
			if len(got) != 10 {
				t.Fatalf("manifest key set: %v", got)
			}
		})
	}
}

func lineLen(t testing.TB, id int, p map[string]any) int {
	t.Helper()
	rec := map[string]any{"_opengtm_lead_id": id}
	for k, v := range p {
		rec[k] = v
	}
	raw, err := pyJSON(rec)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw) + 1
}

func TestSpillsAtBoundAndUnlinks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	payload := map[string]any{"company": "Acme", "email": "a@b.co"}
	n := lineLen(t, 1, payload)
	dest := DestRef{ID: "d", AudienceID: "a"}

	// Exactly at the bound stays in memory (Python rolls when size > max).
	b := NewBuffer("r", "w", dest, n)
	if err := b.Append(1, payload); err != nil {
		t.Fatal(err)
	}
	if b.RolledToDisk() {
		t.Fatal("rolled at the bound")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatal("temp file created while in memory")
	}
	if err := b.Append(1, payload); err != nil {
		t.Fatal(err)
	}
	if !b.RolledToDisk() {
		t.Fatal("did not roll past the bound")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("spill file is still named on disk: %v", ents)
	}
	// Data survives the spill and further appends.
	if err := b.Append(2, payload); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(b.Reader())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "\n"); got != 4 { // manifest + 3 records
		t.Fatalf("lines = %d", got)
	}
	b.Close()

	// Default bound is 1,000,000 bytes.
	d := NewBuffer("r", "w", dest, 0)
	defer d.Close()
	big := map[string]any{"x": strings.Repeat("a", 400_000)}
	for i := 0; i < 2; i++ {
		_ = d.Append(i, big)
	}
	if d.RolledToDisk() {
		t.Fatal("rolled below default bound")
	}
	_ = d.Append(3, big)
	if !d.RolledToDisk() {
		t.Fatal("default bound not enforced")
	}
}

func TestTempFileMode0600(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	b := NewBuffer("r", "w", DestRef{}, 1)
	defer b.Close()
	if err := b.Append(1, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if !b.RolledToDisk() || b.file == nil {
		t.Fatal("expected spill")
	}
	st, err := b.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
}

func TestCloseRemovesData(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	b := NewBuffer("r", "w", DestRef{}, 1)
	_ = b.Append(1, map[string]any{"a": 1})
	f := b.file
	b.Close()
	if _, err := f.Stat(); err == nil {
		t.Fatal("spill descriptor still open after Close")
	}
	if err := b.Append(2, nil); err == nil {
		t.Fatal("Append after Close must fail")
	}
	if _, err := io.ReadAll(b.Reader()); err == nil {
		t.Fatal("Reader after Close must fail")
	}
	b.Close() // idempotent

	m := NewBuffer("r", "w", DestRef{}, 0)
	_ = m.Append(1, map[string]any{"a": 1})
	m.Close()
	if _, err := io.ReadAll(m.Reader()); err == nil {
		t.Fatal("in-memory Reader after Close must fail")
	}
}

func TestFloatAndUnicodeReprMatchPython(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{1.0, `1.0`},
		{1e22, `1e+22`},
		{1e-7, `1e-07`},
		{json.Number("-0.0"), `-0.0`},
		{json.Number("1E22"), `1e+22`},
		{json.Number("1.50"), `1.5`},
		{json.Number("12345678901234567890"), `12345678901234567890`},
		{json.Number("-0"), `0`},
		{0.1, `0.1`},
		{1e16, `1e+16`},
		{1e15, `1000000000000000.0`},
		{123456789012345678.0, `1.2345678901234568e+17`},
		{0.0001, `0.0001`},
		{0.00001, `1e-05`},
		{5e-324, `5e-324`},
		{float32(0.5), `0.5`},
		{int64(-7), `-7`},
		{uint8(7), `7`},
		{true, `true`},
		{nil, `null`},
		{"\x7f\x1f\u2028/<>&\U0001F600\b\f\"\\", `"\u007f\u001f\u2028/<>&\ud83d\ude00\b\f\"\\"`},
		{"東京é", `"\u6771\u4eac\u00e9"`},
		{[]any{1.0, "a"}, `[1.0,"a"]`},
		{[]string{"b", "a"}, `["b","a"]`},
		{map[string]any{"b": 1, "a": map[string]any{"z": nil, "y": []any{}}, "é": 2}, `{"a":{"y":[],"z":null},"b":1,"\u00e9":2}`},
		{struct{ X int }{1}, `"{1}"`}, // default=str
	}
	for _, c := range cases {
		got, err := pyJSON(c.in)
		if err != nil {
			t.Fatalf("%#v: %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("%#v: got %s want %s", c.in, got, c.want)
		}
	}
	// Python json.dumps default allow_nan.
	for _, c := range []struct {
		in   float64
		want string
	}{{inf(1), "Infinity"}, {inf(-1), "-Infinity"}} {
		if got, _ := pyJSON(c.in); string(got) != c.want {
			t.Errorf("%v: got %s", c.in, got)
		}
	}
	if got, _ := pyJSON(nan()); string(got) != "NaN" {
		t.Errorf("NaN: %s", got)
	}
	ts := time.Date(2024, 1, 2, 3, 4, 5, 6000, time.UTC)
	if got, _ := pyJSON(ts); string(got) != `"2024-01-02 03:04:05.000006+00:00"` {
		t.Errorf("time: %s", got)
	}
	if _, err := pyJSON("\xff"); err == nil {
		t.Error("invalid UTF-8 must be rejected")
	}
}

func nan() float64 { var z float64; return z / z }
func inf(s int) float64 {
	var z float64
	if s < 0 {
		return -1 / z
	}
	return 1 / z
}

// ---- Sync ----

type transcriptCase struct {
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
	Secret string         `json:"secret"`
	Rows   []struct {
		LeadID  int            `json:"lead_id"`
		Payload map[string]any `json:"payload"`
	} `json:"rows"`
	Response *struct {
		Status  int               `json:"status"`
		Text    string            `json:"text"`
		Headers map[string]string `json:"headers"`
		JSON    any               `json:"json"`
	} `json:"response"`
	Request *struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Method  string            `json:"method"`
		Sha     string            `json:"body_sha256"`
		Bytes   int               `json:"body_bytes"`
		First   string            `json:"body_first_line"`
	} `json:"request"`
	Raises string `json:"raises"`
	Result *struct {
		Success    bool    `json:"success"`
		Summary    string  `json:"summary"`
		Error      *string `json:"error"`
		ExternalID *string `json:"external_id"`
	} `json:"result"`
}

func loadCases(t testing.TB) []transcriptCase {
	t.Helper()
	var v struct {
		Cases []transcriptCase `json:"cases"`
	}
	readJSON(t, "testdata/sync_transcripts.json", &v)
	if len(v.Cases) < 25 {
		t.Fatalf("vectors missing: %d", len(v.Cases))
	}
	return v.Cases
}

const testSecret = "s3cr3t-warehouse-token"

func getter(m map[string]string, calls *int32) SecretGetter {
	return func(_ context.Context, ws, name string) (string, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if ws != "ws-vec" {
			return "", errors.New("wrong workspace")
		}
		return m[name], nil
	}
}

// tlsClient returns a client trusting srv's certificate, allowed to reach loopback.
func tlsClient(srv *httptest.Server) Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	// The recorded vectors use bare secret names; the production namespace is tested in outbound_test.go.
	return newClient("test", []string{"127.0.0.1"}, pool).WithOutbound(secrets.OutboundPolicy{Prefixes: []string{"WAREHOUSE_AUTH", "K"}})
}

func caseBuffer(t testing.TB, c transcriptCase) (*Buffer, DestRef) {
	dest := DestRef{ID: "dest-9", AudienceID: "aud-9", Config: c.Config}
	b := NewBuffer("run-9", "ws-vec", dest, 0)
	for _, r := range c.Rows {
		if err := b.Append(r.LeadID, r.Payload); err != nil {
			t.Fatal(err)
		}
	}
	return b, dest
}

type seen struct {
	method  string
	headers http.Header
	body    []byte
	te      []string
	clen    int64
	n       int32
}

func recorder(status int, hdr map[string]string, body string, s *seen) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method, s.headers, s.te, s.clen = r.Method, r.Header.Clone(), r.TransferEncoding, r.ContentLength
		s.body, _ = io.ReadAll(r.Body)
		atomic.AddInt32(&s.n, 1)
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func TestSyncRequestMatchesTranscript(t *testing.T) {
	ran := 0
	for _, c := range loadCases(t) {
		if c.Raises != "" || c.Response == nil || c.Response.Status < 200 || c.Request == nil || c.Config["header_secret_ref"] == "" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			body := c.Response.Text
			if c.Response.JSON != nil {
				raw, _ := json.Marshal(c.Response.JSON)
				body = string(raw)
			}
			var s seen
			srv := recorder(c.Response.Status, c.Response.Headers, body, &s)
			defer srv.Close()
			cfg := map[string]any{}
			for k, v := range c.Config {
				cfg[k] = v
			}
			cfg["url"] = srv.URL + "/ingest"
			c.Config = cfg
			b, dest := caseBuffer(t, c)
			defer b.Close()
			res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", dest, b, getter(map[string]string{"WAREHOUSE_AUTH": testSecret}, nil))
			if err != nil {
				t.Fatal(err)
			}
			if s.n != 1 || s.method != c.Request.Method {
				t.Fatalf("requests=%d method=%s", s.n, s.method)
			}
			hn := "Authorization"
			if v, ok := c.Config["header_name"].(string); ok && v != "" {
				hn = v
			}
			if s.headers.Get(hn) != testSecret {
				t.Errorf("%s header = %q", hn, s.headers.Get(hn))
			}
			if got := s.headers.Get("Content-Type"); got != c.Request.Headers["Content-Type"] {
				t.Errorf("Content-Type %q", got)
			}
			if got := s.headers.Get("Idempotency-Key"); got != c.Request.Headers["Idempotency-Key"] {
				t.Errorf("Idempotency-Key %q", got)
			}
			if s.clen != -1 || len(s.te) == 0 || s.te[0] != "chunked" {
				t.Errorf("body not chunked: len=%d te=%v", s.clen, s.te)
			}
			sum := sha256.Sum256(s.body)
			if hex.EncodeToString(sum[:]) != c.Request.Sha || len(s.body) != c.Request.Bytes {
				t.Errorf("body sha/len %x/%d want %s/%d", sum, len(s.body), c.Request.Sha, c.Request.Bytes)
			}
			if first, _, _ := strings.Cut(string(s.body), "\n"); first != c.Request.First {
				t.Errorf("first line %q", first)
			}
			want := c.Result
			if res.Success != want.Success || res.Summary != want.Summary {
				t.Errorf("result %+v want %+v", res, want)
			}
			wantErr := deref(want.Error)
			if rest, ok := strings.CutPrefix(wantErr, "warehouse HTTP "); ok {
				// S-L27: Python appended the receiver's body; Go stores the status alone.
				code, _, _ := strings.Cut(rest, ":")
				wantErr = "warehouse HTTP " + code
			}
			if res.Error != wantErr {
				t.Errorf("error %q want %q", res.Error, wantErr)
			}
			if wantID := deref(want.ExternalID); res.ExternalID != wantID {
				t.Errorf("external_id %q want %q", res.ExternalID, wantID)
			}
			if strings.Contains(res.Summary+res.Error, testSecret) {
				t.Error("secret leaked into result")
			}
		})
	}
	if ran < 20 {
		t.Fatalf("only %d transcript cases replayed", ran)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestSyncMissingSecret(t *testing.T) {
	var s seen
	srv := recorder(200, nil, "", &s)
	defer srv.Close()
	for _, ref := range []any{"WAREHOUSE_AUTH", "", nil} {
		cfg := map[string]any{"url": srv.URL}
		if ref != nil {
			cfg["header_secret_ref"] = ref
		}
		b := NewBuffer("run-9", "ws-vec", DestRef{ID: "d", Config: cfg}, 0)
		_ = b.Append(1, map[string]any{"a": 1})
		res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{ID: "d", Config: cfg}, b, getter(nil, nil))
		b.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.Success || res.Error != "warehouse authentication secret is missing" || res.Summary != "" {
			t.Fatalf("ref %v: %+v", ref, res)
		}
	}
	if s.n != 0 {
		t.Fatal("request sent without a secret")
	}
	// A failing getter is an error and never echoes the getter's text.
	b := NewBuffer("r", "ws-vec", DestRef{}, 0)
	defer b.Close()
	_, err := Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}, b,
		func(context.Context, string, string) (string, error) {
			return "", errors.New("vault says " + testSecret)
		})
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("err = %v", err)
	}
	// Missing url is a config error.
	_, err = Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{Config: map[string]any{"header_secret_ref": "K"}}, b, getter(map[string]string{"K": "v"}, nil))
	if err == nil {
		t.Fatal("missing url must error")
	}
}

func TestSyncNon2xxTruncatedBody(t *testing.T) {
	// 199 cannot be a final status on a real server; classify is the shared decision.
	if r := classify(199, http.Header{}, []byte("early")); r.Success || r.Error != "warehouse HTTP 199" {
		t.Fatalf("199: %+v", r)
	}
	for _, code := range []int{300, 302, 400, 401, 500, 503} {
		long := strings.Repeat("é", 400)
		var s seen
		srv := recorder(code, map[string]string{"Location": "https://elsewhere.example/"}, long+"\x1b[31m", &s)
		b := NewBuffer("run-9", "ws-vec", DestRef{ID: "d"}, 0)
		_ = b.Append(1, map[string]any{"a": 1})
		dest := DestRef{ID: "d", Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}
		res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", dest, b, getter(map[string]string{"K": testSecret}, nil))
		b.Close()
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "warehouse HTTP " + itoa(code)
		if res.Success || res.Error != want || res.ExternalID != "" || res.Summary != "" {
			t.Fatalf("%d: %+v", code, res)
		}
	}
	// The receiver's body is never read into the error, however large.
	huge := bytes.Repeat([]byte("z"), 5<<20)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(502)
		_, _ = w.Write(huge)
	}))
	defer srv.Close()
	b := NewBuffer("run-9", "ws-vec", DestRef{}, 0)
	defer b.Close()
	res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}, b, getter(map[string]string{"K": "v"}, nil))
	if err != nil || res.Error != "warehouse HTTP 502" {
		t.Fatalf("huge body: %v %q", err, res.Error)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestExternalIDSources(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	for _, c := range []struct {
		hdr  http.Header
		body string
		want string
	}{
		{h("x-job-id", "j", "location", "l"), "", "j"},
		{h("location", "l"), "", "l"},
		{h("x-job-id", ""), `{"id":"from-body"}`, "from-body"},
		{h(), `{"job_id":"j-1","id":"i-1"}`, "j-1"},
		{h(), `{"id":99}`, "99"},
		{h(), `{"job_id":0}`, ""},
		{h(), `{"job_id":0,"id":7}`, "7"},
		{h(), `{"job_id":null,"id":null}`, ""},
		{h(), `{"job_id":1.5}`, "1.5"},
		{h(), `{"job_id":1e22}`, "1e+22"},
		{h(), `{"id":12345678901234567890}`, "12345678901234567890"},
		{h(), `{"id":true}`, "True"},
		{h(), `{"id":[1]}`, "[1]"},
		{h(), `{}`, ""},
		{h(), `ok`, ""},
		{h(), `[1,2]`, ""},
		{h(), `"x"`, ""},
		{h(), ``, ""},
	} {
		r := classify(200, c.hdr, []byte(c.body))
		if !r.Success || r.ExternalID != c.want {
			t.Errorf("%v %q: got %+v want id %q", c.hdr, c.body, r, c.want)
		}
	}
}

func TestBlockedHostRefused(t *testing.T) {
	var s seen
	srv := recorder(200, nil, "", &s)
	defer srv.Close()
	// Production-shaped client: no allowlist, no extra roots.
	prod := NewClient("production", nil)
	for _, u := range []string{
		srv.URL,                          // loopback literal
		"https://localhost:1/x",          // blocked hostname
		"https://169.254.169.254/latest", // metadata
		"https://10.0.0.5/x",             // private
		"https://2130706433/x",           // decimal loopback
		"https://u:p@example.com/x",      // credentials in url
	} {
		b := NewBuffer("run-9", "ws-vec", DestRef{}, 0)
		_ = b.Append(1, map[string]any{"a": 1})
		var calls int32
		_, err := Sync(context.Background(), prod, "ws-vec", DestRef{Config: map[string]any{"url": u, "header_secret_ref": "K"}}, b, getter(map[string]string{"K": testSecret}, &calls))
		b.Close()
		if err == nil {
			t.Errorf("%s: expected refusal", u)
			continue
		}
		if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "p@") {
			t.Errorf("%s: leaked: %v", u, err)
		}
	}
	if s.n != 0 {
		t.Fatal("blocked host was contacted")
	}
	// Allowlisted test host is honoured only in test env, never in production.
	if _, err := Sync(context.Background(), newClient("production", []string{"127.0.0.1"}, nil), "ws-vec",
		DestRef{Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}, NewBuffer("r", "ws-vec", DestRef{}, 0), getter(map[string]string{"K": "v"}, nil)); err == nil {
		t.Fatal("allowlist honoured in production")
	}
}

func TestHTTPSOnlyEnforcedAtSend(t *testing.T) {
	var s seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&s.n, 1) }))
	defer srv.Close()
	for _, u := range []string{srv.URL + "/x", "HTTP://example.com/x", "ftp://example.com/x", "file:///etc/passwd", "//example.com/x", ""} {
		b := NewBuffer("run-9", "ws-vec", DestRef{}, 0)
		_ = b.Append(1, map[string]any{"a": 1})
		var calls int32
		_, err := Sync(context.Background(), newClient("test", []string{"127.0.0.1", "example.com"}, nil), "ws-vec",
			DestRef{Config: map[string]any{"url": u, "header_secret_ref": "K"}}, b, getter(map[string]string{"K": testSecret}, &calls))
		b.Close()
		if err == nil {
			t.Errorf("%q: plain http / non-https accepted", u)
		} else if strings.Contains(err.Error(), testSecret) {
			t.Errorf("%q: leaked", u)
		}
		if calls != 0 {
			t.Errorf("%q: secret resolved before the URL was vetted", u)
		}
	}
	if s.n != 0 {
		t.Fatal("plain HTTP request was sent")
	}
}

func TestSyncTransportErrorHasNoSecretOrURL(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()
	b := NewBuffer("run-9", "ws-vec", DestRef{}, 0)
	defer b.Close()
	_ = b.Append(1, map[string]any{"a": 1})
	_, err := Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{Config: map[string]any{"url": srv.URL + "/p?token=abc", "header_secret_ref": "K"}}, b, getter(map[string]string{"K": testSecret}, nil))
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "token=abc") {
		t.Fatalf("leak: %v", err)
	}
	// An invalid header value must not echo the secret either.
	var s seen
	ok := recorder(200, nil, "", &s)
	defer ok.Close()
	_, err = Sync(context.Background(), tlsClient(ok), "ws-vec", DestRef{Config: map[string]any{"url": ok.URL, "header_secret_ref": "K"}}, b, getter(map[string]string{"K": "bad\nvalue" + testSecret}, nil))
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("invalid header: %v", err)
	}
}

func TestSyncDoesNotFollowRedirects(t *testing.T) {
	var hits int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer target.Close()
	var s seen
	srv := recorder(307, map[string]string{"Location": target.URL}, "moved", &s)
	defer srv.Close()
	b := NewBuffer("run-9", "ws-vec", DestRef{}, 0)
	defer b.Close()
	res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}, b, getter(map[string]string{"K": testSecret}, nil))
	if err != nil || res.Success || hits != 0 {
		t.Fatalf("%+v %v hits=%d", res, err, hits)
	}
}

func TestErrorTextNeverLeaksSecret(t *testing.T) {
	secret := "wh-SECRET-0123456789abcdefghij"
	check := func(t *testing.T, name, got string) {
		t.Helper()
		for n := 8; n <= len(secret); n++ {
			for i := 0; i+n <= len(secret); i++ {
				if strings.Contains(got, secret[i:i+n]) {
					t.Errorf("%s: error contains secret fragment %q: %.80q", name, secret[i:i+n], got)
					return
				}
			}
		}
	}
	bodies := map[string]string{
		"echo":      "bad auth header: " + secret,
		"straddle":  strings.Repeat("x", 300-10) + secret + " tail",
		"straddle2": strings.Repeat("y", 300-len(secret)/2) + secret,
		"ctl_split": secret[:12] + "\x00\n" + secret[12:],
		"twice":     secret + secret,
		"cap_cut":   strings.Repeat("z", maxBody-12) + secret,
		"urlenc":    "bad " + url.QueryEscape(secret),
		"base64":    "bad " + base64.StdEncoding.EncodeToString([]byte(secret)),
		"base64url": "bad " + base64.RawURLEncoding.EncodeToString([]byte("Bearer "+secret)),
	}
	for name, body := range bodies {
		var s seen
		srv := recorder(401, nil, body, &s)
		b := NewBuffer("run-9", "ws-vec", DestRef{ID: "d"}, 0)
		_ = b.Append(1, map[string]any{"a": 1})
		dest := DestRef{ID: "d", Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}
		res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", dest, b, getter(map[string]string{"K": secret}, nil))
		b.Close()
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.Success || res.Error != "warehouse HTTP 401" {
			t.Fatalf("%s: %+v", name, res)
		}
		check(t, name, res.Error)
	}
	// The secret echoed in a Location header is not surfaced as external_id either.
	var s seen
	srv := recorder(201, map[string]string{"Location": "https://x/" + secret}, "", &s)
	defer srv.Close()
	b := NewBuffer("run-9", "ws-vec", DestRef{ID: "d"}, 0)
	defer b.Close()
	res, err := Sync(context.Background(), tlsClient(srv), "ws-vec", DestRef{ID: "d", Config: map[string]any{"url": srv.URL, "header_secret_ref": "K"}}, b, getter(map[string]string{"K": secret}, nil))
	if err != nil {
		t.Fatal(err)
	}
	check(t, "external_id", res.ExternalID)
}

// Plan 10 final security L1: a receiver that echoes only the credential part of "Bearer <token>", or its
// JSON-escaped form, must not put the token into the stored error.
func TestRedactsSchemeStrippedAndEscapedSecretForms(t *testing.T) {
	secret := `Bearer sk_live_abcdef0123456789"x`
	for name, body := range map[string]string{
		"token only": `invalid token sk_live_abcdef0123456789"x`,
		"json":       `{"error":"bad sk_live_abcdef0123456789\"x"}`,
		"whole":      `bad ` + secret,
	} {
		r := classify(401, http.Header{}, []byte(body), secretForms(secret)...)
		if strings.Contains(r.Error, "sk_live_abcdef0123456789") {
			t.Errorf("%s: %q", name, r.Error)
		}
	}
}

// S-M2: a stored header_secret_ref outside the outbound namespace reads as missing and nothing is sent
// or looked up, even when the workspace holds a secret of that name.
func TestSyncRefOutsideNamespaceNotSent(t *testing.T) {
	var s seen
	srv := recorder(200, nil, "{}", &s)
	defer srv.Close()
	for _, ref := range []string{"HUBSPOT_API_KEY", "SMTP_PASSWORD", "WRONGPREFIX_TOKEN", " DEST_X"} {
		calls := 0
		get := func(context.Context, string, string) (string, error) { calls++; return "super-secret", nil }
		b := NewBuffer("run-9", "ws-vec", DestRef{ID: "d"}, 0)
		_ = b.Append(1, map[string]any{"a": 1})
		c := newClient("test", []string{"127.0.0.1"}, nil).WithOutbound(secrets.OutboundPolicy{})
		res, err := Sync(context.Background(), c, "ws-vec", DestRef{ID: "d", Config: map[string]any{"url": srv.URL, "header_secret_ref": ref}}, b, get)
		b.Close()
		if err != nil || res.Success || res.Error != "warehouse authentication secret is missing" || calls != 0 || s.n != 0 {
			t.Errorf("%q: %+v err=%v lookups=%d requests=%d", ref, res, err, calls, s.n)
		}
	}
}
