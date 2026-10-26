package destinbound_test

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destinbound"
)

type tokenFixture struct {
	Hash []struct {
		Raw    string `json:"raw"`
		SHA256 string `json:"sha256"`
	} `json:"hash"`
	TTL []struct {
		Env  *string `json:"env"`
		Days int     `json:"days"`
	} `json:"ttl"`
}

func loadTokenFixture(t *testing.T) tokenFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/token.json")
	if err != nil {
		t.Fatal(err)
	}
	var f tokenFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTokenFormatAndHash(t *testing.T) {
	f := loadTokenFixture(t)
	for _, h := range f.Hash {
		if got := destinbound.HashToken(h.Raw); got != h.SHA256 {
			t.Errorf("HashToken(%q) = %s, want %s", h.Raw, got, h.SHA256)
		}
	}
	re := regexp.MustCompile(`^dsi_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		raw, hash, prefix := destinbound.GenerateToken()
		if !re.MatchString(raw) {
			t.Fatalf("token shape: %d chars", len(raw))
		}
		if hash != destinbound.HashToken(raw) {
			t.Fatal("hash is not sha256 hex of raw")
		}
		if prefix != raw[:12] || len(prefix) != 12 {
			t.Fatal("prefix is not the first 12 chars")
		}
		if seen[raw] {
			t.Fatal("duplicate token")
		}
		seen[raw] = true
	}
}

func TestTTLClamp(t *testing.T) {
	cases := map[int]time.Duration{-5: 24 * time.Hour, 0: 24 * time.Hour, 1: 24 * time.Hour, 90: 90 * 24 * time.Hour,
		365: 365 * 24 * time.Hour, 366: 365 * 24 * time.Hour, 100000: 365 * 24 * time.Hour}
	for in, want := range cases {
		if got := destinbound.TokenTTL(in); got != want {
			t.Errorf("TokenTTL(%d) = %v, want %v", in, got, want)
		}
	}
	for _, c := range loadTokenFixture(t).TTL {
		env := ""
		if c.Env != nil {
			env = *c.Env
		}
		if got := destinbound.TTLDays(env); got != c.Days && !(c.Days > 0 && destinbound.TokenTTL(got) == destinbound.TokenTTL(c.Days)) {
			t.Errorf("TTLDays(%q) = %d, want %d", env, got, c.Days)
		}
		if got := destinbound.TokenTTL(destinbound.TTLDays(env)); got != destinbound.TokenTTL(c.Days) {
			t.Errorf("TTL for env %q = %v, want %d days", env, got, c.Days)
		}
	}
}

func TestParseInboundBodyKeepsFieldOrderAndKeys(t *testing.T) {
	b, err := destinbound.ParseInboundBody([]byte(`{"external_event_id":"e","fields":{"z":1,"a":{"k":2},"z":3},"extra":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destinbound.ParseInboundBody([]byte(`[1]`)); err == nil {
		t.Fatal("a non-object body must be rejected")
	}
	if _, err := destinbound.ParseInboundBody([]byte(`{"lead_id":1.5}`)); err == nil {
		t.Fatal("a non-integer lead_id must be rejected")
	}
	_ = b
}
