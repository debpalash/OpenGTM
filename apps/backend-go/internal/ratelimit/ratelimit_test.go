package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

func req(hdr map[string]string, remote string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// Plan 09 final security M2: buckets come from server-side state, never from request headers.
func TestKeyUsesAuthenticatedIdentityNotHeaders(t *testing.T) {
	l := New(nil)
	jwtHead := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." // identical for every HS256 token
	r1 := req(map[string]string{"Authorization": "Bearer " + jwtHead + "payloadA", "X-Workspace-Id": "victim"}, "1.2.3.4:5")
	r2 := req(map[string]string{"Authorization": "Bearer " + jwtHead + "payloadB", "X-Workspace-Id": "victim"}, "1.2.3.4:5")
	if l.key(r1) != l.key(r2) {
		t.Fatal("same client IP, no identity: both fall to the IP bucket, never the header or token prefix")
	}
	if k := l.key(r1); k != "ip:1.2.3.4" {
		t.Fatal(k)
	}
	// authenticated users get one bucket each, whatever their bearer prefix or X-Workspace-Id header
	u1 := r1.WithContext(identity.WithPrincipal(r1.Context(), identity.Principal{User: identity.User{ID: 1}}))
	u2 := r2.WithContext(identity.WithPrincipal(r2.Context(), identity.Principal{User: identity.User{ID: 2}}))
	if l.key(u1) != "user:1" || l.key(u2) != "user:2" {
		t.Fatal(l.key(u1), l.key(u2))
	}
	if k := l.key(req(nil, "1.2.3.4:5").WithContext(WithKey(context.Background(), "ws:7"))); k != "ws:7" {
		t.Fatal(k)
	}
}

func TestKeyIPIgnoresForwardedFor(t *testing.T) {
	t.Setenv("TRUST_PROXY", "true") // the old XFF switch must be inert
	l := New(nil)
	if k := l.key(req(map[string]string{"X-Forwarded-For": "9.9.9.9, 8.8.8.8"}, "8.8.4.4:5")); k != "ip:8.8.4.4" {
		t.Fatal("XFF trusted:", k)
	}
	// behind a trusted proxy (loopback / docker bridge) the client is X-Real-IP; XFF is still ignored
	if k := l.key(req(map[string]string{"X-Real-IP": "203.0.113.7", "X-Forwarded-For": "9.9.9.9"}, "172.18.0.5:5")); k != "ip:203.0.113.7" {
		t.Fatal(k)
	}
	// an untrusted peer cannot choose its bucket with X-Real-IP
	if k := l.key(req(map[string]string{"X-Real-IP": "203.0.113.7"}, "8.8.4.4:5")); k != "ip:8.8.4.4" {
		t.Fatal(k)
	}
	l.SetTrustedProxies(ParseTrustedProxies("10.0.0.0/8", nil))
	if k := l.key(req(map[string]string{"X-Real-IP": "203.0.113.7"}, "10.1.2.3:5")); k != "ip:203.0.113.7" {
		t.Fatal(k)
	}
	if k := l.key(req(map[string]string{"X-Real-IP": "203.0.113.7"}, "172.18.0.5:5")); k != "ip:172.18.0.5" {
		t.Fatal("explicit CIDRs replace the defaults:", k)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func do(h http.Handler, ws string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(nil, "1.1.1.1:1").WithContext(WithKey(context.Background(), "ws:"+ws)))
	return w
}

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
}

func TestFixedWindowResetsAtBoundary(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	h := New(c.now).Limit("2/minute")(ok())
	if do(h, "a").Code != 200 || do(h, "a").Code != 200 {
		t.Fatal("first two should pass")
	}
	c.t = c.t.Add(59 * time.Second)
	if do(h, "a").Code != 429 {
		t.Fatal("still in window")
	}
	c.t = c.t.Add(time.Second)
	if do(h, "a").Code != 200 {
		t.Fatal("window should have reset")
	}
}

func TestExceedReturns429BodyExact(t *testing.T) {
	h := New(time.Now).Limit("1/minute")(ok())
	do(h, "a")
	w := do(h, "a")
	if w.Code != 429 || w.Body.String() != `{"error":"Rate limit exceeded: 1 per 1 minute"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	h = New(time.Now).Limit("30/minute")(ok())
	for i := 0; i < 30; i++ {
		do(h, "a")
	}
	if b := do(h, "a").Body.String(); b != `{"error":"Rate limit exceeded: 30 per 1 minute"}` {
		t.Fatal(b)
	}
}

func TestTenantsDoNotShareBudget(t *testing.T) {
	h := New(time.Now).Limit("1/second")(ok())
	if do(h, "a").Code != 200 || do(h, "b").Code != 200 {
		t.Fatal("separate budgets")
	}
	if do(h, "a").Code != 429 {
		t.Fatal("a exhausted")
	}
}

func TestSpecParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "x/minute", "0/minute", "-1/minute", "5/week", "5", "5/", "/minute", "5/minute/x"} {
		if _, _, _, err := parseSpec(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for _, s := range []string{"5/second", "5/minute", "5/hour", "5/day"} {
		if _, _, _, err := parseSpec(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Limit must panic on bad spec")
		}
	}()
	New(time.Now).Limit("nope")
}

func TestConcurrentCallsNoRace(t *testing.T) {
	h := New(time.Now).Limit("50/minute")(ok())
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if do(h, "a").Code == 200 {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 50 {
		t.Fatal(passed)
	}
}

func TestBoundedSize(t *testing.T) {
	l := New(time.Now)
	h := l.Limit("1/day")(ok())
	for i := 0; i < maxKeys+10; i++ {
		do(h, strings.Repeat("k", 1)+time.Duration(i).String())
	}
	if n := len(l.hits); n > maxKeys {
		t.Fatal(n)
	}
}

// Plan 10 final security M2: a flood of new keys at the cap costs O(log n) per request, never a
// scan of the map, and the map and the heap stay in step and bounded.
func TestFullMapEvictionIsNotAScan(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(func() time.Time { return now })
	l.max = 50_000
	for i := 0; i < l.max; i++ {
		l.hit("fill"+time.Duration(i).String(), 5, time.Minute)
	}
	start := time.Now()
	for i := 0; i < 200_000; i++ {
		l.hit("new"+time.Duration(i).String(), 5, time.Minute)
	}
	// 200k inserts at a full 50k map: a per-insert full scan would take minutes, the heap well under a second.
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("200k new keys at the cap took %s", d)
	}
	if len(l.hits) > l.max || len(l.exp) != len(l.hits) {
		t.Fatalf("hits %d heap %d max %d", len(l.hits), len(l.exp), l.max)
	}
	now = now.Add(2 * time.Minute) // everything has expired; the next hit sweeps it all
	l.hit("after", 5, time.Minute)
	if len(l.hits) != 1 || len(l.exp) != 1 {
		t.Fatalf("after expiry: hits %d heap %d", len(l.hits), len(l.exp))
	}
}

// A window that expires starts fresh at the next hit for its key.
func TestExpiredWindowRestarts(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		l.hit("k", 2, time.Minute)
	}
	if l.hit("k", 2, time.Minute) {
		t.Fatal("over budget must stay blocked")
	}
	now = now.Add(time.Minute)
	if !l.hit("k", 2, time.Minute) {
		t.Fatal("a new window must allow the next hit")
	}
}
