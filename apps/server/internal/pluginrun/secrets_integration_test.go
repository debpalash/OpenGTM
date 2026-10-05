package pluginrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
	"github.com/debpalash/OpenGTM/apps/server/internal/secrets"
)

const (
	wsA = "ws-secrets-a"
	wsB = "ws-secrets-b"

	secretA = "sk-live-AAAA-9f8e7d6c5b4a" // no % or _ so LIKE-free scans stay exact
	secretB = "sk-live-BBBB-1a2b3c4d5e6f"
)

func fakeLegacyRoles(t *testing.T) *httptest.Server {
	t.Helper()
	ids := map[string]authz.Workspace{
		"admin-a":  {UserID: "11", Username: "ada", WorkspaceID: wsA, Slug: "a", Role: "admin"},
		"owner-a":  {UserID: "12", Username: "olive", WorkspaceID: wsA, Slug: "a", Role: "owner"},
		"editor-a": {UserID: "13", Username: "ed", WorkspaceID: wsA, Slug: "a", Role: "editor"},
		"viewer-a": {UserID: "14", Username: "vi", WorkspaceID: wsA, Slug: "a", Role: "viewer"},
		"admin-b":  {UserID: "21", Username: "bea", WorkspaceID: wsB, Slug: "b", Role: "admin"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := ids[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if r.URL.Path != authz.ContextPath || !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"Could not validate credentials"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(id)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureTransport stands in for the vendor API and records what it was sent.
type captureTransport struct {
	mu       sync.Mutex
	keys     []string // X-Api-Key of every request
	echoKey  bool     // answer with a vendor error that quotes the key
	requests int
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	key := req.Header.Get("X-Api-Key")
	c.keys = append(c.keys, key)
	c.requests++
	echo := c.echoKey
	c.mu.Unlock()
	body := `{"company":{"employees":250,"industry":"Software","linkedin_handle":"acme-example"}}`
	if echo {
		body = fmt.Sprintf(`{"error":true,"error_message":"invalid api key %s"}`, key)
	}
	return &http.Response{
		StatusCode: 200, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: req,
	}, nil
}

func (c *captureTransport) lastKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) == 0 {
		return "<no request>"
	}
	return c.keys[len(c.keys)-1]
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type secretsHarness struct {
	t         *testing.T
	mux       *http.ServeMux
	pool      *pgxpool.Pool // the runtime role: NOSUPERUSER NOBYPASSRLS
	owner     *pgxpool.Pool // setup and assertions only
	store     *PGSecrets
	cipher    *secrets.Cipher
	runner    *Runner
	queue     *queue.Queue
	transport *captureTransport
	logs      *syncBuffer
}

func newSecretsHarness(t *testing.T, withCipher bool) *secretsHarness {
	t.Helper()
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	pool := dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 8)
	owner := dbtest.Pool(t, ownerURL, 2)
	ctx := context.Background()
	cleanup := func() {
		for _, q := range []string{
			`DELETE FROM plugin_secrets WHERE workspace_id = ANY($1)`,
			`DELETE FROM governance_audit_events WHERE workspace_id = ANY($1)`,
			`DELETE FROM plugin_runs WHERE workspace_id = ANY($1)`,
			`DELETE FROM jobs WHERE type = 'plugin_run' AND workspace_id = ANY($1)`,
		} {
			if _, err := owner.Exec(ctx, q, []string{wsA, wsB}); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	h := &secretsHarness{t: t, pool: pool, owner: owner, transport: &captureTransport{}, logs: &syncBuffer{}}
	var c *secrets.Cipher
	var cerr error = errors.New("no key for this test")
	if withCipher {
		c, cerr = secrets.NewCipher(secrets.DeriveKey("integration-test-key")), nil
	}
	h.cipher = c
	h.store = NewPGSecrets(pool, c, cerr)
	log := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	root := filepath.Join(dbtest.RepoRoot(t), "plugins", "examples")
	catalog := LoadCatalog(config.Plugins{Dirs: []string{root}, SignaturePolicy: "optional"})
	if e, ok := catalog.Get("acme_firmographics"); !ok || !e.Plugin.HasSecret("ACME_DATA_API_KEY") {
		t.Fatalf("example provider not loaded: %+v", catalog.Errors)
	}
	client, err := egress.New(egress.Options{Transport: h.transport, ReplayWithoutRateLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	h.runner = NewRunner(client, nil, log)
	h.runner.SetSecretStore(h.store)
	reg := queue.NewRegistry()
	NewWorker(pool, catalog, h.runner, log).Register(reg)
	h.queue = queue.New(pool, reg, queue.Options{Concurrency: 2, IdlePoll: 20 * time.Millisecond, ClaimCheckInterval: 50 * time.Millisecond})

	az, err := authz.New(fakeLegacyRoles(t).URL, authz.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.mux = http.NewServeMux()
	NewAPI(pool, catalog, az).Mount(h.mux)
	NewSecretsAPI(h.store, catalog, az).Mount(h.mux)
	return h
}

// raw performs a request and returns the status and the exact response text.
func (h *secretsHarness) raw(token, method, path, body string) (int, string) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-Id", fmt.Sprintf("req-%d", requestSeq.Add(1)))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (h *secretsHarness) json(token, method, path, body string) (int, map[string]any) {
	h.t.Helper()
	code, text := h.raw(token, method, path, body)
	var out map[string]any
	_ = json.Unmarshal([]byte(text), &out)
	return code, out
}

func (h *secretsHarness) put(token, name, value string) (int, map[string]any) {
	b, _ := json.Marshal(map[string]string{"value": value})
	return h.json(token, "PUT", "/api/v2/plugin-secrets/"+name, string(b))
}

const keyName = "ACME_DATA_API_KEY"

var requestSeq atomic.Int64

// everywhere returns how many rows in the whole database contain text.
func (h *secretsHarness) everywhere(text string) map[string]int {
	h.t.Helper()
	ctx := context.Background()
	rows, err := h.owner.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public'`)
	if err != nil {
		h.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	hits := map[string]int{}
	for _, tbl := range tables {
		var n int
		q := `SELECT count(*) FROM ` + pgx.Identifier{tbl}.Sanitize() + ` t WHERE strpos(t::text, $1) > 0`
		if err := h.owner.QueryRow(ctx, q, text).Scan(&n); err != nil {
			h.t.Fatalf("%s: %v", q, err)
		}
		if n > 0 {
			hits[tbl] = n
		}
	}
	return hits
}

// ---- tests --------------------------------------------------------------------

func TestSecretsAPIRoleEnforcement(t *testing.T) {
	h := newSecretsHarness(t, true)
	for _, token := range []string{"editor-a", "viewer-a"} {
		for _, tc := range []struct{ method, path, body string }{
			{"GET", "/api/v2/plugin-secrets", ""},
			{"PUT", "/api/v2/plugin-secrets/" + keyName, `{"value":"` + secretA + `"}`},
			{"DELETE", "/api/v2/plugin-secrets/" + keyName, ""},
		} {
			if code, body := h.raw(token, tc.method, tc.path, tc.body); code != http.StatusForbidden || strings.Contains(body, secretA) {
				t.Errorf("%s %s %s: %d %s", token, tc.method, tc.path, code, body)
			}
		}
	}
	if code, _ := h.raw("nobody", "GET", "/api/v2/plugin-secrets", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", code)
	}
	// Nothing was written by the refused attempts, which are on the audit trail.
	var n int
	if err := h.owner.QueryRow(context.Background(), `SELECT count(*) FROM plugin_secrets WHERE workspace_id = $1`, wsA).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused writes stored %d secrets (%v)", n, err)
	}
	rows, err := h.owner.Query(context.Background(), `SELECT outcome, response_status, actor_role FROM governance_audit_events
		WHERE workspace_id = $1 AND route LIKE '/api/v2/plugin-secrets%' ORDER BY actor_role, method`, wsA)
	if err != nil {
		t.Fatal(err)
	}
	var denied []string
	for rows.Next() {
		var outcome, role string
		var status int
		_ = rows.Scan(&outcome, &status, &role)
		denied = append(denied, fmt.Sprintf("%s/%d/%s", outcome, status, role))
	}
	rows.Close()
	if got := strings.Join(denied, ","); got != "denied/403/editor,denied/403/editor,denied/403/viewer,denied/403/viewer" {
		t.Errorf("denied mutations on the audit trail: %s", got)
	}

	// Admins and the owner may.
	if code, _ := h.put("admin-a", keyName, secretA); code != http.StatusCreated {
		t.Errorf("admin put: %d", code)
	}
	if code, _ := h.put("owner-a", keyName, secretA); code != http.StatusOK {
		t.Errorf("owner put: %d", code)
	}
	if code, _ := h.json("owner-a", "GET", "/api/v2/plugin-secrets", ""); code != http.StatusOK {
		t.Errorf("owner list: %d", code)
	}
}

func TestSecretsAreWriteOnlyAndEncryptedAtRest(t *testing.T) {
	h := newSecretsHarness(t, true)
	code, put := h.put("admin-a", keyName, "  "+secretA+"\n")
	if code != http.StatusCreated {
		t.Fatalf("put: %d %v", code, put)
	}
	if put["name"] != keyName || put["version"] != float64(1) || put["created_by"] != "ada" || put["updated_by"] != "ada" {
		t.Fatalf("metadata: %v", put)
	}
	// Every installed plugin that declares the secret is listed (the Python
	// example declares the same name).
	if plugins, _ := put["plugins"].([]any); !slices.Contains(plugins, any("acme_firmographics")) {
		t.Fatalf("plugins: %v", put["plugins"])
	}

	code, body := h.raw("admin-a", "GET", "/api/v2/plugin-secrets", "")
	if code != http.StatusOK || strings.Contains(body, secretA) || strings.Contains(body, "ciphertext") || strings.Contains(body, "enc:v1") {
		t.Fatalf("list leaks: %d %s", code, body)
	}
	var generic struct {
		Secrets    []map[string]any `json:"secrets"`
		Declared   []map[string]any `json:"declared"`
		Encryption map[string]any   `json:"encryption"`
	}
	if err := json.Unmarshal([]byte(body), &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic.Secrets) != 1 || generic.Secrets[0]["name"] != keyName {
		t.Fatalf("list: %s", body)
	}
	for k := range generic.Secrets[0] {
		switch k {
		case "name", "plugins", "version", "created_at", "created_by", "updated_at", "updated_by":
		default:
			t.Errorf("unexpected field %q in secret metadata", k)
		}
	}
	configured := false
	for _, d := range generic.Declared {
		if d["name"] == keyName {
			configured = d["configured"] == true
		}
	}
	if !configured || generic.Encryption["available"] != true {
		t.Fatalf("declared/encryption: %s", body)
	}

	// At rest: only ciphertext in the shared envelope, readable by the cipher
	// (and, per the secrets package tests, by the Python app).
	var stored string
	if err := h.owner.QueryRow(context.Background(), `SELECT ciphertext FROM plugin_secrets WHERE workspace_id = $1 AND name = $2`, wsA, keyName).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "enc:v1:") || strings.Contains(stored, secretA) {
		t.Fatalf("stored value is not an encrypted envelope: %q", stored)
	}
	if plain, err := h.cipher.Decrypt(stored); err != nil || plain != secretA {
		t.Fatalf("decrypt: %q %v (surrounding whitespace must be stripped)", plain, err)
	}
	if hits := h.everywhere(secretA); len(hits) != 0 {
		t.Fatalf("plaintext found in the database: %v", hits)
	}
	// The table itself refuses anything outside the envelope.
	_, err := h.owner.Exec(context.Background(), `INSERT INTO plugin_secrets (workspace_id, name, ciphertext) VALUES ($1, 'PLAINTEXT_ATTEMPT', 'hunter2')`, wsA)
	if err == nil {
		t.Fatal("the database accepted a plaintext secret")
	}
}

func TestSecretsTenantIsolation(t *testing.T) {
	h := newSecretsHarness(t, true)
	if code, _ := h.put("admin-a", keyName, secretA); code != http.StatusCreated {
		t.Fatalf("a put: %d", code)
	}

	// B sees nothing of A's through the API.
	code, body := h.raw("admin-b", "GET", "/api/v2/plugin-secrets", "")
	if code != http.StatusOK || strings.Contains(body, `"name":"`+keyName+`"`) && !strings.Contains(body, `"secrets":[]`) {
		t.Fatalf("b list: %d %s", code, body)
	}
	if code, _ := h.raw("admin-b", "DELETE", "/api/v2/plugin-secrets/"+keyName, ""); code != http.StatusNotFound {
		t.Fatalf("b deleting a's secret: %d", code)
	}
	ctx := context.Background()
	if v, found, err := h.store.WorkspaceSecret(ctx, wsB, keyName); err != nil || found || v != "" {
		t.Fatalf("b resolves a's secret: %q %v %v", v, found, err)
	}
	// B stores its own value under the same name; the two never mix.
	if code, _ := h.put("admin-b", keyName, secretB); code != http.StatusCreated {
		t.Fatalf("b put: %d", code)
	}
	va, _, _ := h.store.WorkspaceSecret(ctx, wsA, keyName)
	vb, _, _ := h.store.WorkspaceSecret(ctx, wsB, keyName)
	if va != secretA || vb != secretB {
		t.Fatalf("values mixed: a=%q b=%q", va, vb)
	}
	// A's secret is intact after B's delete attempt and B's own delete.
	if code, _ := h.raw("admin-b", "DELETE", "/api/v2/plugin-secrets/"+keyName, ""); code != http.StatusNoContent {
		t.Fatalf("b delete own: %d", code)
	}
	if va, found, _ := h.store.WorkspaceSecret(ctx, wsA, keyName); !found || va != secretA {
		t.Fatalf("a's secret damaged: %q %v", va, found)
	}

	// Row-level security, below the API: as the runtime role, bound to B,
	// A's row does not exist and cannot be written.
	err := db.WithTenant(ctx, h.pool, wsB, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_secrets`).Scan(&n); err != nil || n != 0 {
			return fmt.Errorf("b sees %d rows (%v)", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.WithTenant(ctx, h.pool, wsB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO plugin_secrets (workspace_id, name, ciphertext) VALUES ($1, 'FORGED_NAME', 'enc:v1:x')`, wsA)
		return err
	})
	if err == nil {
		t.Fatal("b wrote a row into a's workspace")
	}
	var n int
	if err := db.WithoutTenant(ctx, h.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM plugin_secrets`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("unbound connection sees %d rows (%v)", n, err)
	}
	var forced bool
	if err := h.owner.QueryRow(ctx, `SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE relname = 'plugin_secrets'`).Scan(&forced); err != nil || !forced {
		t.Fatalf("plugin_secrets must have FORCED row level security (%v)", err)
	}
}

func TestSecretRotationAndDeletion(t *testing.T) {
	h := newSecretsHarness(t, true)
	catalog := LoadCatalog(config.Plugins{Dirs: []string{filepath.Join(dbtest.RepoRoot(t), "plugins", "examples")}, SignaturePolicy: "optional"})
	entry, _ := catalog.Get("acme_firmographics")
	run := func(ws string) (Outcome, error) {
		return h.runner.Run(WithWorkspace(context.Background(), ws), entry.Plugin, map[string]any{"domain": "acme.example"}, nil)
	}
	t.Setenv(keyName, "env-fallback-value-0000")

	// No workspace secret yet: the environment is used.
	if out, err := run(wsA); err != nil || len(out.Records) != 1 || h.transport.lastKey() != "env-fallback-value-0000" {
		t.Fatalf("env fallback: %+v %v key=%q", out, err, h.transport.lastKey())
	}
	// A workspace secret wins over the environment, for that workspace only.
	_, first := h.put("admin-a", keyName, "workspace-key-one-1111")
	if _, err := run(wsA); err != nil || h.transport.lastKey() != "workspace-key-one-1111" {
		t.Fatalf("workspace secret not used: %v key=%q", err, h.transport.lastKey())
	}
	if _, err := run(wsB); err != nil || h.transport.lastKey() != "env-fallback-value-0000" {
		t.Fatalf("another workspace must keep the environment value: %v key=%q", err, h.transport.lastKey())
	}

	// Rotation: the next run uses the new value, metadata records it, and the
	// old value is gone from the database.
	time.Sleep(20 * time.Millisecond)
	code, second := h.put("admin-a", keyName, "workspace-key-two-2222")
	if code != http.StatusOK || second["version"] != float64(2) || second["created_at"] != first["created_at"] || second["updated_at"] == first["updated_at"] {
		t.Fatalf("rotation metadata: %d first=%v second=%v", code, first, second)
	}
	if _, err := run(wsA); err != nil || h.transport.lastKey() != "workspace-key-two-2222" {
		t.Fatalf("rotated secret not used: %v key=%q", err, h.transport.lastKey())
	}
	if hits := h.everywhere("workspace-key-one-1111"); len(hits) != 0 {
		t.Fatalf("the old secret survives rotation: %v", hits)
	}

	// Deleting returns the workspace to the environment value.
	if code, _ := h.raw("admin-a", "DELETE", "/api/v2/plugin-secrets/"+keyName, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := run(wsA); err != nil || h.transport.lastKey() != "env-fallback-value-0000" {
		t.Fatalf("after delete: %v key=%q", err, h.transport.lastKey())
	}
	if code, _ := h.raw("admin-a", "DELETE", "/api/v2/plugin-secrets/"+keyName, ""); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
}

func TestSecretsValidation(t *testing.T) {
	h := newSecretsHarness(t, true)
	put := func(path, body string) (int, string) {
		return h.raw("admin-a", "PUT", "/api/v2/plugin-secrets/"+path, body)
	}
	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"undeclared name":        {"NOT_DECLARED_ANYWHERE", `{"value":"abcdefgh"}`, http.StatusUnprocessableEntity},
		"bad name":               {"9bad", `{"value":"abcdefgh"}`, http.StatusUnprocessableEntity},
		"empty value":            {keyName, `{"value":""}`, http.StatusUnprocessableEntity},
		"blank value":            {keyName, `{"value":"  \n"}`, http.StatusUnprocessableEntity},
		"too large":              {keyName, `{"value":"` + strings.Repeat("a", maxSecretBytes+1) + `"}`, http.StatusUnprocessableEntity},
		"nul":                    {keyName, `{"value":"abc\u0000def"}`, http.StatusUnprocessableEntity},
		"unknown field":          {keyName, `{"value":"abcdefgh","note":"x"}`, http.StatusBadRequest},
		"wrong type":             {keyName, `{"value":12345678}`, http.StatusBadRequest},
		"not json":               {keyName, `value=abcdefgh`, http.StatusBadRequest},
		"missing value":          {keyName, `{}`, http.StatusUnprocessableEntity},
		"way too large for body": {keyName, `{"value":"` + strings.Repeat("a", 10*maxSecretBytes) + `"}`, http.StatusBadRequest},
	} {
		code, body := put(tc.path, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d %.200s", name, code, body)
		}
		// Error responses never echo the submitted value.
		if strings.Contains(body, "abcdefgh") || strings.Contains(body, "12345678") {
			t.Errorf("%s: error body echoes the value: %s", name, body)
		}
	}
	if code, _ := h.raw("admin-a", "DELETE", "/api/v2/plugin-secrets/9bad", ""); code != http.StatusNotFound {
		t.Errorf("delete bad name: %d", code)
	}
	var n int
	if err := h.owner.QueryRow(context.Background(), `SELECT count(*) FROM plugin_secrets WHERE workspace_id = $1`, wsA).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid requests stored %d secrets", n)
	}
}

func TestSecretsAuditTrailHasNoValues(t *testing.T) {
	h := newSecretsHarness(t, true)
	h.put("admin-a", keyName, secretA)
	h.put("owner-a", keyName, secretB)
	h.raw("admin-a", "DELETE", "/api/v2/plugin-secrets/"+keyName, "")

	rows, err := h.owner.Query(context.Background(), `SELECT method, route, resource_path, response_status, outcome, actor_user_id,
		actor_role, request_id, metadata_json::text FROM governance_audit_events
		WHERE workspace_id = $1 AND route LIKE '/api/v2/plugin-secrets%' ORDER BY created_at, id`, wsA)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var method, route, path, outcome, role, reqID, meta string
		var status int
		var actor *int
		if err := rows.Scan(&method, &route, &path, &status, &outcome, &actor, &role, &reqID, &meta); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(meta, secretA) || strings.Contains(meta, secretB) || strings.Contains(meta, "enc:v1") {
			t.Fatalf("audit metadata carries a secret: %s", meta)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(meta), &m)
		a := "nil"
		if actor != nil {
			a = fmt.Sprint(*actor)
		}
		got = append(got, fmt.Sprintf("%s %s %s %d %s actor=%s/%s %v/%v", method, route, path, status, outcome, a, role, m["action"], m["secret"]))
		if reqID == "" {
			t.Error("audit row without a request id")
		}
	}
	want := []string{
		"PUT /api/v2/plugin-secrets/{name} /api/v2/plugin-secrets/ACME_DATA_API_KEY 201 success actor=11/admin set/ACME_DATA_API_KEY",
		"PUT /api/v2/plugin-secrets/{name} /api/v2/plugin-secrets/ACME_DATA_API_KEY 200 success actor=12/owner rotate/ACME_DATA_API_KEY",
		"DELETE /api/v2/plugin-secrets/{name} /api/v2/plugin-secrets/ACME_DATA_API_KEY 204 success actor=11/admin delete/ACME_DATA_API_KEY",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit trail:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSecretsFailClosedWithoutEncryptionKey(t *testing.T) {
	h := newSecretsHarness(t, true)
	h.put("admin-a", keyName, secretA)
	t.Setenv(keyName, "env-fallback-value-0000")

	// The same database, but a host with no usable key.
	bad := NewPGSecrets(h.pool, nil, secrets.ErrNoKey)
	if _, _, err := bad.WorkspaceSecret(context.Background(), wsA, keyName); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("a stored secret without a key must be an error, not an env fallback: %v", err)
	}
	// A workspace with no stored secret is unaffected.
	if _, found, err := bad.WorkspaceSecret(context.Background(), wsB, keyName); err != nil || found {
		t.Fatalf("workspace without a secret: %v %v", found, err)
	}
	// A wrong key is also a loud failure.
	wrong := NewPGSecrets(h.pool, secrets.NewCipher(secrets.DeriveKey("some other key")), nil)
	if _, _, err := wrong.WorkspaceSecret(context.Background(), wsA, keyName); err == nil || strings.Contains(err.Error(), secretA) {
		t.Fatalf("wrong key: %v", err)
	}
	// The API reports and refuses.
	az, _ := authz.New(fakeLegacyRoles(t).URL, authz.Options{})
	mux := http.NewServeMux()
	catalog := LoadCatalog(config.Plugins{Dirs: []string{filepath.Join(dbtest.RepoRoot(t), "plugins", "examples")}, SignaturePolicy: "optional"})
	NewSecretsAPI(bad, catalog, az).Mount(mux)
	call := func(method, body string) (int, string) {
		req := httptest.NewRequest(method, "/api/v2/plugin-secrets/"+keyName, strings.NewReader(body))
		if method == "GET" {
			req = httptest.NewRequest(method, "/api/v2/plugin-secrets", nil)
		}
		req.Header.Set("Authorization", "Bearer admin-a")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call("PUT", `{"value":"`+secretB+`"}`); code != http.StatusServiceUnavailable || strings.Contains(body, secretB) {
		t.Fatalf("put without a key: %d %s", code, body)
	}
	if code, body := call("GET", ""); code != http.StatusOK || !strings.Contains(body, `"available":false`) {
		t.Fatalf("list without a key: %d %s", code, body)
	}
	// A run for the workspace fails instead of using the environment value.
	catalogEntry, _ := catalog.Get("acme_firmographics")
	r := NewRunner(h.runner.client, nil, nil)
	r.SetSecretStore(bad)
	before := h.transport.requests
	if _, err := r.Run(WithWorkspace(context.Background(), wsA), catalogEntry.Plugin, map[string]any{"domain": "acme.example"}, nil); err == nil {
		t.Fatal("run succeeded without being able to read the workspace secret")
	}
	if h.transport.requests != before {
		t.Fatal("a request went out with the wrong credential")
	}
}

func TestWorkspaceSecretNeverLeavesTheRunner(t *testing.T) {
	h := newSecretsHarness(t, true)
	t.Setenv(keyName, "env-fallback-value-0000")
	if code, _ := h.put("admin-a", keyName, secretA); code != http.StatusCreated {
		t.Fatalf("put: %d", code)
	}
	h.startWorker()

	start := func(plugin string) string {
		code, run := h.json("editor-a", "POST", "/api/v2/plugin-runs", `{"plugin":"`+plugin+`","inputs":{"domain":"acme.example"}}`)
		if code != http.StatusAccepted {
			t.Fatalf("create run: %d %v", code, run)
		}
		return run["id"].(string)
	}
	wait := func(id string) map[string]any {
		deadline := time.Now().Add(20 * time.Second)
		for {
			_, run := h.json("editor-a", "GET", "/api/v2/plugin-runs/"+id, "")
			if run["status"] == "completed" || run["status"] == "failed" {
				return run
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s stuck: %v", id, run)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// 1. A normal run uses the workspace secret on the wire.
	id := start("acme_firmographics")
	run := wait(id)
	if run["status"] != "completed" || h.transport.lastKey() != secretA {
		t.Fatalf("run: %v key=%q", run, h.transport.lastKey())
	}
	_, results := h.raw("editor-a", "GET", "/api/v2/plugin-runs/"+id+"/results", "")
	if !strings.Contains(results, `"company_size":250`) {
		t.Fatalf("results: %s", results)
	}

	// 2. A vendor that echoes the key in its error message: the provider
	// error is redacted before it is stored.
	h.transport.mu.Lock()
	h.transport.echoKey = true
	h.transport.mu.Unlock()
	echoID := start("acme_firmographics")
	echoRun := wait(echoID)
	stats, _ := json.Marshal(echoRun["stats"])
	if echoRun["status"] != "completed" || !strings.Contains(string(stats), "REDACTED") {
		t.Fatalf("echoed key was not redacted: %v", echoRun)
	}

	// 3. Nothing observable contains the value: API responses, logs, and
	// every row of every table (jobs, runs, results, audit, ...). The only
	// place it may appear is encrypted.
	for _, id := range []string{id, echoID} {
		for _, path := range []string{"/api/v2/plugin-runs/" + id, "/api/v2/plugin-runs/" + id + "/results", "/api/v2/plugin-runs", "/api/v2/plugins"} {
			if _, body := h.raw("editor-a", "GET", path, ""); strings.Contains(body, secretA) {
				t.Errorf("%s leaks the secret: %s", path, body)
			}
		}
	}
	if _, body := h.raw("admin-a", "GET", "/api/v2/plugin-secrets", ""); strings.Contains(body, secretA) {
		t.Errorf("secret list leaks: %s", body)
	}
	if logs := h.logs.String(); strings.Contains(logs, secretA) || strings.Contains(logs, "enc:v1:") {
		t.Errorf("logs contain the secret or its ciphertext:\n%s", logs)
	}
	if hits := h.everywhere(secretA); len(hits) != 0 {
		t.Errorf("secret found in the database: %v", hits)
	}
	// A run in the other workspace never sees A's secret.
	if _, found, _ := h.store.WorkspaceSecret(context.Background(), wsB, keyName); found {
		t.Error("workspace B resolved A's secret")
	}
}

func (h *secretsHarness) startWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.queue.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-done })
}
