// Package secrets encrypts values at rest in the envelope the Python app uses
// for per-workspace integration secrets
// (apps/api/services/workspace/secrets.py), so either stack can decrypt what
// the other wrote when both share SECRETS_MASTER_KEY (or SECRET_KEY).
//
// The envelope is "enc:v1:" followed by a Fernet token (AES-128-CBC with
// PKCS#7 padding, HMAC-SHA256, URL-safe base64): the format of the Python
// `cryptography` library's Fernet, implemented here with the standard library.
// The Vault Transit provider ("enc:v2:vault:") is not supported; the host
// refuses it rather than guess.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

// ErrInvalidToken is returned for any Fernet token that fails to verify or
// decrypt. It deliberately says nothing more.
var ErrInvalidToken = errors.New("secrets: invalid token (wrong key or corrupted value)")

// Key is a Fernet key: 16 bytes for the HMAC, 16 for AES-128.
type Key struct {
	signing    [16]byte
	encryption [16]byte
}

// pyURLSafeB64Decode mirrors Python's base64.urlsafe_b64decode, which
// translates "-_" to "+/", silently drops characters outside the alphabet and
// then requires valid padding. Fernet(key) uses it, so a key Python accepts
// must be accepted here with the same bytes.
func pyURLSafeB64Decode(s string) ([]byte, error) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '-':
			b.WriteByte('+')
		case r == '_':
			b.WriteByte('/')
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
			b.WriteRune(r)
		}
	}
	return base64.StdEncoding.DecodeString(b.String())
}

// ParseKey parses a Fernet key (URL-safe base64 of 32 bytes), like
// cryptography.fernet.Fernet(key).
func ParseKey(s string) (Key, error) {
	raw, err := pyURLSafeB64Decode(s)
	if err != nil || len(raw) != 32 {
		return Key{}, errors.New("secrets: a Fernet key must be 32 url-safe base64-encoded bytes")
	}
	var k Key
	copy(k.signing[:], raw[:16])
	copy(k.encryption[:], raw[16:])
	return k, nil
}

// DeriveKey is the Python `_derive_fernet_key`: SHA-256 of the material,
// used as the 32 key bytes.
func DeriveKey(material string) Key {
	sum := sha256.Sum256([]byte(material))
	var k Key
	copy(k.signing[:], sum[:16])
	copy(k.encryption[:], sum[16:])
	return k
}

const (
	fernetVersion = 0x80
	headerLen     = 1 + 8 + aes.BlockSize
	macLen        = sha256.Size
)

func (k Key) mac(data []byte) []byte {
	m := hmac.New(sha256.New, k.signing[:])
	m.Write(data)
	return m.Sum(nil)
}

// Encrypt returns a Fernet token for plaintext, stamped with now.
func (k Key) Encrypt(plaintext []byte, now time.Time) (string, error) {
	block, err := aes.NewCipher(k.encryption[:])
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte(nil), plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, headerLen, headerLen+len(padded)+macLen)
	out[0] = fernetVersion
	binary.BigEndian.PutUint64(out[1:9], uint64(now.Unix()))
	iv := out[9:headerLen]
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	out = append(out, ct...)
	out = append(out, k.mac(out)...)
	return base64.URLEncoding.EncodeToString(out), nil
}

// Decrypt verifies and decrypts a Fernet token. Like Python's decrypt without
// a ttl, the timestamp is not checked.
func (k Key) Decrypt(token string) ([]byte, error) {
	raw, err := pyURLSafeB64Decode(token)
	if err != nil || len(raw) < headerLen+macLen+aes.BlockSize || raw[0] != fernetVersion {
		return nil, ErrInvalidToken
	}
	body, tag := raw[:len(raw)-macLen], raw[len(raw)-macLen:]
	if subtle.ConstantTimeCompare(tag, k.mac(body)) != 1 {
		return nil, ErrInvalidToken
	}
	ct := body[headerLen:]
	if len(ct)%aes.BlockSize != 0 {
		return nil, ErrInvalidToken
	}
	block, err := aes.NewCipher(k.encryption[:])
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, body[9:headerLen]).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(pt) {
		return nil, ErrInvalidToken
	}
	for _, b := range pt[len(pt)-pad:] {
		if int(b) != pad {
			return nil, ErrInvalidToken
		}
	}
	return pt[:len(pt)-pad], nil
}
