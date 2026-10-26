# Copilot Agent and Person Intel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the copilot chat surface (7 `copilotkit` routes incl. the SSE chat endpoint, conversation history, tool-approval gate, episodic memory) and the 5 `person_intel` routes (REST, history, LinkedIn-enrichment WebSocket) to Go. The Go API owns auth, SSE framing, the bounded tool-use loop, the human-in-the-loop approval gate, persistence and the enrichment pipeline; every LLM token stream, every non-ported tool and the Python-only deterministic pre-routes cross to the Python worker through one streaming RPC boundary (NDJSON over HTTP) with cancellation and a hard timeout.

**Architecture:** `POST /api/copilotkit` today is a 3,700-line router that interleaves (a) tenant auth, (b) eight deterministic "pre-route" branches that skip the LLM, (c) a recursive provider-streaming function with failover, tool-call accumulation, a round budget and an approval gate, and (d) ~25 tool bodies that call leadgen, workbook, outreach and scraper code. This plan splits that along the seam the inventory fixes ("LLM streaming stays in the Python worker"): **Go** runs the turn state machine (`internal/copilot`: pre-route, round loop, dedup, approval proposal and resume, persistence, memory, SSE bytes); **Python** exposes four internal endpoints under `/internal/copilot/*` (`preroute` and `llm` stream NDJSON frames, `tool` and `system-prompt` are unary). The provider chain, API keys, Gemini message sanitising, 429/5xx/401/403/timeout failover and tool-call accumulation stay inside the Python `llm` endpoint, so no secret ever crosses the wire. Four read-only/simple lead tools run natively in Go; all others call `tool`. Chat history, approvals and builtin memory already live in Postgres (plan 01 tables `chat_conversations`, `chat_messages`, `chat_tool_approvals`, `chat_memories`), so a Go replica and a Python replica serve the same data during canary. `person_intel` (DuckDuckGo Lite + GitHub API + page parsing) is ported in Go; only the optional `deep` page scraping goes through plan 06's `ProviderCall` (`person_intel_deep`).

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, go-redis v9 (not used here), `github.com/coder/websocket` (pinned by plan 06 D7), `golang.org/x/net/html` (pinned by plan 06 Task 14); existing `internal/{httpx,identity,tenancy,canonjson,queue,pyworker,leadsread,leadswrite,config,ratelimit}`. Python/uv for the recorder, seed, vectors and the Python-side RPC endpoints.

**Spec:** `docs/go-port/inventory.md` section 8 row 11 (sections 2 HTTP surface, 5 stores, 7 integrations, 9 risks); `docs/go-port/plans/01-sqlite-to-postgres.md` Tasks 5 and 7 (chat/memory tables); `06-enrichment-jobs.md` (D8 `pyworker`, `python_providers.txt`). Python sources: `apps/api/routers/{copilotkit,person_intel}.py`, `services/{chat_history,memory,person_intel}.py`, `services/leadgen/{collection_intent,targeted_people,people_contacts}.py`, `services/outreach/drafting.py`, `core/security.py` (`authenticate_query_token`), `core/config.py` (`CHAT_REQUIRE_AUTH`).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 01-02 merged (chat/memory tables, identity, tenancy); plan 06 `pyworker` client scaffold and `PY_WORKER_URL`/`PY_WORKER_TOKEN` (assumes plan 06 Task 5/12 provide `pyworker.Client`, `Deps.PyWorker`, config vars, `internal/providers/python_providers.txt`; this plan adds a streaming method to the same client, Task 6). Plan 05 provides `leadsread.Lead`, `LeadsByIDs`, `QueryLeads`, stats and `leadswrite.UpdateStatus` (assumes plan 05: names as in its Interfaces; if a name differs the Task 12 native tools adapt, they are the only consumers).

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. **No new Alembic revision is required**: chat tables are plan 01's, `person_intel` is the baseline table. Task 4 only asserts they exist in `schema.sql` at `RequiredGoSchemaRevision` (no `RequiredGoSchemaRevision` bump). Any later new table needs a Python Alembic task plus Go schema sync.
- Tenant tables (`chat_*`) only inside `tenancy.InWorkspace`. Runtime role non-superuser `NOBYPASSRLS`. Cross-tenant conversation ids behave as Python: GET answers 404 `{"error":"Not found"}` (note: this body, not `detail`), DELETE is a silent no-op answering `{"ok": true}`, a forged `conversation_id` in the chat body starts a fresh conversation. `person_intel` has no workspace column (global, admin only), kept as is.
- **Auth is not `current_workspace`.** Chat routes use `_resolve_chat_workspace` semantics exactly: `CHAT_REQUIRE_AUTH` false (self-host) binds to the active workspace and `user_id=None`; true requires `Authorization: Bearer` access token (refresh tokens rejected), `X-Workspace-Id` or the user's active workspace, membership (`is_member`, owner included): 401 `Could not validate credentials` with `WWW-Authenticate: Bearer`, 403 `No accessible workspace. Ask an admin to add you to one.`, 403 `Workspace access denied` (same for missing and foreign), 404 `Workspace not found`. Person intel: `get_current_admin_user` for REST (401/403 as plan 02), `authenticate_query_token(require_admin=True)` for the WebSocket (close `4403` before accept). `/api/copilotkit/info` is unauthenticated like Python.
- Contract parity is the release bar for the 7+5 routes: paths, methods, JSON key order, status codes, `detail`/`error` strings, and **SSE bytes**: each frame `data: <python json.dumps default separators, ensure_ascii>\n\n` (via `canonjson.Marshal(v, false)`), comment frame `: keepalive\n\n` every 10 s while a tool or the pre-route runs, terminal `data: [DONE]\n\n`; headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`, copied verbatim and verified by fixture (Task 1). Contract fixtures are recorded from FastAPI (`legacy/python/scripts/contract_record.py` + `contract_seed.py`, non-superuser role, `TZ=UTC`) with the LLM replaced by a scripted OpenAI-compatible simulator (`legacy/python/scripts/llm_sim.py`, Task 1), never hand-written.
- Approvals are single-use and tenant-scoped: `consume_tool_approval` is one atomic UPDATE (`status='pending'` guard, TTL 3600 s, `(workspace, user)` match); an unknown tool name never runs inline (`_needs_confirmation` = not in SAFE set, fail closed); a dangerous tool additionally requires `tables.write` role (`admin`/`editor`) for `user_id != None` at execution time. These three are re-asserted in Go regardless of what the Python worker answers.
- **Security/crash hardening (documented deviations, `testdata/contract/README.md`):** non-JSON or non-object body, `messages` not a list, non-string content, more than 200 messages or 1 MiB body answer 422/413 instead of Python's 500; `approved_tool_calls` capped at 20; per-user concurrent chat stream cap (429 `Too many active chat streams`, default 3) and the shared `SSE_MAX_STREAMS` caps; hard turn deadline (`COPILOT_TURN_TIMEOUT_SECONDS`, default 900); tool-result and `tool_data` size cap 1 MiB; `GET /api/person/{id}` with a non-integer id answers 422 like FastAPI (parity), `person/enrich` no longer echoes `str(exc)` for unexpected errors (generic 500 `Enrichment failed`), the WebSocket enforces a read limit, one request per connection, a 120 s deadline and one concurrent enrichment per admin.
- Python-boundary rules (decision D1): bearer token, per-call deadline, cancellation by closing the request (Python cancels the upstream provider stream and the tool task), truncation detection (a stream without a terminal frame is an error), frame size cap 4 MiB, secrets (provider keys) never in a frame, log line or error string (a test greps all three).
- Redis is not used by this plan. Python replicas keep running chat during canary; both stacks share the same tables, so no data migration is needed (rollback is nginx only).
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (documented in `testdata/contract/retained_copilot_routes.txt`, Task 21): none of the 12 routes; but these *tools* execute on Python via `POST /internal/copilot/tool` (collection, enrich, workbook creation, plan execution, outreach drafting, AmbitionBox, people research and verification): list in `internal/copilot/catalog.json` with `"executor":"python"`.

## Review Focus

1. **Approval gate bypass or double execution.** A hallucinated tool name, an approval for another workspace/user, an expired or already consumed approval, two concurrent resubmits of the same approval, or a Python tool answer for a tool the Go catalog marks dangerous must never execute a mutation twice or without consent. Consumption is atomic and happens before execution; the denial message text equals Python's. Tasks 8, 13, 14.
2. **Stream lifecycle and cancellation.** Client disconnect must cancel the Python RPC (no orphaned LLM spend), release the stream-cap slot and end both goroutines; a Python crash mid-stream surfaces as one SSE `error` frame and `[DONE]`, never a hang; the keepalive keeps flowing during a 45 s tool; the turn deadline ends everything. Tasks 6, 7, 13, 17, 20.
3. **SSE byte parity.** `json.dumps` separators and `ensure_ascii`, key order of `confirmation_required` (`confirmation_id, tool_call, name, args, description, level, label`), event order within a round (`tool_call`, `tool_result` per safe call, then gate events), failover `warning` and in-band `content` notices, `conversation_id` first, persistence of tool rows and the assistant row after the stream. Tasks 7, 13, 14, 17.
4. **Tenant leakage through history, memory and tool context.** Every chat query filters `(workspace_id, user_id)` in SQL *and* relies on RLS; memory namespace is `(workspace, user)`; the trusted tool-context system message reads only the caller's owned conversation; the Python RPC always receives the workspace resolved by Go, never a body value. Tasks 8, 9, 10, 12.
5. **Memory backend divergence.** Python prefers the optional `openmemory` library (private SQLite) over the builtin store; Go can only read `chat_memories`. Decision D3. Task 9.
6. **WebSocket and SSRF in person intel.** Query-token auth, admin only, 4403 before accept, URL validation (`linkedin.com/in/`), outbound requests through `internal/egress` (the GitHub API call and every fetched page), bounded bodies, one enrichment per socket, disconnect cancels the pipeline. Tasks 11, 19.

## File Structure

```
internal/pyworker/stream.go                                # (add) Stream: NDJSON frames over POST, cancel, caps
internal/ssestream/{writer,caps}.go                        # python-exact SSE framing, keepalive, per-user/global caps
internal/chathistory/{store,approvals,title}.go            # conversations, messages, tool_approvals on chat_* tables
internal/chatmemory/{store,tokens}.go                      # builtin memory: add/search/all over chat_memories
internal/copilotauth/resolve.go                            # _resolve_chat_workspace
internal/copilot/{catalog.json,catalog.go}                 # tool schemas + DANGEROUS/SAFE sets, generated from Python
internal/copilot/{tools_native,toolctx,format}.go          # 4 native tools, trusted tool context, people/draft formatters
internal/copilot/{turn,round,gate,resume}.go               # turn state machine, round loop, approval gate, approved-call resume
internal/copilot/{preroute,persist}.go                     # Python preroute adapter, message/memory persistence
internal/personintel/{parse,search,extract,github,pipeline,store}.go
internal/api/{copilotscaffold,copilotroutes,copilotchat,personintelroutes,personintelws}.go
internal/api/register.go                                   # (modify once, Task 5)
internal/db/queries/{chat_history,chat_memory,person_intel}.sql (+ gen/)
testdata/contract/{copilot,person}/*.json
testdata/contract/retained_copilot_routes.txt
testdata/copilot/frames/*.ndjson                           # Python-recorded RPC frames per scenario
../../legacy/python/apps/api/routers/internal_copilot.py   # /internal/copilot/{preroute,llm,tool,system-prompt}
../../legacy/python/apps/api/services/copilot/{preroute,llm_stream,tools}.py   # extracted from routers/copilotkit.py
../../legacy/python/scripts/{contract_seed.py,contract_cases/copilot.yaml,llm_sim.py,gen_copilot_vectors.py,gen_copilot_catalog.py,record_copilot_frames.py}
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/copilot-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time; larger waves run as batches). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, simulator, cases, vectors; 2 Python extraction of `services/copilot/*`; 3 Python `/internal/copilot/*` endpoints; 4 Go schema check + sqlc queries; 5 Go scaffold | T1 owns `contract_seed.py`, `llm_sim.py`, `gen_*`, `record_copilot_frames.py`; T2 owns `copilotkit.py` (becomes a thin caller of the extracted modules, behaviour unchanged); T3 owns `internal_copilot.py` and the `main.py` include line; T4 owns the three `.sql` files; T5 owns `register.go`, `Deps` additions and stub route files |
| 1 | 6 pyworker stream, 7 ssestream, 8 chathistory, 9 chatmemory, 10 copilotauth, 11 personintel core | 6 disjoint packages; 6 only adds `stream.go` + its tests to `internal/pyworker/` |
| 2 | 12 catalog + native tools + tool context + formatters, 13 turn/round/gate engine, 14 approval resume, 15 preroute adapter + persistence, 16 person pipeline + store | 12, 13, 14, 15 each own distinct files in `internal/copilot/` (listed per task); 13 consumes 6, 7, 12; 14 consumes 8, 12; 15 consumes 6, 8, 9 |
| 3 | 17 conversation and memory routes, 18 chat route, 19 person REST routes, 20 person WebSocket | each owns exactly one stub `internal/api/*.go` and one fixture dir |
| 4 (sequential) | 21 chaos/e2e and coverage, nginx, runbook | consumes everything |

---

### Task 1: Python seed, LLM simulator, recorder cases, vectors

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/copilot.yaml`, `legacy/python/scripts/llm_sim.py`, `legacy/python/scripts/gen_copilot_vectors.py`, `legacy/python/scripts/gen_copilot_catalog.py`
- Create (output, committed): `testdata/contract/{copilot,person}/*.json`, `internal/{chathistory,chatmemory,copilot,personintel}/testdata/*.json`, `internal/copilot/catalog.json`

**Interfaces:**
- Consumes: plan 04 seed (workspaces A/B, editor/viewer, admin user).
- Produces: seed additions (3 conversations for user editor in A with 4 messages each incl. `tool` rows carrying `tool_data` for `find_people_at_company` and `enrich_people_contacts`, one conversation for B, 2 pending and 1 consumed and 1 expired approval, 6 `chat_memories` rows for A/editor and 2 for B, 3 `person_intel` rows, a `CHAT_REQUIRE_AUTH=true` recording profile and a `false` profile). `llm_sim.py`: an OpenAI-compatible `/chat/completions` SSE server driven by a scenario file (plain text answer; one safe tool call; two parallel safe calls with a duplicate; a dangerous call; an unknown tool name; 429 then fallback provider; 500 then fallback; connect failure; truncated JSON chunk; tool loop that hits the round budget); the recorder points the `PROVIDERS` base URLs at it. Fixtures for chat are the **raw SSE bytes** of each scenario, the conversation rows after the turn, the approvals rows after the turn, the memory rows after the turn. Vector files: `memory_tokens` (stopword and case cases), memory search ordering (overlap, tie by recency, 500 cap), `auto_title_from_message` (empty, 60/61 chars, no space), `_conversation_tool_context` (limit 3, 24,000-char cut inside a multibyte char, non-dict results skipped), `format_*` outputs (`_format_people_research`, `_verification`, `_contacts`, `_signal_tracking`, `_grounded_draft`), `_describe_action` is **not** vectored (stays Python), person-intel parse/extract (username parsing, DDG Lite HTML samples, social-link and email extraction, GitHub payload), `PersonIntelResponse` key order. `gen_copilot_catalog.py` dumps `_build_tools()`, `DANGEROUS_TOOLS`, `SAFE_TOOLS` and `_INFO_RESPONSE` into `catalog.json` with `executor` per tool.

- [ ] **Step 1: Extend the seed** (fixed ids and timestamps; `chat_messages.created_at` ties included).
- [ ] **Step 2: Write `llm_sim.py` and the cases** (`{name, method, path, as, profile, body, scenario, expect_status}`): info GET/POST; conversations list/get/delete for owner, other user, other workspace; memories; chat per scenario above with `conversation_id` absent, owned, foreign; approval resubmit approve, deny, replay (consumed), expired, wrong workspace; `CHAT_REQUIRE_AUTH=true` 401 (no header, refresh token, inactive user), 403 (no workspace, non-member), 404; the pre-route cases (terse company-team clarification, explicit people request, "verify them", "save those", "find emails for them", draft-only request, tracking request without selection, account-discovery proposal); person REST (enrich invalid URL 400, history paging, detail 404, delete 404/200).
- [ ] **Step 3: Write the vector and catalog scripts** (they import the real Python modules).
- [ ] **Step 4: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/copilot.yaml`, `uv run python scripts/gen_copilot_vectors.py`, `uv run python scripts/gen_copilot_catalog.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 5: Commit** (Go/doc side only) `test(go): copilot contract fixtures, vectors and tool catalog (RED)`.

### Task 2: Python extraction of the copilot engine (behaviour-preserving)

**Files:**
- Create: `legacy/python/apps/api/services/copilot/{__init__,preroute,llm_stream,tools,prompt}.py`
- Modify: `legacy/python/apps/api/routers/copilotkit.py` (becomes thin: auth, history, loop; imports the extracted functions)
- Test: `legacy/python/tests/test_copilot_extraction.py` (plus the existing copilotkit tests untouched)

**Interfaces:**
- Produces: `preroute.run(ctx, last_user_msg, conv_id, approved) -> AsyncIterator[PreFrame]` (yields `sse(obj)`, `persist(role, content, tool_data)` or returns `none` when no branch matches; contains the eight branches of `copilot_chat` lines 2940-3510 verbatim and creates approvals through `chat_history.create_tool_approval`); `llm_stream.one_round(messages, tools, provider_cursor, workspace_id) -> AsyncIterator[LlmFrame]` (one provider call plus failover; yields `content`, `warning`, `error`, `tool_calls` (accumulated, complete, ordered by index), `done`, and the final `cursor`); `tools.execute(name, args, ctx) -> str` (= `_execute_tool`); `prompt.build_system_prompt(store) -> str`.
- Behaviour must not change: the full existing suite and the Task 1 SSE fixtures pass against the refactored router unchanged (the router still drives the old recursion by calling `one_round` in a loop).

- [ ] **Step 1: Write failing tests**: `test_router_sse_bytes_unchanged_for_all_scenarios` (replays Task 1 fixtures), `test_one_round_failover_order`, `test_one_round_accumulates_split_tool_call_arguments`, `test_preroute_none_for_plain_text`, `test_preroute_company_team_clarification`, `test_preroute_creates_approval_in_caller_tenant`.
- [ ] **Step 2: Run** `uv run pytest tests/test_copilot_extraction.py` -> FAIL. **Step 3: Move code, no edits to bodies.** **Step 4: Run** the new tests plus `uv run pytest tests -k copilot -q` -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 3: Python internal copilot endpoints

**Files:**
- Create: `legacy/python/apps/api/routers/internal_copilot.py`; modify `legacy/python/apps/api/main.py` (include line), `legacy/python/scripts/record_copilot_frames.py` (new)
- Test: `legacy/python/tests/test_internal_copilot.py`

**Interfaces:**
- Auth: same bearer as `internal_provider.py` (constant-time compare, 401 without).
- `POST /internal/copilot/preroute` body `{"workspace_id","user_id","slug","conversation_id","last_user_message","has_approved_calls"}` -> `application/x-ndjson`, one JSON object per line, flushed per frame: `{"t":"sse","seq":n,"d":<event object>}`, `{"t":"persist","seq":n,"role","content","tool_data"}`, `{"t":"keepalive"}`, terminal `{"t":"none"}` (no branch matched), `{"t":"done"}` (branch finished, caller sends `[DONE]`) or `{"t":"error","message"}`.
- `POST /internal/copilot/llm` body `{"workspace_id","messages","tools_enabled","provider_cursor","round_budget_hit"}` -> frames `{"t":"content","d":str}`, `{"t":"warning","d":str}`, `{"t":"error","d":str}`, `{"t":"tool_calls","d":[...],"cursor":n}`, `{"t":"done","cursor":n}`; the provider chain, keys, base URLs and Gemini sanitising are resolved server-side from `workspace_id`; **no key, URL or header is ever emitted**.
- `POST /internal/copilot/tool` body `{"name","args","workspace_id","user_id","slug","timeout_s"}` -> `200 {"result": "<exact string _execute_tool returned>"}`, `408 {"detail":"timeout"}` (hard cap 120 s, task cancelled), role check for dangerous tools stays inside; `POST /internal/copilot/describe` `{"name","args","workspace_id"}` -> `{"description"}`; `POST /internal/copilot/system-prompt` `{"workspace_id","slug"}` -> `{"system_prompt"}`.
- Streaming endpoints poll `request.is_disconnected()` between frames and cancel the upstream provider stream / tool task; a `call_id` header deduplicates a retried unary call.

- [ ] **Step 1: Write failing tests**: `test_requires_bearer`, `test_llm_frames_never_contain_api_key`, `test_llm_client_disconnect_closes_upstream`, `test_preroute_frames_end_with_terminal`, `test_tool_timeout_cancels_task`, `test_tool_result_is_exact_string`, `test_workspace_comes_from_body_not_session`, `test_frame_seq_monotonic`.
- [ ] **Step 2: Run** `uv run pytest tests/test_internal_copilot.py` -> FAIL. **Step 3: Implement**; then run `record_copilot_frames.py` which replays every Task 1 scenario through these endpoints and writes `testdata/copilot/frames/<scenario>.ndjson` (Go tests replay these files, never hand-written). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** Go-side `test(go): recorded copilot rpc frames (RED)` (frames only); Python side unversioned.

### Task 4: Go schema check and sqlc queries

**Files:**
- Create: `internal/db/queries/{chat_history,chat_memory,person_intel}.sql`
- Test: `internal/db/schema_test.go` (existing drift test, add cases)

**Interfaces:**
- Consumes: plan 01 revision tables in `schema.sql`. Produces named queries: `ChatCreateConversation, ChatListConversations(limit), ChatGetConversation, ChatGetMessages, ChatAddMessage (also bumps updated_at), ChatUpdateTitle, ChatDeleteConversation, ChatCreateApproval, ChatConsumeApproval` (single `UPDATE ... WHERE id=$1 AND workspace_id=$2 AND user_id=$3 AND status='pending' AND created_at_epoch >= $4 RETURNING tool_call_json`), `MemAdd, MemRecent(500), MemAll`, `PersonInsert, PersonHistory(skip, limit), PersonGet, PersonDelete`. Timestamps are formatted in SQL with the `YYYY-MM-DD HH24:MI:SS` shape the Python PG path uses (`chat_history.py` `_pg_ts`).

- [ ] **Step 1: Write failing test** `TestSchemaHasChatAndPersonTables` (tables, FK cascade, RLS flags). **Step 2: Run** `make test` -> FAIL. **Step 3:** write queries, `make sqlc`. **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): chat and person schema (RED)`, `feat(go): chat history, memory and person intel queries`.

### Task 5: Go scaffold

**Files:**
- Modify: `internal/api/register.go` (+`RegisterCopilot`, `RegisterPersonIntel` lines, `Deps` gains `ChatStreams *ssestream.Caps`), `internal/config/config.go` (`CHAT_REQUIRE_AUTH` default = `PG_LEAD_STORE`, `COPILOT_GO_ENABLED` default false, `COPILOT_MAX_BODY_BYTES` 1048576, `COPILOT_MAX_STREAMS_PER_USER` 3, `COPILOT_TURN_TIMEOUT_SECONDS` 900, `COPILOT_TOOL_TIMEOUT_SECONDS` 120)
- Create: stub files `internal/api/{copilotroutes,copilotchat,personintelroutes,personintelws}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), `internal/api/copilotscaffold.go`
- Test: `internal/api/copilotscaffold_test.go`

**Interfaces:** Produces `RegisterCopilot/RegisterPersonIntel(r, d)` (register nothing and log when `COPILOT_GO_ENABLED` is false, or `PyWorker` is nil for chat only); the exact route table of Tasks 17-20.

- [ ] **Step 1: Write failing tests**: `TestCopilotRoutesNotRegisteredWithoutFlag`, `TestCopilotRoutePatterns` (7 + 5, with `/api/person/history` ordered before `/api/person/{record_id}`), `TestChatRequireAuthDefaultsToPGLeadStore`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): copilot scaffold (RED)`, `feat(go): copilot and person intel route scaffold`.

### Task 6: pyworker streaming client

**Files:** Create `internal/pyworker/stream.go`, `internal/pyworker/stream_test.go`, `internal/pyworker/stream_integration_test.go`

**Interfaces:**
- Consumes: plan 06 `pyworker.Client` (bearer, base URL). Produces `type Frame struct{ T string; Seq int; D json.RawMessage; Role, Content, ToolData *string; Cursor *int; Message string }`; `func (c *Client) Stream(ctx context.Context, path string, body any) (*Stream, error)`; `func (s *Stream) Next() (Frame, error)` (returns `io.EOF` only after a terminal frame `done|none|error`; a connection that ends earlier returns `ErrTruncated`; a frame over 4 MiB or a `seq` that does not increase returns `ErrProtocol`); `func (s *Stream) Close()` aborts the HTTP request (cancels Python side); `func (c *Client) Unary(ctx, path string, body, out any, timeout time.Duration) error` for `tool`, `describe`, `system-prompt` (one retry on connection error with the same `X-Call-Id`; no retry after a 2xx header; 408 maps to `ErrTimeout`). Errors never contain the token or request body.

- [ ] **Step 1: Write failing tests** (httptest): `TestStreamReadsFramesInOrder`, `TestStreamTruncationIsError`, `TestStreamSeqRegressionIsProtocolError`, `TestStreamOversizedFrame`, `TestStreamCloseCancelsServerRequest` (server observes `r.Context().Done()`), `TestStreamDeadline`, `TestUnaryRetriesOnceWithSameCallID`, `TestUnaryNoRetryAfterResponseStarted`, `TestErrorsNeverContainToken`, `TestNoGoroutineLeakAfterClose`, integration `TestStreamAgainstPythonEndpoint` (skips unless `TEST_PY_WORKER_URL`).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** with `bufio.Reader` + `ReadSlice` semantics bounded to the frame cap. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): pyworker streaming rpc (RED)`, `feat(go): pyworker ndjson stream and unary client`.

### Task 7: SSE writer and stream caps

**Files:** Create `internal/ssestream/{writer,caps}.go`, tests

**Interfaces:** Produces `type Writer struct{...}`; `func New(w http.ResponseWriter, flush func()) (*Writer, error)` (errors when `ResponseWriter` is not a `Flusher`); `(w *Writer) Event(v any) error` writes `data: ` + `canonjson.Marshal(v,false)` + `\n\n` and flushes; `Done()` writes `data: [DONE]\n\n`; `Keepalive()` writes `: keepalive\n\n`; `Headers(w)` sets the four Python headers; `type Caps` with `Acquire(ctx, workspaceID string, userID int32) (release func(), err error)` honouring `SSE_MAX_STREAMS`, `SSE_MAX_STREAMS_PER_USER` semantics but with `COPILOT_MAX_STREAMS_PER_USER` for chat (429 `Too many active chat streams`), release idempotent. `func KeepaliveLoop(ctx, every time.Duration, w *Writer, done <-chan struct{})` emits `: keepalive` every `every` while `done` is open (10 s for chat).

- [ ] **Step 1: Write failing tests**: `TestEventBytesMatchPythonDumps` (vectors: non-ASCII `ensure_ascii`, nested key order, floats), `TestDoneAndKeepaliveBytes`, `TestHeadersMatchFixture`, `TestCapsPerUserAndGlobal`, `TestReleaseIdempotent`, `TestKeepaliveStopsWhenDone`, `TestWriterErrorAfterClientGone`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): sse writer and chat stream caps (RED)`, `feat(go): python-exact sse framing and stream caps`.

### Task 8: Chat history and approvals

**Files:** Create `internal/chathistory/{store,approvals,title}.go`, tests (uses Task 4 queries)

**Interfaces:**
- Produces: `type Conversation struct{ ID, Title string; ... }` with the Python JSON shape and key order (`id, title, created_at, updated_at` and message shape `id, role, content, tool_data, created_at`; verified by fixtures); `func Create(ctx, tx pgx.Tx, ws string, uid *int32, title string) (Conversation, error)`; `List(ctx, tx, ws, uid, limit=50)`; `Get(ctx, tx, id, ws, uid) (*Conversation, error)` (owner-only; `uid` nil matches the `'0'`/empty namespace exactly as `_uid()`); `Messages(...)`; `AddMessage(ctx, tx, convID, role, content string, toolData *string, ws string) error` (role CHECK, bumps `updated_at`, rejects content over 1 MiB with a typed error); `UpdateTitle`; `Delete` (no error when foreign); `AutoTitle(text string) string` (= `auto_title_from_message`, rune-safe cut at 60 then `rsplit(" ",1)[0] + "..."`); `CreateApproval(ctx, tx, ws, uid, toolCall map[string]any) (id string, err error)` (stored as `json.dumps(sort_keys=True, separators=(",",":"))`); `ConsumeApproval(ctx, tx, id, ws, uid, decision string, ttl time.Duration, now time.Time) (map[string]any, bool, error)` (invalid decision or empty ids -> `nil,false`; the returned call has `id` replaced by the approval id, like Python).

- [ ] **Step 1: Write failing tests**: `TestHistoryJSONMatchesFixtures`, `TestAutoTitleVectors`, `TestOwnerOnlyReads` (user B cannot read A's conversation in same workspace), `TestCrossWorkspaceInvisible` (pool of 1 alternating tenants), `TestDeleteForeignIsNoop`, `TestMessageRoleCheck`, `TestMessagesOrderByCreatedAtThenRowOrder` (ties), `TestApprovalConsumeOnce` (16 goroutines, exactly one wins), `TestApprovalExpired`, `TestApprovalWrongTenant`, `TestApprovalDenyThenApproveFails`, `TestApprovalStoredCanonical`, `TestApprovalReturnedIDIsApprovalID`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): chat history and approvals (RED)`, `feat(go): chat conversations and messages`, `feat(go): atomic single-use tool approvals`.

### Task 9: Builtin chat memory

**Files:** Create `internal/chatmemory/{store,tokens}.go`, tests

**Interfaces:** Produces `func Tokens(text string) map[string]struct{}` (= `_tokens` with `_STOPWORDS`, Unicode lowercase, same split rule); `func Namespace(ws string, uid *int32) string` (= `_namespace`); `func Add(ctx, tx, ns, text string, md map[string]any) error`; `func Search(ctx, tx, ns, query string, limit int) ([]Memory, error)` (last 500 rows by `id DESC`, overlap count > 0, stable sort by overlap descending so recency breaks ties, as `_BuiltinMemory.search`); `func All(ctx, tx, ns) ([]Memory, error)`; `type Memory struct{ Memory string; Metadata map[string]any; CreatedAt string }` JSON key order `memory, metadata, created_at`, `created_at` = Python `astimezone(utc).isoformat()` (microseconds only when non-zero, `+00:00`). `Available() bool` always true. Memory writes are best-effort: callers ignore errors (documented).

- [ ] **Step 1: Write failing tests**: `TestTokensVectors`, `TestSearchOrderingVectors`, `TestSearchCap500`, `TestNamespaceIsolation` (A/editor vs B/editor vs A/other), `TestCreatedAtFormat`, `TestAddThenAllNewestFirst`, `TestSearchEmptyQueryReturnsNothing`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): chat memory (RED)`, `feat(go): builtin chat memory over postgres`.

### Task 10: Chat auth resolver

**Files:** Create `internal/copilotauth/resolve.go`, `internal/copilotauth/resolve_test.go`, `internal/copilotauth/resolve_integration_test.go`

**Interfaces:** Consumes `identity.Store` (`IsMember`, `ActiveWorkspaceID`), `auth.Signer`. Produces `type Resolved struct{ WorkspaceID, Slug string; UserID *int32 }`; `func Resolve(ctx, r *http.Request, d Deps) (Resolved, *httpx.Err)` implementing the Global Constraints auth table, including: `type=="refresh"` rejected, inactive user 401, `X-Workspace-Id` over active workspace, membership includes owner, slug lookup (404 `Workspace not found`), `CHAT_REQUIRE_AUTH=false` -> active workspace with slug fallback `main`. SSO enforcement is **not** applied (Python does not either; parity, documented).

- [ ] **Step 1: Write failing tests** against Task 1 fixtures: `TestMissingBearer401WithWWWAuthenticate`, `TestRefreshTokenRejected`, `TestInactiveUser401`, `TestNoWorkspace403`, `TestNonMemberAndMissing403SameBody`, `TestSlugMissing404`, `TestSelfHostBindsActiveWorkspace`, `TestOwnerIsMember`, `TestHeaderOverridesActive`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): chat auth resolver (RED)`, `feat(go): chat workspace resolver`.

### Task 11: Person intel core (parse, search, extract, GitHub)

**Files:** Create `internal/personintel/{parse,search,extract,github}.go`, tests, `testdata/` vectors from Task 1

**Interfaces:**
- Consumes: `internal/egress` client (plan 06), `golang.org/x/net/html`.
- Produces: `func ParseUsername(url string) (string, error)` (error text equals `Could not parse LinkedIn username from: <url>`); `func BuildQueries(username string) *Obj` (ordered, same keys/strings); `func SearchDDGLite(ctx, c *http.Client, q string, max int) ([]Result, error)` (HTML parse of DuckDuckGo Lite, same `title/url/snippet/source` fields and de-redirect of `uddg=`); `func ExtractLinkedIn(p *Profile, rs []Result)`, `ExtractSocial`, `ExtractArticlesAndMentions`, `ExtractEmails` (regex `EMAIL_PATTERN`), `func EnrichFromGitHub(ctx, c, p *Profile) error`; `type Profile` JSON key order = `PersonProfile` (`model_dump`).
- Hardening: all fetches through the SSRF-guarded client (no redirects to private hosts), 1 MiB body cap, 15 s timeout, at most 8 parallel searches.

- [ ] **Step 1: Write failing tests**: `TestParseUsernameVectors`, `TestBuildQueriesVectors`, `TestDDGLiteParseVectors` (recorded HTML incl. malformed, ads, redirect links), `TestExtractVectors`, `TestExtractEmailsDedupeAndJunk`, `TestGitHubEnrichVectors`, `TestFetchRefusesPrivateHost`, `TestBodyCap`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** (commit per two files). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): person intel extraction (RED)`, `feat(go): linkedin username parsing and ddg lite search`, `feat(go): profile extraction and github enrichment`.

### Task 12: Tool catalog, native tools, trusted tool context, formatters

**Files:** Create `internal/copilot/{catalog.go,tools_native.go,toolctx.go,format.go}`, `internal/copilot/catalog_test.go`, `tools_native_test.go`, `toolctx_test.go`, `format_test.go` (embeds the Task 1 `catalog.json`)

**Interfaces:**
- Produces: `type Tool struct{ Name, Executor string; Dangerous bool; Level, Label, Reason string; Schema json.RawMessage }`; `func Catalog() []Tool`, `func Schemas() []json.RawMessage` (byte-identical to `_build_tools()`), `func NeedsConfirmation(name string) bool` (= not in SAFE; unknown fails closed), `func Meta(name string) (level, label string)` (defaults `high`, `Unrecognized write tool`); native executors `func ExecNative(ctx, tx pgx.Tx, name string, args map[string]any, ws string, uid *int32) (string, bool, error)` for `search_leads`, `get_lead_detail`, `get_lead_stats`, `update_lead_status` returning the exact `json.dumps` string (incl. `default=str` for stats) or `ok=false` so the caller falls through to Python; `func ToolContext(msgs []chathistory.Message, limit int) string` (= `_conversation_tool_context`, `[:24000]` counted in code points); `func LatestToolResult(msgs, names...)`; `FormatPeopleResearch/Verification/Contacts/SignalTracking/GroundedDraft(result map[string]any) string`.
- Drift guard: a Go test fails if `catalog.json` differs from the output of `gen_copilot_catalog.py` (re-run in CI-less mode: the test compares against a committed hash that Task 1's script updates) and if any tool is neither `executor=go` with a Go implementation nor `executor=python`.
- Native-tool rules: `update_lead_status` requires `tables.write` (admin/editor) for `uid != nil` and answers `{"error": "Insufficient workspace role"}` otherwise; a missing `lead_id`/`status` key answers `{"error": "..."}` (Python would raise `KeyError` into a generic error string: hardening, documented).

- [ ] **Step 1: Write failing tests**: `TestSchemasMatchPython`, `TestNeedsConfirmationFailsClosed`, `TestCatalogEveryToolHasExecutor`, `TestSearchLeadsMatchesVector`, `TestGetLeadDetailParsesDecisionMakers`, `TestGetLeadStatsDefaultStr`, `TestUpdateLeadStatusRoleCheck`, `TestToolContextVectors` (multibyte cut at 24,000), `TestFormatVectors` (5 functions).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): copilot catalog, native tools and formatters (RED)`, `feat(go): copilot tool catalog and native lead tools`, `feat(go): trusted tool context and result formatters`.

### Task 13: Turn engine: round loop and approval gate

**Files:** Create `internal/copilot/{turn,round,gate}.go`, `internal/copilot/turn_test.go`, `round_test.go`, `gate_test.go` (frames from `testdata/copilot/frames/`)

**Interfaces:**
- Consumes: Tasks 6, 7, 8, 12.
- Produces: `type Turn struct{ Deps; WS, Slug string; UID *int32; ConvID string; ... }`; `func (t *Turn) Run(ctx context.Context, w *ssestream.Writer, messages []map[string]any) error` implementing `_stream_chat`: loop `llm` rounds with `provider_cursor`; forward `content`/`warning`/`error` frames as SSE events (`{"content":...}`, `{"warning":...}`, `{"error":...}`); on `tool_calls`: parse args (`""` -> `"{}"`, invalid JSON -> `{}`), partition safe/dangerous with `NeedsConfirmation`; safe calls run in order, each emits `tool_call` then `tool_result` (`result` is the parsed JSON of the tool string; non-JSON tool output becomes `{"raw": ...}`: hardening, Python would raise), dedup by `name|json.dumps(args, sort_keys=True, default=str)` within the turn, execution via `ExecNative` else `Unary("/internal/copilot/tool")` with 10 s keepalives; dangerous calls are **never** executed: create an approval per call (`ChatCreateApproval`), emit `confirmation_required` with keys in the order `confirmation_id, tool_call, name, args, description, level, label` (`description` from `Unary describe`; `display_call.id` = approval id), then `awaiting_confirmation: true` and `[DONE]`; round counter `>= max_rounds` (setting `CHAT_MAX_TOOL_ROUNDS`, default 8, floor 1, read once via the plan 03 settings getter) re-issues with `tools_enabled=false` and the exact budget system message; the stream is cancelled at `COPILOT_TURN_TIMEOUT_SECONDS`.
- Errors: Python stream truncation or protocol error -> one `{"error":"AI provider error: ..."}`-shaped frame with a fixed text (`AI provider stream interrupted`), then `[DONE]`; the HTTP status is already 200.

- [ ] **Step 1: Write failing tests**: `TestPlainAnswerStreamsContent`, `TestSafeToolCallRoundTrip`, `TestDuplicateSafeCallReusesResult`, `TestParallelSafeCallsOrder`, `TestDangerousCallNeverExecuted` (fake tool endpoint records zero calls), `TestUnknownToolTreatedDangerous`, `TestConfirmationEventKeyOrder`, `TestApprovalRowCreatedPerDangerousCall`, `TestRoundBudgetDisablesTools`, `TestFailoverWarningPassesThrough`, `TestPythonCrashMidStreamEmitsErrorThenDone`, `TestClientDisconnectCancelsRPC`, `TestKeepaliveDuringSlowTool` (fake clock), `TestTurnDeadline`, `TestToolTimeoutBecomesToolErrorResult`, `TestFramesReplayEqualsLegacyBytes` (every scenario fixture).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): copilot turn engine (RED)`, `feat(go): copilot round loop and tool dispatch`, `feat(go): copilot approval gate`.

### Task 14: Approved-call resume

**Files:** Create `internal/copilot/resume.go`, `internal/copilot/resume_test.go`

**Interfaces:** Produces `func (t *Turn) ResolveApproved(ctx, w *ssestream.Writer, messages *[]map[string]any, approved []Approved) error` (= `_resolve_approved_calls`): per item `ConsumeApproval(id, ws, uid, decision)` **before** any execution; an unusable approval appends the synthetic `invalid_approval` assistant+tool messages with the exact denial text and emits `tool_denied`; `approve` of a tool in the DANGEROUS set re-checks role (`tables.write`) then executes (`ExecNative` or `Unary tool`) with keepalive, emits `tool_call`, `tool_result`, plus the follow-up `content` for `create_people_workbook`, `enrich_people_contacts`, `create_source_workbook`, `track_account_signals`, `draft_grounded_outreach` using Task 12 formatters; `deny` or a non-dangerous name emits `tool_denied` with `User declined this action.` / `Tool is not an explicitly classified mutation.`; messages appended in Python's order. Cap 20 items; duplicates of one id in one request consume once (second is invalid).

- [ ] **Step 1: Write failing tests**: `TestApproveExecutesOnceUnderConcurrentResubmit` (two requests, one execution), `TestDenyNeverExecutes`, `TestExpiredApprovalDenied`, `TestWrongWorkspaceApprovalDenied`, `TestUnknownToolApprovedStillDenied`, `TestRoleReCheckedAtExecution` (viewer demoted after proposal), `TestFollowUpContentPerTool`, `TestDuplicateIdInOneRequest`, `TestMessageOrderMatchesLegacy`, `TestMoreThan20ItemsRejected`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): approved call resume (RED)`, `feat(go): resume approved and denied tool calls`.

### Task 15: Pre-route adapter and persistence

**Files:** Create `internal/copilot/{preroute,persist}.go`, `internal/copilot/preroute_test.go`, `persist_test.go`

**Interfaces:**
- Produces: `func (t *Turn) PreRoute(ctx, w *ssestream.Writer, lastUserMsg string, hasApproved bool) (handled bool, err error)` consuming `/internal/copilot/preroute` frames: `sse` frames are re-emitted via the writer (so key order and bytes come from the recorded Python event objects through `canonjson`), `persist` frames call `chathistory.AddMessage` (single writer: Go), `keepalive` is dropped (Go owns the 10 s timer), `none` returns `handled=false` without having written anything (the caller continues to the LLM path), `done` writes `[DONE]`. Approvals created by Python during pre-route live in the same table, tenant-scoped, and are consumable by Go.
- `persist.go`: `func (t *Turn) CaptureAndStore(...)` accumulating `content` and persisting every `tool_result` as a `tool` row (`"<name> result"`, `tool_data` = `json.dumps({"name","result"})` Python separators) and the assistant row after the stream, then best-effort `chatmemory.Add` of `User asked: <200>\nAssistant answered about: <200>` (code-point slicing) with `{"conversation_id","type":"episodic"}`; persistence errors never break the stream (logged without content).

- [ ] **Step 1: Write failing tests** (recorded frames): `TestPreRouteNoneFallsThrough`, `TestCompanyTeamClarificationBytes`, `TestPeopleResearchBranchPersistsToolAndAssistantRows`, `TestApprovalProposalBranchConfirmationBytes`, `TestDraftBlockedBranch`, `TestPreRouteErrorFrameEndsStreamWithErrorEvent`, `TestPreRouteDisconnectCancels`, `TestPersistToolRowShape`, `TestMemoryStoredBestEffort`, `TestMemoryFailureDoesNotBreakStream`, `TestNoAssistantRowWhenEmpty`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): preroute adapter and persistence (RED)`, `feat(go): preroute rpc adapter`, `feat(go): copilot message and memory persistence`.

### Task 16: Person pipeline and store

**Files:** Create `internal/personintel/{pipeline,store}.go`, tests

**Interfaces:** Consumes Task 11, `pyworker.Client.Call` (`ProviderCall`, provider `person_intel_deep`, assumes plan 06 `python_providers.txt` lists it; Task 3 of plan 06's RPC registers it, this plan adds the Python provider module). Produces `func Enrich(ctx, d Deps, url string, deep bool, notify func(step string, data map[string]any)) (Profile, error)` with the step sequence `parsed, searching, search_progress*, search_done, extracting, ... , completed` and payload keys identical to `services/person_intel.py`; deep scraping is bounded (10 pages, 60 s) and skipped with step `deep_unavailable` when `PyWorker` is nil; `func Save(ctx, tx pgx.Tx, p Profile) (id int64, err error)`; `func History(ctx, tx, skip, limit int) ([]HistoryRow, error)` (order `id DESC`); `Get`, `Delete`; JSON shapes and `created_at` as naive-UTC `isoformat()` (`""` when null).

- [ ] **Step 1: Write failing tests**: `TestStepSequenceMatchesRecordedRun` (scripted HTTP + fixture), `TestDeepSkippedWithoutPyWorker`, `TestDeepBoundedAndCancelled`, `TestSaveRoundTripsJSONColumns`, `TestHistoryShapeAndOrder`, `TestGetDetailKeyOrder`, `TestDeleteMissingFalse`, `TestEnrichHonoursContextCancel`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): person enrichment pipeline (RED)`, `feat(go): person enrichment pipeline`, `feat(go): person intel store`.

### Task 17: Conversation, memory and info routes

**Files:** Modify stub `internal/api/copilotroutes.go`; tests; fixtures dir `testdata/contract/copilot/`

**Interfaces:** Produces `GET|POST /api/copilotkit/info` (static `_INFO_RESPONSE` from `catalog.json`, 200, unauthenticated), `GET /api/copilotkit/conversations` (`{"conversations":[...]}`), `GET /api/copilotkit/conversations/{conv_id}` (`{"conversation","messages"}`; 404 `{"error":"Not found"}`), `DELETE ...` (`{"ok":true}`), `GET /api/copilotkit/memories` (`{"memories":[...],"available":true}`); all authorized by `copilotauth.Resolve` first.

- [ ] **Step 1: Write failing tests**: `TestCopilotRoutesMatchFixtures`, `TestInfoUnauthenticated`, `TestConversationsOwnerOnly`, `TestGetForeignConversation404Body`, `TestDeleteForeignIsNoopOk`, `TestMemoriesNamespaceIsolation`, `TestAuthStatusMatrix` (401/403/404 bodies and headers).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): copilot conversation routes (RED)`, `feat(go): copilot info, conversation and memory routes`.

### Task 18: Chat route

**Files:** Modify stub `internal/api/copilotchat.go`; tests; fixtures `testdata/contract/copilot/chat_*.sse`

**Interfaces:** Produces `POST /api/copilotkit` (SSE). Order, as Python: resolve tenant (before any side effect) -> body validation (hardening statuses) -> conversation ownership check/create (`AutoTitle` of last user message) -> store the user message -> stream-cap acquire -> `conversation_id` event -> `PreRoute` (when `handled`, done) -> `ResolveApproved` -> context assembly (system prompt via `Unary system-prompt`, `ToolContext`, memory block `Relevant memories from past conversations:\n- ...` from `chatmemory.Search(limit 5)`, `Current application context:\n<ctx>`, then client messages, with the Python message sanitiser: keep `role, content, tool_calls, tool_call_id, name`, `function` role -> `tool`, drop tool messages without `tool_call_id`, Gemini empty-content blank is applied by Python `llm` side) -> `Turn.Run` -> persistence. A mid-stream failure after headers never changes the status.

- [ ] **Step 1: Write failing tests** (fake pyworker replaying frames): `TestChatBytesMatchLegacyForEveryScenario`, `TestChatConversationCreatedBeforeStream`, `TestChatForgedConversationIDStartsFresh`, `TestChatUnauthenticated401NoSideEffects` (no conversation row), `TestChatInvalidBody422`, `TestChatBodyTooLarge413`, `TestChatTooManyMessages422`, `TestChatApprovalResubmitFlow`, `TestChatStreamCap429`, `TestChatDisconnectReleasesSlotAndCancelsRPC`, `TestChatPyWorkerDown503BeforeHeaders` (system-prompt unary fails -> 503 `AI worker unavailable` JSON before the stream starts; hardening, Python would 500), `TestChatNoSecretInLogs`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): copilot chat route (RED)`, `feat(go): copilot chat sse route`.

### Task 19: Person intel REST routes

**Files:** Modify stub `internal/api/personintelroutes.go`; tests; fixtures `testdata/contract/person/`

**Interfaces:** Produces `POST /api/person/enrich` (admin; 400 `Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username`; 400 with the `ValueError` text for unparseable usernames; 200 `{"id","status":"success","profile"}`; unexpected error -> 500 `Enrichment failed` (deviation)), `GET /api/person/history?skip&limit` (default `0`, `30`; negative values 422, deviation), `GET /api/person/{record_id}` (404 `Record not found`), `DELETE /api/person/{record_id}` (`{"status":"deleted","id":N}`). Admin gate = `identity.RequireGlobalAdmin()`.

- [ ] **Step 1: Write failing tests**: `TestPersonRoutesMatchFixtures`, `TestPersonNonAdmin403`, `TestPersonEnrichInvalidURL400`, `TestPersonHistoryPagingAndOrder`, `TestPersonDetailNotFound404`, `TestPersonDeleteTwice404`, `TestPersonRecordIDNotInteger422`, `TestPersonEnrichErrorNotEchoed`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): person intel routes (RED)`, `feat(go): person intel rest routes`.

### Task 20: Person intel WebSocket

**Files:** Modify stub `internal/api/personintelws.go`; `go.mod`/`go.sum` already pinned by plan 06; tests

**Interfaces:** Produces `GET /ws/person/enrich?token=` upgrade: `identity.AuthenticateQueryTokenCtx(..., requireAdmin=true)` before accept, else close `4403`; first client frame `{"linkedin_url","deep"}` (read limit `WS_MAX_MESSAGE_BYTES`, 15 s to send it); missing URL -> `{"step":"error","message":"No LinkedIn URL provided"}`; invalid -> the Python message; progress frames are `{"step":...,...}` JSON text frames; final `{"step":"saved","id","profile"}`; any failure `{"step":"error","message":<generic>}` (no `str(exc)`); socket then closed; one concurrent enrichment per admin (second gets `{"step":"error","message":"Enrichment already running"}`); disconnect cancels the pipeline; 120 s deadline.

- [ ] **Step 1: Write failing tests**: `TestWSRejectsBadTokenOrNonAdmin4403`, `TestWSHappyPathStepsMatchFixture`, `TestWSMissingURL`, `TestWSInvalidURL`, `TestWSOversizedFrameClosed`, `TestWSDisconnectCancelsPipeline` (goroutine baseline), `TestWSSecondConcurrentRunRejected`, `TestWSDeadline`, `TestWSNoInternalErrorTextLeaked`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): person intel websocket (RED)`, `feat(go): person intel enrichment websocket`.

### Task 21: Chaos, contract coverage, nginx split, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf` (one delimited section), `scripts/nginx-routing-check.sh` (cases only), `testdata/contract/README.md` (deviations)
- Create: `docs/go-port/runbooks/copilot-cutover.md`, `internal/api/copilot_coverage_test.go`, `internal/copilot/chaos_integration_test.go`, `testdata/contract/retained_copilot_routes.txt`

**Interfaces:** Consumes everything. Produces: nginx blocks for `/api/copilotkit` (POST `""` and `/info`, conversations, memories), `/api/person/*`, `/ws/person/enrich` following the canary bucket; chat location sets `proxy_buffering off`, `proxy_read_timeout 1h`, `proxy_http_version 1.1`, `X-Accel-Buffering` honoured, `proxy_request_buffering off` is **not** set (body is small, bounded by Go); canary is by workspace so one conversation is not split across stacks mid-turn (both stacks read the same tables, so a split is safe but confusing); the WebSocket location adds `Upgrade`/`Connection` headers and `proxy_read_timeout 300s`. Chaos test (integration, fake pyworker): kill the RPC at frame N for N in 1..all, client disconnect at every frame, slow tool beyond keepalive, two approvals resubmitted concurrently, assert: no goroutine leak, one terminal `[DONE]`, no double execution, caps released, no row written after cancel except the user message. Runbook: prerequisites (plan 01 tables backfilled, `PY_WORKER_URL`/`PY_WORKER_TOKEN`, Python replicas must run the Task 2 code so both stacks answer identically, `openmemory` NOT installed on Python workers (D3), `CHAT_REQUIRE_AUTH` equal on both stacks), canary steps 0% -> 5% -> 25% -> 100%, metrics to watch (chat 5xx, stream error frames, RPC p95 first-frame latency, tool timeouts, cap 429s), rollback (nginx 0%, no data migration), and the deviations list.

- [ ] **Step 1: Write failing tests** `TestEveryCopilotAndPersonRouteIsPorted` (parses `routers/{copilotkit,person_intel}.py`: 7 + 5 routes all in Go patterns), `TestEveryCatalogToolHasExecutor`, `TestNoSecretsInFramesOrLogs`, `TestChaos*` above.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3:** record any missing fixtures, write nginx blocks, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (all 12 paths follow the bucket; `/api/copilotkit` POST location has buffering off) -> PASS.
- [ ] **Step 5: Commit** `test(go): copilot coverage and chaos (RED)`, `feat(infra): nginx split for copilot and person intel`, `docs(go-port): copilot cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row 11 "copilotkit SSE, agent, chat_history, memory, WebSockets, person_intel": SSE and loop (7, 13-15, 18), tenant auth (10), chat_history and approvals (8), memory (9), `person_intel` REST and WebSocket (11, 16, 19, 20), LLM streaming boundary (3, 6), agent tool catalog (12). `services/agent/autopilot*` is reached only through the `execute_plan`/`draft_plan` tools, which run on Python via `tool`; no route of its own.
- **Consistency:** `pyworker.Stream/Unary` (6) used by 13, 14, 15, 18; `ssestream.Writer` (7) by 13, 15, 18; `chathistory` (8) by 12, 13, 14, 15, 17, 18; `chatmemory` (9) by 15, 17, 18; `copilotauth.Resolve` (10) by 17, 18; `copilot.Catalog/ExecNative` (12) by 13, 14.
- **Review Focus:** each line has its tests in Tasks 8, 13, 14 (1); 6, 7, 13, 18, 21 (2); 7, 13, 15, 18 (3); 8, 9, 10, 12 (4); 9 (5); 11, 20 (6).
- **Proportion:** interfaces, frame schemas, error strings, test names and file ownership carry the decisions; handler bodies and tool bodies are left to the implementer and to the recorded fixtures.

## Open decisions (defaults apply unless overridden)

- **D1 Python boundary transport.** Default (confirm): HTTP/1.1 POST with `application/x-ndjson` frames (`t`, `seq`, `d`) for `preroute` and `llm`, unary JSON for `tool`, `describe`, `system-prompt`; same bearer/`PY_WORKER_URL` as plan 06; no gRPC. Cancellation = closing the request. Alternative (gRPC bidi streaming) rejected: new toolchain and proto drift for four endpoints.
- **D2 Where the loop lives.** Default (confirm): Go owns the round loop, dedup, budget and approval gate; Python `llm` does one provider round per call (with failover). This keeps the safety gate in the typed language and makes the per-round cancellation point explicit. Cost: one extra RPC per tool round.
- **D3 Memory backend.** Python prefers the optional `openmemory` library whose data is a private SQLite file; Go reads only `chat_memories`. Default (confirm): Go and the Python worker both use the builtin PG store; `openmemory` must not be installed on Python workers at cutover (deviation if it was: its old memories stop being recalled; runbook asks the operator to confirm with `GET /api/copilotkit/memories`).
- **D4 Tools in Go.** Default: only `search_leads`, `get_lead_detail`, `get_lead_stats`, `update_lead_status` are native; the other ~21 tools stay behind `tool` RPC (they call collection, outreach drafting, workbooks and scrapers). Later plans (07-11, 13) may flip individual tools to `executor=go` by editing `catalog.json` and adding a Go executor; the drift test keeps the two lists consistent.
- **D5 Pre-routes stay Python.** Default: the eight deterministic pre-route branches (`collection_intent`, `targeted_people`, grounded drafting) run inside `preroute` on Python and stream their SSE objects; their regexes and heuristics are not ported (they depend on leadgen code that plan 14 leaves in Python).
- **D6 Hardening statuses.** Default: 422/413/429/503 as listed in Global Constraints for inputs and conditions where Python answers 500 or hangs; all recorded in the deviations README.
- **D7 `person_intel` deep scrape.** Default: Python `ProviderCall` `person_intel_deep`; without a reachable worker the step is skipped (`deep_unavailable`) rather than failing the enrichment.

## User decisions

(none yet; the defaults above apply until the user objects)
