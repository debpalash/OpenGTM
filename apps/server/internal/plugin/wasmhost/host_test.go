package wasmhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

const exampleDir = "../../../../../plugins/examples/wasm-echo-provider"

type secretMap map[string]string

func (m secretMap) Secret(_ context.Context, name string) (string, error) { return m[name], nil }

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	plugin *Plugin
	srv    *httptest.Server
	hits   *atomic.Int32
	logs   *syncBuffer
}

// setup copies the committed example module into a temp plugin whose network
// capability covers the local TLS test server.
func setup(t *testing.T, timeoutSeconds float64, maxOutput int64, maxInstances int) fixture {
	t.Helper()
	hits := &atomic.Int32{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"Acme","employees":42}`)
	}))
	t.Cleanup(srv.Close)
	wasm, err := os.ReadFile(filepath.Join(exampleDir, "plugin.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(exampleDir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(src), `network: ["https://api.github.com"]`, `network: ["https://127.0.0.1:*"]`, 1)
	text = strings.Replace(text, "timeout_seconds: 10", fmt.Sprintf("timeout_seconds: %v", timeoutSeconds), 1)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := egress.New(egress.Options{AllowPrivateForTesting: true, DefaultRPS: 1000, TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h := New(Options{Client: client, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), MaxOutputBytes: maxOutput, MaxInstances: maxInstances})
	pl, err := h.Load(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pl.Close(context.Background()) })
	return fixture{plugin: pl, srv: srv, hits: hits, logs: logs}
}

func TestAllowedFetchSecretsAndLogs(t *testing.T) {
	f := setup(t, 5, 0, 2)
	secrets := secretMap{"ECHO_API_KEY": "echo-secret-value", "UNDECLARED_SECRET": "must-not-leak"}
	out, res, err := f.plugin.Invoke(context.Background(), map[string]any{"url": f.srv.URL + "/company"}, nil, secrets)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := out["fields"].(map[string]any)
	if fields["name"] != "Acme" || fields["employees"] != float64(42) || fields["status"] != float64(200) {
		t.Fatalf("fields = %v", fields)
	}
	visible := fmt.Sprint(fields["secrets_visible"])
	if visible != "[ECHO_API_KEY]" {
		t.Fatalf("declared secret only: got %s", visible)
	}
	if len(res.Fetches) != 1 || res.Fetches[0].Status != 200 || res.Fetches[0].SHA256 == "" || f.hits.Load() != 1 {
		t.Fatalf("evidence %+v hits %d", res.Fetches, f.hits.Load())
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "plugin=wasm_echo_provider") || !strings.Contains(logs, "fetching https://127.0.0.1") {
		t.Fatalf("logs: %s", logs)
	}
	// Without the declared secret resolved, nothing is visible.
	out, _, err = f.plugin.Invoke(context.Background(), map[string]any{"url": f.srv.URL + "/company"}, nil, secretMap{"UNDECLARED_SECRET": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if v := fmt.Sprint(out["fields"].(map[string]any)["secrets_visible"]); v != "[]" {
		t.Fatalf("secrets visible without declaration: %s", v)
	}
}

func TestUndeclaredHostDenied(t *testing.T) {
	f := setup(t, 5, 0, 1)
	for _, target := range []string{"https://evil.example/steal", "http://127.0.0.1:1/plain-http", "https://169.254.169.254/latest/meta-data/"} {
		out, res, err := f.plugin.Invoke(context.Background(), map[string]any{"url": target}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		fe, _ := out["fields"].(map[string]any)["fetch_error"].(map[string]any)
		if fe["code"] != "capability_denied" {
			t.Fatalf("%s: expected capability_denied, got %v", target, out)
		}
		if len(res.Fetches) != 0 {
			t.Fatalf("%s: denied fetch recorded evidence", target)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("denied fetch reached a server")
	}
}

func TestTimeoutStopsInfiniteLoop(t *testing.T) {
	f := setup(t, 1, 0, 1)
	start := time.Now()
	_, err := f.plugin.Call(context.Background(), "spin", []byte("x"), nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout, got %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("timeout took %s", el)
	}
	// The poisoned instance is discarded; the pool keeps serving.
	if _, _, err := f.plugin.Invoke(context.Background(), map[string]any{"url": f.srv.URL}, nil, nil); err != nil {
		t.Fatalf("plugin unusable after timeout: %v", err)
	}
}

func TestMemoryLimit(t *testing.T) {
	f := setup(t, 5, 0, 1)
	if _, err := f.plugin.Call(context.Background(), "grow", []byte("4"), nil); err != nil {
		t.Fatalf("4 MiB within a 32 MiB limit: %v", err)
	}
	if _, err := f.plugin.Call(context.Background(), "grow", []byte("64"), nil); err == nil {
		t.Fatal("64 MiB must exceed the 32 MiB memory limit")
	}
	if _, err := f.plugin.Call(context.Background(), "grow", []byte("2"), nil); err != nil {
		t.Fatalf("pool must recover after a trap: %v", err)
	}
}

func TestOutputCap(t *testing.T) {
	f := setup(t, 5, 1024, 1)
	if res, err := f.plugin.Call(context.Background(), "flood", []byte("100"), nil); err != nil || len(res.Output) != 100 {
		t.Fatalf("small output: %v", err)
	}
	if _, err := f.plugin.Call(context.Background(), "flood", []byte("4096"), nil); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("expected output cap, got %v", err)
	}
}

func TestHashMismatchAndConcurrency(t *testing.T) {
	f := setup(t, 5, 0, 2)
	p := *f.plugin.manifest
	w := *p.Wasm
	w.SHA256 = strings.Repeat("0", 64)
	p.Wasm = &w
	if _, err := New(Options{}).Load(context.Background(), &p); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := f.plugin.Invoke(context.Background(), map[string]any{"url": f.srv.URL}, nil, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.plugin.Invoke(context.Background(), map[string]any{}, nil, nil); err == nil {
		t.Fatal("inputs schema must be enforced")
	}
}
