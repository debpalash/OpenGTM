// Command searxng-pool is a high-concurrency search gateway in front of a pool
// of SearXNG instances (our self-hosted one + community public instances).
//
// Why: OpenGTM's website-independent enrichment fallbacks query the web via
// SearXNG, but a single instance's IP gets CAPTCHA'd by Google/DDG/Brave. A pool
// of public instances — each its own IP, many with working Google engines —
// sidesteps that. This service health-checks the pool (liveness + JSON support),
// then fans each query out across several healthy instances concurrently and
// merges the results. Stdlib only; built for Go's concurrency model.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	cfg := LoadConfig()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("searxng-pool ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool := NewPool(cfg)
	pool.Bootstrap(ctx)
	go pool.HealthLoop(ctx)

	mux := http.NewServeMux()

	// GET /search?q=...&engines=bing,qwant&n=4
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, `{"error":"missing q"}`, http.StatusBadRequest)
			return
		}
		engines := r.URL.Query().Get("engines")
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		out := pool.Search(r.Context(), q, engines, n)
		writeJSON(w, http.StatusOK, out)
	})

	// GET /health — liveness + pool summary
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		snaps := pool.Snapshot()
		healthy, jsonOK := 0, 0
		for _, s := range snaps {
			if s.Healthy {
				healthy++
			}
			if s.Healthy && s.JSONOK {
				jsonOK++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            jsonOK > 0,
			"instances":    len(snaps),
			"healthy":      healthy,
			"json_capable": jsonOK,
		})
	})

	// GET /pool — full instance table (ops/debug)
	mux.HandleFunc("/pool", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, pool.Snapshot())
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s (fanout=%d, min_responses=%d)", cfg.Addr, cfg.Fanout, cfg.MinResponses)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
