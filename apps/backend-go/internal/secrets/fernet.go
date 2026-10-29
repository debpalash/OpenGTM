// Package secrets implements the at-rest secret envelope shared with the Python
// backend (legacy/python/apps/api/services/workspace/secrets.py).
//
// This file is a stdlib-only Fernet (AES-128-CBC + HMAC-SHA256) that is
// byte-compatible with Python's cryptography.fernet.Fernet, including its
// lenient urlsafe base64 decoding of keys and tokens.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

var (
	// ErrNotFernetKey means the value is not a Fernet key as Python's
	// Fernet(raw) constructor would judge it (the caller then derives a key).
	ErrNotFernetKey = errors.New("secrets: not a valid fernet key")
	// ErrInvalidToken is returned for every malformed, forged or tampered token.
	ErrInvalidToken = errors.New("secrets: invalid fernet token")
)

const (
	fernetVersion = 0x80
	// version(1) + timestamp(8) + iv(16) + one AES block(16) + hmac(32).
	minTokenLen = 1 + 8 + 16 + 16 + 32
)

// Fernet holds the split signing and encryption keys.
type Fernet struct{ signKey, encKey [16]byte }

// pyURLSafeB64Decode replicates Python base64.urlsafe_b64decode: '-' and '_'
// map to '+' and '/', characters outside the alphabet are silently discarded
// and padding must be valid once the input is exhausted. Like CPython's
// non-strict a2b_base64, a completed padding sequence ends decoding and
// anything after it is ignored; '=' with fewer than 2 data chars pending is
// skipped.
func pyURLSafeB64Decode(in []byte) ([]byte, bool) {
	out := make([]byte, 0, len(in)*3/4+3)
	var acc uint32
	quad, pads := 0, 0
	for _, c := range in {
		switch c {
		case '-':
			c = '+'
		case '_':
			c = '/'
		}
		if c == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					if quad == 2 {
						out = append(out, byte(acc>>4))
					} else {
						out = append(out, byte(acc>>10), byte(acc>>2))
					}
					return out, true
				}
			}
			continue
		}
		var v byte
		switch {
		case c >= 'A' && c <= 'Z':
			v = c - 'A'
		case c >= 'a' && c <= 'z':
			v = c - 'a' + 26
		case c >= '0' && c <= '9':
			v = c - '0' + 52
		case c == '+':
			v = 62
		case c == '/':
			v = 63
		default:
			continue
		}
		pads = 0
		acc = acc<<6 | uint32(v)
		quad++
		if quad == 4 {
			out = append(out, byte(acc>>16), byte(acc>>8), byte(acc))
			acc, quad = 0, 0
		}
	}
	if quad != 0 {
		return nil, false
	}
	return out, true
}

// ParseKey builds a Fernet from raw (already strip()ped) key text exactly as
// Python's Fernet(raw.encode()) would, or returns ErrNotFernetKey.
func ParseKey(raw string) (Fernet, error) {
	key, ok := pyURLSafeB64Decode([]byte(raw))
	if !ok || len(key) != 32 {
		return Fernet{}, ErrNotFernetKey
	}
	var f Fernet
	copy(f.signKey[:], key[:16])
	copy(f.encKey[:], key[16:])
	return f, nil
}

// DeriveKey is urlsafe_b64encode(sha256(material)) fed through ParseKey.
func DeriveKey(material string) Fernet {
	sum := sha256.Sum256([]byte(material))
	f, err := ParseKey(base64.URLEncoding.EncodeToString(sum[:]))
	if err != nil { // unreachable: 32 bytes always round-trip
		panic("secrets: derived key invalid")
	}
	return f
}

// FromMaster resolves key material like Python _load_master_key for a
// non-empty strip()ped SECRETS_MASTER_KEY: use it verbatim when it is a valid
// Fernet key, otherwise hash it.
func FromMaster(raw string) Fernet {
	if f, err := ParseKey(raw); err == nil {
		return f
	}
	return DeriveKey(raw)
}

func (f Fernet) mac(msg []byte) []byte {
	h := hmac.New(sha256.New, f.signKey[:])
	h.Write(msg)
	return h.Sum(nil)
}

// Encrypt returns a Fernet token (URL-safe base64 with padding).
func (f Fernet) Encrypt(plaintext []byte) (string, error) {
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+pad)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(pad)
	}

	msg := make([]byte, 0, 1+8+16+len(padded)+32)
	msg = append(msg, fernetVersion)
	msg = binary.BigEndian.AppendUint64(msg, uint64(time.Now().Unix()))
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	msg = append(msg, iv...)
	block, err := aes.NewCipher(f.encKey[:])
	if err != nil {
		return "", err
	}
	start := len(msg)
	msg = append(msg, padded...)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(msg[start:], msg[start:])
	msg = append(msg, f.mac(msg)...)
	return base64.URLEncoding.EncodeToString(msg), nil
}

// Decrypt verifies and decrypts a token. No TTL is enforced (Python passes
// none). Every malformed or forged input yields ErrInvalidToken.
func (f Fernet) Decrypt(token string) ([]byte, error) {
	data, ok := pyURLSafeB64Decode([]byte(token))
	if !ok || len(data) < minTokenLen || data[0] != fernetVersion {
		return nil, ErrInvalidToken
	}
	body, sig := data[:len(data)-32], data[len(data)-32:]
	if subtle.ConstantTimeCompare(f.mac(body), sig) != 1 {
		return nil, ErrInvalidToken
	}
	iv, ct := body[9:25], body[25:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, ErrInvalidToken
	}
	block, err := aes.NewCipher(f.encKey[:])
	if err != nil {
		return nil, ErrInvalidToken
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(pt) {
		return nil, ErrInvalidToken
	}
	var bad byte
	for _, b := range pt[len(pt)-pad:] {
		bad |= b ^ byte(pad)
	}
	if bad != 0 {
		return nil, ErrInvalidToken
	}
	return pt[:len(pt)-pad], nil
}
