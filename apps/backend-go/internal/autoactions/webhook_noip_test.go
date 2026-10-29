package autoactions

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
)

// fixedPrivate is the stored text for a host that resolves to a blocked address (final review L8).
const fixedPrivate = "blocked url: host resolves to a private address"

// L8: stored run/delivery errors never carry an address the server resolved or dialed (a DNS
// and internal-address oracle for members who can read results). The detail goes to the log.
func TestBlockedErrorsNeverCarryResolvedIPs(t *testing.T) {
	for _, ans := range [][]string{{"93.184.216.34", "10.0.0.5"}, {"100.64.1.1"}, {"::ffff:10.0.0.1"}, {"fd00::1"}} {
		h := newHarness(t, nil)
		h.res.answers = map[string][]string{"hooks.example.com": ans}
		got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
		if got.Status != "failed" || got.SkipReason != "blocked_url" || got.Error != fixedPrivate {
			t.Errorf("%v: %+v", ans, got)
		}
	}
}

type serverRes struct{}

func (serverRes) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: "hooks.example.com", Server: "10.9.8.7:53", IsNotFound: true}
}

func TestResolveErrorOmitsResolverAddress(t *testing.T) {
	h := newHarness(t, nil)
	client := newWebhookClient("production", nil, serverRes{}, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unreachable")
	})
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // never dials
	h.deps.Client = client
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got.SkipReason != "blocked_url" || !strings.HasPrefix(got.Error, "blocked url: cannot resolve host: ") || strings.Contains(got.Error, "10.9.8.7") {
		t.Fatalf("%+v", got)
	}
}

func TestDialErrorOmitsDialedAddress(t *testing.T) {
	h := newHarness(t, nil)
	h.dialErr = &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("93.184.216.34"), Port: 443},
		Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	got := h.run(map[string]any{"url": "https://hooks.example.com/x"})
	if got.Status != "failed" || strings.Contains(got.Error, "93.184.216.34") || got.Error == "" {
		t.Fatalf("%+v", got)
	}
}
