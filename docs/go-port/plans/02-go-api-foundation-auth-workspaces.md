# Go API Foundation, Auth and Workspaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `cmd/api`, a Go HTTP server that serves `/auth/{token,refresh,me}`, `/admin/users`, `/api/workspaces`, `/api/mcp/tokens` and `/scim/v2/{slug}` with byte-compatible JSON, status codes and auth semantics, side by side with FastAPI behind nginx, with a one-line rollback.

**Architecture:** Stdlib `net/http` `ServeMux` (Go 1.22+ method/wildcard patterns, `Request.Pattern` from 1.23 for audit routes) wrapped by a thin `httpx.Router` that restores FastAPI error shapes (`{"detail": ...}`, 404/405/422). Middleware chain: request-id, CORS, gzip, audit, then per-route auth/workspace/role/rate-limit decorators. Identity tables (no RLS) are read through a plain pool; every tenant table goes through `tenancy.InWorkspace`. Both stacks share PostgreSQL (plan 01 done) and `SECRET_KEY`, so tokens and rows are interchangeable and nginx can move prefixes in either direction.

**Tech Stack:** Go 1.27.3, `net/http`, pgx v5 + sqlc, `golang.org/x/crypto/bcrypt`, `github.com/golang-jwt/jwt/v5` (the only new dependencies; pin exact versions), `log/slog`; Python/FastAPI/uv for the fixture recorder; nginx.

**Spec:** `docs/go-port/inventory.md` sections 2 (HTTP surface), 8 (plan 1 "Auth + workspaces"); `docs/go-port/plans/01-sqlite-to-postgres.md` (tables this plan reads); GitHub issue #33.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; every Python path here means `legacy/python/<path>`. Python edits are not versioned (only the recorder in Task 1 and nothing else is touched). Go paths are relative to `apps/backend-go/`.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pin every module; DB access only through pgx v5 + sqlc (committed generated code); `make sqlc-check` clean.
- Runtime DB role non-superuser `NOBYPASSRLS`. Tenant tables only inside `tenancy.InWorkspace(ctx, pool, workspaceID, fn)`; missing workspace id fails closed. Identity-plane tables (`workspaces`, `workspace_members`, `user_active_workspace`, `workspace_oidc_identities`, `workspace_scim_tokens`, `users`) use the plain pool. Go never runs DDL.
- No CI changes. Verify with `cd apps/backend-go && make vet test` and `make test-integration` (needs `TEST_DATABASE_URL`).
- Contract parity is the release bar: same paths, methods, JSON keys/shapes, status codes, `WWW-Authenticate` headers, `X-Workspace-Id` semantics and error `detail` strings as FastAPI, modulo generated ids/timestamps/tokens. Naive UTC datetimes serialise as pydantic does: `2026-10-26T10:00:00.123456`, no `Z`, fractional part omitted when zero.
- JWT: HS256 only, key `SECRET_KEY`, claims `sub`, `exp`, `type` (`access`|`refresh`), `amr` (array). Access TTL `ACCESS_TOKEN_EXPIRE_MINUTES=30`, refresh TTL `REFRESH_TOKEN_EXPIRE_MINUTES=60*24*14`. Refuse to start when `SECRET_KEY` equals the insecure default `INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY` and `APP_ENV` is not a dev value (copy the dev set from `core/config.py:22-30`).
- Secrets (tokens, passwords, SCIM bearer, MCP plaintext) never appear in logs, error strings or audit metadata (reuse the Python redaction regex `token|secret|password|authorization|cookie|credential`).
- Every commit DCO signed (`git commit -s`) as Palash Debnath. Sonnet subagents execute; keep diffs per task small.
- Out of scope, stays on FastAPI: `/auth/sso/*` (OIDC authorization-code flow needs the Starlette session cookie and authlib), SCIM token rotation (lives in the governance router), `/health`, every other router. Go only READS the OIDC config to enforce `enforce_sso`.

## Review Focus

1. **Refresh token accepted as access token.** Python rejects only `type == "refresh"` (or missing `sub`) in `get_access_token_claims`, so a token with no `type` is accepted. A stricter Go check breaks tokens minted by older code; a looser one lets a 14-day refresh token call APIs. Pinned in Task 3 with vectors for refresh, access, no-type, no-sub, wrong alg (`none`, `HS512`), expired, tampered signature.
2. **Inactive user answers 400, not 401.** `get_current_active_user` returns `400 {"detail":"Inactive user"}`; a unified "401 for everything" middleware changes frontend behaviour. Task 4.
3. **`X-Workspace-Id` for a workspace the caller does not belong to.** Must return the same 403 `Workspace access denied` whether the id is missing or foreign (no existence leak), must never fall through to the active workspace, and must not set the RLS GUC before membership is proven. Task 5.
4. **bcrypt edge cases.** Hashes made by Python `bcrypt` (`$2b$`), passwords over 72 bytes (Python truncates or raises depending on version, Go errors), non-ASCII passwords, empty password. A login that returns 500 instead of 401 is a contract break. Task 3.
5. **SCIM semantics drift.** `userName` is lowercased on create, `DELETE` deactivates (does not remove the mapping), deactivation removes membership, `active=false` PATCH on a non-member is a no-op, group member ids must be numeric (400), filter only `userName eq "x"` (400 otherwise), `Content-Type: application/scim+json`. Tasks 13-14 pin each with a fixture.

---

## File Structure

```
cmd/api/main.go                       # wiring, graceful shutdown, schema gate
internal/config/config.go             # (modify) API_ADDR, SECRET_KEY, ALGORITHM, token TTLs, CORS_ORIGINS, APP_ENV
internal/httpx/{router,errors,json,cors,gzip,requestid}.go
internal/auth/{jwt,password}.go
internal/identity/{store,principal,workspace_ctx,roles}.go   # sqlc-backed reads/writes
internal/api/{authroutes,usersroutes,workspacesroutes,mcptokensroutes,scimroutes}.go
internal/ratelimit/ratelimit.go
internal/audit/audit.go
internal/contract/{compare,recorder_test_util}.go             # fixture compare helper (reused by plan 03)
internal/db/queries/{identity,mcp_tokens,scim,audit}.sql      # sqlc inputs (+ gen/)
testdata/contract/<route>/<case>.json                         # recorded FastAPI responses
Dockerfile.api
../../legacy/python/scripts/contract_record.py                # fixture recorder (unversioned)
../../infra/nginx/go-routes.conf, default.conf                # routing split (modify)
../../docs/go-port/runbooks/api-cutover.md
```

---

### Task 1: Contract-parity harness

**Files:**
- Create: `legacy/python/scripts/contract_record.py`, `internal/contract/compare.go`, `internal/contract/compare_test.go`, `testdata/contract/README.md`

**Interfaces:**
- Produces (Python): `python scripts/contract_record.py --base http://localhost:8000 --out ../../apps/backend-go/testdata/contract --cases cases/auth.yaml` runs a scripted case list against FastAPI seeded with fixed users/workspaces and writes `{name}.json` = `{"request": {method, path, headers (Authorization/X-Workspace-Id replaced by "<redacted>"), body}, "response": {status, headers (allow-list: content-type, www-authenticate, allow), body}}`.
- Produces (Go): `package contract`; `type Fixture struct{ Request Req; Response Resp }`; `func Load(t testing.TB, name string) Fixture`; `func AssertEqual(t testing.TB, got Resp, want Resp, opts ...Option)`; `func IgnorePaths(paths ...string) Option` (JSON-path list such as `$.id`, `$.access_token`, `$.created_at`); `func IgnoreShapeOf(paths ...string) Option` (value must have same JSON type, e.g. token strings, timestamps matching the naive-ISO regexp). Comparison is structural (key order and whitespace ignored), status and allow-listed headers exact.

- [ ] **Step 1: Write failing tests** in `compare_test.go`: `TestAssertEqualIgnoresKeyOrder`, `TestAssertEqualFailsOnStatus`, `TestIgnorePathsSkipsGeneratedId`, `TestIgnoreShapeOfRejectsTypeChange` (string vs number), `TestHeaderAllowListOnly`.
- [ ] **Step 2: Run** `go test ./internal/contract/...` -> FAIL.
- [ ] **Step 3: Implement** `compare.go` using `encoding/json` decode to `any` and a recursive diff that reports the first differing JSON path; write the recorder with `httpx`-free stdlib `urllib` (no new Python deps).
- [ ] **Step 4: Run** `go test ./internal/contract/...` -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "test(go): contract fixture comparator for REST parity"`

### Task 2: HTTP router, errors, request id, CORS, gzip

**Files:**
- Create: `internal/httpx/{router,errors,json,cors,gzip,requestid}.go` + tests

**Interfaces:**
- Produces: `type Router struct{ ... }`, `func NewRouter() *Router`, `func (r *Router) Handle(pattern string, h http.Handler, mws ...Middleware)` where `pattern` is `"METHOD /path/{param}"`; `type Middleware func(http.Handler) http.Handler`; `func (r *Router) Use(mws ...Middleware)`; `func (r *Router) Handler() http.Handler`.
- `func WriteJSON(w http.ResponseWriter, status int, v any)`; `func Detail(w http.ResponseWriter, status int, detail string, hdr ...Header)` writes `{"detail": detail}`; `func ValidationError(w http.ResponseWriter, errs ...FieldError)` writes 422 `{"detail":[{"type":"missing","loc":["body","username"],"msg":"Field required","input":null}]}` (`FieldError{Type, Loc []any, Msg string, Input any}`); unknown path -> 404 `{"detail":"Not Found"}`; wrong method -> 405 `{"detail":"Method Not Allowed"}` plus `Allow`.
- Middleware: `RequestID()` (honours `X-Request-Id` matching `^[A-Za-z0-9._:-]{1,64}$`, else uuid4; echoes the header; stores in context `httpx.RequestIDFrom(ctx)`), `CORS(origins string)` (default `*`, `allow_credentials=false`, all methods/headers, preflight 200 `OK`, `Access-Control-Max-Age: 600`), `Gzip(minSize int)` (1000 bytes, skips `text/event-stream` and responses that already set `Content-Encoding`).

- [ ] **Step 1: Write failing tests**: `TestUnknownPathIs404Detail`, `TestWrongMethodIs405WithAllow`, `TestPatternWildcardParams` (`r.PathValue("ws_id")`), `TestRequestIDHonoursSafeHeaderRejectsUnsafe` (header `a b` -> regenerated), `TestCORSPreflight`, `TestCORSSpecificOriginList`, `TestGzipSkipsSSEAndSmallBodies`, `TestValidationErrorShape`.
- [ ] **Step 2: Run** `go test ./internal/httpx/...` -> FAIL.
- [ ] **Step 3: Implement** with `http.ServeMux`; 404/405 produced by registering `"/"` catch-all and inspecting which methods match.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): httpx router with FastAPI-compatible errors, CORS, gzip, request id"`

### Task 3: JWT and password parity

**Files:**
- Create: `internal/auth/{jwt,password}.go`, `internal/auth/auth_test.go`, `testdata/auth/python_vectors.json` (generated by a one-off Python snippet using `apps.api.auth`)

**Interfaces:**
- Produces: `type Claims struct{ Sub string; Type string; AMR []string; ExpiresAt time.Time }`; `type Signer struct{ Secret []byte; AccessTTL, RefreshTTL time.Duration; Now func() time.Time }`; `func (s Signer) Access(sub string, amr []string) (string, error)`; `func (s Signer) Refresh(sub string, amr []string) (string, error)`; `func (s Signer) ParseAccess(tok string) (Claims, error)` (rejects `type=="refresh"` or empty `sub`; HS256 only; requires `exp`); `func (s Signer) ParseAny(tok string) (Claims, error)` (used by `/auth/refresh`; caller checks `Type=="refresh"`); `var ErrInvalidToken`.
- `func HashPassword(pw string) (string, error)` (cost 12, matches `bcrypt.gensalt()` default); `func VerifyPassword(pw, hash string) bool` (never panics; a password over 72 bytes is compared on its first 72 bytes like older Python bcrypt, documented); `func DummyVerify()` (burns one bcrypt compare for unknown users).

- [ ] **Step 1: Write failing tests**: `TestParsesPythonMintedAccessToken` (vector file), `TestParsesPythonMintedRefreshForRefreshOnly`, `TestParseAccessRejectsRefreshType`, `TestParseAccessRejectsMissingSub`, `TestParseAccessAcceptsMissingType`, `TestRejectsAlgNoneAndHS512`, `TestRejectsExpired`, `TestRejectsTamperedSignature`, `TestMintedTokenParsesInPythonVector` (decode with a hand-written HS256 check in the test against the same secret), `TestVerifiesPythonBcryptHash` (`$2b$12$...` vector), `TestVerifyPasswordLongAndNonASCII`, `TestVerifyPasswordEmptyIsFalse`, `TestHashPasswordCost12`.
- [ ] **Step 2: Run** `go test ./internal/auth/...` -> FAIL.
- [ ] **Step 3: Implement** with `jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())`; `exp` encoded as an integer (python-jose emits an int).
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): JWT and bcrypt parity with python-jose and bcrypt"`

### Task 4: Identity store and authentication middleware

**Files:**
- Create: `internal/db/queries/identity.sql`, `internal/identity/{store,principal}.go`, tests; regenerate `internal/db/gen/` via `make sqlc`
- Test: `internal/identity/principal_test.go` (integration tag for DB cases)

**Interfaces:**
- sqlc queries (identity pool): `UserByUsername`, `UserByID`, `ListUsers`, `InsertUser`, `UpdateUserPassword`, `UpdateUserRole`, `DeleteUser`, `TouchLastLogin` (sets `last_login = now() at time zone 'utc'`).
- Produces: `type User struct{ ID int32; Username string; IsActive, IsAdmin bool; Role string; ProfileImage *string; LastLogin, CreatedAt *time.Time }` with `MarshalJSON` producing the `UserResponse` shape (`id, username, is_active, is_admin, role, profile_image, last_login, created_at`; naive ISO datetimes; hashed password never serialised).
- `type Principal struct{ User User; Claims auth.Claims }`; middleware `Authenticate(signer auth.Signer, st *Store) httpx.Middleware` (401 `{"detail":"Could not validate credentials"}` + `WWW-Authenticate: Bearer`; unknown user 401 same; inactive 400 `Inactive user`), `RequireGlobalAdmin()` (403 `Not authorized` unless `is_admin` or role in `admin|superadmin`); `PrincipalFrom(ctx) (Principal, bool)`; `AuthenticateQueryToken(signer, st, token string, requireAdmin bool) (Principal, error)` for later SSE/WS plans (missing -> 401 `Missing token`, refresh/invalid -> 401 `Invalid token`, inactive -> 401 `Invalid token`).

- [ ] **Step 1: Write failing tests**: `TestMissingBearerIs401WithWWWAuthenticate`, `TestRefreshTokenOnAPIIs401`, `TestInactiveUserIs400`, `TestUnknownUserIs401`, `TestNonAdminIs403AdminGate`, `TestSuperadminRoleAllowed`, `TestUserJSONShapeMatchesFixture` (contract fixture from Task 1 `auth/me`), `TestQueryTokenRejectsRefreshAndInactive`.
- [ ] **Step 2: Run** `make test` -> FAIL.
- [ ] **Step 3: Implement** queries and middleware; `OAuth2PasswordBearer` returns 401 `Not authenticated` when the header is absent, so use that detail for a missing header (and `Could not validate credentials` only for bad tokens).
- [ ] **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): identity store and bearer authentication middleware"`

### Task 5: Workspace resolution, role gates, SSO enforcement

**Files:**
- Create: `internal/identity/{workspace_ctx,roles}.go`, queries appended to `internal/db/queries/identity.sql`, tests

**Interfaces:**
- sqlc: `WorkspaceByID`, `WorkspaceBySlug`, `MemberRole`, `IsMember` (member row OR `workspaces.owner_id`, same `UNION` as `manager.is_member`), `ActiveWorkspace`, `FirstAccessibleWorkspace`, `MemberPermissions`, `OIDCConfig` (reads `workspace_settings` key `OIDC_CONFIG` inside `InWorkspace`).
- Produces: `type WorkspaceCtx struct{ User User; WorkspaceID, Slug, Role string; Claims auth.Claims; Pool *pgxpool.Pool }` with `func (c WorkspaceCtx) Tx(ctx context.Context, fn func(pgx.Tx) error) error` (delegates to `tenancy.InWorkspace`); middleware `Workspace(st *Store) httpx.Middleware` (header `X-Workspace-Id` else active workspace; no workspace -> 403 `No accessible workspace. Ask an admin to add you to one.`; not a member -> 403 `Workspace access denied`; slug missing -> 404 `Workspace not found`; `enforce_sso` and role != `owner` and `"sso"` not in `amr` (string or list) -> 403 `This workspace requires SSO. Sign in with your organization identity.`); sets request-scoped `ActorUserID`, `ActorRole`, `WorkspaceID` for audit via `RequestMeta(ctx)`.
- `func RequireRole(permission string, roles ...string) httpx.Middleware` replicating `require_workspace_role`: owner passes; with `permission`: explicit allow/deny override wins, else `role in roles`, else 403 `Workspace permission denied: <permission>`; without permission: 403 `Insufficient workspace role`. Predefined `RequireAdmin = RequireRole("secrets.manage","admin")` (as `mcp_tokens.py:require_admin`).

- [ ] **Step 1: Write failing tests**: `TestHeaderWorkspaceNotMemberIs403SameAsMissing`, `TestNoGUCSetBeforeMembershipProven` (pool size 1, then unscoped query returns zero rows), `TestFallsBackToActiveThenFirstAccessible`, `TestOwnerImpliesAnyRole`, `TestPermissionDenyOverridesRole`, `TestPermissionAllowOverridesRole`, `TestViewerRejectedWithInsufficientRole`, `TestEnforceSSORejectsPasswordTokenAllowsOwner`, `TestAmrAsStringAccepted`, `TestOIDCConfigReadUnderRLS`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL.
- [ ] **Step 3: Implement** exactly the order in `core/tenancy.py:current_workspace` (header, membership, slug, SSO).
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): workspace resolution, role gates and SSO enforcement"`

### Task 6: Rate limiter parity

**Files:**
- Create: `internal/ratelimit/ratelimit.go`, `ratelimit_test.go`

**Interfaces:**
- Produces: `func Key(r *http.Request) string` returning `ws:<X-Workspace-Id>` if present, else `tok:<first 32 chars of bearer token>` if a non-empty bearer exists, else `ip:<remote host>` (first `X-Forwarded-For` entry is NOT trusted unless `TRUST_PROXY=true`); `type Limiter struct{...}`, `func New(now func() time.Time) *Limiter`; `func (l *Limiter) Limit(spec string) httpx.Middleware` where `spec` is `"<n>/minute"` (also `second|hour|day`), fixed window like slowapi/limits memory storage; on exceed respond 429 `{"error":"Rate limit exceeded: 30 per 1 minute"}`. No route in this plan uses a limit (the Python routes it replaces have none); plans 04+ apply `30/minute`, `20/minute`, `60/minute`, `120/minute` from `leads.py`/`workbooks.py`.

- [ ] **Step 1: Write failing tests**: `TestKeyPrecedenceWorkspaceThenTokenThenIP`, `TestFixedWindowResetsAtBoundary` (injected clock), `TestExceedReturns429BodyExact`, `TestTenantsDoNotShareBudget`, `TestSpecParseRejectsGarbage`, `TestConcurrentCallsNoRace` (`-race`).
- [ ] **Step 2: Run** `go test -race ./internal/ratelimit/...` -> FAIL.
- [ ] **Step 3: Implement** with a mutex-guarded map and lazy expiry sweep (bounded size 100k keys).
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): slowapi-compatible fixed-window rate limiter"`

### Task 7: Governance audit middleware

**Files:**
- Create: `internal/db/queries/audit.sql`, `internal/audit/audit.go`, `audit_test.go`

**Interfaces:**
- sqlc: `InsertGovernanceAudit` (columns of `governance_audit_events`, see `internal/db/schema.sql`), executed inside `InWorkspace`.
- Produces: `func Middleware(pool *pgxpool.Pool, log *slog.Logger) httpx.Middleware`; after the handler, if method in `POST|PUT|PATCH|DELETE` and the request meta has a workspace id, insert `{id uuid, workspace_id, actor_user_id, actor_role, method, route (pattern minus method, max 255), resource_path (max 500), response_status, outcome (success <400; denied for 401/403; error otherwise), request_id, metadata_json}`; metadata comes from `audit.SetMetadata(ctx, map[string]any)` and is sanitised by `SafeMetadata(v any, depth int) any` (depth cap 3, 50 keys, 100 list items, strings cut to 500, keys matching the redaction regexp dropped). A failed insert logs and never changes the response; a panic still records status 500.

- [ ] **Step 1: Write failing tests**: `TestWriteWithWorkspaceRecorded`, `TestGetNotRecorded`, `TestNoWorkspaceNotRecorded`, `TestRouteIsPatternNotConcretePath` (`/api/mcp/tokens/{token_id}`), `TestOutcomeMapping`, `TestSafeMetadataDropsSecretKeysAndBoundsDepth`, `TestInsertFailureDoesNotBreakResponse`, `TestPanicRecords500`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL.
- [ ] **Step 3: Implement** with a status-capturing `ResponseWriter` that preserves `http.Flusher`.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): governance audit middleware parity"`

### Task 8: `cmd/api` wiring, config, image

**Files:**
- Create: `cmd/api/main.go`, `Dockerfile.api`; Modify: `internal/config/config.go` (+ test)

**Interfaces:**
- Config additions (names exactly as Python): `API_ADDR` (default `:8000`), `SECRET_KEY`, `ALGORITHM` (only `HS256` accepted), `ACCESS_TOKEN_EXPIRE_MINUTES` (30), `REFRESH_TOKEN_EXPIRE_MINUTES` (20160), `CORS_ORIGINS` (default `*`), `APP_ENV`, `MCP_TOKEN_TTL_DAYS`, `OPENGTM_SCIM_TOKEN_TTL_DAYS` (90, clamp 1..365), `TRUST_PROXY` (false). `Config.String()` keeps redacting secrets.
- `main.go`: loads config, schema gate (refuse start below the Alembic revision from plan 01), builds router with `RequestID, CORS, Gzip, audit.Middleware`, registers routes from Tasks 9-14, serves `GET /healthz` (plain `{"status":"ok"}`, distinct from FastAPI `/health` which nginx keeps), graceful shutdown on SIGTERM (30 s).

- [ ] **Step 1: Write failing tests**: `TestConfigRejectsInsecureSecretInProd`, `TestConfigAcceptsInsecureSecretInDev`, `TestConfigRejectsNonHS256`, `TestScimTTLClamped`, `TestMainBuildsRouter` (`httptest` smoke: `GET /healthz` 200, unknown 404 detail).
- [ ] **Step 2: Run** `go test ./internal/config/... ./cmd/api/...` -> FAIL.
- [ ] **Step 3: Implement**; `Dockerfile.api` mirrors `Dockerfile.providersim` (static build, non-root, digest-pinned base at release).
- [ ] **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): cmd/api skeleton, config and image"`

### Task 9: `/auth/token`, `/auth/refresh`, `/auth/me`

**Files:**
- Create: `internal/api/authroutes.go`, `authroutes_test.go`

**Interfaces:**
- `POST /auth/token` (form `application/x-www-form-urlencoded`, fields `username`, `password`, optional `grant_type`, `scope`; missing field -> 422 per Task 2): unknown user or bad password -> 401 `Incorrect username or password` + `WWW-Authenticate: Bearer` (burn `DummyVerify` for unknown users); success mints access and refresh with `amr:["pwd"]`, updates `last_login`, returns `{"access_token","token_type":"bearer","refresh_token"}`.
- `POST /auth/refresh` JSON `{"refresh_token"}`: any parse failure, `type != "refresh"`, empty `sub`, unknown or inactive user -> 401 `Invalid refresh token` + `WWW-Authenticate: Bearer`; else new pair, `amr` copied from the old token (default `["pwd"]`).
- `GET /auth/me` -> `User` JSON (Task 4), behind `Authenticate`.

- [ ] **Step 1: Write failing contract tests**: `TestTokenSuccessMatchesFixture` (ignore token values via `IgnoreShapeOf`), `TestTokenBadPasswordMatchesFixture`, `TestTokenMissingFieldIs422Fixture`, `TestRefreshRoundTrip`, `TestRefreshRejectsAccessToken`, `TestRefreshRejectsInactiveUser`, `TestRefreshKeepsSSOAmr`, `TestMeMatchesFixture`, `TestTokenAcceptsPythonHash`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL.
- [ ] **Step 3: Implement** handlers; record the fixtures first with Task 1's recorder against FastAPI.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): auth token, refresh and me routes"`

### Task 10: `/admin/users`

**Files:**
- Create: `internal/api/usersroutes.go`, `usersroutes_test.go`

**Interfaces:**
- All behind `Authenticate` + `RequireGlobalAdmin`. `GET /admin/users` -> `[User]`; `POST /admin/users` `{username,password}` -> `User` (duplicate -> 400 `Username already registered`; created with `is_active=true, is_admin=false, role="user"`); `PUT /admin/users/{user_id}/password` `{password}` -> `{"message":"Password updated successfully"}` (404 `User not found`); `DELETE /admin/users/{user_id}` -> `{"message":"User deleted"}` (self -> 400 `Cannot delete your own account`; 404); `PATCH /admin/users/{user_id}/role` `{role}` in `superadmin|admin|editor|user|viewer` else 400 `Invalid role`; sets `is_admin = (role == "admin")` (note: `superadmin` leaves `is_admin=false`, as Python) -> `User`.
- Non-integer `user_id` -> 422 `{"detail":[{"type":"int_parsing","loc":["path","user_id"],...}]}`.

- [ ] **Step 1: Write failing tests**: `TestListUsersMatchesFixture`, `TestCreateDuplicateIs400`, `TestCreateHashesBcrypt`, `TestResetPassword404`, `TestDeleteSelfIs400`, `TestDeleteCascadesNothingElse` (membership rows untouched, parity), `TestRoleSuperadminKeepsIsAdminFalse`, `TestRoleInvalidIs400`, `TestPathIntParsing422`, `TestNonAdminIs403`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): admin users routes"`

### Task 11: `/api/workspaces`

**Files:**
- Create: `internal/api/workspacesroutes.go`, `workspacesroutes_test.go`; queries in `identity.sql`

**Interfaces:** (all behind `Authenticate`, no `X-Workspace-Id`; copy shapes from `routers/workspace_manager.py:29-121`)
- `GET /api/workspaces` -> `{"workspaces":[{id,name,slug,description,icon,owner_id,leads_count,is_active,created_at}],"active_id"}`; `created_at` is the epoch float column; `leads_count` = `SELECT count(*) FROM leads` inside `InWorkspace` per workspace.
- `POST /api/workspaces` `{name, description="", icon="🏢"}` -> `{id,name,slug}`; slug rule `lower`, spaces/underscores to `-`, keep alnum and `-`; the creator becomes `owner` member and active workspace; slug collision -> 500 in Python (`IntegrityError`), Go returns 409 `Workspace slug already exists` (documented deliberate deviation, recorded in `testdata/contract/README.md`).
- `POST /api/workspaces/switch` `{workspace_id}` -> 403 `Workspace access denied` if not a member else `{"active": name, "id": id}`.
- `GET /api/workspaces/dashboard` -> `{total_workspaces,total_leads,workspaces:[{id,name,slug,icon,leads_count,created_at}]}`.
- `GET /api/workspaces/{ws_id}` -> 404 `Workspace not found` for non-member or missing; `DELETE` -> 404 same; non-owner 403 `Only the owner can delete a workspace`; `main` 400 `Cannot delete default workspace`; success `{"status":"ok"}`; deletes children in the order of `manager.py:536-556` in one transaction.
- `/api/workspaces/dashboard` and `/switch` must win over `{ws_id}` (ServeMux specificity).

- [ ] **Step 1: Write failing tests**: `TestListMatchesFixtureEpochFloats`, `TestCreateSlugRule` (table: `"My Team_2!"` -> `my-team-2`), `TestCreateSetsOwnerAndActive`, `TestSwitchForeignWorkspace403`, `TestDashboardRouteNotCapturedByWildcard`, `TestGetNonMember404`, `TestDeleteNonOwner403`, `TestDeleteMainIs400`, `TestDeleteRemovesChildrenUnderRLS`, `TestLeadsCountUsesTenantScope`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): workspaces routes"`

### Task 12: `/api/mcp/tokens`

**Files:**
- Create: `internal/db/queries/mcp_tokens.sql`, `internal/api/mcptokensroutes.go`, tests

**Interfaces:**
- Behind `Authenticate`, `Workspace`, `RequireAdmin`. `POST` body `{name="" (max 200), capabilities=["leads:read"], ttl_days}`; capability vocabulary = `ALL_CAPABILITIES` from `services/mcp/auth.py:40-60` (`leads:read, leads:write, sequences:enroll, workbooks:write, automations:write, outreach:send, leads:delete, workbooks:delete`; copy the exact frozenset from the source when implementing); unknown -> 400 `unknown capabilities: ['x']` (Python list repr, sorted). Token = `ycp_` + 32 random bytes base64url (no padding), `token_hash` = SHA-256 hex, `prefix` = first 12 chars, `expires_at = now + ttl_days` (`ttl_days` default `MCP_TOKEN_TTL_DAYS`; `<= 0` or null means no expiry), capabilities de-duplicated in order, `created_by = user_id`. Response `TokenCreated` = `TokenOut` + `token`, where datetimes are `isoformat()` of aware UTC (`...+00:00`). `GET` lists newest first, `DELETE /{token_id}` revokes idempotently -> `{"ok":true,"id":id}`, cross-tenant id -> 404 `token not found`. All inside `InWorkspace`.

- [ ] **Step 1: Write failing tests**: `TestCreateReturnsPlaintextOnceAndStoresHash`, `TestCreateUnknownCapabilityIs400ListRepr`, `TestCreateDedupesCapabilities`, `TestTtlNullUsesDefaultZeroMeansNever`, `TestListNeverContainsPlaintext`, `TestRevokeIdempotent`, `TestRevokeOtherWorkspace404`, `TestEditorForbidden`, `TestPythonResolvesGoMintedToken` (integration with `legacy` resolver is run manually in Task 15; here a hash-vector test), `TestAuditRowWritten`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): mcp token routes"`

### Task 13: SCIM discovery and Users

**Files:**
- Create: `internal/db/queries/scim.sql`, `internal/api/scimroutes.go` (shared helpers), `internal/api/scimusers.go`, tests

**Interfaces:**
- Content type `application/scim+json` for every body. Auth `scimWorkspace(slug, authorization)`: unknown slug 404 `Workspace not found`; scheme not `bearer` or token check fails -> 401 `Invalid SCIM bearer token` + `WWW-Authenticate: Bearer`. Token check = unexpired (`expires_at`, else `created_at + ttl`) and `subtle.ConstantTimeCompare` of SHA-256 hex digests; `last_used_at` updated at most once per 60 s.
- Routes under `/scim/v2/{workspace_slug}`: `GET /ServiceProviderConfig`, `/ResourceTypes`, `/ResourceTypes/{resource_type}`, `/Schemas`, `/Schemas/{schema_id...}` (static JSON, copy from `routers/scim.py:86-137` into `internal/api/scimstatic.go`), `GET/POST /Users`, `GET/PUT/PATCH/DELETE /Users/{user_id}`.
- Semantics exactly per `routers/scim.py:140-230`: filter regexp `^\s*userName\s+eq\s+"([^"]+)"\s*$` (case-insensitive) else 400 `Only userName eq filtering is supported`; `startIndex>=1`, `0<=count<=200`; POST lowercases `userName`, creates user with a random 48-byte base64url bcrypt password, 409 `SCIM userName already exists` if mapped, adds `viewer` member when `active`; PATCH needs `PatchOp` schema (400 `PatchOp schema is required`), ops `add|replace` only, paths `active|displayName|externalId|userName`, `userName` change -> 409 `userName cannot be changed after provisioning`; `active=false` removes membership; DELETE -> 204 and deactivates the mapping; `meta.created/lastModified` are `datetime.fromtimestamp(x, UTC).isoformat()`.
- Reads and writes run inside `InWorkspace(workspace.ID)` after the token check.

- [ ] **Step 1: Write failing tests**: `TestBadSchemeOrExpiredTokenIs401`, `TestUnknownSlugIs404BeforeTokenCheck`, `TestListFilterUserNameCaseInsensitive`, `TestListFilterOtherAttributeIs400`, `TestListPaginationBounds`, `TestCreateLowercasesAndAddsViewer`, `TestCreateDuplicateIs409`, `TestPatchDeactivateRemovesMembership`, `TestPatchUnsupportedPathIs400`, `TestPutUserNameChangeIs409`, `TestDeleteDeactivatesKeepsMapping`, `TestContentTypeScim`, `TestUsersMatchFixtures`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): scim discovery and users routes"`

### Task 14: SCIM Groups

**Files:**
- Create: `internal/api/scimgroups.go`, tests

**Interfaces:**
- `GET/POST /Groups`, `GET/PUT/PATCH/DELETE /Groups/{group_id}` per `routers/scim.py:233-315` and `services/workspace/scim.py:130-326`: group `id` is a uuid string; `displayName` unique per workspace (conflict -> 409); members must be existing workspace members (`_validated_member_ids`); non-numeric member `value` -> 400 `Each group member requires a numeric value`; resource shape `{"schemas","id","externalId","displayName","members":[{"value","$ref"}],"meta":{...}}`; DELETE -> 204.

- [ ] **Step 1: Write failing tests**: `TestCreateGroupValidatesMembers`, `TestDuplicateDisplayName409`, `TestNonNumericMember400`, `TestPatchAddRemoveMembers`, `TestPutReplacesMembers`, `TestDeleteGroupCascadesMembers`, `TestGroupsMatchFixtures`, `TestGroupsIsolatedBetweenWorkspaces`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): scim groups routes"`

### Task 15: Side-by-side routing, canary and rollback

**Files:**
- Create: `infra/nginx/go-routes.conf`, `docs/go-port/runbooks/api-cutover.md`; Modify: `infra/nginx/default.conf` (add `upstream api_go { zone api_go 64k; server api-go:8000 resolve; }`, `include /etc/nginx/go-routes.conf;`); the compose `api-go` service is added by whoever owns `docker-compose.yml` using the snippet in the runbook (`image: opengtm-api-go:local`, env `API_ADDR=:8000`, `DATABASE_URL`, `SECRET_KEY`, same `depends_on` as `api`).
- Test: `scripts/nginx-routing-check.sh` (asserts `nginx -t` and curl matrix against a compose stack)

**Interfaces:**
- `go-routes.conf` defines `split_clients "${http_authorization}${remote_addr}" $go_bucket { 0% go; * py; }` (percentage edited by the operator: 0 -> 5 -> 25 -> 100) and a `map $go_bucket $auth_upstream { go api_go; default api_backend; }`; exact and prefix locations route to `$auth_upstream`: `location = /auth/token`, `= /auth/refresh`, `= /auth/me`, `^~ /api/workspaces`, `^~ /api/mcp/tokens`, `^~ /admin/users`, `^~ /scim/v2/`. Everything else (including `/auth/sso/`) keeps `api_backend`. Note: the current `default.conf` has no `/admin/` or `/scim/` location, so those two prefixes are reachable only on the API port today; this task adds them (Go and Python both) and records the change in the runbook.
- Rollback (documented, one line each): set the percentage to `0%` and `nginx -s reload` (instant, Python serves the same rows with the same `SECRET_KEY`); or remove the include. Data-level rollback is not needed because Go writes only rows Python already reads; the exception is rows Go creates with the Task 11 slug deviation, listed in the runbook.
- Canary gate: promote only when `contract` tests are green, error rate on Go routes <= Python baseline for 24 h, and a Python-minted token works on Go and vice versa (`scripts/cross_token_check.sh`).

- [ ] **Step 1: Write the failing check**: `scripts/nginx-routing-check.sh` expects `X-Upstream: go` on `/auth/me` at 100% and `py` at 0%, and that `/auth/sso/x` is always `py` (add `add_header X-Upstream $auth_upstream always;` in the new locations only).
- [ ] **Step 2: Run** it against the current config -> FAIL.
- [ ] **Step 3: Implement** the config, the runbook (canary ramp, dashboards to watch, rollback, the cross-token check) and `cross_token_check.sh`.
- [ ] **Step 4: Run** `docker compose exec nginx nginx -t` and the routing check at 0% and 100% -> PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(infra): nginx canary routing for the Go API and rollback runbook"`

---

## Self-Review

- **Spec coverage:** server skeleton and router choice (Tasks 2, 8), JWT/refresh rejection (3, 4), membership + `X-Workspace-Id` + roles (5), rate limiting parity (6), audit parity (7), auth/users/workspaces/mcp_tokens/SCIM routes (9-14), contract tests (1 and per-route fixtures), side-by-side routing and rollback (15). Deliberately out: OIDC login/callback, SCIM token rotation, `/health`.
- **Consistency:** `Signer`, `Principal`, `WorkspaceCtx`, `RequireRole`, `httpx.Detail` are defined in Tasks 2-5 and used by name afterwards.
- **Decisions for the user:** (1) leave `/auth/sso/*` on FastAPI (a Go OIDC flow plus a Starlette-compatible session cookie is a separate plan); (2) Task 11 slug-collision 409 instead of Python's 500; (3) `Authenticate` requires `exp` (Python tolerates its absence; all minted tokens carry it); (4) nginx currently does not proxy `/admin/` or `/scim/`, confirm they should be exposed.
- **Proportion:** interfaces and test names carry the decisions; handler bodies are left to the implementer.
