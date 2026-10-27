package govidp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OIDC authorization-code client with PKCE for the /auth/sso/* routes (S-M5). The issuer is the
// stored, allow-listed one; discovery and the ID token must name it exactly. Only asymmetric ID
// token signatures are accepted (no "none", no HS* with the client secret). Every call goes
// through the caller's HTTP client (the guarded egress client in production).

const (
	maxOIDCBody = 1 << 20
	oidcLeeway  = 60 * time.Second
)

// ErrOIDC is a provider or token failure. Its text never carries tokens or secrets; the route
// answers a generic message for it.
var ErrOIDC = errors.New("govidp: OIDC provider exchange failed")

func oidcErr(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrOIDC}, a...)...)
}

// Discovery is the subset of the OpenID provider metadata the code flow reads.
type Discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

func getJSON(ctx context.Context, hc *http.Client, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return oidcErr("request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return oidcErr("fetch")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcErr("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOIDCBody)).Decode(dst); err != nil {
		return oidcErr("decode")
	}
	return nil
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

// Discover reads {issuer}/.well-known/openid-configuration. The document's issuer must equal
// the configured issuer exactly, and every endpoint must be https.
func Discover(ctx context.Context, hc *http.Client, issuer string) (Discovery, error) {
	var d Discovery
	if err := getJSON(ctx, hc, issuer+"/.well-known/openid-configuration", &d); err != nil {
		return Discovery{}, err
	}
	if d.Issuer != issuer {
		return Discovery{}, oidcErr("discovery issuer mismatch")
	}
	for _, u := range []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI} {
		if !httpsURL(u) {
			return Discovery{}, oidcErr("discovery endpoint is not https")
		}
	}
	return d, nil
}

// Flow is the per-login secret state carried (signed) in the SSO cookie.
type Flow struct {
	State    string
	Nonce    string
	Verifier string
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewFlow draws a fresh state, nonce and PKCE verifier (32 random bytes each).
func NewFlow() (Flow, error) {
	var f Flow
	var err error
	for _, p := range []*string{&f.State, &f.Nonce, &f.Verifier} {
		if *p, err = randomToken(); err != nil {
			return Flow{}, err
		}
	}
	return f, nil
}

// AuthURL is the authorization request (scope openid email profile, PKCE S256).
func (d Discovery) AuthURL(clientID, redirectURI string, f Flow) string {
	sum := sha256.Sum256([]byte(f.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid email profile"},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode()
}

// IDClaims are the ID token claims the callback reads.
type IDClaims struct {
	Subject       string
	Email         string
	EmailVerified bool // true only for a JSON boolean true (Python: `is not True` refuses)
}

// Exchange redeems the code at the token endpoint (client_secret_basic unless the provider
// lists only client_secret_post) and verifies the returned ID token: signature against the
// provider JWKS, iss == issuer exactly, aud contains clientID (azp == clientID when present or
// when aud has several entries), exp/iat with 60 s leeway, and nonce == f.Nonce.
func (d Discovery) Exchange(ctx context.Context, hc *http.Client, clientID, clientSecret, redirectURI, code string, f Flow, now time.Time) (IDClaims, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {f.Verifier},
	}
	post := len(d.TokenAuthMethods) > 0 && !slices.Contains(d.TokenAuthMethods, "client_secret_basic") &&
		slices.Contains(d.TokenAuthMethods, "client_secret_post")
	if post {
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return IDClaims{}, oidcErr("token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !post { // RFC 6749 2.3.1: form-urlencode both parts before Basic
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return IDClaims{}, oidcErr("token fetch")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return IDClaims{}, oidcErr("token status %d", resp.StatusCode)
	}
	var tr struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOIDCBody)).Decode(&tr); err != nil || tr.IDToken == "" {
		return IDClaims{}, oidcErr("token response has no id_token")
	}
	var set jwkSet
	if err := getJSON(ctx, hc, d.JWKSURI, &set); err != nil {
		return IDClaims{}, err
	}
	return verifyIDToken(tr.IDToken, set, d.Issuer, clientID, f.Nonce, now)
}

var idTokenAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func b64Int(s string) (*big.Int, bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return new(big.Int).SetBytes(b), true
}

func (k jwk) publicKey() (any, bool) {
	if k.Use != "" && k.Use != "sig" {
		return nil, false
	}
	switch k.Kty {
	case "RSA":
		n, ok1 := b64Int(k.N)
		e, ok2 := b64Int(k.E)
		if !ok1 || !ok2 || !e.IsInt64() || e.Int64() < 3 || n.BitLen() < 2048 {
			return nil, false
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, true
	case "EC":
		var c elliptic.Curve
		switch k.Crv {
		case "P-256":
			c = elliptic.P256()
		case "P-384":
			c = elliptic.P384()
		case "P-521":
			c = elliptic.P521()
		default:
			return nil, false
		}
		x, ok1 := b64Int(k.X)
		y, ok2 := b64Int(k.Y)
		if !ok1 || !ok2 {
			return nil, false
		}
		pk := &ecdsa.PublicKey{Curve: c, X: x, Y: y}
		if _, err := pk.ECDH(); err != nil { // not on the curve
			return nil, false
		}
		return pk, true
	}
	return nil, false
}

func keyFor(set jwkSet, kid, alg string) (any, error) {
	var cands []jwk
	for _, k := range set.Keys {
		if (kid == "" || k.Kid == kid) && (k.Alg == "" || k.Alg == alg) {
			cands = append(cands, k)
		}
	}
	if len(cands) != 1 { // no kid and several keys: ambiguous, refuse
		return nil, oidcErr("no unique signing key")
	}
	pk, ok := cands[0].publicKey()
	if !ok {
		return nil, oidcErr("unusable signing key")
	}
	switch alg[:2] {
	case "RS", "PS":
		if _, isRSA := pk.(*rsa.PublicKey); !isRSA {
			return nil, oidcErr("key type mismatch")
		}
	case "ES":
		if _, isEC := pk.(*ecdsa.PublicKey); !isEC {
			return nil, oidcErr("key type mismatch")
		}
	}
	return pk, nil
}

func verifyIDToken(raw string, set jwkSet, issuer, clientID, nonce string, now time.Time) (IDClaims, error) {
	p := jwt.NewParser(jwt.WithValidMethods(idTokenAlgs), jwt.WithIssuer(issuer), jwt.WithAudience(clientID),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(oidcLeeway), jwt.WithTimeFunc(func() time.Time { return now }))
	mc := jwt.MapClaims{}
	_, err := p.ParseWithClaims(raw, mc, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return keyFor(set, kid, t.Method.Alg())
	})
	if err != nil {
		return IDClaims{}, oidcErr("id_token rejected")
	}
	// iss is compared exactly by WithIssuer; aud may be a string or a list.
	aud, _ := mc.GetAudience()
	azp, hasAzp := mc["azp"].(string)
	if (hasAzp || len(aud) > 1) && azp != clientID {
		return IDClaims{}, oidcErr("id_token azp mismatch")
	}
	got, _ := mc["nonce"].(string)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return IDClaims{}, oidcErr("id_token nonce mismatch")
	}
	c := IDClaims{}
	c.Subject, _ = mc["sub"].(string)
	c.Email, _ = mc["email"].(string)
	c.EmailVerified, _ = mc["email_verified"].(bool)
	return c, nil
}
