package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachtoken"
)

// pubMasterKey is the contract seed OUTREACH_MASTER_KEY (base64 of bytes 64..95, TEST-ONLY): the recorded unsubscribe
// tokens in testdata/contract/outreach/unsub_*.json are signed under it.
const pubMasterKey = "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="

// pubWebhookSecret is the contract seed OUTREACH_WEBHOOK_SECRET (TEST-ONLY); the fixtures redact it.
const pubWebhookSecret = "ctk_outreach_bounce_webhook_secret_0001"

type pubOpts struct {
	getenv       map[string]string
	secretKey    string
	appEnv       string
	trustProxy   bool
	webhookToken string
}

func pubDeps(o pubOpts) Deps {
	if o.getenv == nil {
		o.getenv = map[string]string{"SECRETS_MASTER_KEY": pubMasterKey}
	}
	if o.secretKey == "" {
		o.secretKey = "unit-test-secret-key-0123456789abcdef"
	}
	if o.appEnv == "" {
		o.appEnv = "test"
	}
	return Deps{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Getenv: func(k string) string { return o.getenv[k] },
		Config: config.APIConfig{SecretKey: o.secretKey, AppEnv: o.appEnv, TrustProxy: o.trustProxy},
		Flags: config.Flags{PGLeadStore: true, Outreach: config.OutreachSettings{
			GoRoutesEnabled: true, BounceWebhookSecret: config.Secret(o.webhookToken), UnsubTTLDays: 90, SoftBounceMax: 3,
			CircuitMinSends: 20, BouncePauseRate: 0.05, ComplaintPauseRate: 0.003}},
	}
}

func pubHandler(d Deps) http.Handler {
	r := httpx.NewRouter()
	registerOutreachPublic(r, d)
	return r.Handler()
}

type pubReq struct {
	method, path, body string
	hdr                map[string]string
	addr               string
}

func (q pubReq) do(h http.Handler) *httptest.ResponseRecorder {
	var rd io.Reader
	if q.body != "" {
		rd = strings.NewReader(q.body)
	}
	req := httptest.NewRequest(q.method, q.path, rd)
	req.RemoteAddr = "192.0.2.10:4242"
	if q.addr != "" {
		req.RemoteAddr = q.addr
	}
	if q.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range q.hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func pubSigner(now func() time.Time) *outreachtoken.Signer {
	return outreachtoken.NewSigner([]byte(pubMasterKey), "https://x.example", 90, now)
}

func pubToken(t *testing.T, ws, email, seq string, ttlDays int) string {
	t.Helper()
	tok, err := pubSigner(nil).Make(ws, email, seq, &ttlDays)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

const pubInvalidHTML = "<h2>Invalid unsubscribe link</h2>"

func pubFixtureBody(t *testing.T, fx contract.Fixture) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(fx.Response.Body, &s); err != nil {
		t.Fatalf("fixture body is not a string: %v", err)
	}
	return s
}

// TestUnsubscribeLandingMatchesFixtures: the recorded raw HTML bodies, byte for byte. The one recorded value with markup
// characters in the email is the D5 hardening case (escaped in Go, raw in Python).
func TestUnsubscribeLandingMatchesFixtures(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{}))
	for _, name := range strings.Fields(`unsub_get_no_token_400 unsub_get_empty_token_400 unsub_get_no_dot_400 unsub_get_forged_400
		unsub_get_expired_renders unsub_get_valid unsub_get_valid_nonascii unsub_get_valid_beta`) {
		fx := contract.Load(t, "outreach/"+name)
		t.Run(name, func(t *testing.T) {
			rec := pubReq{method: "GET", path: fx.Request.Path}.do(h)
			if rec.Code != fx.Response.Status {
				t.Fatalf("status %d, want %d", rec.Code, fx.Response.Status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Fatalf("content-type %q", ct)
			}
			if want := pubFixtureBody(t, fx); rec.Body.String() != want {
				t.Fatalf("body\n got %q\nwant %q", rec.Body.String(), want)
			}
		})
	}
}

// TestLandingEscapesEmailAndToken: the recorded HTML-email token renders escaped (XSS), and the escaped form is the
// recorded body with only & < > " ' replaced.
func TestLandingEscapesEmailAndToken(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{}))
	fx := contract.Load(t, "outreach/unsub_get_valid_html_email")
	rec := pubReq{method: "GET", path: fx.Request.Path}.do(h)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	raw := pubFixtureBody(t, fx)
	if !strings.Contains(raw, `a<b>&"x"@example.com`) {
		t.Fatalf("recorded email changed: %q", raw)
	}
	want := strings.Replace(raw, `a<b>&"x"@example.com`, `a&lt;b&gt;&amp;&#34;x&#34;@example.com`, 1)
	if rec.Body.String() != want {
		t.Fatalf("body\n got %q\nwant %q", rec.Body.String(), want)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<b>") || strings.Count(body, "<") != strings.Count(raw, "<")-1 {
		t.Fatalf("raw markup leaked: %q", body)
	}
	// a script payload in the address never becomes markup, and a crafted token cannot break out of the action attribute:
	// only a token that verifies is reflected, so an unverifiable one gets the generic page.
	tok := pubToken(t, "ws-alpha", `"><script>alert(1)</script>@x.example`, "", 30)
	rec = pubReq{method: "GET", path: "/api/outreach/unsubscribe?token=" + tok}.do(h)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "<script>") || !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	rec = pubReq{method: "GET", path: `/api/outreach/unsubscribe?token=%22%3E%3Cscript%3E.x`}.do(h)
	if rec.Code != 400 || rec.Body.String() != pubInvalidHTML {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if landingHTML("a'b", `t"k`) != `<h2>Unsubscribe</h2><p>Click below to stop receiving emails at a&#39;b.</p>`+
		`<form method="post" action="/api/outreach/unsubscribe?token=t&#34;k"><button type="submit">Unsubscribe</button></form>` {
		t.Fatal("landingHTML escaping")
	}
}

func TestExpiredTokenLandingRenders(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{}))
	tok := pubToken(t, "ws-alpha", "late@example.com", "s1", -2)
	rec := pubReq{method: "GET", path: "/api/outreach/unsubscribe?token=" + tok}.do(h)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "late@example.com") {
		t.Fatalf("an expired token still renders the confirmation page: %d %q", rec.Code, rec.Body.String())
	}
	if c := rec.Header().Get("Cache-Control"); c != "no-store" {
		t.Fatalf("cache-control %q", c)
	}
	if r := rec.Header().Get("Referrer-Policy"); r != "no-referrer" {
		t.Fatalf("referrer-policy %q", r)
	}
}

// TestLandingLastTokenWins: a repeated ?token= follows FastAPI/Starlette (the last value).
func TestLandingLastTokenWins(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{}))
	tok := pubToken(t, "ws-alpha", "dup@example.com", "", 30)
	rec := pubReq{method: "GET", path: "/api/outreach/unsubscribe?token=junk&token=" + tok}.do(h)
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	rec = pubReq{method: "GET", path: "/api/outreach/unsubscribe?token=" + tok + "&token=junk"}.do(h)
	if rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
}

// TestUnsubscribePostRejectsExpiredAndForged: every refusal is the same 400 body, whatever is wrong with the token, and
// none of them touches the database (the handler has no pool here).
func TestUnsubscribePostRejectsExpiredAndForged(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{}))
	forgedSig := pubToken(t, "ws-alpha", "x@example.com", "", 30)
	forgedSig = forgedSig[:len(forgedSig)-2] + "AA"
	paths := []string{
		"/api/outreach/unsubscribe", "/api/outreach/unsubscribe?token=", "/api/outreach/unsubscribe?token=nodothere",
		"/api/outreach/unsubscribe?token=" + forgedSig,
		"/api/outreach/unsubscribe?token=" + pubToken(t, "ws-alpha", "x@example.com", "", -1),
	}
	for i, p := range paths {
		rec := pubReq{method: "POST", path: p, addr: "192.0.2." + strconv.Itoa(20+i) + ":1"}.do(h)
		if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != `{"detail":"invalid or expired token"}` {
			t.Fatalf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	for _, name := range []string{"unsub_post_empty_400", "unsub_post_forged_400", "unsub_post_expired_400", "unsub_post_no_dot_400"} {
		fx := contract.Load(t, "outreach/"+name)
		rec := pubReq{method: "POST", path: fx.Request.Path, addr: "198.51.100." + strconv.Itoa(len(name)) + ":1"}.do(h)
		if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != `{"detail":"invalid or expired token"}` {
			t.Fatalf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestUnsubscribeRateLimitPerIPAndToken(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{}))
	bad := func(ip, tok string) int {
		return pubReq{method: "POST", path: "/api/outreach/unsubscribe?token=" + tok, addr: ip + ":9"}.do(h).Code
	}
	// per IP: 20 pass (here refused as 400, the rate gate comes first), the 21st and the 22nd are 429 whatever the token.
	for i := 1; i <= 20; i++ {
		if c := bad("203.0.113.1", "t"+strconv.Itoa(i)); c != 400 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	rec := pubReq{method: "POST", path: "/api/outreach/unsubscribe?token=fresh", addr: "203.0.113.1:9"}.do(h)
	if rec.Code != 429 || strings.TrimSpace(rec.Body.String()) != `{"detail":"rate limited"}` {
		t.Fatalf("21st: %d %q", rec.Code, rec.Body.String())
	}
	if c := bad("203.0.113.1", "another"); c != 429 {
		t.Fatalf("22nd: %d", c)
	}
	if c := bad("203.0.113.2", "other-ip-fine"); c != 400 {
		t.Fatalf("a different client is not limited: %d", c)
	}
	// per token prefix (32 chars): 20 different IPs share one token; characters after the 32nd do not matter.
	prefix := strings.Repeat("p", 32)
	for i := 1; i <= 20; i++ {
		if c := bad("198.51.100."+strconv.Itoa(i), prefix+strconv.Itoa(i)); c != 400 {
			t.Fatalf("token request %d: %d", i, c)
		}
	}
	if c := bad("198.51.100.200", prefix+"zzz"); c != 429 {
		t.Fatalf("21st use of the token prefix: %d", c)
	}
	if c := bad("198.51.100.201", strings.Repeat("q", 32)); c != 400 {
		t.Fatalf("another token: %d", c)
	}
	// the limited token does not consume the IP budget of the client that was refused (Python's short circuit order is
	// IP first, token second): the refused client keeps 429 only for the token it used.
	if c := bad("198.51.100.200", "unrelated-token"); c != 400 {
		t.Fatalf("refused on the token, the IP is still within budget: %d", c)
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	l := newUnsubLimiter(20, time.Minute, 1000, clock)
	for i := 0; i < 50_000; i++ {
		l.limited("ip:" + strconv.Itoa(i))
	}
	if n := l.size(); n > 1000 {
		t.Fatalf("limiter holds %d keys, cap 1000", n)
	}
	// a window is the last 60 seconds: after it, the budget is back and old entries are swept.
	for i := 0; i < 20; i++ {
		if l.limited("ip:hot") {
			t.Fatalf("hit %d limited", i+1)
		}
	}
	if !l.limited("ip:hot") {
		t.Fatal("21st hit in the window must be limited")
	}
	now = now.Add(61 * time.Second)
	if l.limited("ip:hot") {
		t.Fatal("budget must return once the window has passed")
	}
	for i := 0; i < 10; i++ {
		l.limited("ip:sweep" + strconv.Itoa(i))
	}
	if n := l.size(); n > 11 {
		t.Fatalf("expired entries were not swept: %d keys", n)
	}
	// an entry never holds more than max+1 timestamps, however hard one key is hammered
	for i := 0; i < 100_000; i++ {
		l.limited("ip:flood")
	}
	if n := l.maxEntry(); n > 21 {
		t.Fatalf("one key holds %d timestamps", n)
	}
}

// TestUnsubscribeUsesRealClientIP: behind nginx every request arrives from the proxy address; the client is X-Real-IP
// (set by nginx, never forwarded from the client). Without TRUST_PROXY the header is ignored so it cannot be used to
// dodge the limit.
func TestUnsubscribeUsesRealClientIP(t *testing.T) {
	run := func(trust bool, hdr func(i int) map[string]string) (limitedAt int) {
		h := pubHandler(pubDeps(pubOpts{trustProxy: trust}))
		for i := 1; i <= 25; i++ {
			rec := pubReq{method: "POST", path: "/api/outreach/unsubscribe?token=t" + strconv.Itoa(i), addr: "10.0.0.2:5555", hdr: hdr(i)}.do(h)
			if rec.Code == 429 {
				return i
			}
		}
		return 0
	}
	// trusted: distinct real client IPs behind the same proxy address are not lumped together
	if at := run(true, func(i int) map[string]string { return map[string]string{"X-Real-IP": "198.51.100." + strconv.Itoa(i)} }); at != 0 {
		t.Fatalf("distinct real clients were limited at request %d", at)
	}
	// trusted: one real client is limited on its 21st request, a spoofed X-Forwarded-For does not help it
	if at := run(true, func(i int) map[string]string {
		return map[string]string{"X-Real-IP": "198.51.100.77", "X-Forwarded-For": "1.1.1." + strconv.Itoa(i) + ", 198.51.100.77"}
	}); at != 21 {
		t.Fatalf("one real client limited at %d, want 21", at)
	}
	// not trusted: the proxy-controlled header is ignored, the socket peer is the client
	if at := run(false, func(i int) map[string]string {
		return map[string]string{"X-Real-IP": "198.51.100." + strconv.Itoa(i), "X-Forwarded-For": "9.9.9." + strconv.Itoa(i)}
	}); at != 21 {
		t.Fatalf("without TRUST_PROXY the header must be ignored: limited at %d", at)
	}
	// a malformed header value falls back to the socket peer instead of becoming a key
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.2:5555"
	r.Header.Set("X-Real-IP", "not an ip\r\nx")
	if got := publicClientIP(r, true); got != "10.0.0.2" {
		t.Fatalf("got %q", got)
	}
}

// TestMissingMasterKey503NoPanic: no SECRETS_MASTER_KEY and the insecure default SECRET_KEY outside dev cannot sign or
// verify tokens: answer 503, never panic, and never reveal anything about the token.
func TestMissingMasterKey503NoPanic(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{getenv: map[string]string{}, secretKey: config.InsecureDefaultSecretKey, appEnv: "production"}))
	fx := contract.Load(t, "outreach/unsub_get_valid")
	for _, m := range []string{"GET", "POST"} {
		rec := pubReq{method: m, path: fx.Request.Path}.do(h)
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "unsubscribe not configured") {
			t.Fatalf("%s: %d %q", m, rec.Code, rec.Body.String())
		}
	}
	// in dev the insecure key is allowed, like Python
	hd := pubHandler(pubDeps(pubOpts{getenv: map[string]string{}, secretKey: config.InsecureDefaultSecretKey, appEnv: "dev"}))
	if rec := (pubReq{method: "GET", path: "/api/outreach/unsubscribe?token=x.y"}).do(hd); rec.Code != 400 {
		t.Fatalf("dev: %d", rec.Code)
	}
	// the bounce webhook does not use tokens, so it is not affected
	hw := pubHandler(pubDeps(pubOpts{getenv: map[string]string{}, secretKey: config.InsecureDefaultSecretKey, appEnv: "production", webhookToken: pubWebhookSecret}))
	rec := pubReq{method: "POST", path: "/api/outreach/webhooks/bounce", body: `{}`, hdr: map[string]string{"X-Webhook-Secret": pubWebhookSecret}}.do(hw)
	if rec.Code != 422 {
		t.Fatalf("webhook: %d %q", rec.Code, rec.Body.String())
	}
}

// TestWebhookSecretOrder: the platform secret is checked before the body and before any lookup: unset -> 503 (even
// for a garbage body), wrong or missing -> 403, only then validation (422).
func TestWebhookSecretOrder(t *testing.T) {
	const path = "/api/outreach/webhooks/bounce"
	good := `{"message_id":"<x@y>"}`
	unset := pubHandler(pubDeps(pubOpts{}))
	for _, name := range []string{"wh_unset_503", "wh_unset_503_no_header"} {
		fx := contract.Load(t, "outreach/"+name)
		rec := pubReq{method: "POST", path: path, body: string(fx.Request.Body), hdr: fx.Request.Headers}.do(unset)
		var want, got any
		_ = json.Unmarshal(fx.Response.Body, &want)
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != 503 || toJSON(got) != toJSON(want) {
			t.Fatalf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	if rec := (pubReq{method: "POST", path: path, body: "{not json", hdr: map[string]string{"X-Webhook-Secret": "a"}}).do(unset); rec.Code != 503 {
		t.Fatalf("unset with garbage body: %d", rec.Code)
	}
	h := pubHandler(pubDeps(pubOpts{webhookToken: pubWebhookSecret}))
	for _, c := range []struct {
		name string
		hdr  map[string]string
		body string
	}{
		{"wh_no_header_403", nil, good},
		{"wh_wrong_secret_403", map[string]string{"X-Webhook-Secret": "wrong-secret"}, good},
		{"empty header", map[string]string{"X-Webhook-Secret": ""}, good},
		{"prefix of the secret", map[string]string{"X-Webhook-Secret": pubWebhookSecret[:10]}, good},
		{"secret plus suffix", map[string]string{"X-Webhook-Secret": pubWebhookSecret + "x"}, good},
		{"non-ascii header", map[string]string{"X-Webhook-Secret": "caf\xe9"}, good},
		{"wrong secret, garbage body", map[string]string{"X-Webhook-Secret": "nope"}, "{garbage"},
	} {
		rec := pubReq{method: "POST", path: path, body: c.body, hdr: c.hdr}.do(h)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"invalid signature"}` {
			t.Fatalf("%s: %d %q", c.name, rec.Code, rec.Body.String())
		}
	}
	rec := pubReq{method: "POST", path: path, body: `{}`, hdr: map[string]string{"X-Webhook-Secret": pubWebhookSecret}}.do(h)
	fx := contract.Load(t, "outreach/wh_missing_body_422")
	var want, got any
	_ = json.Unmarshal(fx.Response.Body, &want)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 422 || toJSON(got) != toJSON(want) {
		t.Fatalf("422: %d %q", rec.Code, rec.Body.String())
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestWebhookBodyValidation(t *testing.T) {
	h := pubHandler(pubDeps(pubOpts{webhookToken: pubWebhookSecret}))
	hdr := map[string]string{"X-Webhook-Secret": pubWebhookSecret}
	for _, c := range []struct{ body, typ string }{
		{``, "missing"}, {`[]`, "model_attributes_type"}, {`{"message_id":5}`, "string_type"}, {`{"message_id":null}`, "string_type"},
		{`{"message_id":"a","event":5}`, "string_type"}, {`{"message_id":"a","hard":"maybe"}`, "bool_parsing"},
		{`{"message_id":"a","hard":[1]}`, "bool_type"}, {`{"message_id":"` + strings.Repeat("a", 2000) + `"}`, "string_too_long"},
		{`{"message_id":"a\u0000b"}`, "value_error"},
	} {
		rec := pubReq{method: "POST", path: "/api/outreach/webhooks/bounce", body: c.body, hdr: hdr}.do(h)
		var m struct {
			Detail []struct{ Type string } `json:"detail"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if rec.Code != 422 || len(m.Detail) == 0 || m.Detail[0].Type != c.typ {
			t.Fatalf("%q: %d %s", c.body[:min(len(c.body), 40)], rec.Code, rec.Body.String())
		}
	}
	// the body cap: more than 64 KiB is refused before it is parsed
	rec := pubReq{method: "POST", path: "/api/outreach/webhooks/bounce", body: `{"message_id":"` + strings.Repeat("a", 70_000) + `"}`, hdr: hdr}.do(h)
	if rec.Code != 413 {
		t.Fatalf("oversized body: %d", rec.Code)
	}
}

// TestWebhookConstantTimeCompare: the secret is compared through a fixed-length digest in constant time, so a length
// mismatch or a common prefix takes no different path; equal strings match and nothing else does.
func TestWebhookConstantTimeCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"s3cret", "s3cret", true}, {"s3cret", "s3creT", false}, {"s3cret", "s3cre", false}, {"s3cret", "", false},
		{"", "", true}, {"caf\xe9", "caf\xe9", true}, {"caf\xe9", "cafe", false}, {strings.Repeat("a", 5000), strings.Repeat("a", 5000), true},
	}
	for _, c := range cases {
		if got := secretsEqual(c.a, c.b); got != c.want {
			t.Errorf("secretsEqual(%q,%q)=%v", c.a[:min(len(c.a), 10)], c.b[:min(len(c.b), 10)], got)
		}
	}
	// the route never accepts an empty provided secret against an empty configured one: unset is 503 first
	h := pubHandler(pubDeps(pubOpts{}))
	rec := pubReq{method: "POST", path: "/api/outreach/webhooks/bounce", body: `{"message_id":"x"}`, hdr: map[string]string{"X-Webhook-Secret": ""}}.do(h)
	if rec.Code != 503 {
		t.Fatalf("%d", rec.Code)
	}
}
