package colcompute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type httpCaseFile struct {
	Columns  []map[string]any `json:"columns"`
	LeadData map[string]any   `json:"lead_data"`
	Cases    []struct {
		Name     string                     `json:"name"`
		Column   map[string]json.RawMessage `json:"column"`
		Requests []struct {
			Method          string            `json:"method"`
			URL             string            `json:"url"`
			Headers         map[string]string `json:"headers"`
			Body            string            `json:"body"`
			FollowRedirects bool              `json:"follow_redirects"`
			Timeout         float64           `json:"timeout"`
		} `json:"requests"`
		Result struct {
			Success bool    `json:"success"`
			Value   tagged  `json:"value"`
			Error   *string `json:"error"`
		} `json:"result"`
	} `json:"cases"`
}

// stub is the scripted answer the Python generator's MockTransport gave for a case.
type stub struct {
	status int
	body   string
	header map[string]string
	err    error
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "slow" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func jsonStub(body string) stub {
	return stub{status: 200, body: body, header: map[string]string{"content-type": "application/json"}}
}
func textStub(body string, status int) stub {
	return stub{status: status, body: body, header: map[string]string{"content-type": "text/plain; charset=utf-8"}}
}

func stubFor(name string) stub {
	switch name {
	case "get_json_extract":
		return jsonStub(`{"data":{"email":"a@b.co"}}`)
	case "url_value_is_not_percent_encoded":
		return jsonStub(`{"x":1}`)
	case "extract_number_zero_kept":
		return jsonStub(`{"n":0}`)
	case "extract_false_kept":
		return jsonStub(`{"n":false}`)
	case "extract_empty_string_no_match":
		return jsonStub(`{"n":""}`)
	case "extract_null_no_match":
		return jsonStub(`{"n":null}`)
	case "extract_missing_no_match":
		return jsonStub(`{"n":1}`)
	case "extract_dict_dumped":
		return jsonStub(`{"d":{"b":1,"a":[1,2]}}`)
	case "extract_list_dumped":
		return jsonStub(`{"d":[1,"é"]}`)
	case "extract_list_index":
		return jsonStub(`{"d":[{},{"x":"y"}]}`)
	case "extract_dict_truncated_2000":
		return jsonStub(`{"d":{"k":"` + strings.Repeat("v", 3000) + `"}}`)
	case "extract_non_json":
		return textStub("<html>", 200)
	case "extract_json_array_root":
		return jsonStub(`[5]`)
	case "no_extract_raw_text":
		return textStub("hello", 200)
	case "no_extract_raw_truncated":
		return textStub(strings.Repeat("x", 2500), 200)
	case "no_extract_empty_body":
		return textStub("", 200)
	case "no_extract_unicode_text":
		return textStub(strings.Repeat("日本語", 800), 200)
	case "status_404":
		return textStub("nf", 404)
	case "status_500":
		return textStub("e", 500)
	case "status_301_not_followed":
		return stub{status: 301, header: map[string]string{"location": "http://127.0.0.1/"}}
	case "status_199_like_204":
		return textStub("", 204)
	case "timeout", "connect_timeout":
		return stub{err: timeoutErr{}}
	case "connect_error_message":
		return stub{err: errors.New("connection refused by peer")}
	case "long_error_truncated_120":
		return stub{err: errors.New(strings.Repeat("x", 300))}
	case "post_json_body":
		return jsonStub(`{"ok":"y"}`)
	case "post_str_body":
		return textStub("done", 200)
	case "post_list_body":
		return textStub("done", 200)
	}
	return textStub("ok", 200)
}

type seenReq struct {
	method, url, body string
	headers           map[string]string
	timeout           time.Duration
}

type stubTransport struct {
	st   stub
	seen []seenReq
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	h := map[string]string{}
	for k, v := range r.Header {
		h[strings.ToLower(k)] = strings.Join(v, ",")
	}
	if r.ContentLength > 0 || r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		h["content-length"] = strconv.Itoa(len(body))
	}
	var to time.Duration
	if dl, ok := r.Context().Deadline(); ok {
		to = time.Until(dl)
	}
	s.seen = append(s.seen, seenReq{r.Method, r.URL.String(), string(body), h, to})
	if s.st.err != nil {
		return nil, s.st.err
	}
	resp := &http.Response{StatusCode: s.st.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(s.st.body)), Request: r}
	for k, v := range s.st.header {
		resp.Header.Set(k, v)
	}
	return resp, nil
}

func columnFromVector(t *testing.T, raw map[string]json.RawMessage) HTTPColumn {
	t.Helper()
	var col HTTPColumn
	str := func(key string) string {
		var s string
		if r, ok := raw[key]; ok && string(r) != "null" {
			_ = json.Unmarshal(r, &s)
		}
		return s
	}
	col.URL, col.Method, col.JSONPath = str("http_url"), str("http_method"), str("http_extract")
	if r, ok := raw["http_headers"]; ok {
		var h map[string]any
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.UseNumber()
		if err := dec.Decode(&h); err != nil {
			t.Fatal(err)
		}
		col.Headers = map[string]string{}
		for k, v := range h {
			col.Headers[k] = CellText(v) // Python: str(v)
		}
	}
	if r, ok := raw["http_body"]; ok {
		var s string
		if json.Unmarshal(r, &s) == nil {
			col.Body = s
		} else {
			col.JSONBody = r
		}
	}
	return col
}

func TestHTTPColumnVectors(t *testing.T) {
	var f httpCaseFile
	loadVectors(t, "http_vectors.json", &f)
	if len(f.Cases) < 60 {
		t.Fatalf("vector file shrank: %d", len(f.Cases))
	}
	cols := make([]ColumnInfo, 0, len(f.Columns))
	for _, c := range f.Columns {
		id, _ := c["id"].(string)
		name, _ := c["name"].(string)
		cols = append(cols, ColumnInfo{ID: id, Name: name})
	}
	row := map[string]any{}
	for k, v := range RowValues(f.LeadData, cols) {
		row[k] = v
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			tr := &stubTransport{st: stubFor(c.Name)}
			got, err := RunHTTPColumn(context.Background(), useTransport(t, tr), columnFromVector(t, c.Column), row, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Success != c.Result.Success {
				t.Fatalf("success = %v, want %v (error %q)", got.Success, c.Result.Success, got.Error)
			}
			wantErr := ""
			if c.Result.Error != nil {
				wantErr = *c.Result.Error
			}
			if got.Error != wantErr {
				t.Fatalf("error = %q, want %q", got.Error, wantErr)
			}
			if m := matchTag(c.Result.Value, got.Value); m != "" {
				t.Fatal(m)
			}
			if len(tr.seen) != len(c.Requests) {
				t.Fatalf("%d requests sent, want %d", len(tr.seen), len(c.Requests))
			}
			for i, want := range c.Requests {
				s := tr.seen[i]
				if s.method != want.Method || s.url != want.URL || s.body != want.Body {
					t.Errorf("request %d = %s %s %q, want %s %s %q", i, s.method, s.url, s.body, want.Method, want.URL, want.Body)
				}
				for k, v := range want.Headers {
					if s.headers[k] != v {
						t.Errorf("header %s = %q, want %q", k, s.headers[k], v)
					}
				}
				for k := range s.headers {
					if _, ok := want.Headers[k]; !ok {
						t.Errorf("unexpected header %s=%q", k, s.headers[k])
					}
				}
				if want.Timeout != 20 || s.timeout < 19*time.Second || s.timeout > 20*time.Second {
					t.Errorf("timeout = %v, want 20s", s.timeout)
				}
			}
		})
	}
}

func rowOf(kv ...string) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestHTTPColumnBlocksPrivateAfterTemplate(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	client := (*http.Client)(nil)
	for _, target := range []string{
		srv.URL + "/x",
		"http://127.0.0.1/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:10.0.0.1]/",
		"http://[::ffff:127.0.0.1]:8080/",
		"http://2130706433/",
		"http://10.0.0.5/",
	} {
		got, err := RunHTTPColumn(context.Background(), client, HTTPColumn{URL: "{url}"}, rowOf("url", target), nil)
		if err != nil || got.Success || !strings.HasPrefix(got.Error, "blocked_url: ") {
			t.Errorf("%s: result %+v err %v; want blocked_url", target, got, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked target was contacted %d times", hits.Load())
	}
	// Even with a plain (non-guarded) client the pre-flight check fires first.
	got, _ := RunHTTPColumn(context.Background(), nil, HTTPColumn{URL: srv.URL}, nil, nil)
	if got.Success || hits.Load() != 0 {
		t.Fatalf("pre-flight guard skipped: %+v hits=%d", got, hits.Load())
	}
}

func TestHTTPColumnNoRedirects(t *testing.T) {
	var second atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { second.Add(1); _, _ = w.Write([]byte("hit")) }))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL, http.StatusFound)
	}))
	defer a.Close()
	old := egressCheck
	egressCheck = func(string) error { return nil } // loopback test servers
	defer func() { egressCheck = old }()
	// A redirect-following transport-level client must not make the column follow.
	got, err := RunHTTPColumn(context.Background(), useTransport(t, http.DefaultTransport), HTTPColumn{URL: a.URL}, nil, nil)
	if err != nil || got.Success || got.Error != "http_302" {
		t.Fatalf("result %+v err %v, want http_302", got, err)
	}
	if second.Load() != 0 {
		t.Fatal("redirect target was contacted")
	}
}

func TestHTTPColumnSecretNotInError(t *testing.T) {
	const secret = "sk-live-TOPSECRET-12345"
	secrets := func(name string) (string, bool) {
		if name == "api" {
			return secret, true
		}
		return "", false
	}
	col := HTTPColumn{URL: "https://api.example.test/v1?q={q}", Headers: map[string]string{"Authorization": "Bearer {secret:api}"}}

	// The header is expanded on the wire...
	tr := &stubTransport{st: textStub("ok", 200)}
	got, err := RunHTTPColumn(context.Background(), useTransport(t, tr), col, rowOf("q", "x"), secrets)
	if err != nil || !got.Success || tr.seen[0].headers["authorization"] != "Bearer "+secret {
		t.Fatalf("secret not sent: %+v %v %+v", got, err, tr.seen)
	}
	// ...but a transport error that echoes it (proxies and TLS stacks do) is redacted,
	// and the Go URL wrapper (which carries the URL) is not appended.
	for _, e := range []error{
		errors.New("dial failed for Authorization: Bearer " + secret),
		&net.OpError{Op: "dial", Err: errors.New("bad " + secret)},
	} {
		tr = &stubTransport{st: stub{err: e}}
		got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), col, rowOf("q", "x"), secrets)
		if got.Success || strings.Contains(got.Error, secret) || got.Error == "" {
			t.Fatalf("error %q leaks or is empty", got.Error)
		}
		if strings.Contains(got.Error, "api.example.test") {
			t.Fatalf("error %q carries the request URL", got.Error)
		}
	}
	// Unknown secret: refused before any request, naming the secret only.
	tr = &stubTransport{st: textStub("ok", 200)}
	col2 := HTTPColumn{URL: "https://api.example.test/", Headers: map[string]string{"X": "{secret:other}"}}
	got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), col2, nil, secrets)
	if got.Success || got.Error != "unknown_secret: other" || len(tr.seen) != 0 {
		t.Fatalf("unknown secret: %+v seen=%d", got, len(tr.seen))
	}
	// A secret must never travel to a host chosen by row data.
	tr = &stubTransport{st: textStub("ok", 200)}
	col3 := HTTPColumn{URL: "https://{host}/", Headers: map[string]string{"X": "{secret:api}"}}
	got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), col3, rowOf("host", "evil.example"), secrets)
	if got.Success || got.Error != "secret_with_templated_host" || len(tr.seen) != 0 {
		t.Fatalf("templated host + secret: %+v seen=%d", got, len(tr.seen))
	}
	// Secrets are not expanded in the URL or body: they stay inert column references.
	tr = &stubTransport{st: textStub("ok", 200)}
	col4 := HTTPColumn{URL: "https://api.example.test/?k={secret:api}"}
	got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), col4, nil, secrets)
	if got.Error != "unresolved_url_placeholder" || len(tr.seen) != 0 {
		t.Fatalf("secret in URL: %+v seen=%d", got, len(tr.seen))
	}
}

func TestHTTPColumnBodyCap(t *testing.T) {
	tr := &stubTransport{st: textStub(strings.Repeat("a", 1<<20+10), 200)}
	got, _ := RunHTTPColumn(context.Background(), useTransport(t, tr), HTTPColumn{URL: "https://api.example.test/"}, nil, nil)
	if got.Success || got.Error != "response_too_large" {
		t.Fatalf("over-cap body: %+v", got)
	}
	tr = &stubTransport{st: textStub(strings.Repeat("a", 1<<20), 200)}
	got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), HTTPColumn{URL: "https://api.example.test/"}, nil, nil)
	if !got.Success || len(got.Value.(string)) != 2000 {
		t.Fatalf("at-cap body: success=%v err=%q", got.Success, got.Error)
	}
}

// useTransport swaps the wire transport under the always-guarded client (tests only).
func useTransport(t *testing.T, rt http.RoundTripper) *http.Client {
	t.Helper()
	old := testTransport
	testTransport = rt
	t.Cleanup(func() { testTransport = old })
	return nil
}

func TestHTTPColumnMethodAllowList(t *testing.T) {
	for _, m := range []string{"TRACE", "CONNECT", "OPTIONS", "PROPFIND", "G ET", "get\r\nX: y"} {
		tr := &stubTransport{st: textStub("ok", 200)}
		got, err := RunHTTPColumn(context.Background(), useTransport(t, tr), HTTPColumn{URL: "https://api.example.test/", Method: m}, nil, nil)
		if err != nil || got.Success || got.Error != "unsupported_method" || len(tr.seen) != 0 {
			t.Errorf("%q: %+v %v seen=%d", m, got, err, len(tr.seen))
		}
	}
	for _, m := range []string{"get", "POST", "put", "patch", "head", "delete"} {
		tr := &stubTransport{st: textStub("ok", 200)}
		if got, _ := RunHTTPColumn(context.Background(), useTransport(t, tr), HTTPColumn{URL: "https://api.example.test/", Method: m}, nil, nil); got.Error == "unsupported_method" {
			t.Errorf("%q refused", m)
		}
	}
}

// A caller-supplied client is never trusted: the dial-time-guarded client is always used.
func TestHTTPColumnIgnoresCallerClient(t *testing.T) {
	tr := &stubTransport{st: textStub("ok", 200)}
	rogue := &http.Client{Transport: tr}
	got, _ := RunHTTPColumn(context.Background(), rogue, HTTPColumn{URL: "https://api.example.test/"}, nil, nil)
	if len(tr.seen) != 0 {
		t.Fatalf("caller client was used: %+v", got)
	}
}

func TestHTTPColumnSecretRedactedInValue(t *testing.T) {
	const secret = "sk-live-TOPSECRET-12345"
	secrets := func(string) (string, bool) { return secret, true }
	col := HTTPColumn{URL: "https://api.example.test/", Headers: map[string]string{"Authorization": "Bearer {secret:api}"}}
	tr := &stubTransport{st: jsonStub(`{"headers":{"Authorization":"Bearer ` + secret + `"},"n":1}`)}
	got, _ := RunHTTPColumn(context.Background(), useTransport(t, tr), col, nil, secrets)
	if !got.Success || strings.Contains(got.Value.(string), secret) {
		t.Fatalf("raw value leaks: %+v", got)
	}
	col.JSONPath = "$.headers"
	got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), col, nil, secrets)
	if !got.Success || strings.Contains(got.Value.(string), secret) {
		t.Fatalf("projected value leaks: %+v", got)
	}
	col.JSONPath = "$.headers.Authorization"
	got, _ = RunHTTPColumn(context.Background(), useTransport(t, tr), col, nil, secrets)
	if !got.Success || strings.Contains(got.Value.(string), secret) {
		t.Fatalf("scalar value leaks: %+v", got)
	}
}

func TestHTTPColumnSecretNeverInURLOrBody(t *testing.T) {
	const secret = "sk-live-TOPSECRET-12345"
	secrets := func(string) (string, bool) { return secret, true }
	tr := &stubTransport{st: textStub("ok", 200)}
	col := HTTPColumn{URL: "https://api.example.test/", Method: "POST", Body: "k={secret:api}", JSONBody: nil}
	RunHTTPColumn(context.Background(), useTransport(t, tr), col, nil, secrets)
	col.Body, col.JSONBody = "", []byte(`{"k":"{secret:api}","l":["{secret:api}"]}`)
	RunHTTPColumn(context.Background(), useTransport(t, tr), col, nil, secrets)
	if len(tr.seen) != 2 {
		t.Fatalf("seen %d", len(tr.seen))
	}
	for _, r := range tr.seen {
		if strings.Contains(r.body, secret) || strings.Contains(r.url, secret) || !strings.Contains(r.body, "[secret:api: not found]") {
			t.Fatalf("secret expanded outside headers: %+v", r)
		}
	}
}
