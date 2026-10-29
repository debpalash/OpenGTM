// Package telemetry provides structured job logging and a loopback-only debug
// server (pprof and a minimal /metrics text endpoint).
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync/atomic"
	"time"
)

var abandonedProviderCalls atomic.Int64

// ProviderCallAbandoned records a provider goroutine left running past its
// deadline; call ProviderCallReturned when it finally returns.
func ProviderCallAbandoned() { abandonedProviderCalls.Add(1) }

// ProviderCallReturned releases one abandoned provider call from the gauge.
func ProviderCallReturned() { abandonedProviderCalls.Add(-1) }

// AbandonedProviderCalls is the number of provider goroutines still running
// after their call timed out (exported as provider_calls_abandoned).
func AbandonedProviderCalls() int64 { return abandonedProviderCalls.Load() }

// DefaultDebugAddr is used when GO_WORKER_DEBUG_ADDR is unset.
const DefaultDebugAddr = "127.0.0.1:6060"

var startedAt = time.Now()

// NewLogger returns a JSON slog logger writing to w.
func NewLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

// JobLogger returns l with the job_id, workspace_id and lease_token attrs set.
func JobLogger(l *slog.Logger, jobID int64, workspaceID string, leaseToken int64) *slog.Logger {
	return l.With("job_id", jobID, "workspace_id", workspaceID, "lease_token", leaseToken)
}

// StartDebugServer serves pprof and /metrics on addr, which must be a loopback
// IP literal. The server shuts down when ctx is cancelled.
func StartDebugServer(ctx context.Context, addr string) (*http.Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("telemetry: bad debug addr %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("telemetry: debug addr %q must use a loopback IP", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "go_goroutines %d\nprocess_uptime_seconds %.0f\nprovider_calls_abandoned %d\n", runtime.NumGoroutine(), time.Since(startedAt).Seconds(), AbandonedProviderCalls())
	})
	srv := &http.Server{Addr: ln.Addr().String(), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return srv, nil
}
