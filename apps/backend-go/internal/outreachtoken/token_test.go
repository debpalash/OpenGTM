package outreachtoken

import (
	"encoding/json"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type vectors struct {
	DerivedKeys []struct {
		Name, Master  string
		SigningKeyHex string `json:"signing_key_hex"`
	} `json:"derived_keys"`
	Mints []struct {
		Name, Master, Email, Token string
		Now                        float64
		WorkspaceID                string `json:"workspace_id"`
		SequenceID                 string `json:"sequence_id"`
		TTLDays                    int    `json:"ttl_days"`
	} `json:"mints"`
	Verifies []struct {
		Name, Master, Token string
		AllowExpired        bool           `json:"allow_expired"`
		Now                 float64        `json:"now"`
		Expected            map[string]any `json:"expected"`
	} `json:"verifies"`
	URLs []struct {
		BaseURL            string `json:"base_url"`
		Master, Email, URL string
		Now                float64
		WorkspaceID        string `json:"workspace_id"`
		SequenceID         string `json:"sequence_id"`
		TTLDays            int    `json:"ttl_days"`
	} `json:"urls"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func at(f float64) func() time.Time { return func() time.Time { return time.Unix(int64(f), 0) } }

func TestVerifiesPythonMintedTokens(t *testing.T) {
	v := load(t)
	if len(v.Verifies) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Verifies {
		s := NewSigner([]byte(c.Master), "", 90, at(c.Now))
		got, ok := s.Verify(c.Token, c.AllowExpired)
		want, valid := expectedPayload(c.Expected)
		if !valid {
			// nil: Python returns None; python_raises: Python 500s; ws of a
			// non-string type: Python accepts, Go rejects (documented, stricter).
			if ok {
				t.Errorf("%s: expected invalid, got %+v", c.Name, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s: got %+v ok=%v want %+v", c.Name, got, ok, want)
		}
	}
}

func expectedPayload(e map[string]any) (Payload, bool) {
	if e == nil || e["python_raises"] != nil {
		return Payload{}, false
	}
	var p Payload
	var ok bool
	if p.WS, ok = e["ws"].(string); !ok {
		return Payload{}, false
	}
	p.Email, _ = e["email"].(string)
	p.Seq, _ = e["seq"].(string)
	switch x := e["exp"].(type) {
	case float64:
		p.Exp = int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		p.Exp = n
	}
	return p, true
}

func TestPythonVerifiesGoTokens(t *testing.T) {
	v := load(t)
	for _, m := range v.Mints {
		s := NewSigner([]byte(m.Master), "", 90, at(m.Now))
		ttl := m.TTLDays
		got, err := s.Make(m.WorkspaceID, m.Email, m.SequenceID, &ttl)
		if err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if got != m.Token {
			t.Errorf("%s: token bytes differ from Python", m.Name)
		}
	}
	for _, u := range v.URLs {
		s := NewSigner([]byte(u.Master), u.BaseURL, u.TTLDays, at(u.Now))
		got, err := s.URL(u.WorkspaceID, u.Email, u.SequenceID)
		if err != nil || got != u.URL {
			t.Errorf("url %q: got %q err=%v want %q", u.BaseURL, got, err, u.URL)
		}
	}
}

func TestDefaultTTLUsed(t *testing.T) {
	s := NewSigner([]byte("k"), "", 7, at(1000))
	tok, _ := s.Make("w", "a@b.c", "", nil)
	p, ok := s.Verify(tok, false)
	if !ok || p.Exp != 1000+7*86400 {
		t.Fatalf("%+v %v", p, ok)
	}
}

func TestExpiredRejectedUnlessAllowed(t *testing.T) {
	now := int64(2_000_000_000)
	clock := func() time.Time { return time.Unix(now, 0) }
	s := NewSigner([]byte("k"), "", 1, clock)
	tok, _ := s.Make("w", "a@b.c", "s", nil)
	now += 86400 // exp == now: still valid (Python: exp < now)
	if _, ok := s.Verify(tok, false); !ok {
		t.Fatal("exp==now must verify")
	}
	now++
	if _, ok := s.Verify(tok, false); ok {
		t.Fatal("expired must be rejected")
	}
	if _, ok := s.Verify(tok, true); !ok {
		t.Fatal("allowExpired must accept")
	}
}

func TestForgedSignatureRejected(t *testing.T) {
	s := NewSigner([]byte("k"), "", 1, at(1000))
	other := NewSigner([]byte("other"), "", 1, at(1000))
	tok, _ := s.Make("w", "a@b.c", "s", nil)
	body, sig, _ := strings.Cut(tok, ".")
	forged := []string{
		body + "." + sig[:len(sig)-1] + "A",
		body + ".",
		body,
		"." + sig,
		body + "." + sig + ".x",
	}
	ot, _ := other.Make("w", "a@b.c", "s", nil)
	forged = append(forged, ot)
	evil, _ := other.Make("w", "evil@b.c", "s", nil)
	eb, _, _ := strings.Cut(evil, ".")
	forged = append(forged, eb+"."+sig)
	for _, f := range forged {
		if _, ok := s.Verify(f, true); ok {
			t.Errorf("forged token accepted: %q", f)
		}
	}
}

func TestNonASCIIEmailMatchesPythonEscaping(t *testing.T) {
	v := load(t)
	n := 0
	for _, m := range v.Mints {
		if strings.Contains(m.Name, "nonascii") || strings.Contains(m.Name, "cjk") ||
			strings.Contains(m.Name, "astral") || strings.Contains(m.Name, "quote") || strings.Contains(m.Name, "html") {
			n++
			s := NewSigner([]byte(m.Master), "", 90, at(m.Now))
			ttl := m.TTLDays
			got, _ := s.Make(m.WorkspaceID, m.Email, m.SequenceID, &ttl)
			if got != m.Token {
				t.Errorf("%s differs", m.Name)
			}
		}
	}
	if n < 5 {
		t.Fatalf("only %d escaping vectors", n)
	}
}

func TestMalformedTokensNeverPanic(t *testing.T) {
	s := NewSigner([]byte("k"), "", 1, at(1000))
	good, _ := s.Make("w", "a@b.c", "s", nil)
	rng := rand.New(rand.NewSource(1))
	seeds := []string{"", ".", "..", "a.b", "\x00.\xff", "e30.e30", strings.Repeat("A", 10000), good}
	for i := 0; i < 3000; i++ {
		b := []byte(good)
		for j := 0; j < 1+rng.Intn(4); j++ {
			b[rng.Intn(len(b))] = byte(rng.Intn(256))
		}
		seeds = append(seeds, string(b), string(b[:rng.Intn(len(b))]))
	}
	for _, tok := range seeds {
		s.Verify(tok, true)
		s.Verify(tok, false)
	}
}

func TestSigningKeyMatchesPython(t *testing.T) {
	v := load(t)
	for _, d := range v.DerivedKeys {
		if got := signingKeyHex([]byte(d.Master)); got != d.SigningKeyHex {
			t.Errorf("%s: signing key mismatch", d.Name)
		}
	}
}
