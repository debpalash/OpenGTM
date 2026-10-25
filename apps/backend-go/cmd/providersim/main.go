// Command providersim is a deterministic fake email-finder provider used by
// the M0 benchmarks. Behaviour is driven by SIM_PROFILE and SIM_SEED.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	cfg := Config{
		Profile: getenv("SIM_PROFILE", "ok"),
		Seed:    1,
		Hang:    30 * time.Second,
	}
	if v, err := strconv.ParseInt(os.Getenv("SIM_SEED"), 10, 64); err == nil {
		cfg.Seed = v
	}
	if v, err := strconv.Atoi(os.Getenv("SIM_NOMATCH_EVERY")); err == nil && v > 0 {
		cfg.NoMatchEvery = v
	}
	if d, err := time.ParseDuration(os.Getenv("SIM_HANG")); err == nil {
		cfg.Hang = d
	}
	addr := getenv("SIM_ADDR", ":8099")
	log.Printf("providersim profile=%s seed=%d listening on %s", cfg.Profile, cfg.Seed, addr)
	log.Fatal(http.ListenAndServe(addr, NewSim(cfg)))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
