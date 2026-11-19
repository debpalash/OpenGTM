package manifest_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// Tests for response.success_when, auth.secondary and poll.

// ------------------------------------------------------------------ success_when

const swManifest = `{"name":"sw","capability":"email","default_confidence":0.8,
 "auth":{"type":"header","param":"X-Key","env_var":"K"},
 "request":{"method":"GET","url":"http://api.vendor.example/find","timeout":2},
 "response":{"error_message_path":"message","mappings":{"email":"$.email"},
   "success_when":{"path":"$.status","in":["ok","found"]}}}`

func runWith(t *testing.T, m manifest.Manifest, resp string) manifest.Result {
	t.Helper()
	srv, _ := recorder(t, resp)
	return manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"K": "k", "S": "s3cret"}))
}

func TestSuccessWhen(t *testing.T) {
	m := mk(t, swManifest)
	cases := []struct {
		name, resp string
		ok         bool
		err        string
	}{
		{"in list", `{"status":"found","email":"a@b.co"}`, true, ""},
		{"first of list", `{"status":"ok","email":"a@b.co"}`, true, ""},
		{"200 with another status is a miss", `{"status":"not_found","email":"a@b.co"}`, false, "no_data"},
		{"vendor message wins when given", `{"status":"error","message":"quota exceeded","email":"a@b.co"}`, false, "quota exceeded"},
		{"missing field never matches", `{"email":"a@b.co"}`, false, "no_data"},
		{"null never matches", `{"status":null,"email":"a@b.co"}`, false, "no_data"},
		{"object never matches", `{"status":{"x":1},"email":"a@b.co"}`, false, "no_data"},
		{"case sensitive", `{"status":"OK","email":"a@b.co"}`, false, "no_data"},
	}
	for _, c := range cases {
		res := runWith(t, m, c.resp)
		if res.Success != c.ok || res.Error != c.err {
			t.Errorf("%s: %+v", c.name, res)
		}
	}
}

func TestSuccessWhenEqualsBoolAndNumber(t *testing.T) {
	m := mk(t, `{"name":"sw","capability":"email","auth":{"type":"header","param":"X-Key","env_var":"K"},
 "request":{"method":"GET","url":"http://api.vendor.example/f"},
 "response":{"mappings":{"email":"$.email"},"success_when":{"path":"$.success","equals":true}}}`)
	if res := runWith(t, m, `{"success":true,"email":"a@b.co"}`); !res.Success {
		t.Errorf("true: %+v", res)
	}
	if res := runWith(t, m, `{"success":false,"email":"a@b.co"}`); res.Success || res.Error != "no_data" {
		t.Errorf("false: %+v", res)
	}
	m = mk(t, `{"name":"sw","capability":"email","auth":{"type":"header","param":"X-Key","env_var":"K"},
 "request":{"method":"GET","url":"http://api.vendor.example/f"},
 "response":{"mappings":{"email":"$.email"},"success_when":{"path":"$.code","equals":0}}}`)
	if res := runWith(t, m, `{"code":0,"email":"a@b.co"}`); !res.Success {
		t.Errorf("code 0: %+v", res)
	}
}

func TestSuccessWhenMessageIsScrubbed(t *testing.T) {
	m := mk(t, swManifest)
	res := runWith(t, m, `{"status":"error","message":"bad key k"}`)
	if strings.Contains(res.Error, "k") && strings.Contains(res.Error, "bad key k") {
		t.Errorf("credential not scrubbed: %q", res.Error)
	}
}

func TestExtensionConditionsAreValidated(t *testing.T) {
	bad := []string{
		`{"path":"status","equals":"x"}`,           // not a $. path
		`{"path":"$.status"}`,                      // neither equals nor in
		`{"path":"$.s","equals":"a","in":["b"]}`,   // both
		`{"equals":"a"}`,                           // no path
		`{"path":"$.s","equals":""}`,               // empty literal can never match
		`{"path":"$.s","in":[{"a":1}]}`,            // not comparable
		`{"status":[99]}`,                          // not an HTTP status
		`{}`,                                       // empty
	}
	for _, c := range bad {
		m := mk(t, `{"name":"sw","capability":"email","auth":{"type":"header","param":"X","env_var":"K"},
 "request":{"method":"GET","url":"http://api.vendor.example/f"},
 "response":{"mappings":{"email":"$.email"},"success_when":`+c+`}}`)
		if p := manifest.SecurityProblem(m); !strings.HasPrefix(p, "success_when_invalid") {
			t.Errorf("%s: problem %q", c, p)
		}
	}
}

// ------------------------------------------------------------------ auth.secondary

const secManifest = `{"name":"sec","capability":"email","default_confidence":0.8,
 "auth":{"type":"header","param":"X-Key","env_var":"K","secondary":{"type":"query","param":"secret","env_var":"S"}},
 "request":{"method":"GET","url":"http://api.vendor.example/find","timeout":2},
 "response":{"error_path":"error","error_message_path":"message","mappings":{"email":"$.email"}}}`

func TestSecondaryCredentialPlacement(t *testing.T) {
	srv, seen := recorder(t, `{"email":"a@b.co"}`)
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, secManifest), nil, creds(map[string]string{"K": "key1", "S": "sec2"}))
	if !res.Success {
		t.Fatalf("%+v", res)
	}
	if seen.header.Get("X-Key") != "key1" || seen.query != "secret=sec2" {
		t.Errorf("header %q query %q", seen.header.Get("X-Key"), seen.query)
	}
	if strings.Contains(seen.uri, "key1") || strings.Contains(strings.SplitN(seen.uri, "?", 2)[0], "sec2") {
		t.Errorf("credential in the path: %s", seen.uri)
	}

	hdr := strings.Replace(secManifest, `"type":"query","param":"secret"`, `"type":"header","param":"X-Secret"`, 1)
	srv, seen = recorder(t, `{"email":"a@b.co"}`)
	manifest.Execute(context.Background(), clientTo(srv), mk(t, hdr), nil, creds(map[string]string{"K": "key1", "S": "sec2"}))
	if seen.header.Get("X-Secret") != "sec2" || seen.header.Get("X-Key") != "key1" {
		t.Errorf("headers %v", seen.header)
	}
}

func TestSecondaryMissingMakesNoCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, secManifest), nil, creds(map[string]string{"K": "key1"}))
	if res.Error != "sec: missing S" || calls.Load() != 0 {
		t.Errorf("%+v calls=%d", res, calls.Load())
	}
}

func TestSecondaryNeverLeaksIntoErrors(t *testing.T) {
	// The vendor echoes both credentials in its error message; neither may leave the engine.
	srv, _ := recorder(t, `{"error":true,"message":"bad key key1 / secret sec2"}`)
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, secManifest), nil, creds(map[string]string{"K": "key1", "S": "sec2"}))
	if strings.Contains(res.Error, "key1") || strings.Contains(res.Error, "sec2") || res.Error == "" {
		t.Errorf("error %q", res.Error)
	}
	// Transport failures are scrubbed too (the query-string secret is part of the URL).
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
	}))
	defer srv2.Close()
	res = manifest.Execute(context.Background(), clientTo(srv2), mk(t, secManifest), nil, creds(map[string]string{"K": "key1", "S": "sec2"}))
	if strings.Contains(res.Error, "sec2") || strings.Contains(res.Error, "key1") {
		t.Errorf("transport error %q", res.Error)
	}
}

func TestSecondaryRefusals(t *testing.T) {
	mut := func(from, to string) string { return strings.Replace(secManifest, from, to, 1) }
	cases := []struct{ name, js, want string }{
		{"denied name", mut(`"env_var":"S"`, `"env_var":"SECRET_KEY"`), "secondary_env_denied"},
		{"denied prefix", mut(`"env_var":"S"`, `"env_var":"DATABASE_URL_RO"`), "secondary_env_denied"},
		{"master key", mut(`"env_var":"S"`, `"env_var":"X_MASTER_KEY"`), "secondary_env_denied"},
		{"same as primary", mut(`"env_var":"S"`, `"env_var":"K"`), "secondary_invalid"},
		{"bad type", mut(`"type":"query","param":"secret"`, `"type":"path","param":"secret"`), "secondary_invalid"},
		{"bearer not allowed", mut(`"type":"query","param":"secret"`, `"type":"bearer","param":"secret"`), "secondary_invalid"},
		{"no param", mut(`"param":"secret",`, ``), "secondary_invalid"},
		{"no primary", mut(`"type":"header","param":"X-Key","env_var":"K"`, `"type":"none"`), "secondary_invalid"},
		{"same param", mut(`"type":"query","param":"secret"`, `"type":"header","param":"x-key"`), "secondary_invalid"},
		{"primary in path", mut(`/find"`, `/find/${env:K}"`), "secret_in_url_path"},
		{"secondary in path", mut(`/find"`, `/${env:S}/find"`), "secret_in_url_path"},
	}
	for _, c := range cases {
		m := mk(t, c.js)
		if p := manifest.SecurityProblem(m); !strings.HasPrefix(p, c.want) {
			t.Errorf("%s: problem %q, want %q", c.name, p, c.want)
		}
		if res := manifest.Execute(context.Background(), nil, m, nil, creds(map[string]string{"K": "k", "S": "s", "SECRET_KEY": "x", "DATABASE_URL_RO": "x", "X_MASTER_KEY": "x"})); res.Success || res.Requests != 0 {
			t.Errorf("%s: ran: %+v", c.name, res)
		}
	}
	// A credential in the query string of the URL is not the path; a primary-only manifest is untouched.
	if p := manifest.SecurityProblem(mk(t, mutQuery(secManifest))); p != "" {
		t.Errorf("query placement refused: %q", p)
	}
}

func mutQuery(s string) string { return strings.Replace(s, `/find"`, `/find?k=${env:K}"`, 1) }

func TestSecondaryOnlyOwnNamesAreReadable(t *testing.T) {
	// A template naming a third variable reads nothing: only the two declared names resolve.
	js := strings.Replace(secManifest, `"timeout":2`, `"timeout":2,"headers":{"X-Other":"${env:OTHER_TOKEN}"}`, 1)
	srv, seen := recorder(t, `{"email":"a@b.co"}`)
	manifest.Execute(context.Background(), clientTo(srv), mk(t, js), nil, creds(map[string]string{"K": "k", "S": "s", "OTHER_TOKEN": "leak"}))
	if got := seen.header.Get("X-Other"); got != "" {
		t.Errorf("third env var resolved: %q", got)
	}
	allowed := manifest.AllowedEnvNames(mk(t, js))
	if len(allowed) != 2 {
		t.Errorf("allowed %v", allowed)
	}
}

// ------------------------------------------------------------------ poll

const pollManifest = `{"name":"pl","capability":"email","default_confidence":0.8,
 "auth":{"type":"header","param":"X-Key","env_var":"K","secondary":{"type":"header","param":"X-Secret","env_var":"S"}},
 "request":{"method":"POST","url":"http://api.vendor.example/v1/submit","body_template":{"d":"{{input.domain}}"},"timeout":2},
 "response":{"error_path":"error","error_message_path":"message","mappings":{"email":"$.data.email"}},
 "poll":{"request":{"method":"GET","url":"http://api.vendor.example/v1/jobs/{{response.id}}","timeout":2},
   "interval":0.1,"max_attempts":5,
   "done_when":{"path":"$.state","equals":"done"},
   "failed_when":{"path":"$.state","in":["failed","error"]}}}`

// pollServer answers POST /v1/submit with id and GET /v1/jobs/<id> with the scripted bodies.
type pollServer struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	hdrs   []http.Header
	submit string
	script []string // poll bodies in order; the last repeats
	status []int
}

func newPollServer(t *testing.T, submit string, script ...string) *pollServer {
	ps := &pollServer{submit: submit, script: script}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		ps.paths = append(ps.paths, r.Method+" "+r.URL.EscapedPath())
		ps.hdrs = append(ps.hdrs, r.Header.Clone())
		if r.Method == "POST" {
			io.WriteString(w, ps.submit)
			return
		}
		i := len(ps.paths) - 2
		if i >= len(ps.script) {
			i = len(ps.script) - 1
		}
		if i < len(ps.status) && ps.status[i] != 0 {
			w.WriteHeader(ps.status[i])
		}
		io.WriteString(w, ps.script[i])
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (p *pollServer) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.paths) }

var pollCreds = creds(map[string]string{"K": "key1", "S": "sec2"})

func TestPollHappyPath(t *testing.T) {
	ps := newPollServer(t, `{"id":"job42"}`, `{"state":"pending"}`, `{"state":"pending"}`, `{"state":"done","data":{"email":"a@b.co"}}`)
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), map[string]string{"domain": "b.co"}, pollCreds)
	if !res.Success || res.Fields["email"] != "a@b.co" {
		t.Fatalf("%+v", res)
	}
	if res.Requests != 4 || ps.count() != 4 {
		t.Errorf("requests %d / %d", res.Requests, ps.count())
	}
	if ps.paths[1] != "GET /v1/jobs/job42" {
		t.Errorf("poll path %q", ps.paths[1])
	}
	for i, h := range ps.hdrs { // both credentials on every request, in headers
		if h.Get("X-Key") != "key1" || h.Get("X-Secret") != "sec2" {
			t.Errorf("request %d headers %v", i, h)
		}
	}
}

func TestPollFailedAndErrors(t *testing.T) {
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"failed","message":"no match"}`)
	if res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds); res.Success || res.Error != "poll_failed" {
		// error_path "error" is empty here, so the generic marker is used
		t.Errorf("failed: %+v", res)
	}
	// With the vendor's error flag the vendor message is reported, scrubbed.
	ps = newPollServer(t, `{"id":"j"}`, `{"state":"error","error":true,"message":"denied for key1 sec2"}`)
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds)
	if res.Success || strings.Contains(res.Error, "key1") || strings.Contains(res.Error, "sec2") || !strings.HasPrefix(res.Error, "denied for") {
		t.Errorf("vendor failure: %+v", res)
	}
	// An error on the submit response ends the lookup before any poll request.
	ps = newPollServer(t, `{"error":true,"message":"bad domain"}`, `{"state":"done"}`)
	res = manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds)
	if res.Error != "bad domain" || ps.count() != 1 {
		t.Errorf("submit error: %+v polls=%d", res, ps.count())
	}
	// HTTP error on a poll aborts (no retry loop on 500).
	ps = newPollServer(t, `{"id":"j"}`, `{}`)
	ps.status = []int{500}
	res = manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds)
	if res.Error != "http_500" || ps.count() != 2 {
		t.Errorf("poll 500: %+v n=%d", res, ps.count())
	}
	// A non-JSON poll body is an error, not a pending.
	ps = newPollServer(t, `{"id":"j"}`, `<html>`)
	if res = manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds); res.Error != "non_json_response" {
		t.Errorf("non json: %+v", res)
	}
}

func TestPollMaxAttempts(t *testing.T) {
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"pending"}`)
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds)
	if res.Error != "poll_max_attempts" || ps.count() != 1+5 || res.Requests != 6 {
		t.Errorf("%+v n=%d", res, ps.count())
	}
}

func TestPollHardTotalCap(t *testing.T) {
	// 60 attempts at 0.1 s would take 6 s; max_total 0.35 s ends it first.
	js := strings.Replace(pollManifest, `"max_attempts":5`, `"max_attempts":60,"max_total":0.35`, 1)
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"pending"}`)
	t0 := time.Now()
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, js), nil, pollCreds)
	if res.Error != "poll_timeout" || time.Since(t0) > 2*time.Second {
		t.Errorf("%+v after %v", res, time.Since(t0))
	}
	if n := ps.count(); n < 2 || n > 5 {
		t.Errorf("poll requests %d", n)
	}
}

func TestPollCallerDeadlineAndCancellation(t *testing.T) {
	js := strings.Replace(pollManifest, `"max_attempts":5`, `"max_attempts":60`, 1)
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"pending"}`)
	// The caller's own timeout (the worker's per-call timeout) is shorter than the cap: "timeout".
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	res := manifest.Execute(ctx, clientTo(ps.Server), mk(t, js), nil, pollCreds)
	if res.Error != "timeout" || time.Since(t0) > 2*time.Second {
		t.Errorf("deadline: %+v after %v", res, time.Since(t0))
	}
	// Cancellation stops the loop promptly and makes no further request.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(250 * time.Millisecond); cancel2() }()
	before := ps.count()
	t0 = time.Now()
	res = manifest.Execute(ctx2, clientTo(ps.Server), mk(t, js), nil, pollCreds)
	if res.Success || res.Error == "" || time.Since(t0) > 2*time.Second {
		t.Errorf("cancel: %+v after %v", res, time.Since(t0))
	}
	n := ps.count()
	time.Sleep(400 * time.Millisecond)
	if ps.count() != n || n-before > 6 {
		t.Errorf("requests continued after cancel: %d -> %d", n, ps.count())
	}
}

func TestPollOneLookup(t *testing.T) {
	// However many poll requests it takes, the lookup is one Result from one Execute call, so a
	// caller that reserves cost per Execute pays once.
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"pending"}`, `{"state":"pending"}`, `{"state":"done","data":{"email":"a@b.co"}}`)
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds)
	if !res.Success || res.Requests != 4 {
		t.Errorf("%+v", res)
	}
}

func TestPollStatusAndBodyConditions(t *testing.T) {
	// BetterContact style: 202 while running, then 200 with a terminal status in the body.
	js := strings.Replace(pollManifest, `"done_when":{"path":"$.state","equals":"done"}`, `"done_when":{"status":[200],"path":"$.state","in":["terminated","on_hold"]}`, 1)
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"processing"}`, `{"state":"terminated","data":{"email":"a@b.co"}}`)
	ps.status = []int{202, 200}
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, js), nil, pollCreds)
	if !res.Success || res.HTTPStatus != 200 {
		t.Errorf("%+v", res)
	}
	// Enrow style: done on the HTTP status alone.
	js = strings.Replace(pollManifest, `"done_when":{"path":"$.state","equals":"done"}`, `"done_when":{"status":[200]}`, 1)
	ps = newPollServer(t, `{"id":"j"}`, `{"data":{"email":"a@b.co"}}`, `{"data":{"email":"a@b.co"}}`)
	ps.status = []int{202, 200}
	res = manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, js), nil, pollCreds)
	if !res.Success || res.Requests != 3 {
		t.Errorf("status only: %+v", res)
	}
}

func TestPollSuccessWhenAppliesToFinalResponse(t *testing.T) {
	js := strings.Replace(pollManifest, `"mappings"`, `"success_when":{"path":"$.data.found","equals":true},"mappings"`, 1)
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"done","data":{"found":false,"email":"x@y.co"}}`)
	if res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, js), nil, pollCreds); res.Success || res.Error != "no_data" {
		t.Errorf("%+v", res)
	}
}

// SSRF and injection: the poll host is fixed by the manifest; response values only ever fill a
// path segment or query value, escaped.
func TestPollResponseValuesCannotRedirectTheRequest(t *testing.T) {
	hostile := []string{
		"http://169.254.169.254/latest/meta-data",
		"//evil.example/x",
		"../../admin",
		"..",
		"a/b?x=1#frag",
		"job1\r\nX-Injected: 1",
		"${env:K}",
		"{{input.domain}}",
	}
	for _, id := range hostile {
		ps := newPollServer(t, `{"id":`+jsonStr(id)+`}`, `{"state":"done","data":{"email":"a@b.co"}}`)
		var urls []string
		check := func(raw string) error { urls = append(urls, raw); return nil }
		res := manifest.ExecuteChecked(context.Background(), clientTo(ps.Server), mk(t, pollManifest), map[string]string{"domain": "d.co"}, pollCreds, check)
		if !res.Success {
			t.Errorf("%q: %+v", id, res)
			continue
		}
		if len(urls) != 2 {
			t.Fatalf("%q: checked %v", id, urls)
		}
		u := urls[1]
		if !strings.HasPrefix(u, "http://api.vendor.example/v1/jobs/") {
			t.Errorf("%q: poll URL left the host or path: %s", id, u)
		}
		rest := strings.TrimPrefix(u, "http://api.vendor.example/v1/jobs/")
		if strings.ContainsAny(rest, "/?#\r\n ") || rest == ".." || rest == "." {
			t.Errorf("%q: unescaped value in %s", id, rest)
		}
		if strings.Contains(u, "key1") || strings.Contains(u, "sec2") || strings.Contains(rest, "d.co") {
			t.Errorf("%q: secret or template expanded: %s", id, u)
		}
		if got := ps.paths[1]; strings.Count(got, "/") != 3 {
			t.Errorf("%q: server saw %s", id, got)
		}
	}
}

func jsonStr(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestPollUsesTheGuardedCheckForEveryRequest(t *testing.T) {
	ps := newPollServer(t, `{"id":"j"}`, `{"state":"pending"}`, `{"state":"done","data":{"email":"a@b.co"}}`)
	var n int
	check := func(raw string) error {
		n++
		if n >= 2 {
			return errors.New("private address")
		}
		return nil
	}
	res := manifest.ExecuteChecked(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds, check)
	if !strings.HasPrefix(res.Error, "blocked_url") || ps.count() != 1 {
		t.Errorf("%+v n=%d", res, ps.count())
	}
}

func TestPollMissingReferenceMakesNoPollRequest(t *testing.T) {
	ps := newPollServer(t, `{"nothing":1}`, `{"state":"done"}`)
	res := manifest.Execute(context.Background(), clientTo(ps.Server), mk(t, pollManifest), nil, pollCreds)
	if res.Error != "poll_missing_reference" || ps.count() != 1 {
		t.Errorf("%+v n=%d", res, ps.count())
	}
}

func TestPollRefusals(t *testing.T) {
	mut := func(from, to string) string { return strings.Replace(pollManifest, from, to, 1) }
	cases := []struct{ name, js, want string }{
		{"templated authority", mut(`http://api.vendor.example/v1/jobs/`, `http://{{response.id}}.vendor.example/v1/jobs/`), "templated_authority"},
		{"env in authority", mut(`http://api.vendor.example/v1/jobs/`, `http://${env:K}.vendor.example/v1/jobs/`), "secret_in_url_path"},
		{"other host", mut(`http://api.vendor.example/v1/jobs/`, `http://evil.example/v1/jobs/`), "poll_invalid"},
		{"userinfo host trick", mut(`http://api.vendor.example/v1/jobs/`, `http://api.vendor.example@evil.example/v1/jobs/`), "poll_invalid"},
		{"allowed host with path", mut(`"interval"`, `"allowed_hosts":["evil.example/x"],"interval"`), "poll_invalid"},
		{"allowed host wildcard", mut(`"interval"`, `"allowed_hosts":["*.example"],"interval"`), "poll_invalid"},
		{"no done_when", mut(`"done_when":{"path":"$.state","equals":"done"},`, ``), "poll_invalid"},
		{"no interval", mut(`"interval":0.1,`, ``), "poll_invalid"},
		{"interval too small", mut(`"interval":0.1`, `"interval":0.001`), "poll_invalid"},
		{"interval too large", mut(`"interval":0.1`, `"interval":61`), "poll_invalid"},
		{"too many attempts", mut(`"max_attempts":5`, `"max_attempts":61`), "poll_invalid"},
		{"zero attempts", mut(`"max_attempts":5`, `"max_attempts":0`), "poll_invalid"},
		{"total over cap", mut(`"max_attempts":5`, `"max_attempts":5,"max_total":121`), "poll_invalid"},
		{"bad method", mut(`"method":"GET","url":"http://api.vendor.example/v1/jobs`, `"method":"DELETE","url":"http://api.vendor.example/v1/jobs`), "poll_invalid"},
		{"secret in poll path", mut(`/v1/jobs/{{response.id}}`, `/v1/jobs/${env:S}`), "secret_in_url_path"},
		{"bad condition", mut(`"equals":"done"`, `"equals":""`), "poll_invalid"},
		{"unauthenticated", mut(`"auth":{"type":"header","param":"X-Key","env_var":"K","secondary":{"type":"header","param":"X-Secret","env_var":"S"}}`, `"auth":{"type":"none"}`), "poll_invalid"},
	}
	for _, c := range cases {
		m := mk(t, c.js)
		if p := manifest.SecurityProblem(m); !strings.HasPrefix(p, c.want) {
			t.Errorf("%s: problem %q, want %q", c.name, p, c.want)
		}
		if res := manifest.Execute(context.Background(), nil, m, nil, pollCreds); res.Success || res.Requests != 0 {
			t.Errorf("%s: ran: %+v", c.name, res)
		}
	}
	// An explicitly allowed second host works; a listed host is exact, not a suffix.
	ok := mut(`"interval"`, `"allowed_hosts":["results.vendor.example"],"interval"`)
	ok = strings.Replace(ok, `http://api.vendor.example/v1/jobs/`, `http://results.vendor.example/v1/jobs/`, 1)
	if p := manifest.SecurityProblem(mk(t, ok)); p != "" {
		t.Errorf("allowed host refused: %q", p)
	}
	if p := manifest.SecurityProblem(mk(t, strings.Replace(ok, `results.vendor.example/v1`, `x.results.vendor.example/v1`, 1))); !strings.HasPrefix(p, "poll_invalid") {
		t.Errorf("suffix host accepted: %q", p)
	}
}

func TestPollTotalNeverExceedsHardCap(t *testing.T) {
	if manifest.MaxPollTotal != 120*time.Second || manifest.DefaultPollTotal > manifest.MaxPollTotal {
		t.Fatal("poll caps changed")
	}
}

func TestExtensionDocsAreSignedOnlyWhenDeclared(t *testing.T) {
	plain, err := manifest.CanonicalManifest(mk(t, base))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"poll"`, `"secondary"`, `"success_when"`} {
		if strings.Contains(string(plain), k) {
			t.Errorf("undeclared %s in the signed form", k)
		}
	}
	full, err := manifest.CanonicalManifest(mk(t, strings.Replace(pollManifest, `"mappings"`, `"success_when":{"path":"$.a","equals":"b"},"mappings"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"poll"`, `"secondary"`, `"success_when"`, `"allowed_hosts"`} {
		if k == `"allowed_hosts"` {
			continue
		}
		if !strings.Contains(string(full), k) {
			t.Errorf("declared %s missing from the signed form", k)
		}
	}
	// Changing a poll field changes the signed form.
	other, _ := manifest.CanonicalManifest(mk(t, strings.Replace(pollManifest, `"max_attempts":5`, `"max_attempts":6`, 1)))
	again, _ := manifest.CanonicalManifest(mk(t, pollManifest))
	if string(other) == string(again) {
		t.Error("poll.max_attempts is not covered by the signature")
	}
}
