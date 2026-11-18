package egress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func handler200() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}
}

func TestRuntimeProviderHost(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"http://10.10.9.45:8008/v1", "10.10.9.45:8008", true},
		{"http://localhost:8000/v1", "localhost:8000", true},
		{"https://llm.internal.example/v1", "llm.internal.example:443", true},
		{"http://192.168.1.5:8080", "192.168.1.5:8080", true},
		{"http://169.254.169.254/latest/meta-data", "", false}, // cloud metadata
		{"http://metadata.google.internal", "", false},         // GCE metadata name
		{"http://metadata/", "", false},                        // metadata alias
		{"http://fe80::1/v1", "", false},                       // link-local v6
		{"ftp://host/v1", "", false},                           // scheme
		{"http://10.0.0.5", "10.0.0.5:80", true},               // default http port
		{"http://[fd00::5]:8008/v1", "[fd00::5]:8008", true},   // IPv6 literal
		{"", "", false},        // empty
		{"http://", "", false}, // no host
		{"http://0x0a0a092d:8008/v1", "0x0a0a092d:8008", true}, // numeric encodings pass through: the host string is what gets allow-listed and re-vetted at dial time
	} {
		got, ok := RuntimeProviderHost(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("RuntimeProviderHost(%q) = %q,%v want %q,%v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// NewOperatorClientFunc re-reads the allow-list on every dial: an
// operator-configured host added after the client was built becomes dialable,
// and one removed stops being dialable.
func TestOperatorClientFuncReReadsAllowList(t *testing.T) {
	upstream := httptest.NewServer(handler200())
	defer upstream.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))

	var allowed atomic.Bool
	cl := NewOperatorClientFunc(func() []string {
		if allowed.Load() {
			return []string{net.JoinHostPort(host, port)}
		}
		return nil
	})
	get := func() error {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL+"/v1/models", nil)
		// Never reuse a pooled connection: the guard runs at dial time, and a
		// keep-alive socket returned to the pool asynchronously would let the
		// last request skip the dial (and the re-read allow-list) entirely.
		req.Close = true
		resp, err := cl.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := get(); err == nil {
		t.Fatal("loopback upstream must be refused when not allow-listed")
	}
	allowed.Store(true)
	if err := get(); err != nil {
		t.Fatalf("allow-listed host refused: %v", err)
	}
	allowed.Store(false)
	// The guard is applied at dial time; force a fresh connection so the
	// removed entry is re-evaluated instead of reusing the kept-alive socket.
	cl.CloseIdleConnections()
	if err := get(); err == nil {
		t.Fatal("removed entry must stop being dialable")
	}
}

// The runtime entry admits only the configured port: another port on the same
// host stays refused by the SSRF guard.
func TestRuntimeProviderHostOtherPortRefused(t *testing.T) {
	configured := httptest.NewServer(handler200())
	defer configured.Close()
	other := httptest.NewServer(handler200())
	defer other.Close()
	entry, ok := RuntimeProviderHost(configured.URL + "/v1")
	if !ok {
		t.Fatalf("RuntimeProviderHost(%q) rejected", configured.URL)
	}
	cl := NewOperatorClientFunc(func() []string { return []string{entry} })
	get := func(base string) error {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/v1/models", nil)
		req.Close = true
		resp, err := cl.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := get(configured.URL); err != nil {
		t.Fatalf("configured port refused: %v", err)
	}
	if err := get(other.URL); err == nil {
		t.Fatal("another port on the configured host must be refused")
	}
}
