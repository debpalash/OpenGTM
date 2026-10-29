package govidp

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Only asymmetric ID token signatures verify: "none" and HS256 (keyed with anything) are refused,
// and a key set with several keys and no kid is ambiguous.
func TestVerifyIDTokenRefusesSymmetricAndNone(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := jwk{Kty: "RSA", Kid: "k1", N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}
	set := jwkSet{Keys: []jwk{pub}}
	now := time.Now()
	claims := jwt.MapClaims{"iss": "https://idp.test/t1", "aud": "cid", "sub": "s", "email": "a@b.test", "email_verified": true,
		"nonce": "n1", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	sign := func(m jwt.SigningMethod, k any, kid string) string {
		tok := jwt.NewWithClaims(m, claims)
		if kid != "" {
			tok.Header["kid"] = kid
		}
		s, err := tok.SignedString(k)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	good := sign(jwt.SigningMethodRS256, key, "k1")
	if c, err := verifyIDToken(good, set, "https://idp.test/t1", "cid", "n1", now); err != nil || c.Subject != "s" || !c.EmailVerified {
		t.Fatalf("good token: %+v %v", c, err)
	}
	if _, err := verifyIDToken(good, set, "https://idp.test/t2", "cid", "n1", now); err == nil {
		t.Fatal("other issuer path accepted")
	}
	for name, raw := range map[string]string{
		"hs256": sign(jwt.SigningMethodHS256, []byte("client-secret"), "k1"),
		"none":  sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "k1"),
	} {
		if _, err := verifyIDToken(raw, set, "https://idp.test/t1", "cid", "n1", now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	two := jwkSet{Keys: []jwk{pub, pub}}
	two.Keys[1].Kid = "k2"
	if _, err := verifyIDToken(sign(jwt.SigningMethodRS256, key, ""), two, "https://idp.test/t1", "cid", "n1", now); err == nil {
		t.Fatal("ambiguous key accepted")
	}
}
