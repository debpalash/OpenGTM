package main

import (
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config selects the simulator behaviour.
type Config struct {
	Profile string        // ok | slow | ratelimit | timeout | malformed
	Seed    int64         // seeds latency draws
	Hang    time.Duration // hang duration for the timeout profile
}

// Sim is an http.Handler implementing the fake provider.
type Sim struct {
	cfg      Config
	mu       sync.Mutex
	rng      *rand.Rand
	calls    int
	byStatus map[string]int
}

// NewSim builds a simulator.
func NewSim(cfg Config) *Sim {
	if cfg.Hang == 0 {
		cfg.Hang = 30 * time.Second
	}
	return &Sim{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)), byStatus: map[string]int{}}
}

type plan struct {
	n         int
	delay     time.Duration
	status    int
	malformed bool
	hang      bool
}

// plan decides the behaviour of the next call. Call index n is 1-based; one
// normal draw is always consumed so the sequence depends only on seed + index.
func (s *Sim) plan() plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	n := s.calls
	z := s.rng.NormFloat64()
	p := plan{n: n, status: http.StatusOK}
	switch s.cfg.Profile {
	case "slow":
		// log-normal with median 300ms
		p.delay = time.Duration(300 * math.Exp(0.5*z) * float64(time.Millisecond))
	case "ratelimit":
		if n%10 == 0 {
			p.status = http.StatusTooManyRequests
		}
	case "timeout":
		p.hang = n%20 == 0
	case "malformed":
		p.malformed = n%15 == 0
	}
	return p
}

func (s *Sim) record(status int) {
	s.mu.Lock()
	s.byStatus[strconv.Itoa(status)]++
	s.mu.Unlock()
}

func (s *Sim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/stats":
		s.mu.Lock()
		out := map[string]any{"calls": s.calls, "by_status": s.byStatus}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == "/email-finder":
		s.emailFinder(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Sim) emailFinder(w http.ResponseWriter, r *http.Request) {
	p := s.plan()
	var in struct {
		First  string `json:"first_name"`
		Last   string `json:"last_name"`
		Domain string `json:"domain"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-r.Context().Done():
			return
		}
	}
	if p.hang {
		select {
		case <-time.After(s.cfg.Hang):
		case <-r.Context().Done():
			s.record(499)
			return
		}
	}
	if p.malformed {
		s.record(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"email": `))
		return
	}
	if p.status != http.StatusOK {
		s.record(p.status)
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"error":"rate limited"}`, p.status)
		return
	}
	email := strings.ToLower(in.First+"."+in.Last) + "@" + strings.ToLower(in.Domain)
	s.record(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"email": email})
}
