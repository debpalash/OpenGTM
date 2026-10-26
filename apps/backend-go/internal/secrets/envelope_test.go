package secrets

import (
	"context"
	"strings"
	"testing"
)

const wrongKeyMsg = "failed to decrypt a per-workspace secret: the master key does not match the key used to encrypt it (rotated/incorrect SECRETS_MASTER_KEY?)"

func localEnv(t *testing.T, cfg Config) *Envelope {
	t.Helper()
	e, err := NewEnvelope(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	e := localEnv(t, Config{MasterKey: "k", AppEnv: "dev"})
	got, err := e.Decrypt(context.Background(), "sk-legacy-plain")
	if err != nil || got != "sk-legacy-plain" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDecryptEmptyIsEmpty(t *testing.T) {
	e := localEnv(t, Config{MasterKey: "k", AppEnv: "dev"})
	got, err := e.Decrypt(context.Background(), "")
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEnvelopeRoundTripAndPythonVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		if v.Token == "" {
			continue
		}
		e := localEnv(t, Config{MasterKey: v.Master, SecretKey: v.SecretKey, AppEnv: "dev"})
		got, err := e.Decrypt(context.Background(), EncPrefix+v.Token)
		if err != nil || got != v.Plaintext {
			t.Errorf("%s: got %q, %v", v.Name, got, err)
		}
		enc, err := e.Encrypt(context.Background(), v.Plaintext)
		if err != nil || !strings.HasPrefix(enc, EncPrefix) {
			t.Fatalf("%s: encrypt %v", v.Name, err)
		}
		back, err := e.Decrypt(context.Background(), enc)
		if err != nil || back != v.Plaintext {
			t.Errorf("%s: roundtrip %q %v", v.Name, back, err)
		}
	}
}

func TestDecryptWrongKeyIsSingleError(t *testing.T) {
	vs := loadVectors(t)
	e := localEnv(t, Config{MasterKey: "some other key", AppEnv: "dev"})
	for _, tok := range []string{
		EncPrefix + vs[0].Token,
		EncPrefix + "not-a-token",
		EncPrefix + "gAAAAAéé",
		EncPrefix,
	} {
		_, err := e.Decrypt(context.Background(), tok)
		if err == nil || err.Error() != wrongKeyMsg {
			t.Errorf("token %.12q: err = %v", tok, err)
		}
		if err != nil && strings.Contains(err.Error(), vs[0].Token) {
			t.Error("error leaks stored value")
		}
	}
}

func TestUnsupportedProviderRejected(t *testing.T) {
	_, err := NewEnvelope(Config{Provider: " AWS_KMS ", MasterKey: "k"}, nil)
	if err == nil || err.Error() != "unsupported SECRETS_PROVIDER: aws_kms" {
		t.Fatalf("err = %v", err)
	}
}

func TestInsecureSecretKeyRefusedInProdEncrypt(t *testing.T) {
	e := localEnv(t, Config{SecretKey: InsecureSecretKey, AppEnv: "prod"})
	_, err := e.Encrypt(context.Background(), "x")
	want := "Refusing to encrypt per-workspace secrets: SECRETS_MASTER_KEY is unset and SECRET_KEY is the insecure default while APP_ENV='prod'. Set SECRETS_MASTER_KEY (a Fernet key) or a strong SECRET_KEY in the environment/.env."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	// plaintext passthrough still works; dev env is allowed.
	if got, err := e.Decrypt(context.Background(), "plain"); err != nil || got != "plain" {
		t.Fatalf("%q %v", got, err)
	}
	dev := localEnv(t, Config{SecretKey: InsecureSecretKey, AppEnv: "Dev"})
	if _, err := dev.Encrypt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}
