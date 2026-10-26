package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// TestOperatorAllowListOnlyListedHostsBypass pins the operator egress
// allow-list (EGRESS_ALLOW_HOSTS for the provider client): only exact listed
// host[:port] entries skip the private-IP block, whatever APP_ENV is.
func TestOperatorAllowListOnlyListedHostsBypass(t *testing.T) {
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		return nil, errors.New("dialed " + addr)
	}
	cases := []struct {
		name  string
		allow []string
		url   string
		block bool
	}{
		{"listed host:port", []string{"Reacher.Internal:8080"}, "http://reacher.internal:8080/", false},
		{"listed host:port other port", []string{"reacher.internal:8080"}, "http://reacher.internal:9090/", true},
		{"host-only entry any port", []string{"other.internal"}, "http://other.internal:1234/", false},
		{"unlisted host", []string{"reacher.internal:8080"}, "http://evil.internal:8080/", true},
		{"literal private ip not listed", []string{"reacher.internal:8080"}, "http://10.0.0.5:8080/", true},
		{"literal ip listed exactly", []string{"10.0.0.5:8080"}, "http://10.0.0.5:8080/", false},
		{"empty list blocks all", nil, "http://reacher.internal:8080/", true},
		{"blank entries ignored", []string{" ", ""}, "http://reacher.internal:8080/", true},
	}
	for _, c := range cases {
		res := &fakeResolver{seq: [][]netip.Addr{addrs("10.0.0.5")}}
		cl := newOperatorClient(c.allow, res, dial)
		resp, err := cl.Get(c.url)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatalf("%s: dial stub must fail", c.name)
		}
		if got := errors.Is(err, ErrBlocked); got != c.block {
			t.Errorf("%s: blocked=%v want %v (err=%v)", c.name, got, c.block, err)
		}
	}
}

func TestOperatorClientNeverFollowsRedirects(t *testing.T) {
	cl := NewOperatorClient([]string{"x.internal"})
	if cl.CheckRedirect == nil {
		t.Fatal("operator client must refuse redirects")
	}
}
