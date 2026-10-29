//go:build integration

package api

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func newLimitedAuthEnv(t *testing.T) env {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterAuth(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Limiter: frozenLimiter()})
	return env{h: r.Handler(), st: st}
}

func postFrom(e env, path, ctype, body, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func want429(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != 429 {
		t.Fatalf("%s: want 429, got %d %s", what, rec.Code, rec.Body.String())
	}
	if s, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || s < 1 {
		t.Fatalf("%s: Retry-After %q", what, rec.Header().Get("Retry-After"))
	}
}

// Final security M2: online password guessing is throttled per username, whatever IP it comes from.
func TestTokenRateLimitedPerUsername(t *testing.T) {
	e := newLimitedAuthEnv(t)
	e.seedPW(t, "rl-victim", "right-password-123", true)
	body := form("username", "rl-victim", "password", "wrong")
	for i := 0; i < tokenUserBurst; i++ {
		remote := "203.0.113." + strconv.Itoa(i+1) + ":1234"
		if rec := postFrom(e, "/auth/token", formCT, body, remote); rec.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, rec.Code)
		}
	}
	// A fresh IP, same username (case/space variants included), even with the right password: refused before bcrypt.
	want429(t, postFrom(e, "/auth/token", formCT, form("username", " RL-Victim ", "password", "right-password-123"), "198.51.100.9:1"), "per-username")
	// Another username is unaffected.
	e.seedPW(t, "rl-other", "right-password-123", true)
	if rec := postFrom(e, "/auth/token", formCT, form("username", "rl-other", "password", "right-password-123"), "198.51.100.9:1"); rec.Code != 200 {
		t.Fatalf("other user: %d", rec.Code)
	}
}

// Final security M2: one client IP cannot spray many usernames.
func TestTokenRateLimitedPerIP(t *testing.T) {
	e := newLimitedAuthEnv(t)
	for i := 0; i < tokenIPBurst; i++ {
		body := form("username", "nobody-"+strconv.Itoa(i), "password", "x")
		if rec := postFrom(e, "/auth/token", formCT, body, "192.0.2.7:5555"); rec.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, rec.Code)
		}
	}
	want429(t, postFrom(e, "/auth/token", formCT, form("username", "nobody-x", "password", "x"), "192.0.2.7:5555"), "per-ip")
	if rec := postFrom(e, "/auth/token", formCT, form("username", "nobody-y", "password", "x"), "192.0.2.8:5555"); rec.Code != 401 {
		t.Fatalf("other IP: %d", rec.Code)
	}
}

// Final security M2: /auth/refresh is throttled per client IP.
func TestRefreshRateLimitedPerIP(t *testing.T) {
	e := newLimitedAuthEnv(t)
	for i := 0; i < refreshIPBurst; i++ {
		if rec := postFrom(e, "/auth/refresh", "application/json", `{"refresh_token":"bogus"}`, "192.0.2.9:1"); rec.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, rec.Code)
		}
	}
	want429(t, postFrom(e, "/auth/refresh", "application/json", `{"refresh_token":"bogus"}`, "192.0.2.9:1"), "refresh")
}
