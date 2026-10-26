package secrets

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Store reads raw (possibly ciphertext) settings. AppSetting returns "" for a
// missing key; WorkspaceSetting's bool reports row existence.
type Store interface {
	AppSetting(ctx context.Context, key string) (string, error)
	WorkspaceSetting(ctx context.Context, ws, key string) (string, bool, error)
}

// WriteStore is the optional write side needed by Resolver.Set.
type WriteStore interface {
	Store
	SetWorkspaceSetting(ctx context.Context, ws, key, value string) error
}

// GlobalOnly keys never resolve from a workspace secret (verifier SSRF rule).
var GlobalOnly = map[string]bool{"REACHER_URL": true}

// Resolver resolves credentials: workspace > global (app_settings) > env.
type Resolver struct {
	env    *Envelope
	st     Store
	getenv func(string) string
}

func NewResolver(env *Envelope, st Store, getenv func(string) string) *Resolver {
	return &Resolver{env: env, st: st, getenv: getenv}
}

// Source values returned by Get.
const (
	SourceWorkspace = "workspace"
	SourceGlobal    = "global"
	SourceDefault   = "default"
)

// Get resolves key for workspace ws (empty ws skips the workspace level).
// Empty stored values count as unset. Both ciphertext and legacy plaintext
// globals resolve; a decrypt failure is an error, never the raw ciphertext.
func (r *Resolver) Get(ctx context.Context, ws, key string) (value, source string, err error) {
	if ws != "" && !GlobalOnly[key] {
		raw, found, err := r.st.WorkspaceSetting(ctx, ws, key)
		if err != nil {
			return "", "", err
		}
		if found && raw != "" {
			v, err := r.env.Decrypt(ctx, raw)
			if err != nil {
				return "", "", err
			}
			return v, SourceWorkspace, nil
		}
	}
	raw, err := r.st.AppSetting(ctx, key)
	if err != nil {
		return "", "", err
	}
	if raw != "" {
		v, err := r.env.Decrypt(ctx, raw)
		if err != nil {
			return "", "", err
		}
		if v != "" {
			return v, SourceGlobal, nil
		}
	}
	if v := r.getenv(key); v != "" {
		return v, SourceGlobal, nil
	}
	return "", SourceDefault, nil
}

// Set encrypts and stores a per-workspace secret. Blank values are ignored so
// an empty form field never clobbers a saved secret.
func (r *Resolver) Set(ctx context.Context, ws, key, value string) error {
	if ws == "" || key == "" {
		return errors.New("workspace_id and key are required")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	w, ok := r.st.(WriteStore)
	if !ok {
		return errors.New("secrets: store is read-only")
	}
	ct, err := r.env.Encrypt(ctx, value)
	if err != nil {
		return err
	}
	return w.SetWorkspaceSetting(ctx, ws, key, ct)
}

// Has reports whether a non-empty per-workspace value exists (ignores global).
func (r *Resolver) Has(ctx context.Context, ws, key string) (bool, error) {
	if ws == "" || key == "" {
		return false, nil
	}
	raw, found, err := r.st.WorkspaceSetting(ctx, ws, key)
	return found && raw != "", err
}

// HasTx is Has on a transaction already bound to ws (tenancy.InWorkspace), so a
// caller holding a tenant transaction does not take a second pool connection.
// It needs the PostgreSQL store; with another Store it falls back to Has.
func (r *Resolver) HasTx(ctx context.Context, tx pgx.Tx, ws, key string) (bool, error) {
	if ws == "" || key == "" {
		return false, nil
	}
	if _, ok := r.st.(*PgStore); !ok {
		return r.Has(ctx, ws, key)
	}
	v, err := gen.New(tx).GetWorkspaceSetting(ctx, gen.GetWorkspaceSettingParams{WorkspaceID: ws, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && v != "", err
}

// PgStore is the PostgreSQL Store: app_settings on the plain pool,
// workspace_settings inside tenancy.InWorkspace (FORCE RLS).
type PgStore struct{ pool *pgxpool.Pool }

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

func (s *PgStore) AppSetting(ctx context.Context, key string) (string, error) {
	v, err := gen.New(s.pool).GetAppSetting(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *PgStore) WorkspaceSetting(ctx context.Context, ws, key string) (string, bool, error) {
	var v string
	found := true
	err := tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error {
		var err error
		v, err = gen.New(tx).GetWorkspaceSetting(ctx, gen.GetWorkspaceSettingParams{WorkspaceID: ws, Key: key})
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	return v, found && err == nil, err
}

func (s *PgStore) SetWorkspaceSetting(ctx context.Context, ws, key, value string) error {
	return tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error {
		return gen.New(tx).UpsertWorkspaceSetting(ctx, gen.UpsertWorkspaceSettingParams{WorkspaceID: ws, Key: key, Value: value})
	})
}
