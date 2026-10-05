package pluginrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/secrets"
)

// ErrSecretsUnavailable means workspace secrets cannot be decrypted or
// written on this host (no encryption key, or an unsupported provider).
var ErrSecretsUnavailable = errors.New("workspace secret encryption is not available on this host")

// maxSecretBytes bounds one secret value. API keys and tokens are far
// smaller; a service-account JSON blob fits.
const maxSecretBytes = 8 << 10

var secretNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)

// PGSecrets stores workspace secrets in the plugin_secrets table, encrypted
// at rest in the enc:v1 envelope shared with the Python app. It implements
// SecretStore for the Runner and backs the /api/v2/plugin-secrets endpoints.
type PGSecrets struct {
	pool      *pgxpool.Pool
	cipher    *secrets.Cipher
	cipherErr error
}

// NewPGSecrets builds the store. cipher may be nil with the reason in
// cipherErr (see secrets.FromEnv): reads then fail for any workspace that has
// a stored secret instead of silently falling back to the environment.
func NewPGSecrets(pool *pgxpool.Pool, cipher *secrets.Cipher, cipherErr error) *PGSecrets {
	return &PGSecrets{pool: pool, cipher: cipher, cipherErr: cipherErr}
}

// NewPGSecretsFromEnv builds the store with the key the Python app uses
// (SECRETS_MASTER_KEY, else SECRET_KEY; see internal/secrets). A missing or
// unusable key is logged and leaves the store in its fail-closed state.
func NewPGSecretsFromEnv(pool *pgxpool.Pool, log *slog.Logger) *PGSecrets {
	if log == nil {
		log = slog.Default()
	}
	c, err := secrets.FromEnv(os.LookupEnv)
	if err != nil {
		log.Warn("workspace plugin secrets cannot be encrypted or decrypted", "err", err.Error())
	}
	return NewPGSecrets(pool, c, err)
}

func (s *PGSecrets) available() error {
	if s.cipher == nil {
		return fmt.Errorf("%w: %v", ErrSecretsUnavailable, s.cipherErr)
	}
	return nil
}

// WorkspaceSecret implements SecretStore.
func (s *PGSecrets) WorkspaceSecret(ctx context.Context, workspaceID, name string) (string, bool, error) {
	var stored string
	var found bool
	err := db.WithTenant(ctx, s.pool, workspaceID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT ciphertext FROM plugin_secrets WHERE workspace_id = $1 AND name = $2`,
			workspaceID, name).Scan(&stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	if err := s.available(); err != nil {
		return "", false, err
	}
	v, err := s.cipher.Decrypt(stored)
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ---- HTTP API --------------------------------------------------------------

// SecretsAPI serves /api/v2/plugin-secrets for workspace admins. Values are
// write-only: no endpoint ever returns one.
type SecretsAPI struct {
	store   *PGSecrets
	catalog *Catalog
	authz   *authz.Client
}

// NewSecretsAPI builds the handlers.
func NewSecretsAPI(store *PGSecrets, catalog *Catalog, az *authz.Client) *SecretsAPI {
	return &SecretsAPI{store: store, catalog: catalog, authz: az}
}

// Mount registers the routes. Every route requires the workspace admin role
// (the owner counts), reads included: which credentials a workspace has
// configured is itself sensitive.
func (a *SecretsAPI) Mount(mux *http.ServeMux) {
	auth := func(h http.HandlerFunc) http.Handler {
		if a.authz == nil || a.store == nil || a.store.pool == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authz.WriteError(w, authz.ErrUnavailable)
			})
		}
		return a.authz.Middleware(h)
	}
	mux.Handle("GET /api/v2/plugin-secrets", auth(a.list))
	mux.Handle("PUT /api/v2/plugin-secrets/{name}", auth(a.put))
	mux.Handle("DELETE /api/v2/plugin-secrets/{name}", auth(a.remove))
}

// secretMeta is everything the API says about a secret. There is no value
// field, by design.
type secretMeta struct {
	Name      string    `json:"name"`
	Plugins   []string  `json:"plugins"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy *string   `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy *string   `json:"updated_by"`
}

type declaredSecret struct {
	Name       string   `json:"name"`
	Plugins    []string `json:"plugins"`
	Configured bool     `json:"configured"`
}

// declaredBy maps each secret name to the installed plugins that declare it.
func (a *SecretsAPI) declaredBy() map[string][]string {
	out := map[string][]string{}
	for _, e := range a.catalog.List() {
		for _, name := range e.Plugin.Capabilities.Secrets {
			out[name] = append(out[name], e.Plugin.Name)
		}
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

// requireAdmin answers 403 and records the denied mutation attempt.
func (a *SecretsAPI) requireAdmin(w http.ResponseWriter, r *http.Request, audit bool) (authz.Workspace, bool) {
	ws, _ := authz.FromContext(r.Context())
	if err := authz.RequireRole(r.Context(), "admin"); err != nil {
		if audit {
			// Best effort: a denied attempt is worth recording, but the
			// refusal must not depend on the audit write.
			_ = db.WithTenant(r.Context(), a.store.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
				return recordAudit(r, tx, ws, http.StatusForbidden, "denied", map[string]any{"resource": "plugin_secret"})
			})
		}
		authz.WriteError(w, err)
		return ws, false
	}
	return ws, true
}

func (a *SecretsAPI) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := a.requireAdmin(w, r, false)
	if !ok {
		return
	}
	declared := a.declaredBy()
	stored := []secretMeta{}
	err := db.WithTenant(r.Context(), a.store.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT name, version, created_at, created_by, updated_at, updated_by
			FROM plugin_secrets WHERE workspace_id = $1 ORDER BY name`, ws.WorkspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m secretMeta
			if err := rows.Scan(&m.Name, &m.Version, &m.CreatedAt, &m.CreatedBy, &m.UpdatedAt, &m.UpdatedBy); err != nil {
				return err
			}
			m.Plugins = declared[m.Name]
			if m.Plugins == nil {
				m.Plugins = []string{}
			}
			stored = append(stored, m)
		}
		return rows.Err()
	})
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not list plugin secrets")
		return
	}
	configured := map[string]bool{}
	for _, m := range stored {
		configured[m.Name] = true
	}
	decl := []declaredSecret{}
	for name, plugins := range declared {
		decl = append(decl, declaredSecret{Name: name, Plugins: plugins, Configured: configured[name]})
	}
	sort.Slice(decl, func(i, j int) bool { return decl[i].Name < decl[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"secrets":    stored,
		"declared":   decl,
		"encryption": map[string]any{"available": a.store.available() == nil},
	})
}

type putRequest struct {
	Value string `json:"value"`
}

func (a *SecretsAPI) put(w http.ResponseWriter, r *http.Request) {
	ws, ok := a.requireAdmin(w, r, true)
	if !ok {
		return
	}
	name := r.PathValue("name")
	plugins, ok := a.checkName(w, name)
	if !ok {
		return
	}
	if err := a.store.available(); err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "Workspace secret encryption is not configured on this server; set SECRETS_MASTER_KEY")
		return
	}
	var req putRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*maxSecretBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// Decoder errors describe structure, never the offending value.
		writeDetail(w, http.StatusBadRequest, "Invalid request body: expected {\"value\": \"...\"}")
		return
	}
	// Surrounding whitespace from copy and paste is not part of an API key;
	// the Python resolver strips it too.
	value := strings.TrimSpace(req.Value)
	switch {
	case value == "":
		writeDetail(w, http.StatusUnprocessableEntity, "value must not be empty (use DELETE to remove a secret)")
		return
	case len(value) > maxSecretBytes:
		writeDetail(w, http.StatusUnprocessableEntity, fmt.Sprintf("value exceeds %d bytes", maxSecretBytes))
		return
	case !utf8.ValidString(value) || strings.ContainsRune(value, 0):
		writeDetail(w, http.StatusUnprocessableEntity, "value must be valid UTF-8 text without NUL characters")
		return
	}
	stored, err := a.store.cipher.Encrypt(value)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not encrypt the secret")
		return
	}
	var meta secretMeta
	var created bool
	err = db.WithTenant(r.Context(), a.store.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `INSERT INTO plugin_secrets (workspace_id, name, ciphertext, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT (workspace_id, name) DO UPDATE
			SET ciphertext = EXCLUDED.ciphertext, version = plugin_secrets.version + 1,
			    updated_at = now(), updated_by = EXCLUDED.updated_by
			RETURNING (xmax = 0), name, version, created_at, created_by, updated_at, updated_by`,
			ws.WorkspaceID, name, stored, ws.Username,
		).Scan(&created, &meta.Name, &meta.Version, &meta.CreatedAt, &meta.CreatedBy, &meta.UpdatedAt, &meta.UpdatedBy)
		if err != nil {
			return err
		}
		action, status := "rotate", http.StatusOK
		if created {
			action, status = "set", http.StatusCreated
		}
		return recordAudit(r, tx, ws, status, "success", map[string]any{
			"resource": "plugin_secret", "secret": name, "action": action, "version": meta.Version,
		})
	})
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "Could not store the secret")
		return
	}
	meta.Plugins = plugins
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, meta)
}

func (a *SecretsAPI) remove(w http.ResponseWriter, r *http.Request) {
	ws, ok := a.requireAdmin(w, r, true)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !secretNameRE.MatchString(name) {
		writeDetail(w, http.StatusNotFound, "Plugin secret not found")
		return
	}
	var deleted bool
	err := db.WithTenant(r.Context(), a.store.pool, ws.WorkspaceID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM plugin_secrets WHERE workspace_id = $1 AND name = $2`, ws.WorkspaceID, name)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected() > 0
		if !deleted {
			return nil
		}
		return recordAudit(r, tx, ws, http.StatusNoContent, "success", map[string]any{
			"resource": "plugin_secret", "secret": name, "action": "delete",
		})
	})
	switch {
	case err != nil:
		writeDetail(w, http.StatusInternalServerError, "Could not delete the secret")
	case !deleted:
		writeDetail(w, http.StatusNotFound, "Plugin secret not found")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkName validates a secret name and requires an installed plugin to
// declare it: only declared secrets are ever resolved, so storing any other
// name would be a typo or a place to hide data.
func (a *SecretsAPI) checkName(w http.ResponseWriter, name string) ([]string, bool) {
	if !secretNameRE.MatchString(name) {
		writeDetail(w, http.StatusUnprocessableEntity, "Invalid secret name")
		return nil, false
	}
	plugins := a.declaredBy()[name]
	if len(plugins) == 0 {
		writeDetail(w, http.StatusUnprocessableEntity,
			"No installed plugin declares this secret (capabilities.secrets); install the plugin first")
		return nil, false
	}
	return plugins, true
}

// ---- audit -------------------------------------------------------------------

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// recordAudit appends a governance_audit_events row in the caller's tenant
// transaction, in the shape the Python audit middleware writes for its own
// mutations (apps/api/services/governance/audit.py), so one trail covers both
// stacks. Metadata never carries a secret value: callers pass the secret's
// name and action only.
func recordAudit(r *http.Request, tx pgx.Tx, ws authz.Workspace, status int, outcome string, meta map[string]any) error {
	id := r.Header.Get("X-Request-Id")
	if !safeRequestID.MatchString(id) {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id = hex.EncodeToString(b[:])
	}
	var actor *int
	if n, err := strconv.Atoi(ws.UserID); err == nil {
		actor = &n
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	route := "/api/v2/plugin-secrets"
	if strings.Contains(r.URL.Path, "/plugin-secrets/") {
		route += "/{name}"
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO governance_audit_events
		(id, workspace_id, actor_user_id, actor_role, method, route, resource_path,
		 response_status, outcome, request_id, metadata_json)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10::json)`,
		ws.WorkspaceID, actor, ws.Role, r.Method, route, truncateRunes(r.URL.Path, 500),
		status, outcome, id, string(raw))
	return err
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
