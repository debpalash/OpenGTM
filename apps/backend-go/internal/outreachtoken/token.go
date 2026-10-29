// Package outreachtoken mints and verifies unsubscribe HMAC tokens, byte
// compatible with legacy/python/apps/api/services/outreach/tokens.py in both
// directions (tokens already in sent emails must keep verifying).
package outreachtoken

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Payload is the signed token body.
type Payload struct {
	WS    string `json:"ws"`
	Email string `json:"email"`
	Seq   string `json:"seq"`
	Exp   int64  `json:"exp"`
}

// Signer mints and verifies tokens under a key derived from the master key.
type Signer struct {
	key     []byte
	base    string
	ttlDays int
	now     func() time.Time
}

// NewSigner derives the HMAC key sha256("outreach-unsub:" + masterKey). A nil
// now defaults to time.Now.
func NewSigner(masterKey []byte, publicBaseURL string, ttlDays int, now func() time.Time) *Signer {
	if now == nil {
		now = time.Now
	}
	sum := sha256.Sum256(append([]byte("outreach-unsub:"), masterKey...))
	return &Signer{key: sum[:], base: publicBaseURL, ttlDays: ttlDays, now: now}
}

func signingKeyHex(master []byte) string {
	sum := sha256.Sum256(append([]byte("outreach-unsub:"), master...))
	return hex.EncodeToString(sum[:])
}

var b64 = base64.RawURLEncoding

func (s *Signer) sign(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return b64.EncodeToString(m.Sum(nil))
}

// Make mints "body.sig". ttlDays nil uses the signer default.
func (s *Signer) Make(workspaceID, email, sequenceID string, ttlDays *int) (string, error) {
	ttl := s.ttlDays
	if ttlDays != nil {
		ttl = *ttlDays
	}
	exp := s.now().Unix() + int64(ttl)*86400
	if !utf8.ValidString(workspaceID) || !utf8.ValidString(email) || !utf8.ValidString(sequenceID) {
		return "", errors.New("outreachtoken: invalid UTF-8 in token field")
	}
	// Sorted keys: email, exp, seq, ws; compact; ensure_ascii.
	var buf bytes.Buffer
	buf.WriteString(`{"email":`)
	pyJSONString(&buf, pyLowerStrip(email))
	buf.WriteString(`,"exp":`)
	buf.WriteString(strconv.FormatInt(exp, 10))
	buf.WriteString(`,"seq":`)
	pyJSONString(&buf, sequenceID)
	buf.WriteString(`,"ws":`)
	pyJSONString(&buf, workspaceID)
	buf.WriteByte('}')
	body := b64.EncodeToString(buf.Bytes())
	return body + "." + s.sign(body), nil
}

// Verify returns the payload when the token is authentic (constant-time) and,
// unless allowExpired, not expired. It never panics on malformed input.
func (s *Signer) Verify(token string, allowExpired bool) (Payload, bool) {
	var zero Payload
	if token == "" {
		return zero, false
	}
	body, sig, found := strings.Cut(token, ".")
	if !found {
		return zero, false
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(body))) != 1 {
		return zero, false
	}
	raw, err := base64.URLEncoding.DecodeString(body + strings.Repeat("=", (4-len(body)%4)%4))
	if err != nil {
		return zero, false
	}
	if !utf8.Valid(raw) {
		return zero, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return zero, false
	}
	var p Payload
	for k, dst := range map[string]*string{"ws": &p.WS, "email": &p.Email, "seq": &p.Seq} {
		if v, ok := m[k]; ok && json.Unmarshal(v, dst) != nil && k != "seq" {
			return zero, false // ws/email must be strings; seq is informational
		}
	}
	exp, ok := pyInt(m["exp"])
	if !ok {
		return zero, false
	}
	p.Exp = exp
	if !allowExpired && p.Exp < s.now().Unix() {
		return zero, false
	}
	if p.WS == "" || p.Email == "" {
		return zero, false
	}
	return p, true
}

// pyInt mirrors int(payload.get("exp", 0)): absent is 0, numbers truncate,
// numeric strings parse; null, bool, other types and unparsable strings fail
// (Python raises, which the route turns into a 500; Go answers invalid).
func pyInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, true
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		switch {
		case f >= math.MaxInt64:
			return math.MaxInt64, true // Python big int: never expired
		case f <= math.MinInt64:
			return math.MinInt64, true
		}
		return int64(f), true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		n, err := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// pyJSONString writes a string like Python json.dumps(ensure_ascii=True).
func pyJSONString(b *bytes.Buffer, s string) {
	const hexd = "0123456789abcdef"
	u := func(r rune) {
		b.WriteString(`\u`)
		for i := 12; i >= 0; i -= 4 {
			b.WriteByte(hexd[(r>>uint(i))&0xf])
		}
	}
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			u(r)
		case r < 0x80:
			b.WriteRune(r)
		case r >= 0x10000:
			r -= 0x10000
			u(0xd800 + (r >> 10))
			u(0xdc00 + (r & 0x3ff))
		default:
			u(r)
		}
	}
	b.WriteByte('"')
}

// pyLowerStrip is (email or "").strip().lower() for the cases that differ from
// Go: Python's strip whitespace set, U+0130 expanding to "i̇", and the
// word-final sigma rule.
func pyLowerStrip(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
	rs := []rune(s)
	var out strings.Builder
	for i, r := range rs {
		switch {
		case r == 0x130:
			out.WriteString("i̇")
		case r == 'Σ':
			before := casedBefore(rs, i)
			after := casedAfter(rs, i)
			if before && !after {
				out.WriteRune('ς')
			} else {
				out.WriteRune('σ')
			}
		default:
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return out.String()
}

// isCased is Unicode Cased: Lu, Ll, Lt plus Other_Lowercase/Other_Uppercase.
func isCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

// isCaseIgnorable is Unicode Case_Ignorable: Mn, Me, Cf, Lm, Sk plus the
// Word_Break MidLetter/MidNumLet/Single_Quote characters.
func isCaseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '^', '`', 0xa8, 0xad, 0xaf, 0xb4, 0xb7, 0xb8, 0x2018, 0x2019, 0x2024, 0x2027,
		0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a, 0x387, 0x55f, 0x5f4:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

func casedBefore(rs []rune, i int) bool {
	for j := i - 1; j >= 0; j-- {
		if isCaseIgnorable(rs[j]) {
			continue
		}
		return isCased(rs[j])
	}
	return false
}

func casedAfter(rs []rune, i int) bool {
	for j := i + 1; j < len(rs); j++ {
		if isCaseIgnorable(rs[j]) {
			continue
		}
		return isCased(rs[j])
	}
	return false
}
