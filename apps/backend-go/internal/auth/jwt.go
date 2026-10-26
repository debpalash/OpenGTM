// Package auth holds JWT and password primitives that are wire-compatible
// with the Python API (python-jose HS256 tokens, bcrypt $2b$ hashes).
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken is the only error callers see for any token failure, so
// nothing about the token (or secret) can leak through error strings.
var ErrInvalidToken = errors.New("invalid token")

// Claims are the verified claims of a token.
type Claims struct {
	Sub       string
	Type      string
	AMR       []string
	ExpiresAt time.Time
}

// Signer mints and parses HS256 tokens.
type Signer struct {
	Secret     []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Now        func() time.Time // nil means time.Now
}

func (s Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Signer) mint(sub string, amr []string, typ string, ttl time.Duration) (string, error) {
	if amr == nil {
		amr = []string{}
	}
	// exp as int64 so it serialises as a JSON integer, like python-jose.
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  sub,
		"amr":  amr,
		"exp":  s.now().Add(ttl).Unix(),
		"type": typ,
	})
	return t.SignedString(s.Secret)
}

// Access mints an access token (type "access").
func (s Signer) Access(sub string, amr []string) (string, error) {
	return s.mint(sub, amr, "access", s.AccessTTL)
}

// Refresh mints a refresh token (type "refresh").
func (s Signer) Refresh(sub string, amr []string) (string, error) {
	return s.mint(sub, amr, "refresh", s.RefreshTTL)
}

// ParseAny verifies signature, HS256-only alg and a present, unexpired exp.
// The caller must check Type (the /auth/refresh handler wants "refresh").
func (s Signer) ParseAny(tok string) (Claims, error) {
	p := jwt.NewParser(
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	mc := jwt.MapClaims{}
	parsed, err := p.ParseWithClaims(tok, mc, func(*jwt.Token) (any, error) { return s.Secret, nil })
	if err != nil || !parsed.Valid {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	c.Sub, _ = mc["sub"].(string)
	c.Type, _ = mc["type"].(string)
	if arr, ok := mc["amr"].([]any); ok {
		for _, a := range arr {
			if str, ok := a.(string); ok {
				c.AMR = append(c.AMR, str)
			}
		}
	}
	if exp, err := mc.GetExpirationTime(); err == nil && exp != nil {
		c.ExpiresAt = exp.Time
	}
	return c, nil
}

// ParseAccess mirrors get_access_token_claims: reject type=="refresh" or an
// empty sub. A token with no "type" claim is accepted (older code minted those).
func (s Signer) ParseAccess(tok string) (Claims, error) {
	c, err := s.ParseAny(tok)
	if err != nil {
		return Claims{}, err
	}
	if c.Type == "refresh" || c.Sub == "" {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}
