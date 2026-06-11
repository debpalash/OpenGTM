package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is env-driven so it composes with docker-compose / the Yupcha .env.
type Config struct {
	Addr string // listen address, e.g. ":8889"

	// Seed pool. SeedInstances are always-included, high-trust members (e.g. our
	// own self-hosted SearXNG). PublicFeed pulls the searx.space registry to
	// fill the pool with community instances (each is its own IP, so Google/DDG
	// work across the pool even though they CAPTCHA our single server IP).
	SeedInstances []string
	Proxies       []string // optional outbound proxies, round-robined across instances
	UsePublicFeed bool
	FeedURL       string
	MinGrade      string // minimum searx.space HTTP grade to admit (A+, A, A-, B)
	MaxInstances  int

	DefaultEngines string // passed through to SearXNG (empty = instance defaults)

	HealthInterval time.Duration
	HealthTimeout  time.Duration
	SearchTimeout  time.Duration // overall budget for one /search
	PerTryTimeout  time.Duration // per-instance request timeout
	Fanout         int           // how many healthy instances to query per search
	MinResponses   int           // return once this many instances answered
	PerInstanceGap time.Duration // min spacing between hits to the same instance (politeness)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func LoadConfig() Config {
	return Config{
		Addr:           env("SEARXPOOL_ADDR", ":8889"),
		SeedInstances:  splitCSV(env("SEARXPOOL_SEED", "http://localhost:8888")),
		Proxies:        splitCSV(os.Getenv("SEARXPOOL_PROXIES")),
		UsePublicFeed:  envBool("SEARXPOOL_PUBLIC_FEED", true),
		FeedURL:        env("SEARXPOOL_FEED_URL", "https://searx.space/data/instances.json"),
		MinGrade:       env("SEARXPOOL_MIN_GRADE", "B"),
		MaxInstances:   envInt("SEARXPOOL_MAX_INSTANCES", 40),
		DefaultEngines: os.Getenv("SEARXPOOL_DEFAULT_ENGINES"),
		HealthInterval: time.Duration(envInt("SEARXPOOL_HEALTH_INTERVAL_S", 120)) * time.Second,
		HealthTimeout:  time.Duration(envInt("SEARXPOOL_HEALTH_TIMEOUT_S", 8)) * time.Second,
		SearchTimeout:  time.Duration(envInt("SEARXPOOL_SEARCH_TIMEOUT_S", 12)) * time.Second,
		PerTryTimeout:  time.Duration(envInt("SEARXPOOL_PER_TRY_TIMEOUT_S", 9)) * time.Second,
		Fanout:         envInt("SEARXPOOL_FANOUT", 4),
		MinResponses:   envInt("SEARXPOOL_MIN_RESPONSES", 2),
		PerInstanceGap: time.Duration(envInt("SEARXPOOL_PER_INSTANCE_GAP_MS", 1500)) * time.Millisecond,
	}
}
