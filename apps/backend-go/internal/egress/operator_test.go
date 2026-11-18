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

// An allow-listed name is still resolved, and an answer in the metadata or
// link-local ranges is refused: listing a host must not open a path to cloud
// instance credentials (for example a CUSTOM_OPENAI_BASE_URL hostname that
// resolves or is rebound to 169.254.169.254).
func TestOperatorAllowListedHostRefusesMetadataAndLinkLocal(t *testing.T) {
	var dialed string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return nil, errors.New("dialed " + addr)
	}
	for _, c := range []struct {
		name  string
		ips   []netip.Addr
		url   string
		block bool
		want  string
	}{
		{"name to metadata v4", addrs("169.254.169.254"), "http://llm.internal:8008/", true, ""},
		{"name to link-local", addrs("169.254.10.10"), "http://llm.internal:8008/", true, ""},
		{"name to metadata v6", addrs("fd00:ec2::254"), "http://llm.internal:8008/", true, ""},
		{"mixed answer", addrs("10.0.0.5", "169.254.169.254"), "http://llm.internal:8008/", true, ""},
		{"name to private ip dials vetted ip", addrs("10.0.0.5"), "http://llm.internal:8008/", false, "10.0.0.5:8008"},
		{"name to loopback allowed", addrs("127.0.0.1"), "http://llm.internal:8008/", false, "127.0.0.1:8008"},
	} {
		dialed = ""
		res := &fakeResolver{seq: [][]netip.Addr{c.ips}}
		cl := newOperatorClient([]string{"llm.internal:8008"}, res, dial)
		resp, err := cl.Get(c.url)
		if resp != nil {
			resp.Body.Close()
		}
		if got := errors.Is(err, ErrBlocked); got != c.block {
			t.Errorf("%s: blocked=%v want %v (err=%v)", c.name, got, c.block, err)
		}
		if dialed != c.want {
			t.Errorf("%s: dialed %q want %q", c.name, dialed, c.want)
		}
	}
	// A literal metadata IP is refused even when listed exactly.
	cl := newOperatorClient([]string{"169.254.169.254:80"}, &fakeResolver{}, dial)
	if resp, err := cl.Get("http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlocked) {
		if resp != nil {
			resp.Body.Close()
		}
		t.Errorf("listed metadata literal: err=%v, want blocked", err)
	}
}
