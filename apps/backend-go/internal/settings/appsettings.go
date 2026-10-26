package settings

import (
	"context"
	"errors"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// AppWriter is the write side of the global settings store (secrets.PgStore).
type AppWriter interface {
	SetAppSetting(ctx context.Context, key, value string) error
}

// App is the global settings store (routers/settings.py _db_get/_db_set).
type App struct {
	Store  secrets.Store
	Env    *secrets.Envelope
	Secret func(key string) bool // membership in the registry's secret_app_keys
	Getenv func(string) string
}

// SecretSet builds a Secret predicate from the registry's secret_app_keys.
func (r *Registry) SecretSet() func(string) bool {
	m := make(map[string]bool, len(r.SecretAppKeys))
	for _, k := range r.SecretAppKeys {
		m[k] = true
	}
	return func(k string) bool { return m[k] }
}

// Get returns the stored value (decrypted), else the environment value, else
// def. Read errors fall through to the environment like _db_get; a decrypt
// failure is an error and never yields ciphertext.
func (a *App) Get(ctx context.Context, key, def string) (string, error) {
	raw, err := a.Store.AppSetting(ctx, key)
	if err == nil && raw != "" {
		return a.Env.Decrypt(ctx, raw)
	}
	if v := a.Getenv(key); v != "" {
		return v, nil
	}
	return def, nil
}

// Set writes value; secret keys are stored as envelope ciphertext (fail
// closed: an encryption error never falls back to plaintext). An empty value
// is stored as-is, as in _db_set.
func (a *App) Set(ctx context.Context, key, value string) error {
	w, ok := a.Store.(AppWriter)
	if !ok {
		return errors.New("settings: store is read-only")
	}
	stored := value
	if value != "" && a.Secret != nil && a.Secret(key) {
		ct, err := a.Env.Encrypt(ctx, value)
		if err != nil {
			return err
		}
		stored = ct
	}
	return w.SetAppSetting(ctx, key, stored)
}
