package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

type fakeResolver struct {
	mu    sync.Mutex
	calls int
	seq   [][]netip.Addr
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.seq) {
		i = len(f.seq) - 1
	}
	f.calls++
	return f.seq[i], nil
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for _, x := range s {
		out = append(out, netip.MustParseAddr(x))
	}
	return out
}

func TestRedirectNotFollowed(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer srv.Close()
	u := strings.TrimPrefix(srv.URL, "http://")
	host, _, _ := net.SplitHostPort(u)
	c := NewClientForEnv("test", []string{host})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 1 {
		t.Fatalf("status=%d hits=%d", resp.StatusCode, hits)
	}
	if c.Jar != nil {
		t.Fatal("cookie jar must be nil")
	}
}

func TestDialTimeRebindBlocked(t *testing.T) {
	res := &fakeResolver{seq: [][]netip.Addr{addrs("93.184.216.34"), addrs("127.0.0.1")}}
	var dialed []string
	c := newClient(nil, "", res, func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("fake dial")
	})
	// First lookup is public: dial goes to the vetted IP, never the hostname.
	_, _ = c.Get("http://rebind.example/")
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:80" {
		t.Fatalf("dialed %v", dialed)
	}
	// Rebinding answer (loopback) must be rejected before any dial.
	_, err := c.Get("http://rebind.example/")
	if err == nil || !strings.Contains(err.Error(), "blocked") || len(dialed) != 1 {
		t.Fatalf("err=%v dialed=%v", err, dialed)
	}
}

func TestDialRejectsAnyBlockedIP(t *testing.T) {
	res := &fakeResolver{seq: [][]netip.Addr{addrs("93.184.216.34", "10.0.0.5")}}
	dialed := 0
	c := newClient(nil, "", res, func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("fake dial")
	})
	if _, err := c.Get("http://mixed.example/"); err == nil || dialed != 0 {
		t.Fatalf("err=%v dialed=%d", err, dialed)
	}
}

func TestAllowlistIgnoredOutsideBench(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	for _, env := range []string{"", "production", "dev"} {
		c := NewClientForEnv(env, []string{host})
		if resp, err := c.Get(srv.URL); err == nil {
			resp.Body.Close()
			t.Fatalf("env %q: loopback should be blocked", env)
		}
	}
	for _, env := range []string{"test", "bench", " TEST "} {
		c := NewClientForEnv(env, []string{strings.ToUpper(host)})
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("env %q: %v", env, err)
		}
		resp.Body.Close()
	}
}

func TestPoolSettings(t *testing.T) {
	c := NewClient(nil)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.MaxIdleConnsPerHost != 64 || tr.IdleConnTimeout.Seconds() != 90 {
		t.Fatalf("bad transport %+v", c.Transport)
	}
}
