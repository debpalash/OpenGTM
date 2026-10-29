package ratelimit

import (
	"net/http/httptest"
	"testing"
	"time"
)

// A 429 tells the client when the window reopens (RFC 6585 Retry-After, whole seconds, at least 1).
func TestExceedSetsRetryAfter(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	h := New(c.now).Limit("1/minute")(ok())
	if w := do(h, "a"); w.Code != 200 || w.Header().Get("Retry-After") != "" {
		t.Fatalf("allowed: %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	c.t = c.t.Add(20*time.Second + 300*time.Millisecond)
	w := do(h, "a")
	if w.Code != 429 || w.Header().Get("Retry-After") != "40" {
		t.Fatalf("got %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

// Keyed limits a caller-derived key (a login username) with the same windows, 429 body and Retry-After.
func TestKeyedCheck(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	check := New(c.now).Keyed("2/minute")
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		if !check(w, "user:alice") {
			t.Fatalf("hit %d refused", i)
		}
	}
	w := httptest.NewRecorder()
	if check(w, "user:alice") {
		t.Fatal("third hit allowed")
	}
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || w.Body.String() != `{"error":"Rate limit exceeded: 2 per 1 minute"}` {
		t.Fatalf("got %d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if !check(httptest.NewRecorder(), "user:bob") {
		t.Fatal("bob shares alice's budget")
	}
	c.t = c.t.Add(time.Minute)
	if !check(httptest.NewRecorder(), "user:alice") {
		t.Fatal("window did not reset")
	}
}
