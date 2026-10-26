package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type vector struct {
	Name      string `json:"name"`
	Master    string `json:"master_key_raw"`
	SecretKey string `json:"secret_key"`
	Effective string `json:"effective_key_b64"`
	Plaintext string `json:"plaintext"`
	Token     string `json:"token"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/secrets/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 10 {
		t.Fatalf("too few vectors: %d", len(vs))
	}
	return vs
}

// keyFor mirrors Python _load_master_key for the test environment.
func keyFor(v vector) Fernet {
	if m := strings.TrimSpace(v.Master); m != "" {
		return FromMaster(m)
	}
	return DeriveKey(v.SecretKey)
}

func TestDecryptsPythonTokens(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			if v.Token == "" {
				if strings.TrimSpace(v.Plaintext) != "" {
					t.Fatal("vector without token must have blank plaintext")
				}
				return
			}
			got, err := keyFor(v).Decrypt(v.Token)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if string(got) != v.Plaintext {
				t.Fatal("plaintext mismatch")
			}
		})
	}
}

func TestDeriveKeyMatchesPython(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			want, err := ParseKey(v.Effective)
			if err != nil {
				t.Fatalf("effective key unparsable: %v", err)
			}
			if keyFor(v) != want {
				t.Fatal("Go effective key differs from Python's")
			}
		})
	}
}

func TestParseKeyQuirks(t *testing.T) {
	valid := base64.URLEncoding.EncodeToString(seq(32))
	// Expectations recorded from Python cryptography.fernet.Fernet(raw.encode()).
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"padded valid", valid, true},
		{"unpadded", strings.TrimRight(valid, "="), false},
		{"internal newline", valid[:20] + "\n" + valid[20:], true},
		{"trailing garbage after padding", valid + "zzzz", true},
		{"extra trailing pad", valid + "=", true},
		{"leading pad", "=" + valid, true},
		{"pad mid string", valid[:10] + "=" + valid[10:], true},
		{"dots discarded", valid[:10] + "..." + valid[10:], true},
		{"non-ascii discarded", valid[:10] + "é" + valid[10:], true},
		{"spaces discarded", valid[:5] + "  " + valid[5:], true},
		{"missing pad", valid[:len(valid)-1], false},
		{"junk after unpadded", strings.TrimRight(valid, "=") + "!", false},
		{"data after pad", valid[:len(valid)-1] + "=AAAA", true},
		{"standard alphabet", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 16)), true},
		{"31 bytes", base64.URLEncoding.EncodeToString(seq(31)), false},
		{"33 bytes", base64.URLEncoding.EncodeToString(seq(33)), false},
		{"non-base64", "!!!not base64!!!", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseKey(c.raw)
			if c.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !c.ok && !errors.Is(err, ErrNotFernetKey) {
				t.Fatalf("want ErrNotFernetKey, got %v", err)
			}
		})
	}
}

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func testKey() Fernet { return FromMaster(base64.URLEncoding.EncodeToString(seq(32))) }

func TestEncryptDecryptRoundTrip(t *testing.T) {
	f := testKey()
	for _, pt := range []string{"", "a", strings.Repeat("x", 15), strings.Repeat("y", 16), strings.Repeat("z", 17), "p4ss-wörd-🔑"} {
		tok, err := f.Encrypt([]byte(pt))
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.Decrypt(tok)
		if err != nil || string(got) != pt {
			t.Fatalf("round trip failed for len %d: %v", len(pt), err)
		}
	}
}

// forge builds a token with a valid MAC over arbitrary body parts.
func forge(f Fernet, version byte, iv, ct []byte) string {
	msg := append([]byte{version}, make([]byte, 8)...)
	msg = append(msg, iv...)
	msg = append(msg, ct...)
	msg = append(msg, f.mac(msg)...)
	return base64.URLEncoding.EncodeToString(msg)
}

func TestPythonDecryptsGoTokens(t *testing.T) {
	vs := loadVectors(t)
	type verifyCase struct {
		Master    string `json:"master_key_raw"`
		SecretKey string `json:"secret_key"`
		Plaintext string `json:"plaintext"`
		Token     string `json:"token"`
	}
	var cases []verifyCase
	for _, v := range vs {
		if v.Token == "" {
			continue
		}
		f := keyFor(v)
		tok, err := f.Encrypt([]byte(v.Plaintext))
		if err != nil {
			t.Fatal(err)
		}
		// Structure: padded url-safe base64, version byte, length, HMAC.
		if len(tok)%4 != 0 {
			t.Fatalf("%s: token not padded base64", v.Name)
		}
		raw, err := base64.URLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatal(err)
		}
		if raw[0] != 0x80 {
			t.Fatalf("%s: bad version", v.Name)
		}
		if (len(raw)-1-8-16-32)%16 != 0 {
			t.Fatalf("%s: ciphertext not block multiple", v.Name)
		}
		if !bytes.Equal(f.mac(raw[:len(raw)-32]), raw[len(raw)-32:]) {
			t.Fatalf("%s: HMAC recomputation mismatch", v.Name)
		}
		cases = append(cases, verifyCase{v.Master, v.SecretKey, v.Plaintext, tok})
	}
	if os.Getenv("PYTHON_VERIFY") != "1" {
		t.Skip("PYTHON_VERIFY!=1: structural checks passed; Python decrypt not run")
	}
	in, _ := json.Marshal(cases)
	cmd := exec.Command("uv", "run", "python", "scripts/gen_secret_vectors.py", "--verify")
	cmd.Dir = "../../../../legacy/python"
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.CombinedOutput()
	t.Logf("python --verify output: %s", out)
	if err != nil {
		t.Fatalf("python could not decrypt Go tokens: %v", err)
	}
}

func TestRejectsWrongVersion(t *testing.T) {
	f := testKey()
	tok := forge(f, 0x81, make([]byte, 16), make([]byte, 16))
	if _, err := f.Decrypt(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestRejectsTruncatedToken(t *testing.T) {
	f := testKey()
	tok, _ := f.Encrypt([]byte("hello"))
	raw, _ := base64.URLEncoding.DecodeString(tok)
	for _, n := range []int{0, 1, 9, 25, 56, 72} {
		short := base64.URLEncoding.EncodeToString(raw[:n])
		if _, err := f.Decrypt(short); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("len %d: got %v", n, err)
		}
	}
	// Truncating the tail breaks the MAC too.
	if _, err := f.Decrypt(base64.URLEncoding.EncodeToString(raw[:len(raw)-1])); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("tail-truncated token accepted")
	}
	if _, err := f.Decrypt("not base64 at all !"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("garbage accepted")
	}
}

func TestRejectsBadMac(t *testing.T) {
	f := testKey()
	tok, _ := f.Encrypt([]byte("hello"))
	raw, _ := base64.URLEncoding.DecodeString(tok)
	for _, idx := range []int{1, 10, 30, len(raw) - 1} {
		c := append([]byte(nil), raw...)
		c[idx] ^= 0x01
		if _, err := f.Decrypt(base64.URLEncoding.EncodeToString(c)); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("flip at %d: got %v", idx, err)
		}
	}
	other := DeriveKey("another key")
	if _, err := other.Decrypt(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong key: got %v", err)
	}
}

func encryptBlocks(f Fernet, iv, pt []byte) []byte {
	block, _ := aes.NewCipher(f.encKey[:])
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)
	return ct
}

func TestRejectsBadPadding(t *testing.T) {
	f := testKey()
	iv := make([]byte, 16)
	last := func(b ...byte) []byte {
		blk := bytes.Repeat([]byte{'a'}, 16)
		copy(blk[16-len(b):], b)
		return blk
	}
	cases := map[string][]byte{
		"pad zero":         last(0),
		"pad over block":   last(17),
		"pad inconsistent": last(1, 3, 3),
		"pad 4 mismatch":   last(9, 4, 4, 4),
	}
	for name, pt := range cases {
		tok := forge(f, 0x80, iv, encryptBlocks(f, iv, pt))
		if _, err := f.Decrypt(tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
	// Sanity: a correct pad of the same shape is accepted.
	good := last(4, 4, 4, 4)
	got, err := f.Decrypt(forge(f, 0x80, iv, encryptBlocks(f, iv, good)))
	if err != nil || len(got) != 12 {
		t.Fatalf("valid padding rejected: %v", err)
	}
}

func TestRejectsCiphertextNotBlockMultiple(t *testing.T) {
	f := testKey()
	iv := make([]byte, 16)
	for _, n := range []int{0, 1, 15, 17, 31} {
		if _, err := f.Decrypt(forge(f, 0x80, iv, make([]byte, n))); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("ct len %d: got %v", n, err)
		}
	}
}

func FuzzDecryptNeverPanics(f *testing.F) {
	k := testKey()
	tok, _ := k.Encrypt([]byte("seed"))
	raw, _ := base64.URLEncoding.DecodeString(tok)
	f.Add(tok)
	f.Add("")
	f.Add("gAAAAA")
	f.Add(base64.URLEncoding.EncodeToString(raw[:60]))
	f.Add(forge(k, 0x80, make([]byte, 16), make([]byte, 16)))
	f.Add(strings.Repeat("A", 200))
	f.Fuzz(func(t *testing.T, s string) {
		pt, err := k.Decrypt(s)
		if err != nil && !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("unexpected error type: %v", err)
		}
		if err != nil && pt != nil {
			t.Fatal("plaintext returned with error")
		}
	})
}
