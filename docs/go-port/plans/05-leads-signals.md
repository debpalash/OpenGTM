# Leads and Signals Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the tenant lead and signal surface to Go: 15 of the 35 `leads.py` route decorators (lead list/get/create/update/status/delete, stats, filters, timeline, similar, CSV export, fuzzy dedup + merge, bulk-enrich enqueue, the `/api/events` SSE stream) and 5 of the 6 `signals.py` routes, with contract-identical JSON, status codes, `detail` strings and auth gates, served side by side with FastAPI behind nginx.

**Architecture:** Same shape as plans 02-04: pure domain packages (`leadswrite`, `leaddedup`, `signals`, `leadevents`, `leadtimeline`, plus a list/similar extension file in plan 04's `leadsread`) sit under thin handlers in `internal/api`; every tenant-table access runs inside `tenancy.InWorkspace`. The Postgres lead store is the only store Go supports. Everything that needs the SQLite job ledger, an LLM, a scraper or an external provider stays on FastAPI (listed in Global Constraints, routed per path by nginx). The SSE stream is a Redis pub/sub relay that re-encodes events exactly like Python's `json.dumps`.

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, `github.com/redis/go-redis/v9` (already a dependency via `internal/progress`), existing `internal/{httpx,identity,tenancy,audit,contract,queue,canonjson,leadsread,config}`; no new modules. Python/uv for recorder, seed and vector scripts.

**Spec:** `docs/go-port/inventory.md` section 8 row 4 "Leads and signals CRUD" (sections 2 HTTP surface, 5 data stores, risks on SSE and RLS); `docs/go-port/plans/04-workbook-crud.md` (leadsread, conventions, nginx split, runbook); `docs/go-port/plans/02-go-api-foundation-auth-workspaces.md`; GitHub issue #33. Python sources: `legacy/python/apps/api/routers/{leads,signals}.py`, `services/leadgen/{store,models,export,progress}.py`, `services/dedup.py`, `services/signals/store.py`, `services/queue_service.py` (`add_job`).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths below (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plan 04 Tasks 1 (seed), 2 (scaffold, `Deps.Flags`, `workbooksEnabled`), 4 (`leadsread`: ordered `Lead`, `QueryLeads`, `LeadsByIDs`, `Stats`, `FilterOptions`) are merged. This plan only adds files and one `register.go` line per area; it never edits plan 04 files except `contract_seed.py` (Task 1, append-only).

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean; no new modules. Go never runs DDL (Alembic owns it); `make schema` is not needed because `leads`, `signals`, `jobs` and the timeline tables are already in `internal/db/schema.sql`.
- Tenant tables (`leads`, `signals`, `audience_membership_events`, `destination_deliveries`, `outreach_sends`, `workbook_rows` reads) only inside `tenancy.InWorkspace`; `audience_destinations`/`audiences` are read in the same transaction. `jobs` is the global control table (plain pool or the caller's tx). Runtime role non-superuser `NOBYPASSRLS`. Cross-workspace or missing ids answer 404, never 403.
- Contract parity is the release bar: paths, methods, JSON keys and key order within a response object, status codes, `detail` strings, auth gates. Gates verbatim from Python: reads = `current_workspace` = `identity.Workspace`; lead writes, dedup merge, bulk-enrich = `identity.RequireRole("tables.write", "editor", "admin")`; `signals/mark-read` = `identity.RequireRole("signals.write", "editor", "admin")`; `POST /api/signals/scan` = authenticated global admin (`get_current_admin_user`, same gate as plan 04 functions create); `GET /api/events` = no bearer header, `?token=` access token (`identity.AuthenticateQueryTokenCtx`), then workspace membership and SSO enforcement.
- Dedup run (`POST /api/leads/dedup`) is a read: `current_workspace` only, exactly like Python.
- FastAPI validation maps to 422 `{"detail":[{type,loc,msg,input}]}` via `httpx.ValidationError`; `Query(ge/le)` limits and pydantic coercions (`lead_id: int` path, `Optional[bool]` query strings `true/false/1/0/yes/no/on/off/t/f/y/n`) copied per route. `/api/leads` vs `/api/leads/` redirect parity comes from the router.
- Lead JSON is the plan 04 ordered `leadsread.Lead` (dataclass field order, `id` first). Timestamps in `leads` are strings exactly as stored; signal `created_at` is a float epoch rendered with Python float repr (`canonjson.FloatRepr`); signal `read` renders as `1`/`0`.
- Rate limits: none of the ported routes carries `@limiter.limit`. All six limited routes (`enrich` 30/min, `collect` 20/min, `search/unified` 60/min, `scraper/scrape` 30/min, `verify-email` 60/min, `tech-stack` 30/min) stay on FastAPI together with their slowapi counters, so `internal/ratelimit` is not used here. The new SSE route is protected by a stream cap instead (decision D3).
- **Retained on FastAPI (documented, not dropped), 21 routes, listed once in `testdata/contract/retained_leads_routes.txt` (Task 17):** `POST /api/lead/{id}/enrich` (SSE, OSM/DDG), `POST /api/collect`, `GET /api/jobs`, `GET /api/jobs/{id}`, `/jobs/{id}/stages`, `/jobs/{id}/leads`, `/jobs/{id}/events`, `POST /jobs/{id}/cancel`, `DELETE /jobs/{id}`, `POST /jobs/{id}/retry` (all read the per-workspace SQLite `leads.db` job ledger, which Go cannot open; decision D1), `GET /api/system-stats`, `GET /api/workspaces`, `POST /api/workspaces`, `DELETE /api/workspaces/{ws_id}` (leads.py duplicates; plan 02 already serves `/api/workspaces`, Task 17 verifies which handler wins in `main.py`), `GET /api/v2/search/unified`, `POST /api/v2/scraper/scrape`, `POST /api/leads/verify-email`, `/leads/score`, `/leads/tech-stack`, `/leads/domain-intel`, `GET /api/signals/sources` (certificate verification, plan 06).
- Scrapers and enrichment stay a Python worker: `bulk-enrich` and `signals/scan` only enqueue `bulk_enrich` / `signal_scan` jobs (`executor='python'`) that the Python worker runs. Go never writes signals (the scanner owns `add_signal` and the `on_signal` automation emit), so `internal/automationhook` is not used here; Python lead-creating routes never called `on_row_added` either (checked: `add_lead` does not).
- Go supports only the Postgres lead store: `RegisterLeads`/`RegisterSignals`/`RegisterEvents` register nothing when `PG_LEAD_STORE` is false (`Deps.workbooksEnabled`).
- SSE: the Go server has no `WriteTimeout`; handlers flush after every frame; `httpx.Gzip` already skips `text/event-stream`. Headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`. nginx must set `proxy_buffering off` and a long `proxy_read_timeout` for `/api/events` (Task 17).
- Redis in tests: `TEST_REDIS_URL` (127.0.0.1:56380). Integration tests skip when unset, like `internal/progress`.
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.

## Review Focus

1. **SSE bytes and tenancy.** Python parses each Redis payload and re-emits `json.dumps(event)` (default `", "`/`": "` separators, `ensure_ascii`, key order preserved, floats in Python repr); the heartbeat is `data: {"type": "heartbeat"}` every 15 s of silence. A compact pass-through or a Go-style escape (`<` as `<`) changes bytes. A client must only ever see `leadgen:progress:<its workspace>`; a `workspace_id` query param for a workspace the user is not a member of is 403 `Workspace access denied` before any subscription; client disconnect must close the subscription and goroutine (leak under reconnect storms). Tasks 7, 16.
2. **Dedup numeric and text parity.** Python regex `\b`, `.lower()`, `len`/slicing and `round()` are Unicode/code-point and decimal-exact; Go RE2 `\b` is ASCII-only and `len` is bytes, so a name like `éco ltd` or `İstanbul` silently changes blocking keys, scores and clusters. Block cap (skip blocks over 100), `limit <= 5000`, `threshold` in [0.5, 1.0], stable orderings (`clusters` insertion order, `pairs` by score desc then discovery order, completeness sort stable) and float repr (`1.0`, `0.9234`) must match. Task 5, 13.
3. **Mass assignment and tenancy on writes.** `PUT /lead/{id}` and `dedup/merge` write caller-shaped dicts: only the mutable column set (never `id`, `workspace_id`, `created_at`, `updated_at`, `search_tsv`) may change, `updated_at` must advance, a foreign or unknown id writes nothing and answers 404, and a type that Postgres would reject (Python answers 500) must answer 422 without a partial write (D2). Merge deletes duplicates in the same transaction as the master update (D4). Tasks 3, 10, 13.
4. **Create-only race.** `POST /lead` on an existing `(workspace, company, city)` answers 409 with `{"detail":{"message":...,"lead_id":N}}` without modifying the row, including when two requests race (exactly one 200, the other 409 with the winner's id). Empty-string `city` is a real key. Task 3, 10.
5. **Signal cursors and analytics bucketing.** Cursors are urlsafe-base64 JSON `[created_at, id]`; garbage, wrong arity, non-string id and `NaN`/`Infinity` floats (Python's `json.loads` accepts them) must give 422 `invalid signal cursor` or an empty page, never a 500; ties on `created_at` must keep `(created_at, id)` keyset order across pages. `analytics` buckets with `CAST(created_at / 86400 AS INTEGER)` which Postgres rounds (not floors); the SQL keeps that cast verbatim for byte parity and the deviation is documented. Task 6, 15.

## File Structure

```
internal/leadswrite/{store,fields}.go                      # create-only insert, status, field update, delete (+ tests)
internal/leadsread/list.go                                 # ListLeads, SimilarLeads (extends plan 04 package; new file only)
internal/leaddedup/{normalize,jaro,dedup,merge}.go         # port of services/dedup.py (+ tests, Python vectors)
internal/signals/{store,cursor,analytics}.go               # signal reads, counts, mark-read, analytics
internal/leadevents/{relay,encode}.go                      # Redis pub/sub relay + Python-compatible event encoding
internal/leadtimeline/timeline.go                          # unified account timeline read
internal/queue/enqueue.go                                  # Enqueue(ctx, q, type, payload, fireKey) for python-executor jobs
internal/api/{leadsroutes,leadswrites,leadstimeline,leadsexport,leadsdedup,leadsbulk,signalsroutes,eventsroutes}.go
internal/api/register.go                                   # (modify once, Task 2: RegisterLeads, RegisterSignals, RegisterEvents)
internal/db/queries/{leads_write,leads_list,leads_timeline,signals,jobs_enqueue}.sql  (+ gen/)
testdata/contract/{leads,leadwrites,timeline,export,dedup,signals,events}/*.json
testdata/contract/retained_leads_routes.txt
../../legacy/python/scripts/contract_seed.py               # (modify, Task 1)
../../legacy/python/scripts/contract_cases/{leads,signals,events}.yaml
../../legacy/python/scripts/gen_dedup_vectors.py           # Python-generated dedup/csv_safe/cursor/json vectors
../../infra/nginx/go-routes.conf                           # (modify, Task 17)
../../docs/go-port/runbooks/leads-signals-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time; wave 2 has 8 tasks, run as two batches). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors; 2 scaffold + enqueue helper | Task 1 owns `contract_seed.py`, `gen_dedup_vectors.py`; Task 2 owns `register.go`, `Deps` additions, all stub route files and `internal/queue/enqueue.go` + `jobs_enqueue.sql` |
| 1 | 3 leadswrite, 4 leadsread list, 5 leaddedup, 6 signals store, 7 events relay, 8 timeline | 6 disjoint packages, each with its own `.sql`; only 4 touches `internal/leadsread/` (new file `list.go`) |
| 2 | 9 lead reads, 10 lead writes, 11 timeline route, 12 CSV export, 13 dedup routes, 14 bulk-enrich, 15 signals routes, 16 SSE route | each owns exactly one stub `internal/api/*.go` file and its contract fixture dir; no two touch the same file; 9 consumes 4, 10 consumes 3, 11 consumes 8, 12 consumes 4, 13 consumes 3+5, 14 consumes 2, 15 consumes 2+6, 16 consumes 7 |
| 3 | 17 contract coverage, nginx split, runbook | consumes everything |

---

### Task 1: Python contract seed, recorder cases, vector generator

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only; plan 04 Task 1 owns the rest)
- Create: `legacy/python/scripts/contract_cases/{leads,signals,events}.yaml`, `legacy/python/scripts/gen_dedup_vectors.py`
- Create (output, committed): `testdata/contract/{leads,leadwrites,timeline,export,dedup,signals,events}/*.json`, `internal/leaddedup/testdata/vectors.json`, `internal/signals/testdata/cursors.json`, `internal/leadevents/testdata/encode.json`, `internal/api/testdata/csv_safe.json`

**Interfaces:**
- Consumes: plan 04 seed (workspace A/B, users incl. editor/viewer/admin, leads incl. ties on `score`, workbook rows referencing a lead).
- Produces: deterministic seed additions (30 signals across 5 types with equal-`created_at` ties and one unread/read mix, signals for workspace B, audience events, deliveries, outreach sends for one lead, a lead with city `""`, a lead with a non-ASCII company such as `Éco Solutions Pvt Ltd`, duplicates cluster of 4 leads) and recorded fixtures that Tasks 9-16 assert against; vector files that Tasks 5, 6, 7, 12 load.

- [ ] **Step 1: Extend the seed** with the rows above (fixed ids/timestamps; floats chosen so `round(x, 4)` is not a tie).
- [ ] **Step 2: Write the cases** (each `{name, method, path, as, expect_status}`): every ported route happy path and each error (`404 Lead not found`, `400 Invalid lead status`, 409 create collision, 400 `master_id and duplicate_ids required`, 400 `duplicate_ids must contain a lead other than master_id`, 404 `No valid duplicate leads found`, 404 `Master lead N not found`, 400 `lead_ids required`, 400 `action must be 'find_emails' or 'scrape_website'`, 422 bad cursor, 422 limit bounds, 401/403 gates for viewer vs editor vs non-member, `/api/events` 401 `Missing token`/`Invalid token`, 403 `Workspace access denied`). `/api/events` is recorded as a bounded read: first frames then close.
- [ ] **Step 3: `gen_dedup_vectors.py`** imports `services.dedup` and writes: 40 `compare_leads` pairs (ASCII, accents, Turkish dotted I, CJK, empty fields, phone/domain boosts), 6 `find_duplicates` result documents (incl. a >100 block and a 5000-lead synthetic set trimmed to hash + counts), 8 `merge_leads` cases (int `0`, `"N/A"`, `"nan"`, `"0"` values), `csv_safe` cases (`=`, `+`, `-`, `@`, tab, CR, phone-like `+91 (22) 123-4567`, `None`, ints), cursor encode/decode cases (valid, bad base64, `[1,2]`, `["x","y"]`, `[NaN,"a"]`), and 12 raw Redis payloads with their Python `json.dumps` re-encoding (non-ASCII, nested, float `1.0`, `1e-05`).
- [ ] **Step 4: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/leads.yaml,signals.yaml,events.yaml` and `uv run python scripts/gen_dedup_vectors.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 5: Commit** (Go/doc side only; Python edits are not versioned) `test(go): lead and signal contract fixtures and Python vectors (RED)`.

### Task 2: Scaffold: Deps, register lines, stubs, enqueue helper

**Files:**
- Modify: `internal/api/register.go` (+1 line each for `RegisterLeads`, `RegisterSignals`, `RegisterEvents`; `Deps` gains `Redis *redis.Client`, `SSEMaxStreams int`), `internal/config/config.go` (`REDIS_URL` into `APIConfig.RedisURL` if absent; `SSE_MAX_STREAMS` default 1000, must be >= 1)
- Create: the 8 stub files `internal/api/{leadsroutes,leadswrites,leadstimeline,leadsexport,leadsdedup,leadsbulk,signalsroutes,eventsroutes}.go` (each `func registerX(r *httpx.Router, d Deps)`, empty body), `internal/queue/enqueue.go`, `internal/db/queries/jobs_enqueue.sql`
- Test: `internal/api/leadsscaffold_test.go`, `internal/queue/enqueue_integration_test.go`

**Interfaces:**
- Produces: `func RegisterLeads(r *httpx.Router, d Deps)` (calls the 6 lead stubs), `RegisterSignals`, `RegisterEvents`; `func Enqueue(ctx context.Context, q gen.DBTX, jobType string, workspaceID string, payload any, fireKey string) (id int64, err error)` in package `queue`, mirroring `queue_service.add_job` (`executor='python'`, `priority=1`, `status='pending'`, naive-UTC `created_at`/`next_run_at`, `max_retries=3`, `retry_count=0`, `workspace_id` from the argument, payload encoded with `SetEscapeHTML(false)`).

- [ ] **Step 1: Write failing tests**: `TestLeadsNotRegisteredWithoutPGStore` (registers nothing and logs when `Flags.PGLeadStore=false`), `TestLeadsRegisteredPatterns` (patterns present exactly: the 15 + 5 routes, none of the retained ones), `TestEnqueueRowShape` (integration: row columns equal a Python-created `add_job` row for the same input, `fire_key` stored, payload key order preserved), `TestEnqueueRollsBackWithTx`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead scaffold and enqueue helper (RED)`, `feat(go): leads, signals and events scaffold`, `feat(go): python-executor job enqueue helper`.

### Task 3: Lead write store

**Files:**
- Create: `internal/db/queries/leads_write.sql`, `internal/leadswrite/{store,fields}.go`, tests

**Interfaces:**
- Consumes: `tenancy.InWorkspace`; `leadsread.LeadsByIDs` for existence checks.
- Produces: `type NewLead struct{ Company, City, Website, Email, Phone, Specialization, ContactPerson, LinkedinURL, Notes, Source string }`; `var ErrExists struct{ LeadID int64 }` as `type ExistsError struct{ LeadID int64 }`; `func Create(ctx, tx pgx.Tx, workspaceID string, n NewLead) (id int64, err error)` (all other columns at the `Lead` dataclass defaults: `score 0`, `score_tier "unqualified"`, `status "new"`, `created_at`/`updated_at` Python `isoformat()` strings, `workspace_id` forced); `func UpdateStatus(ctx, tx, id int64, status string) (found bool, err error)` (note is ignored, like Python); `func UpdateFields(ctx, tx, id int64, fields map[string]any) (found bool, err error)`; `func Delete(ctx, tx, id int64) (found bool, err error)`; `var MutableColumns map[string]ColType` (all `leads` columns minus `id`, `workspace_id`, `created_at`, `updated_at`, `search_tsv`).

- [ ] **Step 1: Write failing tests**: `TestCreateMatchesPythonRow` (every column equals the Python-created row), `TestCreateDuplicateReturnsExistsError` (also empty `city`), `TestCreateRaceExactlyOneWins` (16 goroutines), `TestUpdateFieldsIgnoresImmutable` (body with `id`, `workspace_id`, `created_at`, `updated_at`, `search_tsv` unchanged), `TestUpdateFieldsUnknownKeysDropped`, `TestUpdateFieldsAdvancesUpdatedAt`, `TestUpdateFieldsTypeError` (string into `score`, null into `company` -> typed `FieldError{Field}`, nothing written), `TestUpdateFieldsEmptyAfterFilterIsNoop` (no `updated_at` bump, like Python), `TestDeleteRemovesRowOnly` (workbook rows referencing it behave as in Python), `TestWritesTenantIsolation` (workspace B id: `found=false`, no write).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement** with `INSERT ... ON CONFLICT DO NOTHING RETURNING id`, falling back to select of the winner for `ExistsError`; `UpdateFields` builds the SET from the allow-list with typed coercion per column (int columns accept JSON integers only; text columns strings or null where nullable). **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead write store (RED)`, `feat(go): lead create-only, status, field update, delete`.

### Task 4: Lead list and similar queries

**Files:**
- Create: `internal/db/queries/leads_list.sql`, `internal/leadsread/list.go`, `internal/leadsread/list_integration_test.go`

**Interfaces:**
- Consumes: plan 04 `leadsread.Lead`, `Filter`.
- Produces: `type ListParams struct{ Status, City, Source, ScoreTier, Search, CompanySize, CollectionJobID string; ScoreMin, ScoreMax *int; HasEmail, HasPhone *bool; Limit, Offset int; OrderBy string; ExcludeDead bool }`; `func ListLeads(ctx, tx pgx.Tx, p ListParams) ([]Lead, error)` equal to `PgLeadStore.get_leads` (store.py 247-315): order allow-list `{"score DESC","score ASC","company ASC","company DESC","created_at DESC","created_at ASC","updated_at DESC","city ASC","city DESC","status ASC"}` else `score DESC`, always with `id` tiebreaker in the same direction, `search` = `websearch_to_tsquery` OR ILIKE over company/city/specialization/notes/description, `collection_job_id` also matches `source = 'job:<id>'`; `func SimilarLeads(ctx, tx, ref Lead, limit int) ([]Lead, error)` implementing `similar_leads` (search by specialization with `limit+1`, else by city, drop the reference, cut to `limit`).

- [ ] **Step 1: Write failing tests** against Task 1 seed and Python vectors: `TestListLeadsFilters` (each filter alone and combined), `TestListLeadsOrderAllowList` (unknown value and SQL-injection string fall back to `score DESC`), `TestListLeadsTieOrderStable` (pages of 3 over tied scores, no overlap), `TestExcludeDeadDefault`, `TestHasEmailEmptyStringCountsAsMissing`, `TestSimilarFallsBackToCity`, `TestSimilarExcludesSelf`, `TestListTenantIsolation`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** (single sqlc query with CASE-based ORDER BY over the allow-list; never string-concatenate `OrderBy`). **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead list and similar queries (RED)`, `feat(go): lead list ordering allow-list and similar leads`.

### Task 5: Dedup engine

**Files:**
- Create: `internal/leaddedup/{normalize,jaro,dedup,merge}.go`, tests, `testdata/vectors.json` (Task 1)

**Interfaces:**
- Produces: `func NormalizeCompany(s string) string`, `NormalizeDomain`, `NormalizePhone`, `NormalizeEmail`; `func JaroWinkler(a, b string) float64`; `type Lead = map[string]any` ordered input is `[]Rec` where `type Rec struct{ Keys []string; Vals map[string]any }`; `func Compare(a, b Rec) MatchResult`; `func FindDuplicates(leads []Rec, threshold float64) Result` where `Result` marshals with keys in Python order (`clusters`, `pairs`, `stats`, `merge_suggestions`; or the 4-key short form for fewer than 2 leads) and floats in Python repr via `canonjson.FloatRepr`; `func MergeLeads(master Rec, dups []Rec) Rec` (waterfall merge with Python truthiness: int `0` is "empty", `"0"`, `"N/A"`, `"nan"` count as empty strings; `source` joined `|` sorted; `score` max).

- [ ] **Step 1: Write failing tests** from the Python vectors: `TestNormalizeMatchesPython` (accents, `İ`, CJK, suffix words at Unicode boundaries such as `éco`), `TestJaroWinklerMatchesPython` (exact float equality after `round(,3|4)` via decimal-exact formatting), `TestCompareBoosts` (domain 0.90, phone 0.92), `TestFindDuplicatesMatchesPython` (deep byte equality of marshalled JSON for the 6 documents), `TestBlockOver100Skipped`, `TestClusterAndPairOrder`, `TestMergeLeadsMatchesPython`, `TestFindDuplicates5000Bounded` (completes < 2 s, no allocation blow-up).
- [ ] **Step 2: Run** `go test ./internal/leaddedup/...` -> FAIL. **Step 3: Implement** on `[]rune`; hand-written Unicode word-boundary matcher for the suffix list (RE2 `\b` is ASCII-only); `strings.ToLower` plus a table for the cases Python `.lower()` differs on that the vectors expose; round with `strconv.ParseFloat(strconv.FormatFloat(x,'f',n,64))`. **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead dedup engine vectors (RED)`, `feat(go): fuzzy lead dedup and waterfall merge`.

### Task 6: Signals store

**Files:**
- Create: `internal/db/queries/signals.sql`, `internal/signals/{store,cursor,analytics}.go`, tests

**Interfaces:**
- Consumes: `tenancy.InWorkspace`.
- Produces: `type Signal struct{ ID, WorkspaceID string; LeadID *int64; Company, SignalType, Title, Description, Source, SourceURL string; Weight *int64; CreatedAt float64; Read int }` with JSON keys in Python order (`id, workspace_id, lead_id, company, signal_type, title, description, source, source_url, weight, created_at, read`); `func List(ctx, tx, signalType string, leadID *int64, limit, offset int) ([]Signal, error)` (`created_at desc, id desc`); `func Page(ctx, tx, signalTypes []string, leadID *int64, limit int, cursor string) (Page, error)` (`Page{Signals, NextCursor *string, HasMore bool}`; returns `ErrBadCursor`); `func EncodeCursor(createdAt float64, id string) string`; `func Counts(ctx, tx) (Counts, error)` (type counts sorted by name, then `total`, `unread`); `func MarkRead(ctx, tx, ids []string) error`; `func Analytics(ctx, tx, days int, now time.Time) (AnalyticsResult, error)` with Python key order and `momentum_pct` rounded half-even to 1 decimal, `null` when previous is 0.

- [ ] **Step 1: Write failing tests**: `TestCursorVectors` (Task 1 file: valid roundtrip, missing padding accepted, bad base64, arity, non-string id, `NaN` -> `ErrBadCursor` or empty page exactly as the vector says), `TestListOrderTieBreak`, `TestPageKeysetAcrossTies` (25 signals sharing one `created_at`, page size 7), `TestPageFilterByTypeAndLead`, `TestCountsKeys`, `TestMarkReadOnlyOwnWorkspace` (ids from workspace B untouched), `TestAnalyticsMatchesPython` (fixed `now`, 30/90 days, empty workspace, bucket rounding quirk), `TestSignalsTenantIsolation`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** Keep `CAST(created_at / 86400 AS INTEGER)` verbatim. **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): signals store (RED)`, `feat(go): signals list, cursor pages, counts, mark-read, analytics`.

### Task 7: Progress events relay

**Files:**
- Create: `internal/leadevents/{relay,encode}.go`, tests, `testdata/encode.json` (Task 1)

**Interfaces:**
- Consumes: `*redis.Client`.
- Produces: `func Channel(workspaceID string) string` = `leadgen:progress:<id>`; `func EncodeEvent(raw []byte) ([]byte, error)` (decode with `canonjson.Decode`, re-encode like Python `json.dumps` default: `", "`/`": "` separators, `ensure_ascii`, key order kept, `FloatRepr` floats; invalid JSON returns an error and the caller skips the frame); `func HeartbeatFrame() []byte` = `data: {"type": "heartbeat"}\n\n`; `func Frame(event []byte) []byte`; `type Relay struct{...}`; `func NewRelay(c *redis.Client, heartbeat time.Duration) *Relay`; `func (r *Relay) Stream(ctx context.Context, workspaceID string, w io.Writer, flush func()) error` (subscribes to exactly one channel, writes frames, heartbeat after `heartbeat` of silence, nil client or lost Redis => reconnect with backoff 1 s..5 s while still sending heartbeats, returns when `ctx` is done and always closes the subscription).

- [ ] **Step 1: Write failing tests**: `TestEncodeEventMatchesPython` (12 vectors), `TestEncodeEventKeepsKeyOrderAndFloats`, `TestHeartbeatBytes`, `TestStreamDeliversOnlyOwnWorkspace` (integration, publish to A and B), `TestStreamHeartbeatAfterSilence` (injected short interval), `TestStreamClosesSubscriptionOnCancel` (no goroutine or `PUBSUB NUMSUB` left), `TestStreamSurvivesRedisRestart`, `TestStreamSkipsMalformedPayload`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS (race detector: `go test -race ./internal/leadevents/...`).
- [ ] **Step 5: Commit** `test(go): progress event relay (RED)`, `feat(go): redis progress relay with python-compatible encoding`.

### Task 8: Account timeline reader

**Files:**
- Create: `internal/db/queries/leads_timeline.sql`, `internal/leadtimeline/timeline.go`, `internal/leadtimeline/timeline_integration_test.go`

**Interfaces:**
- Consumes: `signals.List` (Task 6 is not required; own SQL over `signals`), `leadsread.Lead` timestamps.
- Produces: `type Item struct{ ID, Kind, Title, Description, Status string; OccurredAt float64; Metadata map[string]any }` (JSON key order `id, kind, title, description, status, occurred_at, metadata`); `type Page struct{ Items []Item; NextBefore *float64; NextBeforeID *string }`; `func Build(ctx, tx pgx.Tx, workspaceID string, lead leadsread.Lead, limit int, before *float64, beforeID string) (Page, error)` equal to `lead_timeline` (lead_timeline in leads.py 190-276): up to 200 rows each from signals, audience membership events, destination deliveries, outreach sends, plus `lead:created|updated|enriched` rows when the timestamp is non-zero; `_timeline_timestamp` semantics (epoch number, naive datetime as UTC, ISO string with `Z`, anything else 0.0); sort `(occurred_at, id)` descending; `before` filter `(occurred_at, id) < (before, before_id)`.

- [ ] **Step 1: Write failing tests** against the seed: `TestTimelineMatchesPython` (recorded fixture), `TestTimestampParsing` (`Z`, offset, naive, empty, garbage), `TestTimelinePagingStable` (limit 3 walk until `next_before` is null, no duplicates), `TestTimelineOnlyThisLead`, `TestTimelineTitleCaseEvent` (`"Joined X"` from `event_type.title()`), `TestTimelineTenantIsolation` (events of workspace B with the same lead id absent).
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** (one SQL per source, each `LIMIT 200`). **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead timeline (RED)`, `feat(go): unified lead timeline reader`.

### Task 9: Lead read routes

**Files:**
- Modify: `internal/api/leadsroutes.go` (stub from Task 2)
- Test: `internal/api/leadsroutes_test.go`, `internal/api/leads_contract_integration_test.go`

**Interfaces:**
- Consumes: Task 4 `ListLeads`/`SimilarLeads`, plan 04 `leadsread.{LeadsByIDs,Stats,FilterOptions}`, `identity.Workspace`.
- Produces: `GET /api/leads`, `GET /api/stats`, `GET /api/filters`, `GET /api/lead/{lead_id}`, `GET /api/lead/{lead_id}/similar`. `/api/leads` query: `status, city, source, score_min, score_max, tier, search, workspace_id (ignored), has_email, has_phone, company_size, limit=200, offset=0, order_by="score DESC"`; `"__all__"` and `""` mean unset (`_clean`); `exclude_dead = status != "dead"`; body is a bare JSON array of leads. `/api/filters` = `{cities, sources, statuses, tiers}` with `statuses=["new","contacted","qualified","negotiating","converted","dead"]`, `tiers=["hot","warm","cold","unqualified"]`. 404 `Lead not found`. `similar` returns `{reference, count, similar_leads}` with `limit` default 10.

- [ ] **Step 1: Write failing tests**: `TestListLeadsContract`, `TestListLeadsCleanAllSentinel`, `TestListLeadsBadBool422`, `TestListLeadsNegativeLimitOffset422` (D2), `TestListLeadsLimitCappedAt10000`, `TestStatsMatchesPython`, `TestFiltersStatusesConstant`, `TestGetLead404`, `TestGetLeadNonIntPath422`, `TestSimilarContract`, `TestReadsRequireWorkspace` (no/foreign `X-Workspace-Id`), `TestReadsTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement** the handlers. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): lead read routes (RED)`, `feat(go): lead list, stats, filters, detail, similar routes`.

### Task 10: Lead write routes

**Files:**
- Modify: `internal/api/leadswrites.go`
- Test: `internal/api/leadswrites_test.go`

**Interfaces:**
- Consumes: Task 3 store, `identity.RequireRole("tables.write", "editor", "admin")`, `audit` (only if Python audits these routes: it does not, so none).
- Produces: `POST /api/lead` (body `AddLeadRequest`: `company` required, strings default `""`, `source` default `"manual"`; 200 `{"ok":true,"id":N}`; collision 409 `{"detail":{"message":"A lead already exists for this company and city. Update the existing lead instead.","lead_id":N}}`), `PUT /api/lead/{lead_id}` (any JSON object; 200 `{"ok":true}`), `POST /api/lead/{lead_id}/status` (`{status, note=""}`; 400 `Invalid lead status` when not in the six statuses; 404 `Lead not found`), `DELETE /api/lead/{lead_id}`. Existence check precedes the write (404 `Lead not found`).

- [ ] **Step 1: Write failing tests**: `TestAddLeadCreatesAndReturnsId`, `TestAddLeadConflict409Shape`, `TestAddLeadMissingCompany422`, `TestPutLeadUpdatesAllowedFields`, `TestPutLeadIgnoresIdAndWorkspaceId`, `TestPutLeadWrongType422` (D2, nothing written), `TestPutLeadNonObjectBody422`, `TestStatusInvalid400`, `TestStatusNoteIgnored`, `TestDeleteLead`, `TestWriteRoutesViewerForbidden403` (exact Python detail from the recorder), `TestWriteRoutesCrossWorkspace404`, `TestWritesAdvanceUpdatedAt`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead write routes (RED)`, `feat(go): lead create, update, status, delete routes`.

### Task 11: Timeline route

**Files:**
- Modify: `internal/api/leadstimeline.go`
- Test: `internal/api/leadstimeline_test.go`

**Interfaces:**
- Consumes: Task 8 `leadtimeline.Build`.
- Produces: `GET /api/lead/{lead_id}/timeline?limit=100&before=&before_id=`: `limit` `ge=1 le=200`, `before` float, `before_id` `max_length=255`; 404 `Lead not found`; body `{items, next_before, next_before_id}`.

- [ ] **Step 1: Write failing tests**: `TestTimelineContract`, `TestTimelineLimitBounds422`, `TestTimelineBeforeNotFloat422`, `TestTimelineBeforeIDTooLong422`, `TestTimeline404`, `TestTimelineCrossWorkspace404`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): timeline route (RED)`, `feat(go): lead timeline route`.

### Task 12: CSV export route

**Files:**
- Modify: `internal/api/leadsexport.go`
- Test: `internal/api/leadsexport_test.go`, `internal/api/testdata/csv_safe.json` (Task 1)

**Interfaces:**
- Consumes: Task 4 `ListLeads` (`Limit: 10000`, `OrderBy: "score DESC"`, `ExcludeDead: false`, no `_clean`: the raw `status, city, tier, score_min` values filter).
- Produces: `GET /api/export/csv`; `func csvSafe(v any) any` (leading `'` for text starting with `= + - @ \t \r` unless it matches `^[+-]?[0-9][0-9 ().-]*$`; `nil` -> `""`); response `text/csv` with `Content-Disposition: attachment; filename=opengtm_leads.csv`, header row of exactly 27 titles in Python order, `\r\n` line endings, `encoding/csv` quoting identical to Python `csv.writer` (minimal quoting).

- [ ] **Step 1: Write failing tests**: `TestCSVSafeVectors`, `TestExportHeaderRow`, `TestExportBytesMatchPython` (recorded fixture, incl. commas, quotes, newlines, non-ASCII), `TestExportFormulaCellsNeutralised`, `TestExportFiltersRaw` (no `__all__` handling), `TestExportCapped10000`, `TestExportTenantIsolation`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** (build in memory like Python; 10,000 rows is bounded). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): lead CSV export (RED)`, `feat(go): lead CSV export with formula neutralisation`.

### Task 13: Dedup routes

**Files:**
- Modify: `internal/api/leadsdedup.go`
- Test: `internal/api/leadsdedup_test.go`

**Interfaces:**
- Consumes: Task 5 engine, Task 4 `ListLeads`, Task 3 `UpdateFields`/`Delete`, `leadsread.LeadsByIDs`.
- Produces: `POST /api/leads/dedup?threshold=0.85&city=&source=&tier=&search=&limit=2000` (`threshold` `ge=0.5 le=1.0`, `limit` `ge=1 le=5000`, `_clean` filters; empty set returns `{"stats":{"total_leads":0,"duplicates_found":0},"pairs":[],"clusters":{},"scanned":0}`; otherwise engine result plus `scanned`); `POST /api/leads/dedup/merge` body `{master_id, duplicate_ids}`: 400 `master_id and duplicate_ids required`, 404 `Master lead {id} not found`, 400 `duplicate_ids must contain a lead other than master_id`, 404 `No valid duplicate leads found`; success `{"status":"merged","master_id","duplicates_deleted","merged_lead"}`. Master update plus deletes run in ONE transaction (D4); non-integer ids answer the same 400 as missing ids (D2).

- [ ] **Step 1: Write failing tests**: `TestDedupRunMatchesPython`, `TestDedupBounds422`, `TestDedupEmptyShape`, `TestDedupFiltersScopeScanned`, `TestMergeSuccessMatchesPython`, `TestMergeErrorsInPythonOrder` (each of the four above), `TestMergeIgnoresDuplicateIdsRepeatedAndMaster`, `TestMergeNonIntegerIds400`, `TestMergeIsAtomic` (forced failure on delete rolls back the master update), `TestMergeCrossWorkspaceIds404`, `TestMergeRequiresEditor`, `TestDedupRunAllowsViewer`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): dedup routes (RED)`, `feat(go): lead dedup analysis and merge routes`.

### Task 14: Bulk-enrich enqueue route

**Files:**
- Modify: `internal/api/leadsbulk.go`
- Test: `internal/api/leadsbulk_test.go`

**Interfaces:**
- Consumes: Task 2 `queue.Enqueue`, `identity.RequireRole("tables.write", ...)`.
- Produces: `POST /api/leads/bulk-enrich` body `{lead_ids: []int, action="find_emails"}`: ids de-duplicated keeping first occurrence order, cut to 100; 400 `lead_ids required` when empty after cut; 400 `action must be 'find_emails' or 'scrape_website'`; enqueues job type `bulk_enrich` with payload keys in order `job_id, ids, action, workspace_id, slug` (`job_id` = `uuid4().hex`, `slug` from the workspace ctx) and no `fire_key`; 200 `{"ok":true,"job_id","count","action"}`. Validation order: pydantic body (422) then ids then action.

- [ ] **Step 1: Write failing tests**: `TestBulkEnrichEnqueuesPythonJob` (row equals a Python-created one: executor, type, payload bytes, priority, status), `TestBulkEnrichDedupAndCap100`, `TestBulkEnrichEmpty400`, `TestBulkEnrichBadAction400`, `TestBulkEnrichBodyNotInts422`, `TestBulkEnrichRequiresEditor`, `TestBulkEnrichEnqueueFailureIs500NoLeak` (Python lets the exception surface as 500; Go answers a generic 500, no error text).
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): bulk enrich enqueue (RED)`, `feat(go): bulk enrich enqueue route`.

### Task 15: Signals routes

**Files:**
- Modify: `internal/api/signalsroutes.go`
- Test: `internal/api/signalsroutes_test.go`

**Interfaces:**
- Consumes: Task 6 `signals.*`, Task 2 `queue.Enqueue`, `identity.RequireRole("signals.write", "editor", "admin")`, global-admin gate.
- Produces: `GET /api/signals` (`signal_type, lead_id, limit=50 [1,200], offset=0 ge 0, cursor max_length 1024`; with cursor the body key order is `signals, next_cursor, has_more, limit, offset(null), counts, signal_types`, without cursor `signals, has_more, next_cursor, limit, offset, counts, signal_types`; `signal_types` maps the 8 `SIGNAL_TYPES` keys to labels in Python order: hiring, funding, tech_change, website_change, news, growth, job_change, social_activity; bad cursor 422 `{"detail":"invalid signal cursor"}`), `GET /api/signals/counts`, `POST /api/signals/mark-read` (`{signal_ids: []string}` -> `{"status":"ok","count":len(ids)}`), `GET /api/signals/analytics?days=30` (`ge=7 le=90`), `POST /api/signals/scan` (enqueues `signal_scan` with payload `{}`, no workspace, no fire_key; `{"status":"started"}`; 403 for non-admin with the Python detail).

- [ ] **Step 1: Write failing tests**: `TestListSignalsOffsetShape`, `TestListSignalsCursorShape` (key order differs), `TestListSignalsBadCursor422`, `TestListSignalsLimitBounds422`, `TestSignalsCountsContract`, `TestMarkReadCountEchoesRequest`, `TestMarkReadViewerForbidden`, `TestMarkReadForeignIdsUntouched`, `TestAnalyticsContract`, `TestAnalyticsDaysBounds422`, `TestScanEnqueuesSignalScan`, `TestScanRequiresAdmin`, `TestSignalsTenantIsolation`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): signals routes (RED)`, `feat(go): signals list, counts, mark-read, analytics, scan routes`.

### Task 16: SSE events route

**Files:**
- Modify: `internal/api/eventsroutes.go`
- Test: `internal/api/eventsroutes_test.go`, `internal/api/events_integration_test.go`

**Interfaces:**
- Consumes: Task 7 `leadevents.Relay`, `identity.AuthenticateQueryTokenCtx`, identity membership and `Store.EnforceSSO`, `Deps.Redis`, `Deps.SSEMaxStreams`.
- Produces: `GET /api/events?token=&workspace_id=`, registered WITHOUT the `Workspace` middleware: errors before the stream use the normal JSON error format (401 `Missing token`, 401 `Invalid token`, 403 `Workspace access denied`, 403 `This workspace requires SSO. Sign in with your organization identity.`); target workspace = `workspace_id` or the user's active workspace; success streams via `Relay.Stream` with the SSE headers and `http.Flusher`; a process-wide semaphore of `SSEMaxStreams` answers 503 `{"detail":"Too many event streams"}` when full (D3). Handler must not set write deadlines and must not log `r.URL.RawQuery`.

- [ ] **Step 1: Write failing tests**: `TestEventsMissingToken401`, `TestEventsRefreshTokenRejected401`, `TestEventsNonMemberWorkspace403`, `TestEventsNoActiveWorkspace403`, `TestEventsSSORequired403`, `TestEventsStreamsPublishedEvent` (integration: bytes equal Python re-encoding), `TestEventsHeartbeat`, `TestEventsHeadersAndFlush`, `TestEventsClientDisconnectReleasesSlot` (semaphore and subscription freed), `TestEventsStreamCap503`, `TestEventsGzipSkippedBehindMiddleware`, `TestEventsTokenNotLogged`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/api/... ` and `make test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): events SSE route (RED)`, `feat(go): authenticated SSE progress stream`.

### Task 17: Contract coverage, nginx split, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf`, `scripts/nginx-routing-check.sh` (cases only)
- Create: `testdata/contract/retained_leads_routes.txt`, `docs/go-port/runbooks/leads-signals-cutover.md`, `internal/api/leads_coverage_test.go`

**Interfaces:**
- Consumes: everything; the 20 ported patterns from Tasks 9-16, the 21 retained paths from Global Constraints.
- Produces: per-route nginx blocks (ported paths follow the canary bucket; retained paths always `py`; `/api/events` gets `proxy_buffering off; proxy_read_timeout 1h; proxy_http_version 1.1; add_header X-Accel-Buffering no;` and sticky behaviour is unnecessary because both stacks read the same Redis channel); runbook with rollback (set bucket to 0), the Redis requirement, deviations list and the open-decision outcomes.

- [ ] **Step 1: Write failing test** `TestEveryPythonLeadSignalRouteIsPortedOrRetained` (parses `legacy/python/apps/api/routers/{leads,signals}.py` decorators; fails on any route that is in neither the Go router patterns nor `retained_leads_routes.txt`; expected 20 + 21 = 41) and `TestRetainedRoutesNotRegisteredInGo`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3:** resolve which `/api/workspaces` handler wins in `main.py` (note in runbook), record any missing fixtures with the recorder, write the nginx block, runbook and deviations (D1-D6, signal counts ordering, analytics bucket cast, merge atomicity).
- [ ] **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100%: retained paths always `py`, `/api/leads` and `/api/events` follow the bucket, `/api/collect` and `/api/jobs/x` stay `py` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead and signal route coverage (RED)`, `feat(infra): nginx per-route split for leads and signals`, `docs(go-port): leads and signals cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row "leads router CRUD/export/SSE, signals, dedup, Redis pubsub + SSE": lead CRUD/stats/filters/similar (Tasks 3, 4, 9, 10), timeline (8, 11), export (12), dedup (5, 13), bulk-enrich enqueue (2, 14), signals (6, 15), Redis pub/sub + SSE (7, 16). Every one of the 41 route decorators is either ported (20) or listed as retained (21) and a test (Task 17) fails on any route in neither list. Rate limits: all six `@limiter.limit` routes are retained, so none is ported; stated in Global Constraints.
- **Consistency:** `leadsread.Lead`/`ListParams` (Task 4), `leadswrite.{Create,UpdateStatus,UpdateFields,Delete,ExistsError,FieldError}` (3), `leaddedup.{FindDuplicates,MergeLeads,Rec}` (5), `signals.{Page,List,Counts,MarkRead,Analytics,ErrBadCursor}` (6), `leadevents.Relay` (7), `leadtimeline.Build` (8), `queue.Enqueue` (2) are each defined once and referenced by name later.
- **Review Focus:** each line has its test in Tasks 3, 5, 6, 7, 10, 13, 15, 16.
- **Proportion:** interfaces, detail strings, key orders and test names carry the decisions; SQL and handler bodies are left to the implementer.

## Open decisions (defaults apply unless overridden)

- **D1 Job ledger.** Collection (`/collect`, `/jobs*`, `enrich`) stays on FastAPI because jobs/stages live in per-workspace SQLite `leads.db`. Default: retained; revisit when the ledger moves to Postgres (separate plan, with enrichment, plan 06).
- **D2 Crash-to-422 hardening.** Negative `limit`/`offset` on `/api/leads`, wrong-typed or null-into-NOT-NULL values in `PUT /lead/{id}`, and non-integer ids in `dedup/merge` produce a Python 500 or an unhandled error; Go answers 422 (or the existing 400 for merge), writes nothing, and `limit` is capped at 10,000. Default: harden, documented deviation.
- **D3 SSE resource cap.** Python has no cap on concurrent streams; Go adds `SSE_MAX_STREAMS` (default 1000, 503 when full) and, without Redis, streams heartbeats only (Go processes never emit progress themselves). Default: cap on, Redis required for real events.
- **D4 Merge atomicity.** Python commits the master update and each delete separately; Go does one transaction. Default: atomic, documented deviation.
- **D5 Dedup text semantics.** Reimplement `\b`, lowercase and rounding Unicode-correctly to match Python vectors rather than ASCII-only RE2 behaviour. Default: Python parity, verified by vectors.
- **D6 Ordering of `signals` counts.** Python's group-by order is unspecified; Go sorts type names ascending (then `total`, `unread`). Default: sorted, documented.

## User decisions

(none yet; the defaults above apply until the user objects)

## User decisions (2026-10-26) — defaults applied under the standing rules (harden security/crash bugs as documented deviations, otherwise parity; scrapers stay a Python worker)

- D1: collection, `/api/jobs*` and enrich routes stay on Python until the job ledger moves to Postgres.
- D2: crash-class inputs answer 422 (non-integer merge ids 400 with the same detail); `/api/leads` limit capped at 10,000; documented deviations.
- D3: `SSE_MAX_STREAMS=1000` (503 when full); Redis required for real events, heartbeats only without it.
- D4: dedup merge is atomic (one transaction).
- D5: Unicode-correct dedup matching Python, verified by Python-generated vectors.
- D6: signal counts sorted by type name (documented).
