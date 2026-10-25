# Settings, Secrets and Billing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the `/api/settings` router (14 of its 19 routes), the envelope-encryption secret store (byte-compatible with Python `cryptography` Fernet and Vault Transit), LLM key storage and `/api/billing` incl. the Stripe webhook to Go, and let the Go worker resolve credentials locally instead of calling the Python internal endpoint.

**Architecture:** A new `internal/secrets` package owns Fernet + Vault Transit envelopes and the three-level resolver (workspace secret, then global `app_settings`, then process env), exactly as `services/workspace/secrets.py` and `_default_env_resolver` do. `internal/settings` serves the router from embedded registries generated from the Python source (so provider/integration tables cannot drift silently). `internal/billing` ports `credit`, balance and webhook verification with hand-rolled Stripe signature checking and one Checkout REST call (no SDK). All routes ride the plan 02 foundation (auth, workspace, role middleware, nginx canary).

**Tech Stack:** Go 1.27.3 stdlib crypto (`crypto/aes`, `crypto/cipher`, `crypto/hmac`, `crypto/sha256`), pgx v5 + sqlc, `net/http`; no new third-party modules. Python/uv only for registry export and golden-vector generation.

**Spec:** `docs/go-port/inventory.md` section 8 plan 2 ("Settings + secrets"); `docs/go-port/plans/01-sqlite-to-postgres.md` (`app_settings`, `workspace_settings`); `docs/go-port/plans/02-go-api-foundation-auth-workspaces.md` (middleware, `httpx`, `contract`); GitHub issue #33.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths below (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Python edits are not versioned; this plan only adds two generator scripts there. Go paths are relative to `apps/backend-go/`.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean; no new Go modules.
- Tenant tables (`workspace_settings`, `workspace_credits`, `credit_ledger_entries`, `llm_usage_daily`) only inside `tenancy.InWorkspace`. `app_settings` is global (plain pool). Runtime role non-superuser `NOBYPASSRLS`; Go never runs DDL.
- No CI changes. Verify with `cd apps/backend-go && make vet test` and `make test-integration`.
- Decrypting every ciphertext Python ever wrote is mandatory: `enc:v1:<fernet token>` and `enc:v2:vault:<vault ciphertext>`; values without a prefix are legacy plaintext returned as-is. Ciphertext Go writes must decrypt in Python (`decrypt_value`) and vice versa. Golden vectors come from the real Python module, never from a Go-only round trip.
- Fernet key material (`SECRETS_MASTER_KEY`): `strip()`; if it is a valid Fernet key (Python `urlsafe_b64decode` yields 32 bytes) use it verbatim, else key = `urlsafe_b64encode(sha256(raw))`; empty -> derive from `SECRET_KEY` the same way; refuse to encrypt when deriving from the insecure default `INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY` and `APP_ENV` is not dev (error text identical to `secrets.py:_load_master_key`). Providers: `SECRETS_PROVIDER` = `local` (default) | `vault_transit`; anything else -> `unsupported SECRETS_PROVIDER: <x>`.
- Secrets never appear in logs, error strings, audit metadata, fixtures or cache keys; API responses never return a stored secret (masks as Python does: provider keys `key[:8] + "••••" + key[-4:]`, or `••••••••` when `< 16` chars and non-empty, integration secrets `…` + last 4).
- Contract parity: same paths, JSON keys, status codes, `detail` strings and auth gates as FastAPI (`router dependencies=[get_current_active_user]`; admin routes add `get_current_admin_user`). Recorded fixtures compare modulo timestamps.
- Every commit DCO signed (`git commit -s`) as Palash Debnath. Sonnet subagents execute.
- Stays on FastAPI (documented, not silently dropped): the 5 `sources*` routes (`GET /sources`, `GET /sources/health`, `PUT /sources/{id}`, `POST /sources/{id}/health/reset`, `GET /sources/registry`) because they read the Python leadgen registry and source-health layer; Stripe `check_and_debit` (called by Python runs until plan 05 moves enrichment); `rotate_encrypted_secrets` (Python CLI keeps working against PostgreSQL).

## Review Focus

1. **Fernet key quirks.** Python `urlsafe_b64decode` silently discards non-alphabet characters and requires correct padding; a master key with an embedded newline or missing `=` decodes (or fails) differently than Go's strict `base64.URLEncoding`. A wrong choice makes every stored secret undecryptable after cutover. Task 1 pins vectors for padded, unpadded, newline-containing, wrong-length and non-base64 master keys.
2. **Token integrity before decryption.** Fernet verifies the HMAC before touching AES; Go must compare in constant time, reject version byte != `0x80`, short tokens (< 73 bytes), ciphertext length not a multiple of 16, and bad PKCS7 padding, all as `InvalidToken` -> the same "Failed to decrypt a per-workspace secret ..." error, never a panic and never plaintext leakage. Task 1.
3. **Resolution order and the verifier-URL SSRF rule.** `REACHER_URL` must resolve only from global config/env, never from a workspace secret (`VERIFIER_GLOBAL_ONLY_KEYS`); `workspace-integrations` writes must reject keys outside the integration's field list. Empty stored values count as unset and fall through to env. Tasks 2 and 6.
4. **Stripe webhook replay and tenant forgery.** Signature header `t=<ts>,v1=<hex>[,v1=...]`, HMAC-SHA256 over `"<ts>.<payload>"`, 300 s tolerance, constant-time compare, raw body bytes (not re-marshalled JSON). A valid event for workspace A must never credit workspace B; redelivery must not double-credit (`idempotency_key = "stripe:<session_id>"`, unique index). Task 8.
5. **`os.environ` write-through disappears.** Python `_db_set` also sets `os.environ[key]` in the API process; Go cannot. Any Python code in the SAME process that reads env directly after a settings write would see stale values until restart. Task 6 includes a grep gate over `apps/api` for direct env reads of provider keys and fixes them to use `_db_get`; the Python worker already reads the table.

---

## File Structure

```
internal/secrets/{fernet,vault,envelope,resolver}.go      # crypto + resolution (+ tests)
internal/settings/{registry,appsettings,providers,perf,system,models,llmusage,integrations}.go
internal/settings/registry.json                           # generated from Python (embedded)
internal/billing/{stripe,ledger}.go
internal/api/{settingsroutes,billingroutes}.go
internal/credentials/local.go                             # local CredentialSource for the worker
internal/db/queries/{app_settings,workspace_settings,billing,llm_usage}.sql  (+ gen/)
cmd/worker/main.go                                        # (modify) CREDENTIALS_SOURCE switch
testdata/secrets/python_vectors.json                      # generated by legacy script
testdata/contract/settings/*.json, billing/*.json         # recorded FastAPI responses
../../legacy/python/scripts/export_settings_registry.py   # writes registry.json
../../legacy/python/scripts/gen_secret_vectors.py         # writes python_vectors.json
../../infra/nginx/go-routes.conf                          # (modify) add prefixes
../../docs/go-port/runbooks/settings-cutover.md
```

---

### Task 1: Fernet envelope compatible with Python

**Files:**
- Create: `legacy/python/scripts/gen_secret_vectors.py`, `internal/secrets/fernet.go`, `internal/secrets/fernet_test.go`, `testdata/secrets/python_vectors.json`

**Interfaces:**
- Produces: `type Fernet struct{ signKey, encKey [16]byte }`; `func ParseKey(raw string) (Fernet, error)` (`raw` is the value after `strip()`; replicates Python `urlsafe_b64decode`: map `-_` to `+/`, drop characters outside `A-Za-z0-9+/=`, require padding validity, result must be 32 bytes, else `ErrNotFernetKey`); `func DeriveKey(material string) Fernet` (`urlsafe_b64encode(sha256(material))` then `ParseKey`); `func (f Fernet) Encrypt(plaintext []byte) (string, error)` (version `0x80`, 8-byte big-endian unix seconds, 16 random IV bytes, AES-128-CBC PKCS7, HMAC-SHA256 over `version|ts|iv|ct`, URL-safe base64 WITH padding); `func (f Fernet) Decrypt(token string) ([]byte, error)` (no TTL check, as Python passes none) returning `ErrInvalidToken` for every malformed/forged input.
- Vector file shape: `[{"name","master_key_raw","effective_key_b64","plaintext","token"}]`, generated by calling `apps.api.services.workspace.secrets` with `SECRETS_MASTER_KEY` set (cases: valid Fernet key; passphrase; key with embedded `\n`; unpadded 43-char key; unicode plaintext `"p4ss-wörd-🔑"`; empty-after-strip not encrypted).

- [ ] **Step 1: Write failing tests**: `TestDecryptsPythonTokens` (every vector), `TestPythonDecryptsGoTokens` (Go encrypts; the test shells the vector generator's `--verify` mode only when `PYTHON_VERIFY=1`, otherwise checks the structure: base64 length, version byte, HMAC recomputation), `TestParseKeyQuirks` (table: padded valid, unpadded -> error, internal newline accepted, 31/33-byte -> error, non-base64 -> `ErrNotFernetKey`), `TestDeriveKeyMatchesPython`, `TestRejectsWrongVersion`, `TestRejectsTruncatedToken`, `TestRejectsBadMac`, `TestRejectsBadPadding`, `TestRejectsCiphertextNotBlockMultiple`, `TestDecryptNeverPanicsOnFuzz` (`testing/fuzz` seed corpus, 5 s).
- [ ] **Step 2: Run** `go test ./internal/secrets/...` -> FAIL.
- [ ] **Step 3: Implement** with stdlib only; compare MACs with `hmac.Equal` before decrypting.
- [ ] **Step 4: Run** `go test -race ./internal/secrets/...` -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): fernet envelope compatible with python cryptography"`

### Task 2: Envelope, Vault Transit and the resolver

**Files:**
- Create: `internal/secrets/{vault,envelope,resolver}.go`, tests; `internal/db/queries/{app_settings,workspace_settings}.sql`

**Interfaces:**
- Envelope (matches `secrets.py:123-175`): `const EncPrefix = "enc:v1:"; VaultPrefix = "enc:v2:vault:"`; `type Config struct{ Provider, MasterKey, SecretKey, AppEnv string; VaultAddr, VaultToken, VaultTokenFile, VaultTransitKey, VaultNamespace, VaultCACert string }`; `type Envelope struct{...}`; `func NewEnvelope(cfg Config, hc *http.Client) (*Envelope, error)`; `func (e *Envelope) Encrypt(ctx, plaintext string) (string, error)`; `func (e *Envelope) Decrypt(ctx, stored string) (string, error)` (empty -> `""`; no prefix -> returned as-is; `enc:v1:` -> Fernet; `enc:v2:vault:` -> Vault; wrong key -> `failed to decrypt a per-workspace secret: the master key does not match the key used to encrypt it (rotated/incorrect SECRETS_MASTER_KEY?)`).
- Vault: `VAULT_ADDR` must be a clean `https` URL (no userinfo/query/fragment, host present) else `VAULT_ADDR must be a clean HTTPS URL`; token from `VAULT_TOKEN` or file `VAULT_TOKEN_FILE`; requires `VAULT_TRANSIT_KEY` (default `opengtm`); `POST {addr}/v1/transit/{encrypt|decrypt}/{url.PathEscape(key)}` with headers `X-Vault-Token`, optional `X-Vault-Namespace`, body `{"plaintext": b64}` or `{"ciphertext": s}`, 10 s timeout, TLS verify against `VAULT_CACERT` when set; response `data.ciphertext` / `data.plaintext` (base64, strict) required non-empty. Use a plain `http.Client` (operator-configured trusted host), not the SSRF-guarded egress client.
- Resolver: `type Store interface{ AppSetting(ctx, key string) (string, error); WorkspaceSetting(ctx, ws, key string) (string, bool, error) }`; `type Resolver struct{...}`; `func NewResolver(env *Envelope, st Store, getenv func(string) string) *Resolver`; `func (r *Resolver) Get(ctx, ws, key string) (value, source string, err error)` with `source` in `workspace|global|default`: workspace ciphertext (non-empty) decrypted; else `AppSetting` non-empty; else env non-empty; else `""`. `func (r *Resolver) Set(ctx, ws, key, value string) error` (blank/whitespace ignored; trims; stores ciphertext via `ON CONFLICT (workspace_id,key) DO UPDATE`); `func (r *Resolver) Has(ctx, ws, key string) (bool, error)`. Keys in `GlobalOnly = {"REACHER_URL"}` skip the workspace level.

- [ ] **Step 1: Write failing tests**: `TestDecryptPlaintextPassthrough`, `TestDecryptEmptyIsEmpty`, `TestVaultRoundTripAgainstFakeServer` (httptest TLS server asserting headers, path escaping of a key like `my key/1`, namespace header), `TestVaultRejectsHTTPAndUserinfoAddr`, `TestVaultTokenFromFile`, `TestVaultInvalidBase64PlaintextErrors`, `TestUnsupportedProviderRejected`, `TestInsecureSecretKeyRefusedInProdEncrypt`, `TestResolverOrder` (workspace > global > env > default with `source` values), `TestResolverEmptyGlobalFallsToEnv`, `TestResolverReacherURLNeverFromWorkspace`, `TestSetBlankIgnored`, `TestSetStoresCiphertextNotPlaintext` (integration, reads the raw row), `TestResolverDecryptsRowWrittenByPython` (vector row inserted as owner).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL.
- [ ] **Step 3: Implement**; sqlc queries `GetAppSetting`, `UpsertAppSetting`, `GetWorkspaceSetting`, `UpsertWorkspaceSetting`, `ListWorkspaceSettingKeys`.
- [ ] **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): secret envelope, vault transit and credential resolver"`

### Task 3: Embedded registries from the Python source

**Files:**
- Create: `legacy/python/scripts/export_settings_registry.py`, `internal/settings/registry.go`, `internal/settings/registry.json`, `internal/settings/registry_test.go`

**Interfaces:**
- Exporter writes `{"providers": {id: {name, env_key, env_url, env_model, default_url, default_model, docs, free_tier, icon, openai_compatible}}, "enrichment_providers": {id: {name, env_key, capability, free_tier, docs, icon}}, "integrations": [{id, name, icon, description, fields:[{key,label,secret,placeholder}]}], "enrichment_defaults": {...}, "verifier_global_only": [...], "daily_limits": {...}}` from `PROVIDERS`, `ENRICHMENT_PROVIDERS`, `INTEGRATIONS`, `_ENRICHMENT_DEFAULTS`, `VERIFIER_GLOBAL_ONLY_KEYS` and the `DAILY_LIMITS` literal in `get_llm_usage` (`routers/settings.py:735-744`), preserving dict order (JSON object order matters for response order: decode with an ordered decoder).
- Produces: `//go:embed registry.json`; `type Registry struct{ Providers *OrderedMap[Provider]; EnrichmentProviders *OrderedMap[EnrichmentProvider]; Integrations []Integration; ... }`; `func Load() (*Registry, error)`; `func (r *Registry) Provider(id string) (Provider, bool)`.

- [ ] **Step 1: Write failing tests**: `TestRegistryProviderOrderPreserved` (first provider id equals Python's first key, from fixture), `TestRegistryEveryProviderHasEnvKey`, `TestRegistryIntegrationFieldKeysUnique`, `TestRegistryMatchesRecordedProvidersFixture` (builds the list response from the registry and compares to `testdata/contract/settings/providers_list.json` ignoring configured state).
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** exporter and loader. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): embed settings registries exported from python"`

### Task 4: Global settings store and LLM provider routes

**Files:**
- Create: `internal/settings/{appsettings,providers}.go`, `internal/api/settingsroutes.go` (providers section), tests

**Interfaces:**
- `type App struct{ Store secrets.Store; Getenv func(string) string }`; `func (a *App) Get(ctx, key, def string) string` (table value non-empty, else env, else `def`; DB errors are swallowed to env fallback, as `_db_get`); `func (a *App) Set(ctx, key, value string) error`.
- Routes (all behind `Authenticate`; `PUT`/`POST test` also `RequireGlobalAdmin`), shapes from `routers/settings.py:375-470`:
  - `GET /api/settings/providers` -> `{"providers":[{id,name,icon,configured,api_key_masked,base_url,model,default_model,docs_url,free_tier,is_default,openai_compatible}],"default_provider"}`; default provider key `LLM_DEFAULT_PROVIDER` default `openrouter`.
  - `PUT /api/settings/providers/{provider_id}` body `{api_key?, base_url?, model?, set_default?}` (unknown -> 404 `Provider '<id>' not found`; only provided fields written; `base_url`/`model` ignored when the provider has no `env_url`/`env_model`) -> `{"status":"ok","provider":id}`. Keys stored as plaintext in `app_settings` exactly as Python does today (see Self-Review decision 1).
  - `POST /api/settings/providers/{provider_id}/test` -> unconfigured key `{"status":"error","error":"API key not configured"}` (HTTP 200); else one 20 s request with the same body per `openai_compatible` (read `routers/settings.py:454-557` for the Anthropic vs OpenAI-compatible request/response mapping and the Cerebras `max_completion_tokens` special case) returning the same `{status, ...}` keys; outbound client = egress guard client (`internal/egress`) so a tenant-controlled `base_url` cannot reach private ranges.

- [ ] **Step 1: Write failing tests**: `TestProvidersListMatchesFixture`, `TestProvidersMaskShortAndLongKeys` (table: `""`, `"abc"` -> `••••••••`, 20-char key), `TestUpdateUnknownProvider404Detail`, `TestUpdatePartialFields`, `TestUpdateRequiresGlobalAdmin`, `TestSetDefaultWritesKey`, `TestTestProviderUnconfigured200`, `TestTestProviderBlocksPrivateBaseURL` (base_url `http://127.0.0.1:1`), `TestTestProviderAnthropicShape` (httptest server), `TestEmptyStoredValueFallsToEnv`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): settings app store and llm provider routes"`

### Task 5: Enrichment-perf, system, models, enrichment-providers, llm-usage

**Files:**
- Create: `internal/settings/{perf,system,models,llmusage}.go`, extend `settingsroutes.go`, `internal/db/queries/llm_usage.sql`, tests

**Interfaces:**
- `GET /api/settings/enrichment-perf` -> `{row_concurrency, provider_workers, provider_timeout, max_providers, retry_passes, "defaults": {...}}` with defaults `12, 8, 10, 8, 1` read from `ENRICH_<KEY>` via `App.Get` (non-integer stored value -> default). `PUT` (admin) clamps with bounds `row_concurrency 1..64, provider_workers 1..64, provider_timeout 3..120, max_providers 0..20, retry_passes 0..5`, writes `ENRICH_<KEY>` as decimal strings, returns the same body as GET.
- `GET /api/settings/system` -> `{debug (env DEBUG == "true"), api_host (API_HOST or "0.0.0.0"), api_port (int API_PORT or 8000), cors_origins (CORS_ORIGINS or "*"), default_provider, google_search_configured (GOOGLE_API_KEY non-empty), linkedin_configured (LINKEDIN_LI_AT_COOKIE non-empty)}`; env, not `app_settings`, for those keys (parity).
- `GET /api/settings/models/{provider_id}?free_only=bool`: static lists for `anthropic` and `google_ai` and the other non-OpenRouter providers copied from `routers/settings.py:594-710` into the registry export (extend Task 3's exporter with a `models` map); `openrouter` fetches `https://openrouter.ai/api/v1/models` through the egress client with the same filtering as `_fetch_openrouter_models` (676-712); unknown provider 404 `Provider '<id>' not found`.
- `GET /api/settings/enrichment-providers` -> `{"providers":[{id,name,icon,capability,configured,api_key_masked,free_tier,docs_url}]}`; `PUT /api/settings/enrichment-providers/{provider_id}` (admin) -> 404 `Enrichment provider '<id>' not found` or `{"status":"ok","provider":id}`.
- `GET /api/settings/llm-usage?date=` behind `Workspace`: reads `llm_usage_daily` for the workspace (`date` defaults to today UTC `YYYY-MM-DD`); response keys and the `DAILY_LIMITS` fallback logic exactly as `routers/settings.py:713-790` (`limit_label` `"<n> RPD (live)"` when the provider header limit > 0, `pct = min(100, round(calls/limit*100))` using Python banker's rounding, `remaining = max(0, limit - calls)`); all-time totals sum all rows. Note: the PG lead store returns an empty usage list today (`"does not yet persist LLM usage"`); Go reads the table, so a populated table is a deliberate improvement recorded in the fixture README.

- [ ] **Step 1: Write failing tests**: `TestPerfDefaultsAndClamp` (table: `0`->min, `999`->max, garbage stored -> default), `TestPerfPutIgnoresNull`, `TestSystemReadsEnvNotSettings`, `TestModelsStaticListsMatchFixture`, `TestModelsOpenRouterFreeOnlyFilter` (httptest), `TestModelsUnknown404`, `TestEnrichmentProviderMaskAndUpdate`, `TestLLMUsageLimitFallbackAndLive`, `TestLLMUsagePctBankersRounding` (calls 1 of 8 -> 12, 1 of 24 -> 4), `TestLLMUsageTenantIsolation` (FORCE RLS), `TestLLMUsageEmptyShape`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): enrichment perf, system, models and llm usage settings routes"`

### Task 6: Global and per-workspace integrations

**Files:**
- Create: `internal/settings/integrations.go`, extend `settingsroutes.go`, tests

**Interfaces:**
- `GET /api/settings/integrations` (active user) -> `{"integrations":[{id,name,icon,description,connected,fields:[{key,label,secret,placeholder,set,value,masked}]}]}`; non-secret fields echo `value`, secrets `""`; `masked` = `…` + last 4 chars (secret, len >= 4) else `"set"`/`""`; `connected` = all fields set. `PUT /api/settings/integrations/{integration_id}` (global admin) body `{"values": {key: value}}`, unknown id 404 `Integration not found`, only keys in the integration's field list and non-blank values written (trimmed), returns `{"status":"ok","saved":[keys],"integration":view}`.
- `GET /api/settings/workspace-integrations?workspace_id=` and `PUT /api/settings/workspace-integrations/{integration_id}?workspace_id=` per `routers/settings.py:1179-1262`: `resolveActiveWorkspace` (explicit or active; none -> 400 `No active workspace`; not a member -> 403 `Not a member of this workspace`), PUT additionally requires permission `integrations.manage` with default roles `admin, editor` (403 `Insufficient workspace role`); field `source` is `workspace|global|unset`; `value` always `""` and `masked` `"set"`/`""`; PUT stores via `Resolver.Set` (encrypted) and returns `{"status":"ok","saved":[...],"workspace_id","integration":view}`. Keys in `GlobalOnly` are never written at workspace level even if listed in a field list.
- These routes do NOT use the `X-Workspace-Id` middleware (they take the query param and replicate Python's explicit check), so they use `identity.IsMember`/`HasPermission` from plan 02 directly.

- [ ] **Step 1: Write failing tests**: `TestIntegrationViewNeverEchoesSecrets`, `TestIntegrationPutIgnoresBlankAndForeignKeys`, `TestIntegrationPutUnknown404`, `TestIntegrationPutRequiresGlobalAdmin`, `TestWorkspaceIntegrationSourcesWorkspaceGlobalUnset`, `TestWorkspaceIntegrationPutEncryptsAtRest` (raw row starts `enc:v1:`), `TestWorkspaceIntegrationViewerForbidden`, `TestWorkspaceIntegrationOtherWorkspace403`, `TestWorkspaceIntegrationNoActiveWorkspace400`, `TestWorkspaceIntegrationPermissionDenyOverride`, `TestPythonReadsGoWrittenSecret` (marker-gated vector check), `TestReacherURLNotWorkspaceSettable`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement**, and run the Python grep gate `rg -n 'environ(\.get)?\(?\[?"(ANTHROPIC|OPENROUTER|GROQ|HUNTER|APOLLO)[A-Z_]*_API_KEY' legacy/python/apps/api | grep -v routers/settings.py`: each hit is reported in the PR description and converted to `_db_get` in the Python tree. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): global and workspace integration routes with encrypted secrets"`

### Task 7: Worker-local credential resolution

**Files:**
- Create: `internal/credentials/local.go`, `local_test.go`; Modify: `cmd/worker/main.go` (credential source wiring near line 214), `internal/config/config.go` (+test)

**Interfaces:**
- Config: `CREDENTIALS_SOURCE` = `http` (default, current behaviour) | `local`; `local` requires `DATABASE_URL`, `SECRET_KEY`, `SECRETS_PROVIDER` (+ Vault vars) and builds `secrets.Resolver`.
- `type Local struct{...}`; `func NewLocal(r *secrets.Resolver, allow func(envVar string) bool, now func() time.Time) *Local`; `func (l *Local) Get(ctx context.Context, workspaceID, envVar string) (string, error)` satisfying `workbook.CredentialSource`. Semantics identical to `credentials.Client`: empty workspace or env var -> error; not allowed -> `("", ErrUnknownCredential)`; empty value is `("", nil)` and never cached; non-empty cached 60 s per `(workspace, envVar)`; errors never contain values; `allow` is built from the `auth.env_var` set of the manifests the worker already loaded.

- [ ] **Step 1: Write failing tests**: `TestLocalMatchesHTTPClientSemantics` (table run against both implementations using an `httptest` fake of the Python endpoint), `TestLocalRejectsEnvVarNotInManifests`, `TestLocalWorkspaceSecretBeatsGlobal`, `TestLocalCachesNonEmptyOnly`, `TestLocalErrorsHaveNoSecret`, `TestLocalIsolatesWorkspaces` (same env var, two workspaces, different values), `TestConfigCredentialsSourceValidation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): worker resolves credentials locally behind CREDENTIALS_SOURCE"`

### Task 8: Billing routes and Stripe

**Files:**
- Create: `internal/billing/{stripe,ledger}.go`, `internal/api/billingroutes.go`, `internal/db/queries/billing.sql`, tests; fixtures `testdata/contract/billing/*.json`

**Interfaces:**
- Config: `BILLING_ENABLED` (bool), `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_API_BASE` (default `https://api.stripe.com`, overridable only when `APP_ENV` is `test`).
- `func VerifyStripeSignature(payload []byte, header, secret string, now time.Time, tolerance time.Duration) error`: parse `t=`/`v1=` pairs (multiple `v1` allowed, any may match), reject missing/non-numeric `t`, `|now-t| > tolerance` (300 s), no `v1`, or HMAC mismatch (`hmac.Equal`); `ErrBadSignature`.
- `type Stripe struct{...}`; `func (s *Stripe) CreateCheckoutSession(ctx, amountUSD float64, workspaceID, successURL, cancelURL string) (id, url string, err error)`: `POST /v1/checkout/sessions` form body `mode=payment`, `line_items[0][price_data][currency]=usd`, `[unit_amount]=int(round-half-even(amount*100))`, `[product_data][name]=OpenGTM enrichment credits`, `line_items[0][quantity]=1`, `metadata[workspace_id]`, `metadata[credit_usd]=canonjson.FloatRepr(amount)` (Python `str(float)`, so `25` is `"25.0"`), `success_url`, `cancel_url`; `Authorization: Bearer <secret>`; `Idempotency-Key: <uuid>`; 15 s timeout.
- Ledger: `func Credit(ctx, tx pgx.Tx, workspaceID string, amountUSD float64, reason, idemKey string) (DebitResult, error)` with `DebitResult{ChargedUSD, BalanceUSD float64; IdempotentReplay bool}`; behaviour of `billing/service.py:130-174`: amount <= 0 error; existing `idempotency_key` -> replay with `ChargedUSD 0`; `SELECT ... FOR UPDATE` on `workspace_credits` (created at 0.0 if absent); balance `round(bal + amount, 6)`, returned `round(x, 4)`; unique-violation on the key -> replay.
- Routes: `GET /api/billing/balance` (Workspace middleware) -> `{billing_enabled, workspace_id, balance_usd, entries:[{id,workspace_id,amount_usd (4 dp),reason,run_id,created_at}]}` (50 newest by id desc); `POST /api/billing/checkout` body `{amount_usd > 0 (422 otherwise), success_url="https://app.yupcha.com/billing?status=success", cancel_url="https://app.yupcha.com/billing?status=cancel"}`: disabled -> 400 `Billing is disabled on this deployment.`, missing key -> 503 `Stripe is not configured: set STRIPE_SECRET_KEY (and STRIPE_WEBHOOK_SECRET) in the environment.`, success `{"session_id","url"}`; `POST /api/billing/webhook` (no auth): disabled -> `{"status":"ignored","reason":"billing_disabled"}`; no key -> 503 same text; bad signature/parse -> 400 `Invalid webhook signature`; other event types -> `{"status":"ignored","type":T}`; `payment_status` not in (absent, `paid`) -> `{"status":"ignored","reason":"unpaid"}`; missing `metadata.workspace_id`, `credit_usd <= 0` or session id -> 400 `Malformed checkout session`; else `InWorkspace(metadata.workspace_id)` credit with reason `stripe_topup`, key `stripe:<session_id>` -> `{"status":"ok","credited_usd","balance_usd","idempotent_replay"}`.

- [ ] **Step 1: Write failing tests**: `TestSignatureAcceptsStripeDocVector` (fixed secret/payload/timestamp from Stripe docs), `TestSignatureRejectsTamperedPayload`, `TestSignatureRejectsOldTimestamp`, `TestSignatureAcceptsSecondV1`, `TestSignatureRejectsMissingV1`, `TestCheckoutFormEncoding` (httptest asserts every field incl. `credit_usd=25.0` and `unit_amount=2500`, and half-even: `0.125` -> `12`), `TestCreditIdempotentReplay`, `TestCreditConcurrentSameKeyOnlyOnce` (16 goroutines, one ledger row), `TestWebhookCreditsMetadataWorkspaceOnly` (event for A with header workspace B -> credits A only), `TestWebhookDisabledAndUnpaidAndMalformed`, `TestWebhookUsesRawBody` (JSON with unusual whitespace still verifies), `TestBalanceMatchesFixture`, `TestBalanceTenantIsolation` (FORCE RLS), `TestCheckoutAmountZero422`, `TestCheckoutBillingDisabled400`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): billing routes and stripe signature verification"`

### Task 9: Contract fixtures and routing

**Files:**
- Create: `testdata/contract/settings/*.json`, `testdata/contract/billing/*.json` (recorded with plan 02 Task 1's `contract_record.py`, new case files `cases/settings.yaml`, `cases/billing.yaml`), `internal/api/settings_contract_test.go`, `docs/go-port/runbooks/settings-cutover.md`; Modify: `infra/nginx/go-routes.conf`

**Interfaces:**
- Consumes: `contract.Load`, `contract.AssertEqual`, `IgnorePaths` (plan 02).
- nginx additions (same `$auth_upstream` canary map): `^~ /api/settings/providers`, `^~ /api/settings/enrichment-perf`, `= /api/settings/system`, `^~ /api/settings/models/`, `^~ /api/settings/enrichment-providers`, `= /api/settings/llm-usage`, `^~ /api/settings/integrations`, `^~ /api/settings/workspace-integrations`, `^~ /api/billing/`. `/api/settings/sources*` and everything else stay on `api_backend`. Because `/api/` is a prefix location and these are `^~` (longer), nginx picks them first.
- Runbook content: ordering (deploy Go with `CREDENTIALS_SOURCE=http`, canary settings routes, then flip `CREDENTIALS_SOURCE=local` on one worker), the Stripe dashboard step (add a second webhook endpoint pointing at the Go route during the canary, then disable the old one, never both enabled with `BILLING_ENABLED` splitting traffic because idempotency makes duplicates safe but doubles Stripe retries), rollback (percentage to 0, `CREDENTIALS_SOURCE=http`; secrets written by Go decrypt in Python, no data rollback), and the `SECRETS_MASTER_KEY` change freeze during the canary.

- [ ] **Step 1: Write failing tests** `TestSettingsRoutesMatchRecordedFixtures` (table over every fixture file: replays `request` against the Go handler with seeded DB, `AssertEqual` with `IgnorePaths("$.created_at")`), `TestBillingRoutesMatchRecordedFixtures`, `TestSourcesRoutesNotRegistered` (the 5 retained routes return 404 from Go so nginx misconfig is visible), `TestEveryMigratedRouteHasAFixture` (route table minus the retained list equals fixture set).
- [ ] **Step 2: Run** `make test-integration` -> FAIL (no fixtures).
- [ ] **Step 3: Record** fixtures against FastAPI with the recorder, write the nginx include and the runbook.
- [ ] **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` (plan 02) at 0% and 100% for the new prefixes -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "test(go): settings and billing contract fixtures, nginx routes, cutover runbook"`

---

## Self-Review

- **Spec coverage:** settings router (Tasks 4-6, 9; 14 of 19 routes, 5 `sources*` deliberately retained), Fernet byte-compat with decrypt-existing mandatory (1, 2), Vault Transit (2), LLM key storage (4), billing + Stripe webhook signature parity (8), worker credential path (7), side-by-side routing and rollback (9).
- **Consistency:** `secrets.Resolver`, `Envelope`, `App`, `Registry`, `VerifyStripeSignature`, `Credit`, `DebitResult` are each defined once and referenced by name later.
- **Decisions for the user:** (1) Provider API keys in `app_settings` are plaintext today; this plan preserves that for parity and for the Python worker. Encrypting them (`enc:v1:` in `app_settings`) is a one-line follow-up in `App.Set/Get` but needs the Python `_db_get` changed in lockstep: choose now or later. (2) The 5 `sources*` routes stay on Python until the leadgen registry is ported (plan 13). (3) Stripe is hand-rolled (signature check + one REST call) instead of `stripe-go`; confirm you accept pinning to the account's default API version.
- **Proportion:** interfaces, vectors and test names are specified; bodies are left to the implementer.

## User decisions (2026-10-26) — override task text where they conflict

- Accepted defaults: `/auth/sso/*` stays on FastAPI; `/api/settings/sources*` stays on Python until leadgen is ported; workspace slug collision returns 409; Go auth requires token `exp`; `workbook_functions` and `chat_memories` stay global (no RLS).
- LLM provider keys are **encrypted** with the same envelope encryption as workspace secrets. Python's `_db_get`/writer change in lockstep, with a one-time migration that encrypts existing plaintext rows. Plan 03 tasks must be revised for this before execution.
- Stripe uses the official **stripe-go** SDK (pinned exact version) for webhook verification and Checkout. Do not hand-roll the signature code.
- `/admin/` and `/scim/` stay **unexposed** in nginx, as today.
