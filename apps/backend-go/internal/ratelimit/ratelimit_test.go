package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func req(hdr map[string]string, remote string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestKeyPrecedenceWorkspaceThenTokenThenIP(t *testing.T) {
	t.Setenv("TRUST_PROXY", "")
	tok := strings.Repeat("a", 40)
	if got := Key(req(map[string]string{"X-Workspace-Id": "7", "Authorization": "Bearer " + tok}, "1.2.3.4:5")); got != "ws:7" {
		t.Fatal(got)
	}
	if got := Key(req(map[string]string{"Authorization": "bearer  " + tok + " "}, "1.2.3.4:5")); got != "tok:"+tok[:32] {
		t.Fatal(got)
	}
	if got := Key(req(map[string]string{"Authorization": "Bearer   "}, "1.2.3.4:5")); got != "ip:1.2.3.4" {
		t.Fatal(got)
	}
	if got := Key(req(map[string]string{"X-Forwarded-For": "9.9.9.9"}, "1.2.3.4:5")); got != "ip:1.2.3.4" {
		t.Fatal("XFF trusted:", got)
	}
	t.Setenv("TRUST_PROXY", "true")
	if got := Key(req(map[string]string{"X-Forwarded-For": "9.9.9.9, 8.8.8.8"}, "1.2.3.4:5")); got != "ip:9.9.9.9" {
		t.Fatal(got)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func do(h http.Handler, ws string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(map[string]string{"X-Workspace-Id": ws}, "1.1.1.1:1"))
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
