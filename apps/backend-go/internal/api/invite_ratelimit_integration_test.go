//go:build integration

package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Final security review M2: the routes that add an account to a workspace (this API has no separate
// invite/accept flow: POST /api/governance/members and POST /admin/users) are throttled per workspace
// and per user, 429 + Retry-After.
func TestMemberAddRateLimitedPerWorkspace(t *testing.T) {
	e := newRBEnv(t, rbLimited)
	for i := 0; i < memberAddBurst; i++ {
		rec, _ := e.call("alice", "ws-alpha", "POST", "/api/governance/members", `{}`)
		if rec.Code == 429 {
			t.Fatalf("request %d limited too early", i)
		}
	}
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/governance/members", `{}`)
	want429(t, rec, "member add")
	// Another workspace has its own budget.
	if rec, _ := e.call("bob", "ws-beta", "POST", "/api/governance/members", `{}`); rec.Code == 429 {
		t.Fatalf("ws-beta must not share ws-alpha's budget")
	}
	// Reads are not throttled by the add budget.
	if rec, _ := e.call("alice", "ws-alpha", "GET", "/api/governance/rbac", ""); rec.Code == 429 {
		t.Fatalf("rbac read limited")
	}
}

func TestAdminUserCreateRateLimitedPerUser(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterUsers(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Limiter: ratelimit.New(nil)})
	e := env{h: r.Handler(), st: st}
	_, hdr := e.seedAdmin(t, "rl-admin")
	for i := 0; i < userCreateBurst; i++ {
		req := httptest.NewRequest("POST", "/admin/users", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", hdr["Authorization"])
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code == 429 {
			t.Fatalf("request %d limited too early", i)
		}
	}
	req := httptest.NewRequest("POST", "/admin/users", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", hdr["Authorization"])
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	want429(t, rec, "admin user create")
}
