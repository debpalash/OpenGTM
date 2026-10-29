package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type tokVec struct {
	Token         string `json:"token"`
	PythonDecodes bool   `json:"python_decodes"`
}
type pwVec struct {
	Password          string  `json:"password"`
	Hash              *string `json:"hash"`
	PythonHashError   *string `json:"python_hash_error"`
	PythonVerify      *bool   `json:"python_verify"`
	PythonVerifyWrong *bool   `json:"python_verify_wrong"`
}
type vectors struct {
	Secret    string            `json:"secret"`
	FarExp    int64             `json:"far_exp"`
	Tokens    map[string]tokVec `json:"tokens"`
	Passwords map[string]pwVec  `json:"passwords"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../testdata/auth/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func signer(v vectors) Signer {
	return Signer{Secret: []byte(v.Secret), AccessTTL: 30 * time.Minute, RefreshTTL: 14 * 24 * time.Hour,
		Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
}

func TestParsesPythonMintedAccessToken(t *testing.T) {
	v := load(t)
	c, err := signer(v).ParseAccess(v.Tokens["access"].Token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sub != "alice" || c.Type != "access" || len(c.AMR) != 1 || c.AMR[0] != "pwd" || c.ExpiresAt.Unix() < 4_000_000_000 {
		t.Fatalf("claims %+v", c)
	}
}

func TestParsesPythonMintedRefreshForRefreshOnly(t *testing.T) {
	v := load(t)
	s := signer(v)
	c, err := s.ParseAny(v.Tokens["refresh"].Token)
	if err != nil || c.Type != "refresh" || c.Sub != "alice" || len(c.AMR) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := s.ParseAccess(v.Tokens["refresh"].Token); err == nil {
		t.Fatal("refresh accepted as access")
	}
}

func TestParseAccessRejectsRefreshType(t *testing.T) {
	v := load(t)
	s := signer(v)
	tok, err := s.Refresh("alice", []string{"pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParseAccess(tok); err != ErrInvalidToken {
		t.Fatalf("got %v", err)
	}
}

func TestParseAccessRejectsMissingSub(t *testing.T) {
	v := load(t)
	if _, err := signer(v).ParseAccess(v.Tokens["no_sub"].Token); err != ErrInvalidToken {
		t.Fatalf("got %v", err)
	}
}

func TestParseAccessAcceptsMissingType(t *testing.T) {
	v := load(t)
	c, err := signer(v).ParseAccess(v.Tokens["no_type"].Token)
	if err != nil || c.Sub != "alice" || c.Type != "" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRejectsAlgNoneAndHS512(t *testing.T) {
	v := load(t)
	s := signer(v)
	for _, k := range []string{"alg_none", "hs512"} {
		if _, err := s.ParseAny(v.Tokens[k].Token); err != ErrInvalidToken {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestRejectsExpired(t *testing.T) {
	v := load(t)
	if _, err := signer(v).ParseAny(v.Tokens["expired"].Token); err != ErrInvalidToken {
		t.Fatal(err)
	}
}

func TestRejectsMissingExp(t *testing.T) {
	v := load(t)
	if _, err := signer(v).ParseAny(v.Tokens["no_exp"].Token); err != ErrInvalidToken {
		t.Fatal(err)
	}
}

func TestRejectsTamperedSignature(t *testing.T) {
	v := load(t)
	s := signer(v)
	for _, k := range []string{"tampered", "wrong_secret"} {
		if _, err := s.ParseAny(v.Tokens[k].Token); err != ErrInvalidToken {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if _, err := s.ParseAny("not.a.jwt"); err != ErrInvalidToken {
		t.Fatal(err)
	}
}

func b64d(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMintedTokenParsesInPythonVector(t *testing.T) {
	v := load(t)
	s := signer(v)
	for _, mint := range []func(string, []string) (string, error){s.Access, s.Refresh} {
		tok, err := mint("bob", []string{"pwd", "sso"})
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			t.Fatal(tok)
		}
		mac := hmac.New(sha256.New, []byte(v.Secret))
		mac.Write([]byte(parts[0] + "." + parts[1]))
		if !hmac.Equal(mac.Sum(nil), b64d(t, parts[2])) {
			t.Fatal("bad HS256 signature")
		}
		var hdr map[string]any
		_ = json.Unmarshal(b64d(t, parts[0]), &hdr)
		if hdr["alg"] != "HS256" {
			t.Fatalf("hdr %v", hdr)
		}
		raw := string(b64d(t, parts[1]))
		var p struct {
			Sub  string   `json:"sub"`
			Type string   `json:"type"`
			AMR  []string `json:"amr"`
			Exp  json.Number
		}
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		var m map[string]any
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		exp, ok := m["exp"].(json.Number)
		if !ok || strings.ContainsAny(exp.String(), ".eE") {
			t.Fatalf("exp not integer: %v", m["exp"])
		}
		_ = p
		if m["sub"] != "bob" {
			t.Fatal(m)
		}
	}
	a, _ := s.Access("bob", []string{"pwd"})
	c, _ := s.ParseAny(a)
	if c.ExpiresAt.Unix() != 1_800_000_000+1800 || c.Type != "access" {
		t.Fatalf("%+v", c)
	}
	r, _ := s.Refresh("bob", nil)
	c, _ = s.ParseAny(r)
	if c.ExpiresAt.Unix() != 1_800_000_000+14*86400 || c.Type != "refresh" {
		t.Fatalf("%+v", c)
	}
}

func TestVerifiesPythonBcryptHash(t *testing.T) {
	v := load(t)
	p := v.Passwords["ascii"]
	if !strings.HasPrefix(*p.Hash, "$2b$12$") {
		t.Fatal(*p.Hash)
	}
	if !VerifyPassword(p.Password, *p.Hash) {
		t.Fatal("python hash rejected")
	}
	if VerifyPassword(p.Password+"!", *p.Hash) {
		t.Fatal("wrong password accepted")
	}
}

func TestVerifyPasswordLongAndNonASCII(t *testing.T) {
	v := load(t)
	na := v.Passwords["non_ascii"]
	if !VerifyPassword(na.Password, *na.Hash) {
		t.Fatal("non-ascii rejected")
	}
	l72 := v.Passwords["len72"]
	if !VerifyPassword(l72.Password, *l72.Hash) {
		t.Fatal("72-byte rejected")
	}
	// Installed Python bcrypt (5.x) raises on >72 bytes; Go must answer false, never panic or error up.
	if l72.PythonVerifyWrong != nil {
		t.Fatal("vector assumption changed")
	}
	if VerifyPassword(v.Passwords["len73"].Password, *l72.Hash) {
		t.Fatal(">72 bytes must be false")
	}
	if VerifyPassword(strings.Repeat("x", 100), *l72.Hash) {
		t.Fatal(">72 bytes must be false")
	}
	if _, err := HashPassword(v.Passwords["len73"].Password); err == nil {
		t.Fatal("hash of >72 bytes should error like Python")
	}
}

func TestVerifyPasswordEmptyIsFalse(t *testing.T) {
	v := load(t)
	if VerifyPassword("", *v.Passwords["empty"].Hash) {
		t.Fatal("empty password accepted")
	}
	if VerifyPassword("x", "") || VerifyPassword("x", "garbage") {
		t.Fatal("garbage hash accepted")
	}
}

func TestHashPasswordCost12(t *testing.T) {
	h, err := HashPassword("pässwörd")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := bcrypt.Cost([]byte(h)); c != 12 {
		t.Fatalf("cost %d", c)
	}
	if !strings.HasPrefix(h, "$2") || !VerifyPassword("pässwörd", h) {
		t.Fatal(h)
	}
	DummyVerify()
}
