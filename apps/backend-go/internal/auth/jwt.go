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

// WorkspaceClaim names the workspace an SSO session is scoped to. Go-only: tokens without it
// (password logins, Python-minted tokens) are unscoped. python-jose ignores unknown claims, so
// Python still accepts scoped tokens but does not enforce the scope.
const WorkspaceClaim = "sso_ws"

// Claims are the verified claims of a token.
type Claims struct {
	Sub       string
	Type      string
	AMR       []string
	Workspace string // sso_ws; "" means unscoped
	ExpiresAt time.Time
}

// AllowsWorkspace reports whether the token may act in workspace ws: an unscoped token may act
// anywhere (membership is checked separately), a scoped one only in its own workspace.
func (c Claims) AllowsWorkspace(ws string) bool {
	return c.Workspace == "" || c.Workspace == ws
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

func (s Signer) mint(sub string, amr []string, ws, typ string, ttl time.Duration) (string, error) {
	if amr == nil {
		amr = []string{}
	}
	// exp as int64 so it serialises as a JSON integer, like python-jose.
	mc := jwt.MapClaims{
		"sub":  sub,
		"amr":  amr,
		"exp":  s.now().Add(ttl).Unix(),
		"type": typ,
	}
	if ws != "" {
		mc[WorkspaceClaim] = ws
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, mc).SignedString(s.Secret)
}

// Access mints an unscoped access token (type "access").
func (s Signer) Access(sub string, amr []string) (string, error) {
	return s.mint(sub, amr, "", "access", s.AccessTTL)
}

// Refresh mints an unscoped refresh token (type "refresh").
func (s Signer) Refresh(sub string, amr []string) (string, error) {
	return s.mint(sub, amr, "", "refresh", s.RefreshTTL)
}

// AccessFor mints an access token scoped to workspace ws (sso_ws); ws "" is Access.
func (s Signer) AccessFor(sub string, amr []string, ws string) (string, error) {
	return s.mint(sub, amr, ws, "access", s.AccessTTL)
}

// RefreshFor mints a refresh token scoped to workspace ws (sso_ws); ws "" is Refresh.
func (s Signer) RefreshFor(sub string, amr []string, ws string) (string, error) {
	return s.mint(sub, amr, ws, "refresh", s.RefreshTTL)
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
	if one, ok := mc["amr"].(string); ok {
		c.AMR = []string{one} // tenancy.py wraps a string amr into a list
	}
	if v, present := mc[WorkspaceClaim]; present {
		// A present scope must be a non-empty string; anything else fails closed rather than
		// reading as unscoped.
		ws, ok := v.(string)
		if !ok || ws == "" {
			return Claims{}, ErrInvalidToken
		}
		c.Workspace = ws
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
