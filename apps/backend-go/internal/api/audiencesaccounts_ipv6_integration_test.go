//go:build integration

package api

import (
	"strings"
	"testing"
)

// Issue 114 follow-up: invalid IPv6 website literals answer 200 (Python 500ed), each member keyed by its company.
func TestAccountsInvalidIPv6WebsiteNoCrash(t *testing.T) {
	e := newAudEnv(t)
	accSeed(t, "ws-alpha", "acc-ipv6", []accMember{
		{9401, `{"company":"Six Bad","website":"http://[bad]/"}`},
		{9402, `{"company":"Six Zz","website":"https://[::zz]:443/x"}`},
		{9403, `{"company":"Six Long","website":"http://[1:2:3:4:5:6:7:8:9]/"}`},
		{9404, `{"company":"Six Ok","website":"http://[::1]:8080/"}`},
	})
	code, m, raw := e.accounts(t, "alice", "ws-alpha", "/api/audiences/acc-ipv6/accounts")
	if code != 200 {
		t.Fatalf("invalid IPv6 website: %d %s", code, raw)
	}
	got := strings.Join(accKeys(m), "|")
	for _, k := range []string{"company:six bad", "company:six zz", "company:six long", "::1"} {
		if !strings.Contains(got, k) {
			t.Fatalf("%s missing in %s", k, got)
		}
	}
}
