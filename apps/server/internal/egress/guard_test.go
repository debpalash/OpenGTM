package egress

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
)

// goStricter lists URLs Python's url_guard allows but the Go guard blocks on
// purpose (documented in docs/plugins/README.md).
var goStricter = map[string]string{
	"http://100.64.0.1/":      "CGNAT 100.64.0.0/10",
	"http://100.127.255.254/": "CGNAT 100.64.0.0/10",
	"http://127.1/":           "inet_aton short form resolves to 127.0.0.1",
	"http://[fec0::1]/":       "deprecated IPv6 site-local",
}

func TestGuardParityWithPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/guard_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		URL     string `json:"url"`
		Blocked bool   `json:"blocked"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	g := &Guard{}
	for _, c := range cases {
		err := g.CheckString(c.URL)
		got := err != nil
		if why, stricter := goStricter[c.URL]; stricter {
			if c.Blocked || !got {
				t.Errorf("%s: expected python-allow/go-block (%s), got python=%v go=%v", c.URL, why, c.Blocked, err)
			}
			continue
		}
		if got != c.Blocked {
			t.Errorf("%s: python blocked=%v (%s), go err=%v", c.URL, c.Blocked, c.Reason, err)
			continue
		}
		if got && c.Reason != "" && err.Error() != "blocked_url: "+c.Reason {
			// IPv6 renderings differ only in spelling; compare the class of reason.
			var be *BlockedError
			if !errors.As(err, &be) {
				t.Errorf("%s: wrong error type %T", c.URL, err)
			}
		}
	}
	if len(cases) < 60 {
		t.Fatalf("corpus too small: %d", len(cases))
	}
}

func TestIsBlockedIPExtras(t *testing.T) {
	for _, s := range []string{"100.64.0.1", "::ffff:169.254.169.254", "64:ff9b::7f00:1", "2002:7f00:1::1", "fd00:ec2::254", "fec0::1"} {
		if !IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func TestResolveRejectsAnyPrivateAnswer(t *testing.T) {
	g := &Guard{Resolver: fakeResolver{
		"mixed.test":  {"93.184.216.34", "10.0.0.1"},
		"public.test": {"93.184.216.34"},
		"mapped.test": {"::ffff:127.0.0.1"},
	}}
	if _, err := g.Resolve(context.Background(), "mixed.test"); err == nil {
		t.Fatal("one private answer must block the host")
	}
	if _, err := g.Resolve(context.Background(), "mapped.test"); err == nil {
		t.Fatal("IPv4-mapped loopback must be blocked")
	}
	ips, err := g.Resolve(context.Background(), "public.test")
	if err != nil || len(ips) != 1 || ips[0].String() != "93.184.216.34" {
		t.Fatalf("public: %v %v", ips, err)
	}
	if _, err := g.Resolve(context.Background(), "unknown.test"); err == nil {
		t.Fatal("resolution failure must block")
	}
}
