package emailverify

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestResolveMXNegativeCacheRecovery(t *testing.T) {
	for _, response := range []string{`"SERVFAIL"`, `[]`} {
		t.Run(response, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			w := newWorld(map[string]json.RawMessage{"example.com": json.RawMessage(response)}, nil)
			d := NewDeps()
			d.Resolver, d.Now = w, func() time.Time { return now }
			if got := ResolveMX(context.Background(), d, "example.com"); got != "" {
				t.Fatalf("failed lookup = %q", got)
			}
			w.mx["example.com"] = json.RawMessage(`[[10,"mx.example.com."]]`)
			now = now.Add(30 * time.Second)
			if got := ResolveMX(context.Background(), d, "example.com"); got != "" || len(w.log) != 1 {
				t.Fatalf("negative cache not reused: host %q lookups %d", got, len(w.log))
			}
			now = now.Add(30 * time.Second)
			if got := ResolveMX(context.Background(), d, "example.com"); got != "mx.example.com" {
				t.Fatalf("DNS recovered but cached failure persisted: %q", got)
			}
			now = now.Add(2 * time.Minute)
			w.mx["example.com"] = json.RawMessage(`"SERVFAIL"`)
			if got := ResolveMX(context.Background(), d, "example.com"); got != "mx.example.com" || len(w.log) != 2 {
				t.Fatalf("positive cache expired early: host %q lookups %d", got, len(w.log))
			}
		})
	}
}

func TestResolveMXCancelledCallerDoesNotCache(t *testing.T) {
	w := newWorld(map[string]json.RawMessage{"example.com": json.RawMessage(`"SERVFAIL"`)}, nil)
	d := NewDeps()
	d.Resolver = w
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ResolveMX(ctx, d, "example.com"); got != "" {
		t.Fatalf("cancelled lookup = %q", got)
	}
	w.mx["example.com"] = json.RawMessage(`[[10,"mx.example.com."]]`)
	if got := ResolveMX(context.Background(), d, "example.com"); got != "mx.example.com" {
		t.Fatalf("cancelled caller poisoned cache: %q", got)
	}
}
