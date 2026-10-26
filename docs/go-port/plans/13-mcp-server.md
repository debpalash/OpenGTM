# MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `cmd/mcp`, a Go binary that replaces `python -m apps.mcp.server`: the JSON-RPC 2.0 MCP surface (`initialize`, `notifications/initialized`, `tools/list`, `tools/call`, `ping`) over stdio and over the loopback SSE/HTTP transport, the token authentication and tenant scoping, the capability filtering, the write guard rails (role re-check, daily cap, idempotency, audit) and all 15 tools. Tools that enrich or score go through the plan 06 enrichment packages and the `pyworker` RPC; the Python MCP server is retired after cutover.

**Architecture:** One package `internal/mcp` holds protocol, auth, catalog, audit, caps, write pipeline and tool groups; transports (`transport_stdio.go`, `transport_sse.go`) are thin and share one `Server.Handle(ctx, Session, rawMessage) (response []byte, ok bool)`. Tools never import other domain packages directly: each group depends on a small port interface (`ports.go`) whose adapters are wired in `cmd/mcp`, so plans 05-08 land independently and fakes cover the unit tests. Every store access runs inside `tenancy.InWorkspace` with the workspace taken from the authenticated token, never from arguments. Write tools run reservation, mutation and settle in ONE transaction; the audit row is written in a separate transaction so denials and rollbacks are still recorded.

**Tech Stack:** Go 1.27.3 stdlib (`net/http`, `bufio`), pgx v5 + sqlc, existing `internal/{tenancy,identity,canonjson,queue,leadsread,pyworker,ratelimit,config,db}`; no new modules. Python/uv for the recorder, seed, vectors and the Python-side RPC tools.

**Spec:** `docs/go-port/inventory.md` section 8 row 12 "MCP server" (sections 2 MCP surface, 4 `mcp` GO-NATIVE, 5 stores, 9 risks); `docs/go-port/plans/02-go-api-foundation-auth-workspaces.md` Task 12 (`/api/mcp/tokens`, capability vocabulary), `05-leads-signals.md`, `06-enrichment-jobs.md`. Python sources: `apps/mcp/server.py`, `apps/api/services/mcp/{auth,tools,caps,audit,models}.py`, `services/automations/caps.py` (reservation axes used), `routers/mcp_tokens.py` (read only).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 02 (identity, `mcp_tokens` queries, capability vocabulary) and 05 (`leadsread`, `leadswrite`) merged; plan 06 Tasks 12 and 15 (`pyworker`, `emailverify.Cascade`) merged. Plans 07 (automations) and 08 (outreach) are consumed through ports: until they land, those tools answer `{"error": "<tool> is not available in this build"}` and the catalog still lists them (identical to Python's catalog, so clients see no change). All tables (`mcp_tokens`, `mcp_audit_log`, `trigger_cap_reservations`) exist in `internal/db/schema.sql`; no Alembic change, no `RequiredGoSchemaRevision` bump.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL. No new modules.
- Trust boundary: the workspace and user come only from the resolved token (hash lookup, revocation, expiry, then a LIVE membership re-check and slug lookup). Tenant tables only inside `tenancy.InWorkspace`; `mcp_tokens` has no RLS and is always queried by hash. A tool argument can never name a workspace. Cross-tenant ids are a miss, never an error that reveals existence.
- Wire parity is the release bar: tool catalog (names, descriptions, `inputSchema` key order), the JSON-RPC envelopes, error codes and messages, the text payload of every tool (`json.dumps` default separators, `indent=2` for read tools, `default=str`, `ensure_ascii`), `isError` always `false` as in Python (D2), HTTP 401 body `{"error":"unauthorized","detail":<message>}` with `WWW-Authenticate: Bearer`. Recorded from Python, never hand-written.
- Capability vocabulary and write gates verbatim: `leads:read`; writes `leads:write`, `sequences:enroll`, `workbooks:write`, `automations:write`, `outreach:send`, `leads:delete`, `workbooks:delete`; admin-only (role floor `admin`, owner passes): `automations:write`, `outreach:send`, `leads:delete`, `workbooks:delete`; other writes accept `admin|member|editor`. `tools/list` shows a tool only when the token holds its capability and, for writes, `MCP_WRITE_ENABLED` is on.
- Secrets (plaintext token, `OPENGTM_MCP_TOKEN`, `Authorization` header) never appear in logs, errors, audit rows or fixtures. stdout carries protocol bytes only (logs go to stderr).
- Hardening deviates from Python only as a documented deviation (D3-D8) in `testdata/contract/README.md`: no malformed input reaches a handler panic or a Python exception string; bodies and frames are bounded; the SSE server binds loopback only unless explicitly overridden.
- Config names exactly as Python: `MCP_REQUIRE_AUTH` (default true under the Go Postgres-only stack), `MCP_WRITE_ENABLED` (false), `MCP_TOKEN_TTL_DAYS`, `MCP_MAX_WRITES_PER_DAY` (0 = unlimited), `AUTOMATIONS_ENABLED`, `AUTOMATIONS_ALLOW_LEGACY_OUTREACH`, `AUTOMATIONS_MAX_RULES_PER_WS` (50), `OPENGTM_MCP_TOKEN` with fallback `YUPCHA_MCP_TOKEN`, `OPENGTM_MCP_SSE_HOST` with fallback `YUPCHA_MCP_SSE_HOST` (127.0.0.1). New: `MCP_MAX_ENRICH_PER_DAY` (0), `MCP_TOOL_TIMEOUT_SECONDS` (120), `MCP_STDIO_REVALIDATE_SECONDS` (30), `OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK` (false).
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; each task stages only its own `<name>.sql.go` and `.sql`.

## Review Focus

1. **Cross-tenant and confused deputy.** A token bound to workspace A with arguments naming B's ids, a member removed after minting, a downgrade to viewer, an expired or revoked token on a long-lived stdio session, a `workspace_id` smuggled inside `arguments`. All must read as a miss or a denial, and every write denial is audited. Tasks 5, 10, 11, 12, 15.
2. **Write guard rails.** Idempotency replays must not re-apply; a failed write must free its cap slot and stay retryable with the same key (Python refuses the retry: D5); concurrent calls with one key apply once; the daily cap counts only applied writes; a crash mid-write leaves no held reservation. Tasks 6, 10.
3. **Money and side effects behind a read grant.** `enrich_company`, `verify_email`, `get_hiring_signals` spend provider credits and probe third-party hosts with only `leads:read`; `send_email` reaches real inboxes. Each must be audited, capped where configured, bounded by timeout, and SSRF-guarded. Tasks 9, 13.
4. **Malformed JSON-RPC.** Non-object body, `params: null`, `arguments` as a string, `limit: "x"`, missing `lead_id`, a 5 MB frame, an invalid `Content-Length`, a request without `id`, an unknown notification. Python raises (stdio: silent hang; SSE: HTTP 500 with a stack trace); Go answers a JSON-RPC error and keeps serving. Tasks 7, 8, 15, 16.
5. **Transport exposure.** SSE binds loopback, `GET /sse` is the only unauthenticated route and reveals nothing, every `/message` is authenticated before the body is read, bodies are capped, slow clients time out, and stdout is never polluted by logs. Tasks 15, 16, 17.
6. **Audit completeness and secrecy.** Nested secrets inside `arguments` (an automation action config with an API key, `body_html` with a link token) must be redacted, long strings truncated, an audit write failure must never crash or block a tool but must be logged. Task 6.

## File Structure

```
internal/mcp/pyjson/dumps.go                                  # json.dumps parity (ordered objects, indent, ensure_ascii, float repr)
internal/mcp/{ports,server,protocol,catalog}.go               # ports, Server.Handle, JSON-RPC envelopes, TOOL_SPECS
internal/mcp/{auth,roles}.go                                  # token resolve, Ctx, capability sets, live role check
internal/mcp/{audit,caps}.go                                  # redaction + record, reservation (held/settled/released)
internal/mcp/{execute,write}.go                               # execute_tool flow, write pipeline
internal/mcp/tools_read.go                                    # find_leads, get_lead_detail, get_pipeline_stats
internal/mcp/tools_enrich.go                                  # enrich_company, verify_email, get_hiring_signals, score_lead
internal/mcp/tools_leads.go                                   # create_lead, update_lead, delete_lead
internal/mcp/tools_workbooks.go                               # create_workbook, delete_workbook, planColumns
internal/mcp/tools_outreach.go                                # enroll_leads, send_email
internal/mcp/tools_automation.go                              # create_automation
internal/mcp/transport_stdio.go, transport_sse.go
cmd/mcp/main.go                                               # flags, wiring, graceful shutdown
internal/db/queries/{mcp_auth,mcp_audit,mcp_caps}.sql (+ gen/)
testdata/mcp/{rpc,tools,auth,sse,stdio}/*.json                # Python-recorded request/response pairs
../../legacy/python/apps/api/services/mcp/rpc_tools.py       # Python RPC providers (Task 2)
../../legacy/python/scripts/{mcp_record.py,mcp_seed.py,gen_mcp_vectors.py}
../../docs/go-port/runbooks/mcp-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time; larger waves run as batches). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, recorder, vectors; 2 Python RPC tools; 3 Go scaffold; 4 `pyjson` | T1 owns `mcp_seed.py`, `mcp_record.py`, `gen_mcp_vectors.py`; T2 owns `rpc_tools.py`; T3 owns `ports.go`, config additions, `cmd/mcp/main.go` skeleton, and one stub file per later task with its constructor (`func readTools(d Deps) []Tool` etc., returning nil); T4 owns `pyjson/` |
| 1 | 5 auth, 6 audit + caps, 7 catalog + protocol, 8 read tools, 9 enrich tools | 5 disjoint files; each owns its `.sql` (5, 6) |
| 2 | 10 write pipeline, 11 lead write tools, 12 workbook tools, 13 outreach tools, 14 automation tool, 15 stdio transport, 16 SSE transport | 7 tasks run as batches of 4 + 3; each owns its own file; 11-14 consume only T10's `WriteTool` type from `ports.go` (frozen in T3) |
| 3 (sequential) | 17 server assembly and end-to-end contract; 18 coverage + runbook | 17 owns `cmd/mcp/main.go` and `internal/mcp/server.go` wiring; 18 consumes everything |

---

### Task 1: Python seed, recorder, vectors

**Files:**
- Create: `legacy/python/scripts/mcp_seed.py`, `legacy/python/scripts/mcp_record.py`, `legacy/python/scripts/gen_mcp_vectors.py`
- Create (output, committed): `testdata/mcp/{rpc,tools,auth,sse,stdio}/*.json`, `internal/mcp/testdata/*.json`

**Interfaces:**
- Consumes: plan 04/05 seed (workspaces A/B, users alice owner, bob editor, carol viewer, leads, a sequence, workbooks).
- Produces: seeded tokens with fixed plaintext (`ycp_seed_read`, `_write`, `_admin`, `_all`, `_expired`, `_revoked`, `_removed_member`, `_workspace_b`) whose sha256 hashes are stored; recorder profiles `read` (writes off), `writes` (`MCP_WRITE_ENABLED=true`, `AUTOMATIONS_ENABLED=true`, `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=true`), `capped` (`MCP_MAX_WRITES_PER_DAY=2`), `outreach_off`. Fixtures are `{profile, token, request, response}`: JSON-RPC cases run through `apps.mcp.server.handle_message` with the resolved `MCPCtx`; transport cases through `build_sse_app()` and Starlette `TestClient` (401 missing/invalid/expired/revoked/non-member, 200 initialize/tools list/tools call, `GET /sse` event text, notification returns `null`). Cases: `initialize`, `notifications/initialized`, `ping`, unknown method, `tools/list` per token (read, write with flag off and on, admin), every tool happy path and each error path (`Lead not found`, unknown tool, capability not granted with `required`, writes disabled, viewer downgraded, cap exceeded, replay `duplicate`, `legacy_outreach_disabled`, `automations disabled`, `max rules per workspace reached`, `company is required`, cross-tenant lead and workbook ids). Vectors: `redact_arguments` (top-level, nested, long strings, `email`), `_plan_workbook_columns` (12 descriptions incl. unicode and empty), `json.dumps` formatting corpus (non-ASCII, floats `1e-05`, `0.1`, large ints, nested empties, `default=str` datetimes), `hash_token`, `upsert_lead` semantics (new, same company+city, differing case), `_lead_write_fields` filtering.

- [ ] **Step 1: Extend the seed and recorder** (fixed ids, timestamps and token plaintexts; recorder asserts a non-superuser role and `TZ=UTC`).
- [ ] **Step 2: Run** `uv run python scripts/mcp_seed.py && uv run python scripts/mcp_record.py && uv run python scripts/gen_mcp_vectors.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 3: Commit** (Go/doc side only) `test(go): mcp contract fixtures and vectors (RED)`.

### Task 2: Python RPC tools for the Python-only calls

**Files:**
- Create: `legacy/python/apps/api/services/mcp/rpc_tools.py`
- Test: `legacy/python/tests/test_mcp_rpc_tools.py`

**Interfaces:**
- Consumes: plan 06 Task 3 `POST /internal/provider-call` and its provider registry (assumes plan 06 Task 3 exposes a registration hook; if it only reads `python_providers.txt`, this task adds the three names there and one import line in `internal_provider.py`, append-only, coordinated with plan 06).
- Produces: providers `mcp.enrich_company` (input `{"lead_id": int}`; loads the lead through the workspace-scoped store of the RPC envelope workspace, runs `WaterfallEnricher().enrich(lead)`, output `{"enriched_fields": ..., "provider": ...}`, or `{"not_found": true}`), `mcp.get_hiring_signals` (input `{"company": str, "lead_id": int}`, output `{"hiring": bool, "data": object}`; same code path as `JobSpySignalProvider.enrich`), `mcp.score_lead` (input `{"lead_id": int}`, output `leadgen.scoring.score_lead(lead)` dict or `{"not_found": true}`). The workspace always comes from the RPC envelope, never from `input`.

- [ ] **Step 1: Write failing tests**: `test_enrich_company_rpc_matches_direct_call`, `test_score_lead_rpc_matches_direct_call`, `test_hiring_rpc_matches_direct_call`, `test_rpc_ignores_workspace_in_input`, `test_rpc_cross_tenant_lead_is_not_found`.
- [ ] **Step 2: Run** `uv run pytest tests/test_mcp_rpc_tools.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in the PR).

### Task 3: Go scaffold: ports, config, constructors, `cmd/mcp` skeleton

**Files:**
- Create: `internal/mcp/ports.go`, stub files `internal/mcp/{tools_read,tools_enrich,tools_leads,tools_workbooks,tools_outreach,tools_automation}.go`, `cmd/mcp/main.go`
- Modify: `internal/config/config.go`
- Test: `internal/mcp/scaffold_test.go`

**Interfaces:**
- Produces (frozen here, consumed by Tasks 5-17):
  - `type Ctx struct{ WorkspaceID, Slug string; Capabilities map[string]bool; UserID *int32; TokenID string }` and `func (c Ctx) Has(cap string) bool`.
  - `type Tool struct{ Name, Capability string; Handler func(ctx context.Context, c Ctx, args Args) (string, error) }` for reads and `type WriteTool struct{ Name, Capability string; Roles []string; Gate func(cfg Config) string; Apply func(ctx context.Context, tx pgx.Tx, c Ctx, args Args) (result string, err error) }` for writes; `type Args map[string]any` with helpers `Int(key string) (int64, bool, error)`, `String(key string) (string, bool, error)`, `StringList`, `IntList` that return typed errors `ArgError{Field, Want string}` (never panic).
  - Constructors returning `nil` here: `readTools(d Deps) []Tool`, `enrichTools(d Deps) []Tool`, `leadWriteTools(d Deps) []WriteTool`, `workbookWriteTools(d Deps) []WriteTool`, `outreachWriteTools(d Deps) []WriteTool`, `automationWriteTools(d Deps) []WriteTool`.
  - Ports (each with an in-memory fake in `internal/mcp/fakes_test.go`): `LeadPort{ List(ctx, tx, p ListParams) ([]Lead, error); ByID(ctx, tx, id int64) (*Lead, error); Stats(ctx, tx) (ordered, error); Upsert(ctx, tx, f LeadFields) (id int64, err error); UpdateFields(ctx, tx, id int64, f LeadFields) error; SetStatus(ctx, tx, id int64, status string) error; Delete(ctx, tx, id int64) error }` (assumes plans 04/05 `leadsread`/`leadswrite`), `EnrichPort{ VerifyEmail(ctx, ws, email string) (VerifyResult, error); Py(ctx, ws, provider string, in any) (json.RawMessage, error) }` (plan 06 `emailverify.Cascade`, `pyworker`), `WorkbookPort{ Create(ctx, tx, ws, name, description string, columns []Column) (id string, err error); Delete(ctx, tx, ws, id string) (found bool, err error) }` (assumes plan 04 `wbcrud`), `OutreachPort{ SequenceExists, IsSuppressed, Enroll, QueueSend }` (assumes plan 08), `AutomationPort{ CountRules, ValidateRule, CreateTrigger, BootstrapSchedule }` (assumes plan 07; `ValidateRule` returns `*HTTPError{Status int; Detail string}`), `ErrUnavailable` for a nil port.
  - Config fields for every name in Global Constraints; `cmd/mcp` parses `--sse [port]` (default port 3100, digits only like Python) else stdio, and exits 1 with a stderr message when neither transport can start.

- [ ] **Step 1: Write failing tests**: `TestConfigDefaultsMatchPython`, `TestConfigTokenEnvFallback` (`YUPCHA_MCP_TOKEN`), `TestArgsHelpersReturnTypedErrors`, `TestNilPortReturnsUnavailable`, `TestMainRefusesNonLoopbackWithoutOverride`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp scaffold (RED)`, `feat(go): mcp ports, config and cmd skeleton`.

### Task 4: `pyjson`: `json.dumps` parity

**Files:** Create `internal/mcp/pyjson/dumps.go`, `internal/mcp/pyjson/dumps_test.go`

**Interfaces:** Produces `type Obj struct{ Keys []string; Vals map[string]any }` with `func NewObj(kv ...any) *Obj`; `func Dumps(v any, indent int) ([]byte, error)` equal to Python `json.dumps(v, indent=<0 means none>, default=str)` for `*Obj`, `[]any`, `string`, `bool`, `nil`, `int64`, `float64` (via `canonjson.FloatRepr`), `json.RawMessage` (re-encoded, key order kept through `canonjson.Decode`), `time.Time` (rendered with Python `str(datetime)`: `YYYY-MM-DD HH:MM:SS[.ffffff][+00:00]`); `ensure_ascii` (`\uXXXX`, surrogate pairs), separators `, ` and `: ` (with `indent`: `,` + newline and `: `), empty containers `{}` / `[]`; NaN and Inf render `NaN`, `Infinity`, `-Infinity` like Python.

- [ ] **Step 1: Write failing tests**: `TestDumpsCorpusMatchesPython` (the Task 1 corpus, compact and indent 2), `TestDumpsKeyOrderPreserved`, `TestDumpsEnsureASCIISurrogates`, `TestDumpsFloatRepr`, `TestDumpsDatetimeStr`, `TestDumpsRejectsUnsupportedTypes`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): pyjson dumps parity (RED)`, `feat(go): python json.dumps compatible encoder`.

### Task 5: Token authentication, capabilities, live role check

**Files:** Create `internal/db/queries/mcp_auth.sql`, `internal/mcp/{auth,roles}.go`, `internal/mcp/auth_test.go`, `internal/mcp/auth_integration_test.go`

**Interfaces:**
- Produces: `var ReadCaps, WriteCaps, AdminOnlyCaps, AllCaps` (verbatim sets), `var WriteRoles = []string{"admin","member","editor"}`, `AdminRoles = []string{"admin"}`; `type AuthError struct{ Msg string }`; `func Resolve(ctx context.Context, d Deps, raw string) (Ctx, error)`: with `MCP_REQUIRE_AUTH` false -> the keyless context (active workspace from app setting `ACTIVE_WORKSPACE`, else the main workspace; `ReadCaps`; `UserID` nil); otherwise empty raw -> `missing MCP token`; unknown hash -> `invalid MCP token`; `revoked_at` set -> `MCP token revoked`; `expires_at <= now` -> `MCP token expired`; `last_used_at` touched in the same transaction; then `identity.Store.IsMember` false -> `MCP token user is no longer a workspace member`; no slug -> `MCP token workspace not found`; lookup by `sha256` hex with constant-time-safe behaviour (no early exit on prefix); `func RequireRole(ctx, d Deps, c Ctx, roles ...string) error` (`UserID` nil passes; owner passes; else `insufficient workspace role`); `func BearerToken(header string) (string, bool)` (scheme case-insensitive, one space split like Python `partition(" ")`).

- [ ] **Step 1: Write failing tests**: `TestResolveErrorStringsMatchPython`, `TestResolveRevokedExpiredNonMember`, `TestResolveTouchesLastUsed`, `TestResolveKeylessReadOnlyMainWorkspace`, `TestResolveTokenFromOtherWorkspaceScopedToItsOwn`, `TestRequireRoleOwnerAndDowngrade`, `TestRequireRoleSelfHostPasses`, `TestBearerTokenParsing` (`bearer x`, `Bearer`, `Basic x`, empty), `TestResolveNeverLogsToken`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp token auth (RED)`, `feat(go): mcp token resolution`, `feat(go): mcp live role check`.

### Task 6: Audit log and write-cap reservations

**Files:** Create `internal/db/queries/{mcp_audit,mcp_caps}.sql`, `internal/mcp/{audit,caps}.go`, tests

**Interfaces:**
- Produces: `func RedactArguments(args Args) map[string]any` (case-insensitive keys `token, password, secret, api_key, authorization, email` become `***redacted***`; strings longer than 500 become the first 500 characters plus `…`; ALSO recursive over nested objects and lists up to depth 4 with the broader pattern `token|secret|password|authorization|cookie|credential|api_?key`, D6); `func Record(ctx context.Context, d Deps, c Ctx, tool string, args Args, o AuditOutcome)` with `AuditOutcome{Status string; Error string; CostUSD float64; IdempotencyKey string}` (status one of `ok, denied, error, capped`; own transaction under `tenancy.InWorkspace`; failure logs `mcp audit write failed` with tool and status and returns, never panics or blocks beyond a 5 s timeout); `type Reservation struct{ OK, Replay bool; Reason, Key string; id int64 }`; `func Reserve(ctx, tx pgx.Tx, ws, rule string, cap int, key string) (Reservation, error)` against `trigger_cap_reservations` (`trigger_id = "__mcp_write__"`, `day_utc` UTC `YYYY-MM-DD`, `reserved_usd 0`, `reserved_actions 1`, state `held`); `pg_advisory_xact_lock` on `(workspace, day)` serialises reservers; same key in state `held` or `settled` -> `Replay`; state `released` -> a NEW reservation is allowed (D5); `cap > 0` and held+settled actions today `+1 > cap` -> `{OK:false, Reason:"cap"}`; empty key -> `mcpw_<uuid hex>`; `Settle`, `Release` (state transitions only from `held`). Enrichment cap reuse: the same functions with rule `__mcp_enrich__` and `MCP_MAX_ENRICH_PER_DAY` (Task 9).

- [ ] **Step 1: Write failing tests**: `TestRedactVectorsMatchPython` (top-level cases), `TestRedactNestedSecretsAndLists`, `TestRedactTruncatesLongStrings`, `TestRecordWritesRowInTenantScope`, `TestRecordFailureDoesNotPanic` (closed pool), `TestRecordTimeoutBounded`, `TestReserveReplayOnHeldAndSettled`, `TestReserveAfterReleaseAllowed`, `TestReserveCapExceeded`, `TestReserveConcurrentSameKeyOnce` (16 goroutines), `TestReserveDailyBucketRollover`, `TestReserveTenantIsolation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp audit and write caps (RED)`, `feat(go): mcp audit log writer`, `feat(go): mcp write cap reservations`.

### Task 7: Tool catalog and JSON-RPC protocol

**Files:** Create `internal/mcp/{catalog,protocol,server}.go`, tests

**Interfaces:**
- Produces: `var Specs []ToolSpec` (15 entries in Python order, each `Name, Capability, Description, InputSchema *pyjson.Obj` copied byte for byte from `TOOL_SPECS`, verified against the Task 1 `tools/list` fixtures); `func (s *Server) List(c Ctx) []any` (visibility rule in Global Constraints, `capability` key removed); `type Server struct{ ... }`, `func NewServer(d Deps, tools Registry) *Server`; `func (s *Server) Handle(ctx context.Context, c Ctx, raw []byte) (resp []byte, respond bool)`: parses with `canonjson.Decode` (depth and size bounded); non-object -> `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`; invalid JSON -> `-32700 Parse error`; `params` present but not an object -> `-32602 Invalid params`; `initialize` -> `protocolVersion "2024-11-05"`, `capabilities {"tools":{"listChanged":false}}`, `serverInfo {"name":"opengtm","version":"3.0.0"}`; `notifications/initialized` and any other `notifications/*` -> `respond=false` (Python answers `-32601` for the latter: D4); `tools/list` -> `{"tools": [...]}`; `tools/call` -> `{"content":[{"type":"text","text":<result>}],"isError":false}`; `ping` -> `{}`; anything else -> `-32601 Method not found: <method>`; the request `id` is echoed with its original JSON type (number, string, null; absent -> `null`); a call with a deadline of `MCP_TOOL_TIMEOUT_SECONDS`; envelope key order `jsonrpc, id, result|error`; encoded with `pyjson`. `Registry` maps tool names to `Tool` or `WriteTool` (built in Task 17).

- [ ] **Step 1: Write failing tests**: `TestCatalogMatchesPythonToolsListFixtures` (per recorded token), `TestListHidesWriteToolsWhenFlagOff`, `TestInitializeMatchesFixture`, `TestPingAndUnknownMethodMatchFixtures`, `TestNotificationsGetNoResponse`, `TestIDTypePreserved` (7, "a", null, 1.5, large int), `TestParseErrorAndInvalidRequest`, `TestParamsNotObjectIsInvalidParams`, `TestToolCallEnvelopeMatchesFixture`, `TestHandlerNeverPanicsOnFuzz` (200 random byte and structure cases), `TestToolTimeoutReturnsErrorText`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp catalog and protocol (RED)`, `feat(go): mcp tool catalog`, `feat(go): mcp json-rpc handler`.

### Task 8: Read tools

**Files:** Modify stub `internal/mcp/tools_read.go`; tests; fixtures `testdata/mcp/tools/read_*.json`

**Interfaces:** Consumes `LeadPort`, `pyjson`, `Args`. Produces `readTools(d Deps) []Tool` with `find_leads` (args `query`, `city`, `tier`, `limit` default 10; maps to `ListParams{Search, City, ScoreTier, Limit}` with the same defaults and ordering as `store.get_leads`; result `{"leads":[{id, company, city, email, phone, score, tier, status, website, specialization, company_size, contact_person}], "count": N}` with `indent=2`), `get_lead_detail` (`lead.to_dict()` ordered as the plan 04 `Lead`, `decision_makers` parsed from its JSON string when it parses, left as is otherwise; `{"error": "Lead not found"}` for a miss and for another workspace's id; `indent=2`, `default=str`), `get_pipeline_stats` (`indent=2`). Argument errors (D3): missing `lead_id` -> `{"error": "lead_id is required"}`, non-integer -> `{"error": "lead_id must be an integer"}`, `limit` non-integer or `< 1` -> `{"error": "limit must be a positive integer"}` and `limit` is capped at 200 (Python is unbounded). All run inside `tenancy.InWorkspace(c.WorkspaceID)`.

- [ ] **Step 1: Write failing tests**: `TestFindLeadsMatchesFixtures`, `TestFindLeadsLimitDefaultAndCap`, `TestFindLeadsTierCityFilters`, `TestFindLeadsUnicodeCompany`, `TestGetLeadDetailMatchesFixture`, `TestGetLeadDetailDecisionMakersParsedOrKept`, `TestGetLeadDetailCrossTenantIsNotFound`, `TestGetLeadDetailBadArgs`, `TestGetPipelineStatsMatchesFixture`, `TestReadToolsRunScopedToTokenWorkspace` (pool size 1, no GUC leak).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp read tools (RED)`, `feat(go): mcp find_leads, lead detail and stats tools`.

### Task 9: Enrich tools (spend behind a read grant)

**Files:** Modify stub `internal/mcp/tools_enrich.go`; tests; fixtures `testdata/mcp/tools/enrich_*.json`

**Interfaces:** Consumes `EnrichPort`, `LeadPort`, Task 6 `Reserve`/`Record`. Produces `enrichTools(d Deps) []Tool`: `verify_email` (`emailverify.Cascade(ctx, ws, email)`; result keys `email, status, valid, deliverable, confidence, source, detail` with `indent=2`; failure -> `{"email": ..., "error": <generic message>}`), `enrich_company` (lead existence check through `LeadPort` first -> `{"error": "Lead not found"}`; then `Py("mcp.enrich_company", {"lead_id"})`; result `{"enriched_fields": ..., "provider": ...}` `indent=2`), `get_hiring_signals` (`Py("mcp.get_hiring_signals", {"company","lead_id"})`; `{"company", "hiring", "data"}` `indent=2, default=str`; failure `{"company", "error"}`), `score_lead` (`Py("mcp.score_lead")`, `indent=2, default=str`; miss `{"error": "Lead not found"}`). Hardening (D7): the three cost-bearing tools write an `mcp_audit_log` row per call (status `ok`/`error`, no arguments beyond redaction), count against `MCP_MAX_ENRICH_PER_DAY` through `Reserve(rule "__mcp_enrich__")` (cap 0 = unlimited; exceeded -> `{"error": "mcp enrich cap exceeded"}`, audited `capped`), run under `MCP_TOOL_TIMEOUT_SECONDS`, never echo Python exception text (generic `enrichment failed`, detail only in the server log), and `pyworker.ErrUnavailable` -> `{"error": "enrichment backend unavailable"}`. `email` is required and must be a string up to 320 characters.

- [ ] **Step 1: Write failing tests** (fake ports): `TestVerifyEmailResultMatchesFixture`, `TestVerifyEmailBadArgs`, `TestEnrichCompanyMissesCrossTenantLeadBeforeSpending` (no `Py` call), `TestEnrichCompanyResultShape`, `TestHiringSignalsShape`, `TestScoreLeadShape`, `TestCostToolsAreAudited`, `TestEnrichCapExceededIsAuditedCapped`, `TestEnrichUnavailableBackend`, `TestEnrichErrorTextNeverLeaksPythonException`, `TestEnrichToolTimeout`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS; `make test-integration` with `TEST_PY_WORKER_URL` set also runs `TestEnrichAgainstPythonRPC` (skips otherwise).
- [ ] **Step 5: Commit** `test(go): mcp enrich tools (RED)`, `feat(go): mcp verify_email and enrichment tools`.

### Task 10: Execute flow and write pipeline

**Files:** Create `internal/mcp/{execute,write}.go`, tests

**Interfaces:** Consumes Tasks 5-7, `Registry`. Produces `func (s *Server) Execute(ctx context.Context, c Ctx, name string, args Args) string` equal to `execute_tool`: unknown tool -> `{"error": "Unknown tool: <name>"}`; capability missing -> `{"error": "capability not granted", "required": <cap>}` (audited `denied`/`capability not granted` for write tools only); write tool with `MCP_WRITE_ENABLED` off -> `{"error": "mcp writes disabled"}` audited `denied`; reads -> handler inside the tenant scope, a returned error or panic becomes `{"error": "<message>"}` (messages from typed `ArgError`/domain errors only). Write pipeline (`func (s *Server) executeWrite(...)`), in order: (1) tool `Gate(cfg)` reason -> `{"error": reason}` audited `denied` (`legacy_outreach_disabled`, `automations disabled`); (2) live `RequireRole` with the tool's roles -> `{"error": "insufficient workspace role"}` audited `denied`; (3) ONE transaction under `tenancy.InWorkspace`: `Reserve` (replay -> `{"status":"duplicate","idempotency_key":<key>,"message":"already applied (idempotent replay)"}` audited `ok`/`idempotent_replay`; cap -> `{"error":"mcp write cap exceeded"}` audited `capped`/`daily MCP write cap exceeded`), `Apply`, and on a result whose JSON has an `error` key or a returned error -> rollback (reservation freed, D5) and audit `error`, else `Settle` + commit and audit `ok` with the idempotency key; (4) the audit row is always written outside the transaction. Arguments `idempotency_key` is a string up to 128 characters (longer or non-string -> `{"error": "idempotency_key must be a string up to 128 characters"}`); a panic inside `Apply` rolls back, audits `error` with a generic message and returns `{"error": "internal error"}`.

- [ ] **Step 1: Write failing tests** (fake `WriteTool`s and real Postgres): `TestUnknownToolAndCapabilityMessagesMatchFixtures`, `TestWriteDisabledAuditedDenied`, `TestGateDeniedAudited`, `TestRoleDowngradeDeniedAndAudited`, `TestAdminOnlyToolsRejectEditor`, `TestReplayDoesNotReapply`, `TestFailedWriteRollsBackAndKeyRetryable`, `TestStructuredErrorResultFreesCapSlot`, `TestCapExceededAudited`, `TestConcurrentSameKeyAppliesOnce`, `TestApplyPanicRollsBackAndAudits`, `TestOneAuditRowPerAttempt`, `TestIdempotencyKeyValidation`, `TestWorkspaceInArgumentsIgnored`, `TestCrashBetweenReserveAndApplyLeavesNoHeldRow` (kill the connection mid-transaction).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp execute and write pipeline (RED)`, `feat(go): mcp execute flow`, `feat(go): mcp guarded write pipeline`.

### Task 11: Lead write tools

**Files:** Modify stub `internal/mcp/tools_leads.go`; tests; fixtures `testdata/mcp/tools/lead_*.json`

**Interfaces:** Consumes `LeadPort`, Task 10 `WriteTool`. Produces `leadWriteTools(d Deps) []WriteTool`: `create_lead` (cap `leads:write`, roles `WriteRoles`; allowlisted fields `company, website, email, phone, contact_person, contact_title, city, state, address, specialization, company_size, description, industry_tags, linkedin_url, twitter_url, facebook_url, notes, status, source`; `null` values dropped; blank `company` -> `{"error": "company is required"}`; non-string values -> `{"error": "<field> must be a string"}` (`industry_tags` as in the `Lead` dataclass); `LeadPort.Upsert` with the Python `upsert_lead` semantics recorded in Task 1; result `{"status":"created","lead_id":N,"company":<company>}`), `update_lead` (`lead_id` integer, existence check through the scoped port -> `{"error": "Lead not found"}`; fields minus `status` via `UpdateFields`, `status` via `SetStatus`; result `{"status":"updated","lead_id":N,"fields":[sorted names, "status" included last by sort]}`), `delete_lead` (cap `leads:delete`, roles `AdminRoles`; miss -> `{"status":"not_found","lead_id":N}`; hit -> `{"status":"deleted","lead_id":N}`). Non-integer `lead_id` -> `{"error": "lead_id must be an integer"}` (D3). `workspace_id` and unknown keys are ignored.

- [ ] **Step 1: Write failing tests**: `TestCreateLeadMatchesFixture`, `TestCreateLeadDedupSemanticsMatchUpsert`, `TestCreateLeadAllowlistDropsUnknownAndWorkspace`, `TestCreateLeadBlankCompany`, `TestCreateLeadNonStringField`, `TestUpdateLeadFieldsAndStatus`, `TestUpdateLeadCrossTenantNotFound`, `TestUpdateLeadNoFieldsStillOK`, `TestDeleteLeadIdempotent`, `TestDeleteLeadCrossTenantNoop`, `TestLeadWritesRequireCapabilityRoleAndFlag` (end to end through `Execute`).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp lead write tools (RED)`, `feat(go): mcp create, update and delete lead tools`.

### Task 12: Workbook tools

**Files:** Modify stub `internal/mcp/tools_workbooks.go`; tests; fixtures `testdata/mcp/tools/workbook_*.json`

**Interfaces:** Consumes `WorkbookPort`. Produces `workbookWriteTools(d Deps) []WriteTool`: `create_workbook` (cap `workbooks:write`; `description` required string, `name` optional; default name `"MCP — " + first 40 characters (code points) of description`; `func planColumns(description string) []Column` ported from `_plan_workbook_columns` (lowercase substring rules; ids `company, email, phone, linkedin_url, contact_person, contact_title, city`; labels as Python; always ends with `email`, `contact_person` when absent; no duplicate ids; each column `{"id","name","type":"lead_field","lead_field"}` in that key order); stored with `source_type "empty"`, empty `source_config`/`filter_criteria`; result `{"status":"created","workbook_id":<id>,"name":<name>,"columns":[names]}`), `delete_workbook` (cap `workbooks:delete`, roles `AdminRoles`; miss or other workspace -> `{"status":"not_found","workbook_id":<id>}`; `workbook_id` must be a non-empty string of at most 64 characters). A missing `description` answers `{"error": "description is required"}` (Python: `'description'`, D3).

- [ ] **Step 1: Write failing tests**: `TestPlanColumnsVectorsMatchPython`, `TestPlanColumnsUnicodeAndEmpty`, `TestCreateWorkbookMatchesFixture`, `TestCreateWorkbookDefaultNameTruncatesByCodePoint`, `TestCreateWorkbookStoredColumnsByteIdentical`, `TestCreateWorkbookMissingDescription`, `TestDeleteWorkbookHardDeleteAndIdempotent`, `TestDeleteWorkbookCrossTenantNotFound`, `TestWorkbookToolsRequireCapabilityAndRole`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp workbook tools (RED)`, `feat(go): mcp create and delete workbook tools`.

### Task 13: Outreach tools

**Files:** Modify stub `internal/mcp/tools_outreach.go`; tests; fixtures `testdata/mcp/tools/outreach_*.json`

**Interfaces:** Consumes `OutreachPort` (assumes plan 08) and `LeadPort`. Produces `outreachWriteTools(d Deps) []WriteTool`: `enroll_leads` (cap `sequences:enroll`, roles `WriteRoles`; `sequence_id` must exist in the workspace else `{"error": "Sequence not found"}`; per lead id through the scoped `LeadPort` (other workspace = miss): no normalized email -> skipped `no_email`; suppressed -> `suppressed`; `Enroll(seq, lead, email, consent_source "mcp_enroll", consent_at now)` nil -> `already_enrolled`; result `{"enrolled":N,"skipped":[{"lead_id","reason"}],"total_lead_ids":N}`; `lead_ids` must be a list of integers, at most 1000 (D3)), `send_email` (cap `outreach:send`, roles `AdminRoles`, `Gate` = `legacy_outreach_disabled` unless `AUTOMATIONS_ALLOW_LEGACY_OUTREACH`; `lead_id` required integer; lead data fields `email, company, contact_person, contact_title, city, website, phone`; config `{sequence_id, step_number, subject, body_html}`; `OutreachPort.QueueSend` enqueues through the durable `send` job in the SAME transaction (suppression, rate limits, debit-on-success and the bounce breaker stay in the send handler); result `{"status","summary"[,"skip_reason"][,"error"]}`; `body_html` at most 200 KB, `subject` at most 998 characters). Both tools return `{"error": "<tool> is not available in this build"}` when the port is nil.

- [ ] **Step 1: Write failing tests** (fake port): `TestEnrollLeadsMatchesFixture`, `TestEnrollSkipsNoEmailSuppressedAndDuplicate`, `TestEnrollUnknownSequenceNotFound`, `TestEnrollCrossTenantLeadSkipped`, `TestEnrollTooManyIdsRejected`, `TestSendEmailQueuesInSameTransaction` (rollback leaves no job), `TestSendEmailDisabledGateAudited`, `TestSendEmailCrossTenantLeadSkippedNeverSends`, `TestSendEmailResultShape`, `TestSendEmailIdempotentReplayNoSecondJob`, `TestSendEmailBodyLimits`, `TestOutreachToolsUnavailableWithoutPort`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp outreach tools (RED)`, `feat(go): mcp enroll_leads tool`, `feat(go): mcp send_email tool`.

### Task 14: Automation tool

**Files:** Modify stub `internal/mcp/tools_automation.go`; tests; fixtures `testdata/mcp/tools/automation_*.json`

**Interfaces:** Consumes `AutomationPort` (assumes plan 07). Produces `automationWriteTools(d Deps) []WriteTool`: `create_automation` (cap `automations:write`, roles `AdminRoles`, `Gate` = `automations disabled` unless `AUTOMATIONS_ENABLED`; `name` trimmed non-empty else `{"error": "name is required"}`; defaults `trigger_config {}`, `actions []`, `condition ""`, `scope_workbook_ids []`, `enabled true`, `stop_on_error false`; rule count at or above `AUTOMATIONS_MAX_RULES_PER_WS` -> `{"error": "max rules per workspace reached (<n>)"}`; `ValidateRule` failure -> `{"error": <detail>, "status_code": <n>}`; insert with `created_by = c.UserID`, `schedule_anchor = now` for `on_schedule`; for an enabled `on_schedule` rule `BootstrapSchedule` runs after the commit and its failure is logged only; result `{"status":"created","automation_id":<id>,"automation":<to_api>}` with `default=str`). Types: `trigger_type` string, `actions` list of objects at most 50, `max_actions_per_day` integer, `max_spend_usd_per_day` number (wrong types -> `{"error": "<field> must be <type>"}`, D3).

- [ ] **Step 1: Write failing tests** (fake port): `TestCreateAutomationMatchesFixture`, `TestCreateAutomationDisabledGate`, `TestCreateAutomationValidationErrorShape`, `TestCreateAutomationMaxRules`, `TestCreateAutomationScheduleBootstrapAfterCommit`, `TestCreateAutomationBootstrapFailureDoesNotFailCreate`, `TestCreateAutomationNeedsAdmin`, `TestCreateAutomationRejectsWrongTypes`, `TestCreateAutomationUnavailableWithoutPort`, `TestCreateAutomationAuditRedactsNestedSecrets` (action config with `api_key`).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp create_automation tool (RED)`, `feat(go): mcp create_automation tool`.

### Task 15: stdio transport

**Files:** Create `internal/mcp/transport_stdio.go`, `internal/mcp/transport_stdio_test.go`

**Interfaces:** Consumes Tasks 5, 7. Produces `func ServeStdio(ctx context.Context, s *Server, d Deps, in io.Reader, out io.Writer, token string) error`: authenticates ONCE at start with the token; failure -> error string `MCP stdio refused: <message>. Set OPENGTM_MCP_TOKEN to a valid token.` on stderr and a non-nil error (exit code 1); framing = `Content-Length: N\r\n\r\n<body>` read with a bounded reader (frame cap 4 MiB; invalid or negative length -> `-32600` response and resync at the next header; headers are case-insensitive and tolerate `Content-Type`); additionally newline-delimited JSON is auto-detected when a line starts with `{` and then answered in the same style (the framing real MCP clients use; Python ignores it, D4); responses written as `Content-Length` frames (or NDJSON) with `pyjson` bytes, flushed per message; messages are handled strictly in order; the session context is revalidated (`Resolve` again) at most every `MCP_STDIO_REVALIDATE_SECONDS` before a `tools/list` or `tools/call` and a failure answers a JSON-RPC error `{"code":-32001,"message":"unauthorized: <message>"}` and ends the session (D8); EOF or context cancel returns nil; nothing but protocol bytes is written to `out`.

- [ ] **Step 1: Write failing tests**: `TestStdioInitializeListCallRoundTrip` (fixtures), `TestStdioRefusesWithoutValidToken`, `TestStdioIgnoresPartialAndOversizedFrames`, `TestStdioBadContentLengthDoesNotHang`, `TestStdioNDJSONAutoDetect`, `TestStdioNotificationNoReply`, `TestStdioRevokedTokenEndsSession`, `TestStdioExpiredMidSessionStopsServing`, `TestStdioOutputOnlyProtocolBytes`, `TestStdioSequentialOrder`, `TestStdioEOFReturnsNil`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp stdio transport (RED)`, `feat(go): mcp stdio transport`.

### Task 16: SSE/HTTP transport

**Files:** Create `internal/mcp/transport_sse.go`, `internal/mcp/transport_sse_test.go`

**Interfaces:** Consumes Tasks 5, 7. Produces `func SSEHandler(s *Server, d Deps) http.Handler` and `func ListenSSE(ctx context.Context, h http.Handler, host string, port int) error`. `GET /sse` (no auth, as Python): `text/event-stream`, body `data: {"type": "endpoint", "url": "/message"}\n\n`, then the stream closes. `POST /message`: authenticate FIRST from `Authorization: Bearer` (`BearerToken`), failure -> 401 `{"error":"unauthorized","detail":<message>}` with `WWW-Authenticate: Bearer` before reading the body; then the body is read with a 1 MiB cap (413 beyond), `Handle` runs under the request context with the tool timeout, the response is the JSON-RPC response (`Content-Type: application/json`) or the literal `null` when no response is due (Python parity). Invalid JSON -> 200 with the `-32700` error body (Python: 500, D4). Server timeouts: `ReadHeaderTimeout 10s`, `ReadTimeout 30s`, `WriteTimeout` tool timeout plus 10 s, `IdleTimeout 60s`, `MaxHeaderBytes 16 KiB`, at most 64 concurrent requests (503 beyond). `ListenSSE` refuses a non-loopback `host` unless `OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK` is true (D6) and logs `OpenGTM MCP SSE server on http://<host>:<port> (loopback, bearer-auth)` to stderr. Per-IP failed-auth limiter through `internal/ratelimit` (30 failures per minute -> 429).

- [ ] **Step 1: Write failing tests**: `TestSSEEndpointEventBytesMatchFixture`, `TestMessageUnauthorizedMatchesFixtures` (missing, invalid, expired, revoked, non-member), `TestMessageAuthBeforeBodyRead` (a never-ending body is not consumed), `TestMessageInitializeListCallMatchFixtures`, `TestMessageNotificationReturnsNull`, `TestMessageInvalidJSONIs200ParseError`, `TestMessageBodyCap413`, `TestMessageCrossTenantTokenIsolation`, `TestListenRefusesNonLoopback`, `TestListenAllowsOverride`, `TestFailedAuthRateLimit429`, `TestConcurrentRequestCap503`, `TestSlowClientTimesOut`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp sse transport (RED)`, `feat(go): mcp sse transport`.

### Task 17: Server assembly and end-to-end contract

**Files:** Modify `internal/mcp/server.go` (registry build only), `cmd/mcp/main.go`; create `cmd/mcp/main_integration_test.go`, `internal/mcp/e2e_contract_test.go`

**Interfaces:** Consumes everything. Produces `func BuildRegistry(d Deps) Registry` (concatenates the six constructors, then verifies at startup that the registry names equal the 15 catalog names in catalog order, else refuses to start); `cmd/mcp` wiring: config, pgx pool with the runtime role (non-superuser check as `cmd/api`), `pyworker` client from `PY_WORKER_URL`/`PY_WORKER_TOKEN`, ports adapters (leads from plans 04/05, workbook from plan 04, emailverify from plan 06, nil for outreach and automation until plans 07/08 provide adapters), `slog` to stderr, SIGTERM/SIGINT graceful shutdown (finish the in-flight call, stop reading); `--sse [port]` or stdio like Python (`--sse` followed by digits only is the port). Binary name `opengtm-mcp`; `Makefile` target `build-mcp`.

- [ ] **Step 1: Write failing tests**: `TestRegistryMatchesCatalog`, `TestEveryRecordedFixtureReplaysByteForByte` (all `testdata/mcp/**` through `Handle`), `TestSSEEndToEndAgainstFixtures` (real listener on 127.0.0.1:0), `TestStdioEndToEndBinary` (spawns the built binary with `OPENGTM_MCP_TOKEN`, speaks the protocol), `TestBinaryExitsOneWithoutToken`, `TestBinaryRefusesSuperuserRole`, `TestGracefulShutdownFinishesInFlightCall`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp end-to-end contract (RED)`, `feat(go): mcp server assembly and binary`.

### Task 18: Coverage, deviations, runbook

**Files:**
- Modify: `testdata/contract/README.md` (deviations section), `scripts/nginx-routing-check.sh` (one case: `/api/mcp/tokens` unaffected)
- Create: `internal/mcp/coverage_test.go`, `docs/go-port/runbooks/mcp-cutover.md`

**Interfaces:** Consumes everything. Produces: coverage tests `TestEveryPythonToolIsPortedOrUnavailable` (parses `TOOL_SPECS` from `services/mcp/tools.py`; the 15 names equal the Go catalog and registry), `TestEveryPythonConfigNamePresent`, `TestCapabilityVocabularyEqualsPlan02Tokens` (the `/api/mcp/tokens` allowed set equals `AllCaps`), `TestNoToolImportsDomainPackagesDirectly` (import graph: tool files import only `ports.go` types); the runbook: no nginx change because the Python SSE server is not proxied today (it binds loopback; remote access, if any, is the operator's own reverse proxy and must keep forwarding `Authorization`); client config switch (`"command": "opengtm-mcp"` with `OPENGTM_MCP_TOKEN`, or `"python -m apps.mcp.server"` for rollback); SSE service swap (`opengtm-mcp --sse 3100`); prerequisites (plan 06 `PY_WORKER_URL` for `enrich_company`, `get_hiring_signals`, `score_lead`; plans 07/08 adapters for `create_automation`, `enroll_leads`, `send_email`, otherwise those answer `not available in this build` and the runbook says to keep Python for them); `MCP_WRITE_ENABLED` stays off during cutover and is enabled last; rollback; deviations (D2-D8, atomic reserve and write, audit of cost-bearing tools, nested redaction).

- [ ] **Step 1: Write failing tests** as listed. **Step 2: Run** `make test` -> FAIL. **Step 3:** write the deviations, runbook and nginx case. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` -> PASS.
- [ ] **Step 5: Commit** `test(go): mcp coverage (RED)`, `docs(go-port): mcp cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row 12 "stdio + SSE JSON-RPC, 15 tools, audit; enrich tools go through plan 5": protocol (7), stdio (15), SSE (16), auth and capability filtering (5, 7), audit (6, 10), caps and idempotency (6, 10), 15 tools: `find_leads, get_lead_detail, get_pipeline_stats` (8), `enrich_company, verify_email, get_hiring_signals, score_lead` (9, 2), `create_lead, update_lead, delete_lead` (11), `create_workbook, delete_workbook` (12), `enroll_leads, send_email` (13), `create_automation` (14). Token CRUD `/api/mcp/tokens` is already plan 02.
- **Consistency:** `Ctx`, `Tool`, `WriteTool`, `Args` (3) used in 5-16; `Reserve/Settle/Release/Record` (6) in 9, 10; `Resolve/RequireRole/BearerToken` (5) in 10, 15, 16; `pyjson.Dumps` (4) in 7-14; `Registry` (7) built in 17.
- **Review Focus:** 1 -> 5, 10-12, 15; 2 -> 6, 10; 3 -> 9, 13; 4 -> 7, 8, 15, 16; 5 -> 15-17; 6 -> 6.
- **Proportion:** signatures, exact strings, key orders and test names carry the decisions; handler bodies are left to the implementer and the recorded fixtures.

## Open decisions (defaults apply unless overridden)

- **D1 Process model.** Default (confirm): a separate `opengtm-mcp` binary (stdio or `--sse`), not routes inside `cmd/api`; SSE stays loopback-only and not proxied by nginx, exactly as the Python server.
- **D2 `isError`.** Python reports `isError: false` even when the text is `{"error": ...}`. Default (confirm): kept for byte parity; flipping it to `true` for error payloads is a one-line change after the clients are checked.
- **D3 Argument hardening.** Missing or mistyped arguments answer `{"error": "<field> is required|must be <type>"}` instead of Python exception text (`'lead_id'`), and `limit`, `lead_ids`, `actions` and string lengths are bounded. Documented deviation.
- **D4 Protocol hardening.** Invalid JSON and malformed requests answer JSON-RPC errors (Python: HTTP 500 on SSE, silent drop on stdio); unknown `notifications/*` get no reply (Python replies `-32601`); stdio also accepts newline-delimited JSON. Documented deviation.
- **D5 Reservation semantics.** Reserve, mutate and settle in one transaction; a failed write frees its slot and the same key can be retried (Python commits the reservation first and then refuses a retry of a released key; a crash leaves a held slot). Documented deviation.
- **D6 Audit and exposure.** Redaction recurses into nested arguments; SSE refuses non-loopback binds without `OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK`; failed-auth rate limit. Documented deviation.
- **D7 Cost-bearing read tools.** `enrich_company`, `verify_email`, `get_hiring_signals` are audited and can be capped with `MCP_MAX_ENRICH_PER_DAY` (default 0, unlimited, as today). Default (confirm): audit on, cap off.
- **D8 stdio session revalidation.** A revoked or expired token or removed member stops a long-lived stdio session within `MCP_STDIO_REVALIDATE_SECONDS` (default 30); Python authenticates once at start. Documented deviation.
- **D9 Dependencies on plans 07 and 08.** Default: `create_automation`, `enroll_leads`, `send_email` run through ports; until those plans provide adapters the tools answer `not available in this build` and the cutover runbook keeps the Python server for deployments that use them.
- **D10 `score_lead`, `enrich_company`, `get_hiring_signals`.** Default (confirm): through the Python RPC until the leadgen plan ports scoring and the waterfall enricher; no Go reimplementation here.

## User decisions

(none yet; the defaults above apply until the user objects)
