# Outreach Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the outreach domain to Go: the 22 `/api/outreach/*` routes (2 grounded-draft reads, 11 sequence routes, 3 SMTP routes, 3 suppression routes, public unsubscribe GET/POST, the platform bounce webhook), the `send` job (at-most-once SMTP send with send window, suppression, hourly/daily caps, caps reservation and debit-on-success), the `outreach_inbound_poll` job (IMAP DSN/ARF/heuristic bounce ingestion with the idempotent ledger), the sequence tick, and the signed unsubscribe tokens, so that Go owns the whole BYO-SMTP outreach path and Python only keeps the LLM-backed `POST /api/campaigns/generate`.

**Architecture:** The outreach store already lives in Postgres behind RLS (`outreach_*` tables, `PgOutreachStore`); `services/outreach/sequence.py` (SQLite `outreach.db`) is dead code (no caller; Task 1 proves it) and is not ported. Go adds `internal/{emailnorm,outreachtoken,outreachcfg,smtpmail,smtpclient,imapclient,dsnparse,outreachstore,outreachsend,outreachinbound}` and route files under `internal/api`. All DB work runs in short `tenancy.InWorkspace` transactions that are never held open across SMTP or IMAP network I/O (the `in_flight` marker is committed before the SMTP handoff, exactly like Python). Customer-configured SMTP/IMAP hosts are dialed through a new `egress` dial guard. The cross-tenant `message_id` lookup used by the bounce webhook (Python uses an owner connection with no GUC) becomes a `SECURITY DEFINER` function created by Alembic. Python's scheduler remains the only ticker (`bootstrap_outreach_schedules`, `bootstrap_inbound_schedules`); it enqueues with an `executor` chosen by flag, and Go exposes `outreachsend.Tick` for the in-request `start` route and for a later Go scheduler.

**Tech Stack:** Go 1.27.3 stdlib (`net/smtp`, `mime`, `net/mail`, `crypto/hmac`), pgx v5 + sqlc, `golang.org/x/net/idna` and `golang.org/x/text/unicode/norm` (pinned, Task 6), `github.com/emersion/go-imap/v2` (pinned, decision D4, incl. `imapmemserver` for tests); existing `internal/{httpx,identity,tenancy,audit,contract,queue,secrets,egress,billing,canonjson,leadsread,ratelimit,config}`. Python/uv for recorder, seed, vectors and Alembic.

**Spec:** `docs/go-port/inventory.md` section 8 row 8 "Outreach" (sections 2 HTTP surface, 3 job types `send` and `outreach_inbound_poll`, 5 stores, 6 scheduler, 7 SMTP/IMAP, 9 risks); `docs/go-port/plans/06-enrichment-jobs.md` (`queue.EnqueueOnce`, `billing.CheckAndDebit`, executor routing pattern, `Deps` additions); plan 03 (`secrets.Resolver`/`Envelope`); plan 05 (`leadsread`). Python sources: `apps/api/routers/{outreach,campaigns}.py`, `services/outreach/{store,sending,sender,inbound,dsn_parse,tokens,normalize,orm_models,drafting}.py`, `migrations/versions/{b2c3d4e5f6a7_outreach_rls,d4e5f6a7b8c9_outreach_inbound,0a1b2c3d4e5f_grounded_outreach_drafts}.py`.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`, `migrations/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 02-06 merged (identity, tenancy, secrets, `leadsread`, `queue.Enqueue`/`EnqueueOnce`, `billing.CheckAndDebit`, `Deps.Limiter`, Go worker claim loop and `queue.HandBack`). Plans 07 (connectors) and 08 (automations) are written concurrently: this plan consumes exactly one symbol from plan 08 (`caps` reservation, below) and none from plan 07. **assumes plan 08:** `automations/caps` exposes `TryReserve(ctx, tx, workspaceID, triggerID string, costUSD float64, idemKey string) (Reservation, error)`, `Reservation.OK bool`, `Settle(ctx, tx, Reservation) error`, `Release(ctx, tx, Reservation) error` (Python `caps.try_reserve/settle/release`). `outreachsend` depends on a local `Caps` interface (Task 5) so the plan compiles without it; with `OUTREACH_SEND_COST_USD > 0` and no `Caps` wired, the Go worker hands the job back to Python (D7).

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL: Alembic in `legacy/python/migrations` owns every schema change (Tasks 2, 4). New modules: only `golang.org/x/net` (idna), `golang.org/x/text` (norm) and `github.com/emersion/go-imap/v2` (D4), pinned.
- Tenant tables only inside `tenancy.InWorkspace` (API and worker). Runtime role non-superuser `NOBYPASSRLS`. Cross-workspace or unknown ids answer 404, never 403. SMTP/IMAP passwords, the unsubscribe signing key and the bounce webhook secret never appear in payloads, logs, fixtures, receipts, error strings or `audit` metadata (`GET /smtp/status` never returns the password).
- Contract parity is the release bar: paths, methods, JSON key order, status codes, `detail` strings, auth gates, rate limits. Gates verbatim: reads = `current_workspace`; every mutation = `RequireRole("outreach.write","admin")` (Python `require_workspace_role("admin", permission="outreach.write")`); unsubscribe GET/POST = public (token); `webhooks/bounce` = public + `X-Webhook-Secret` (503 `bounce webhook not configured` when the secret is unset, checked first; 403 `invalid signature`; 404 `message not found`).
- Behaviour parity of job output is the release bar: `outreach_sends` row transitions (`in_flight` committed before SMTP, terminal statuses and `skip_reason` strings), enrollment transitions, idempotency keys (`seq:<ws>:<seq>:<enrollment>:<step>`), `fire_key` strings (`inbound:<ws>:<iso>`), receipts in the inbound ledger. Python-generated vectors for every algorithm (email normalization, token sign/verify in both directions, send window, DSN parse corpus, bounce classification, breaker, template render, header sanitize); never hand-written.
- Security and crash hardening deviates from Python only as a documented deviation (D2-D6, D8) recorded in `testdata/contract/README.md`: SSRF-guarded dial on SMTP/IMAP hosts (`egress`, dial-time IP check); HTML-escaped unsubscribe landing; 422 instead of 500 for values the Postgres column types reject; bounded `enroll` list; real client IP and bounded memory in the public rate limiter; no panic when the master key is unset.
- Executors: `send` and `outreach_inbound_poll` are enqueued by Python with `executor='go'` only when `GO_EXECUTOR_OUTREACH_SEND` / `GO_EXECUTOR_OUTREACH_INBOUND` are true (default false); Go never claims a Python row and the at-most-once marker (`uq_outreach_send_idem`) makes a double claim a replay-skip, never a double send.
- Master switch: Go honours `AUTOMATIONS_ENABLED` exactly like `handle_send` (job returns without effect when off) and `OUTREACH_INBOUND_POLL_ENABLED` for inbound.
- Redis not used. Postgres in tests: the existing `testpg` helper; SMTP in tests: `internal/testsmtp` (Task 11); IMAP in tests: `imapmemserver`. Nothing touches the internet.
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (documented in `testdata/contract/retained_outreach_routes.txt`, Task 26): `POST /api/campaigns/generate` (LLM, D1). `services/outreach/drafting.create_grounded_draft` has no route; its callers (automation actions, MCP, agent) are in plans 08/11 and import it from Python until then.

## Review Focus

1. **At-most-once send.** `send_email` is not idempotent. The `in_flight` row must be committed (own transaction) before the SMTP dial; a retry or a second claimer that finds `in_flight` finalizes `skipped/replay` and never sends; a crash between SMTP success and finalize is a replay-skip (Python parity, accepted); the debit is `run:<idem>` idempotent and happens only after SMTP success; a failed or transient send is never charged and releases its caps reservation. No transaction or row lock is held across the SMTP exchange. Tasks 13, 16, 19.
2. **Unsubscribe and suppression compliance.** The token HMAC key is `sha256("outreach-unsub:" + master)`; tokens already in sent emails (90-day TTL) must verify in Go and Go tokens must verify in Python (both directions, including non-ASCII emails: Python `json.dumps` default `ensure_ascii`). POST rejects expired and forged tokens, GET renders for expired ones; one-click POST is idempotent and takes no cookie/CSRF; the same canonical email keys (`emailnorm`) are used at enroll, send, suppress, webhook and unsubscribe; a suppressed address is re-checked immediately before send; locked (`unsubscribe`/`complaint`) suppressions cannot be removed (403). Tasks 6, 7, 14, 16, 23, 25.
3. **SSRF and credential tenancy on customer-configured hosts.** `SMTP_HOST`/`IMAP_HOST` are attacker-controllable by a workspace admin: loopback, link-local, private, IPv4-mapped IPv6 and DNS rebinding at dial time must be refused (STARTTLS and implicit TLS both verify certificates); workspace A's SMTP/IMAP secrets never serve workspace B; `smtp/test` cannot be used as an open relay probe (recipient validated, 10/min per workspace). Tasks 8, 11, 15, 22.
4. **Bounce accounting.** The inbound ledger insert is the idempotency gate; `apply_bounce` runs only when the ledger row is new (IMAP re-fetch, restart and webhook replay never double-count the breaker); only hard `Message-ID` matches to our own sends are acted on (a spoofed DSN cannot trip another tenant's breaker); the circuit breaker thresholds (`OUTREACH_CIRCUIT_MIN_SENDS`, bounce and complaint rates) and soft-bounce cap match Python. The webhook's cross-tenant lookup uses only the `SECURITY DEFINER` function and returns nothing for other tenants beyond the routing tuple. Tasks 2, 14, 18, 25.
5. **Scheduling and backoff.** Inbound failures back off `min(interval * 2^(n-1), 86400)` and auto-disable at the cap; one bad mailbox never blocks others; enqueue of the next poll is single-flight per `fire_key`; `send` enqueue dedups against pending/processing jobs and existing send rows (`/execute` plus the ticker collapse). Tasks 17, 18.
6. **Crash bugs behind 500.** Values Postgres rejects (`send_window_tz` over 40 chars, `name` over 200, `status`/`consent_source` over their column width, negative or absurd `daily_limit`, window hours outside 0-23) answer 422 in Go; an unset master key never panics the process. Task 20-23.

## File Structure

```
internal/emailnorm/{normalize,keys}.go                      # normalize_email, suppression_match_keys
internal/outreachtoken/{token,url}.go                       # make/verify unsubscribe token, unsubscribe_url
internal/secrets/master.go                                  # (add) MasterKeyBytes, same resolution as Python _load_master_key
internal/outreachcfg/{smtp,imap,footer}.go                  # SMTP/IMAP/footer config from workspace secrets
internal/smtpmail/{sanitize,render,build,mime}.go           # sanitize_header_value, validate_recipient, render_template, build_message, MIME + Message-ID
internal/smtpclient/{client,classify}.go                    # SMTP dial (egress), STARTTLS/implicit TLS, error classification
internal/testsmtp/server.go                                 # in-process fake SMTP server for tests
internal/egress/dial.go                                     # (add) DialContext guard reused by SMTP/IMAP
internal/imapclient/{client,fake_test_helpers}.go           # Client interface + go-imap/v2 implementation
internal/dsnparse/{parse,dsn,arf,heuristic}.go              # RFC 3464 DSN, ARF, text heuristic
internal/outreachstore/{sequences,enrollments,sends,suppressions,inbound,schedules,breaker}.go
internal/outreachsend/{handler,window,enqueue,tick,idem,ports}.go
internal/outreachinbound/{poll,process,schedule}.go
cmd/worker/main.go                                          # (modify, Task 19: send, outreach_inbound_poll handlers)
internal/api/{outreachsequences,outreachactions,outreachsmtp,outreachsuppressions,outreachdrafts,outreachpublic}.go
internal/api/register.go                                    # (modify once, Task 5)
internal/db/queries/{outreach_sequences,outreach_enrollments,outreach_sends,outreach_suppressions,outreach_inbound,outreach_schedules,outreach_drafts}.sql (+ gen/)
internal/db/schema.sql                                      # (regenerated, Task 4)
testdata/contract/outreach/*.json                           # incl. raw HTML bodies for unsubscribe
testdata/contract/retained_outreach_routes.txt
internal/{emailnorm,outreachtoken,smtpmail,dsnparse,outreachsend,outreachstore}/testdata/*.json
testdata/dsn/*.eml                                          # Python-recorded DSN/ARF/heuristic corpus
../../legacy/python/migrations/versions/<rev>_outreach_message_lookup.py
../../legacy/python/apps/api/services/go_executor.py        # (modify, Task 3)
../../legacy/python/scripts/{contract_seed.py,contract_cases/outreach.yaml,gen_outreach_vectors.py,record_dsn_corpus.py}
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/outreach-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors, corpus; 2 Python Alembic message lookup function; 3 Python executor routing; 4 Go schema sync; 5 Go scaffold | T1 owns `contract_seed.py`, `gen_outreach_vectors.py`, `record_dsn_corpus.py`; T2 owns migrations; T3 owns `go_executor.py` and the `send`/inbound enqueue sites in `sending.py`/`inbound.py`; T4 owns `schema.sql` and the revision constant; T5 owns `register.go`, `Deps`/`config` additions, all stub route files, `outreachsend/ports.go` |
| 1 | 6 emailnorm, 7 outreachtoken, 8 outreachcfg, 9 smtpmail, 10 dsnparse, 11 egress dial + smtpclient + testsmtp | 6 disjoint packages; 7 also adds `internal/secrets/master.go` only; 11 adds `internal/egress/dial.go` only |
| 2 | 12 outreachstore sequences/enrollments/schedules, 13 outreachstore sends, 14 outreachstore suppressions/inbound/breaker, 15 imapclient | 12-14 own disjoint files and `.sql` files of the one `outreachstore` package; 15 own package |
| 3 | 16 send handler, 17 enqueue and tick, 18 inbound poll | 16 owns `outreachsend/{handler,window,idem}.go`; 17 owns `outreachsend/{enqueue,tick}.go`; 18 own package |
| 4 (sequential) | 19 worker wiring | edits `cmd/worker/main.go` and `internal/config` |
| 5 | 20 sequence CRUD routes, 21 sequence action routes, 22 SMTP routes, 23 suppression routes, 24 drafts routes, 25 public unsubscribe + bounce webhook | each owns exactly one stub `internal/api/outreach*.go` and one fixture dir; 6 tasks |
| 6 | 26 coverage + nginx + runbook | consumes everything |

---

### Task 1: Python seed, recorder cases, vectors, DSN corpus

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/outreach.yaml`, `legacy/python/scripts/gen_outreach_vectors.py`, `legacy/python/scripts/record_dsn_corpus.py`
- Create (output, committed): `testdata/contract/outreach/*.json`, `internal/{emailnorm,outreachtoken,smtpmail,dsnparse,outreachsend,outreachstore}/testdata/*.json`, `testdata/dsn/*.eml`

**Interfaces:**
- Consumes: plan 04 seed (workspaces A/B, editor/viewer/admin users, leads).
- Produces: seed additions (workspace A: SMTP secrets for a second recording profile, `OUTREACH_FOOTER`, two sequences (one with 3 steps and `consent_basis`, one draft), enrollments in each status, sends in `in_flight/sent/failed/bounced/skipped`, suppressions (locked and unlocked, a Gmail dot/plus pair), two drafts with evidence, inbound ledger rows; workspace B with a sequence and a send whose `message_id` is the webhook target); env for the recorder: `OUTREACH_BOUNCE_WEBHOOK_SECRET`, `OUTREACH_UNSUB_TTL_DAYS=36500`, `OUTREACH_PUBLIC_BASE_URL=http://localhost:8000`, fixed master key). Vectors: `normalize_email`/`suppression_match_keys` (60 cases: NFKC, IDNA 2003 punycode, Gmail dots/plus, no `@`, empty), token sign/verify in both directions (Python-minted valid, expired via `ttl_days=-1`, forged, non-ASCII email, empty fields) plus the derived key from a fixed master, `sanitize_header_value`/`validate_recipient`/`render_template` (control chars, CRLF, 998 cut, `{{x}}` missing, spaces), `build_message` (html, text, headers, `no_footer`), `_in_send_window`/`_next_window_slot` (UTC, DST zones, wrap midnight, invalid tz, `start == end`), `_is_hard_bounce` (30 strings), breaker and `apply_bounce` sequences (hard, soft up to `OUTREACH_SOFT_BOUNCE_MAX`, complaint, thresholds, replay), `sequence_step_idem`, send-flow transcripts for `_do_send` against a stub SMTP (every skip reason, replay, window reschedule, hourly and daily caps, hard bounce, transient failure, success + advance/complete). DSN corpus: >= 25 `.eml` files (Gmail, Outlook, Postfix, Exim DSNs with multiple recipients, ARF complaints, text-only heuristics, malformed MIME, huge headers) each with the Python `parse_message` result as `<name>.json`.

- [ ] **Step 1: Prove `sequence.py` is dead:** add `scripts/outreach_deadcode_check.py` (grep import graph) asserting no module imports `services.outreach.sequence`; record the result in the seed's header comment.
- [ ] **Step 2: Extend the seed** (fixed ids, timestamps and token values; profile `outreach_smtp` selected by env for the recorder).
- [ ] **Step 3: Write the cases** (each `{name, method, path, as, profile, expect_status}`): drafts list (default, `limit=1&offset=1`, `limit=0` 422, `limit=201` 422), draft get (200 with evidence, foreign 404); sequences list/create (defaults, with steps, missing name 422, as editor 403, as viewer 403)/get (stats)/put (steps, status, empty body, 404)/delete (200, 404); start (400 `SMTP not configured`, 400 `consent_basis required before activating`, 400 `OUTREACH_FOOTER (physical address) required`, 200 `{"status":"active"}`, 404); pause (200, 404); enroll (ok, `no_email`, `suppressed`, `already_enrolled`, unknown lead, 404, empty list); execute (`{"enqueued":N}`, 404); stats (200, 404); sends; `smtp/status` (unconfigured, configured), `smtp/config` (each field, none), `smtp/test` (400 error text for unconfigured); suppressions list/add (new, duplicate, Gmail variant)/delete (ok, 404, 403 locked); unsubscribe GET (empty token 400, forged 400, expired renders, valid renders) and POST (invalid, expired, valid twice, 21st call 429); `webhooks/bounce` (secret unset 503, no header 403, wrong 403, unknown id 404, hard, soft, complaint, replay `duplicate`).
- [ ] **Step 4: Write the vector and corpus scripts**; they import the real Python modules.
- [ ] **Step 5: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/outreach.yaml`, `uv run python scripts/gen_outreach_vectors.py`, `uv run python scripts/record_dsn_corpus.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 6: Commit** (Go/doc side only) `test(go): outreach contract fixtures, vectors and DSN corpus (RED)`.

### Task 2: Python Alembic: cross-tenant message lookup function

**Files:**
- Create: `legacy/python/migrations/versions/<rev>_outreach_message_lookup.py`
- Test: `legacy/python/tests/test_migration_outreach_message_lookup.py`

**Interfaces:**
- Produces: `CREATE FUNCTION outreach_send_by_message_id(p_message_id text) RETURNS TABLE (workspace_id text, send_id integer, sequence_id text, enrollment_id integer, to_email text) LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp` returning the newest (`ORDER BY id DESC LIMIT 1`) `outreach_sends` row for the message id (the exact query of `bounce_webhook`); `REVOKE ALL ... FROM PUBLIC`, `GRANT EXECUTE` to the application role only; owner is the migration role that bypasses RLS. `downgrade` drops it.

- [ ] **Step 1: Write the failing migration test** (`upgrade` creates it; a non-superuser role with no GUC set gets exactly the routing tuple for a known id and zero rows for an unknown id; the role still cannot `SELECT` from `outreach_sends` without a GUC; `PUBLIC` cannot execute; `downgrade` drops). **Step 2: Run** `uv run pytest tests/test_migration_outreach_message_lookup.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS and `alembic heads` shows one head.
- [ ] **Step 5: Commit** (Python side is unversioned: note the revision id in the PR description; nothing to `git add`).

### Task 3: Python executor routing for `send` and `outreach_inbound_poll`

**Files:**
- Modify: `legacy/python/apps/api/services/go_executor.py` (`outreach_executor(job_type) -> str`), `legacy/python/apps/api/services/outreach/sending.py` (`_enqueue_if_absent`), `legacy/python/apps/api/services/outreach/inbound.py` (`_enqueue_inbound_if_absent`)
- Test: `legacy/python/tests/test_outreach_executor_routing.py`

**Interfaces:**
- Produces: `outreach_executor("send")` returns `"go"` iff env `GO_EXECUTOR_OUTREACH_SEND` is truthy, else `"python"`; same for `outreach_inbound_poll` with `GO_EXECUTOR_OUTREACH_INBOUND`; both enqueue sites pass `executor=` into `queue_service.add_job` / the `Job(...)` constructor; no other behaviour change (`fire_key`, payload, priority, `max_retries` untouched).

- [ ] **Step 1: Write failing tests**: `test_send_enqueue_executor_follows_flag`, `test_inbound_enqueue_executor_follows_flag`, `test_flags_default_python`, `test_payload_and_fire_key_unchanged`. **Step 2: Run** `uv run pytest tests/test_outreach_executor_routing.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 4: Go schema sync

**Files:**
- Modify: `internal/db/schema.sql` (via `make schema` against a migrated database), `internal/config/config.go` (`RequiredGoSchemaRevision`)
- Test: `internal/db/schema_test.go` (existing drift test)

**Interfaces:** Consumes the Task 2 revision id. Produces the `outreach_send_by_message_id` function in `schema.sql` and the bumped revision (the worker refuses to start below it). `outreach_*` tables already exist in `schema.sql` (verified by the test below).

- [ ] **Step 1: Write failing test** `TestSchemaHasOutreachMessageLookup` (function exists, `SECURITY DEFINER`, return columns; the 8 `outreach_*` tables exist, 7 with RLS forced, `outreach_schedules` and `outreach_inbound_schedules` without RLS as in Python). **Step 2: Run** `make test` -> FAIL. **Step 3:** run `make schema`, bump `RequiredGoSchemaRevision`. **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach lookup function schema (RED)`, `feat(go): sync outreach lookup schema and required revision`.

### Task 5: Scaffold: Deps, config, register lines, stubs, ports

**Files:**
- Modify: `internal/api/register.go` (+`RegisterOutreach` line, `Deps` gains `OutreachPublicBaseURL string`, `Egress` allow-hosts, `OutreachWebhookSecret string`), `internal/config/config.go` (`GO_OUTREACH_ROUTES_ENABLED` default false, `OUTREACH_PUBLIC_BASE_URL`, `OUTREACH_BOUNCE_WEBHOOK_SECRET`, `OUTREACH_UNSUB_TTL_DAYS` 90, `OUTREACH_SEND_COST_USD` 0, `OUTREACH_TICK_INTERVAL` 900, `OUTREACH_TICK_MAX_ENQUEUE` 200, `OUTREACH_SOFT_BOUNCE_MAX` 3, `OUTREACH_BOUNCE_PAUSE_RATE` 0.05, `OUTREACH_COMPLAINT_PAUSE_RATE` 0.003, `OUTREACH_CIRCUIT_MIN_SENDS` 20, `OUTREACH_INBOUND_POLL_ENABLED` false, `OUTREACH_INBOUND_POLL_INTERVAL` 900, `OUTREACH_INBOUND_MAX_FETCH` 100, `OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES` 10, `OUTREACH_INBOUND_LOOKBACK_DAYS` 3, `AUTOMATIONS_ENABLED`, `GO_EXECUTOR_OUTREACH_SEND`, `GO_EXECUTOR_OUTREACH_INBOUND`; defaults identical to `core/config.py`)
- Create: stub files `internal/api/{outreachsequences,outreachactions,outreachsmtp,outreachsuppressions,outreachdrafts,outreachpublic}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), `internal/outreachsend/ports.go`
- Test: `internal/api/outreachscaffold_test.go`

**Interfaces:**
- Produces: `RegisterOutreach(r *httpx.Router, d Deps)` (registers nothing and logs when `PG_LEAD_STORE` or `GO_OUTREACH_ROUTES_ENABLED` is false); `outreachsend.Caps` interface `{ TryReserve(ctx, tx pgx.Tx, workspaceID, triggerID string, costUSD float64, idem string) (Reservation, error); Settle(ctx, tx, Reservation) error; Release(ctx, tx, Reservation) error }` with `Reservation{OK bool; Ref any}` (**assumes plan 08**; a `nil` Caps is valid when `OUTREACH_SEND_COST_USD == 0`); `outreachsend.Mailer interface { Send(ctx, cfg smtpclient.Config, from, to string, msg []byte) (messageID string, err error) }` so the handler is testable without a socket.

- [ ] **Step 1: Write failing tests**: `TestOutreachRoutesNotRegisteredWithoutFlags`, `TestOutreachRoutePatterns` (exactly the 22 routes of Tasks 20-25), `TestConfigDefaultsMatchPython` (table of the settings above).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach scaffold (RED)`, `feat(go): outreach route scaffold, config and ports`.

### Task 6: Email normalization

**Files:**
- Create: `internal/emailnorm/{normalize,keys}.go`, `internal/emailnorm/normalize_test.go`; `go.mod`/`go.sum` (`golang.org/x/net`, `golang.org/x/text`, pinned)

**Interfaces:**
- Produces: `func Normalize(email string) string` (= `normalize_email`: NFKC, trim, lower, rpartition on `@`, IDNA2003-style punycode of the domain with fallback to plain lower-case on error); `func MatchKeys(email string) []string` (= `suppression_match_keys`: canonical, plus dot/plus-collapsed form for `gmail.com`/`googlemail.com` only). IDNA must reproduce Python's `str.encode("idna")` (IDNA 2003, nameprep) on the vector set; where `x/net/idna` profiles differ, the vector file is authoritative and the difference is handled in code, never by editing the vector.

- [ ] **Step 1: Write failing tests**: `TestNormalizeMatchesVectors`, `TestMatchKeysMatchesVectors`, `TestNormalizeNoAtSign`, `TestNormalizeIDNAFailureFallsBack`, `TestGmailCollapseOnlyForGmail`, `FuzzNormalize` (never panics, idempotent: `Normalize(Normalize(x)) == Normalize(x)` on the vector domain).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): email normalization vectors (RED)`, `feat(go): email normalization and suppression match keys`.

### Task 7: Unsubscribe tokens

**Files:**
- Create: `internal/outreachtoken/{token,url}.go`, `internal/secrets/master.go`, tests `internal/outreachtoken/token_test.go`, `internal/secrets/master_test.go`

**Interfaces:**
- Produces: `secrets.MasterKeyBytes(rawMasterKey, secretKey string, insecureDefault, devEnv bool) ([]byte, error)` (same resolution as Python `_load_master_key`: verbatim when a valid Fernet key, else the derived key; falls back to `SECRET_KEY` with the same refusal rule; returns an error instead of panicking); `type Signer struct{...}`, `func NewSigner(masterKey []byte, publicBaseURL string, ttlDays int, now func() time.Time) *Signer`; `func (s *Signer) Make(workspaceID, email, sequenceID string, ttlDays *int) (string, error)` (payload `{"ws","email"(lower, stripped),"seq","exp"}` serialized with sorted keys, compact separators and Python `ensure_ascii` escaping; `base64url` without padding; `body.sig`, HMAC-SHA256 under `sha256("outreach-unsub:" + master)`); `func (s *Signer) Verify(token string, allowExpired bool) (Payload, bool)` (constant-time compare; `exp` compared as int; missing `ws` or `email` -> invalid); `func (s *Signer) URL(workspaceID, email, sequenceID string) (string, error)` (`<base>/api/outreach/unsubscribe?token=<token>`, base right-stripped of `/`).

- [ ] **Step 1: Write failing tests**: `TestVerifiesPythonMintedTokens` (vectors), `TestPythonVerifiesGoTokens` (golden: Go output for fixed inputs and time equals Python bytes), `TestExpiredRejectedUnlessAllowed`, `TestForgedSignatureRejected`, `TestNonASCIIEmailMatchesPythonEscaping`, `TestMalformedTokensNeverPanic` (fuzz), `TestMasterKeyResolutionMatchesPython`, `TestMasterKeyInsecureDefaultRefused`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** (reuse `canonjson` only if it can emit Python `ensure_ascii` output; otherwise a local encoder pinned by the golden test). **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): unsubscribe token vectors (RED)`, `feat(go): master key bytes and unsubscribe tokens`.

### Task 8: SMTP, IMAP and footer configuration

**Files:**
- Create: `internal/outreachcfg/{smtp,imap,footer}.go`, `internal/outreachcfg/cfg_test.go`

**Interfaces:**
- Consumes: `secrets.Resolver` (plan 03, workspace secret first then global then env) and `secrets.Envelope` (set).
- Produces: `type SMTP struct{Host string; Port int; Email, Password, FromName string; UseTLS bool; MaxPerHour int}`, `func LoadSMTP(ctx, r *secrets.Resolver, workspaceID string) SMTP` (defaults `587`, `OpenGTM`, TLS on unless `SMTP_USE_TLS != "1"`, `50`; any parse error yields the zero `SMTP{}` like Python's blanket `except`), `func (SMTP) Configured() bool`; `type IMAP struct{Host string; Port int; User, Password, Folder string; UseSSL bool}`, `LoadIMAP` (user/password default to the SMTP email/password, folder `INBOX`, port 993), `Configured()`; `func Footer(ctx, r, workspaceID string) string` (`OUTREACH_FOOTER`, trimmed); `func SetSMTP(ctx, e *secrets.Envelope, workspaceID string, u SMTPUpdate) error` writing `SMTP_HOST/PORT/EMAIL/PASSWORD/FROM_NAME/MAX_PER_HOUR/USE_TLS` only for non-nil fields (`"1"`/`"0"` for TLS), values never logged. Validation of host/port is done by the route (Task 22), not here.

- [ ] **Step 1: Write failing tests**: `TestLoadSMTPDefaults`, `TestLoadSMTPWorkspaceBeatsGlobal`, `TestLoadSMTPBadPortYieldsZeroConfig`, `TestLoadIMAPFallsBackToSMTPCreds`, `TestFooterTrimmed`, `TestSetSMTPOnlyGivenFields`, `TestConfigNeverLogsPassword`, `TestConfigIsolatesWorkspaces`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach config loaders (RED)`, `feat(go): outreach SMTP, IMAP and footer config`.

### Task 9: Message building (pure functions)

**Files:**
- Create: `internal/smtpmail/{sanitize,render,build,mime}.go`, `internal/smtpmail/mail_test.go`

**Interfaces:**
- Produces: `SanitizeHeader(v string, maxLen int) string` (drops C0 controls and DEL, CR/LF to space, trims, cuts to `maxLen` runes as Python slices by code point); `ValidRecipient(addr string) bool` (the Python `_EMAIL_RE`, no CR/LF); `Render(template string, vars map[string]any) string` (`{{name}}` replaced by `str(value)` with Python `str` for floats/ints/bools, missing and `None` empty, result trimmed); `LeadVariables(lead leadsread.Lead) map[string]any`; `Build(footer, unsubURL, bodyHTML string) (html, text string, headers map[string]string, err error)` (`ErrNoFooter` when footer empty; text derived by the same four regex steps and `html.unescape` semantics, footer HTML-escaped in the HTML part, `List-Unsubscribe: <url>` and `List-Unsubscribe-Post: List-Unsubscribe=One-Click`); `func Compose(m Message) (raw []byte, messageID string)` building `multipart/alternative` (plain first, html second, UTF-8), `From: "<sanitized from name 200>" <email>`, sanitized `Subject`, extra headers sanitized to 2000, and a `Message-ID` of the `make_msgid` shape `<unixtime.pid.random@fromdomain>` with high-entropy local part and the SMTP account's domain.

- [ ] **Step 1: Write failing tests**: `TestSanitizeMatchesVectors`, `TestValidRecipientMatchesVectors`, `TestRenderMatchesVectors`, `TestBuildMatchesVectors`, `TestBuildNoFooter`, `TestComposeHeadersAndStructure` (parse back with `net/mail` + `mime/multipart`: parts, charset, headers, no header injection when subject/from name contain CRLF), `TestMessageIDShapeAndDomain`, `TestMessageIDUnique` (10k ids).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach message building vectors (RED)`, `feat(go): outreach message building and MIME composition`.

### Task 10: DSN, ARF and heuristic bounce parsing

**Files:**
- Create: `internal/dsnparse/{parse,dsn,arf,heuristic}.go`, `internal/dsnparse/parse_test.go`

**Interfaces:**
- Consumes: `testdata/dsn/*.eml` + expected JSON (Task 1).
- Produces: `type Record struct{Recipient, Kind, Diagnostic, OriginalMessageID string}`; `func Parse(raw []byte) []Record` (= `parse_message`: `multipart/report; report-type=delivery-status` -> RFC 3464 per-recipient blocks with `classify_status(status, action)`; `feedback-report` -> ARF complaint; else text heuristic; any MIME error yields `nil`); `func SourceMessageID(raw []byte) string` (= `source_message_id`). Bounded work: raw input over 1 MiB is truncated before parsing and nested multiparts deeper than 8 levels are ignored (documented hardening, D6).

- [ ] **Step 1: Write failing tests**: `TestParseMatchesPythonCorpus` (every `.eml`, deep-equal on ordered records), `TestClassifyStatusVectors`, `TestMalformedInputNeverPanics` (fuzz + truncated corpus), `TestOversizedInputBounded`, `TestDeepNestingBounded`, `TestSourceMessageIDMatchesCorpus`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** with `net/mail`, `mime`, `mime/multipart` and `mime/quotedprintable`; where Go's stdlib is stricter than Python's `email` package (folded headers, bare LF, 8-bit headers) a tolerant pre-pass normalizes line endings; every divergence found by the corpus is fixed in code, never in the expected files. **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): DSN corpus parity (RED)`, `feat(go): DSN, ARF and heuristic bounce parser`.

### Task 11: Egress dial guard, SMTP client, test SMTP server

**Files:**
- Create: `internal/egress/dial.go` (+test `internal/egress/dial_test.go`), `internal/smtpclient/{client,classify}.go`, `internal/smtpclient/client_test.go`, `internal/testsmtp/server.go`

**Interfaces:**
- Produces: `egress.DialContext(appEnv string, allowHosts []string) func(ctx context.Context, network, addr string) (net.Conn, error)` (resolves, rejects any blocked IP, then dials the resolved IP so rebinding cannot swap it; same block list and bench-only allowlist as `NewClient`); `smtpclient.Config{Host string; Port int; Email, Password string; UseTLS bool; Dial func(...)}`; `smtpclient.Client{Timeout time.Duration}` with `Send(ctx, cfg Config, from, to string, msg []byte) (messageID string, err error)` implementing the Python protocol: connect; `UseTLS && port == 587` -> STARTTLS (verified certificate, `ServerName = host`); `UseTLS && port == 465` -> implicit TLS; `UseTLS` on any other port -> STARTTLS when offered; `!UseTLS` -> plain only when the host is an allow-listed test host (hardening, D2: Python's `use_tls=False` sends the password in clear); AUTH PLAIN/LOGIN; whole exchange bounded by `ctx` and `Timeout` (default 60 s); errors carry the server reply text verbatim in the same `str(exc)` shape the Python classifier (`_is_hard_bounce`) matches on (`550 5.1.1 ...`); `smtpclient.IsHardBounce(err string) bool` (exact port of `_is_hard_bounce`); `testsmtp.Server` (in-process, STARTTLS with a self-signed cert, scripted per-recipient replies, records messages).

- [ ] **Step 1: Write failing tests**: `TestDialRejectsPrivateLoopbackLinkLocalMapped`, `TestDialRebindBlocked`, `TestAllowlistIgnoredOutsideBench` (egress); `TestSendDeliversMessage`, `TestSTARTTLSOn587Semantics`, `TestImplicitTLSOn465`, `TestCertificateVerificationRequired`, `TestPlainTextRefusedForNonTestHosts`, `TestAuthFailureError`, `TestHardBounceClassificationMatchesVectors`, `TestTimeoutHonoured`, `TestContextCancelClosesConnection`, `TestErrorNeverContainsPassword`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): egress dial guard and SMTP client (RED)`, `feat(go): egress dial guard`, `feat(go): SMTP client with TLS and bounce classification`.

### Task 12: Store: sequences, enrollments, schedules

**Files:**
- Create: `internal/db/queries/{outreach_sequences,outreach_enrollments,outreach_schedules}.sql`, `internal/outreachstore/{sequences,enrollments,schedules}.go`, `internal/outreachstore/seq_integration_test.go`

**Interfaces:**
- Consumes: Task 4 schema; `tenancy.InWorkspace`; `emailnorm`.
- Produces (all take `tx pgx.Tx` already scoped; sequence dicts keep Python's key order, `steps` as stored JSON): `CreateSequence(ctx, tx, wsID string, in SeqInput) (Seq, error)` (uuid4 id), `ListSequences`, `GetSequence`, `SequenceExists`, `UpdateSequence(ctx, tx, wsID, id string, updates map[string]any) (*Seq, error)` (only keys present, same as `model_dump(exclude_none=True)`), `SetSequenceStatus`, `DeleteSequence` (cascades enrollments/sends/schedule exactly as Python `delete_sequence`), `Enroll(ctx, tx, wsID, seqID string, leadID int, email, consentSource string, consentAt time.Time) (id int, created bool, err error)` (`uq` conflict -> `created=false`), `GetEnrollment`, `DueEnrollments(ctx, tx, seqID string, limit int) ([]Enrollment, error)` (the `ix_enroll_ws_due` ordering), `RescheduleEnrollment`, `AdvanceEnrollment(ctx, tx, id int, a Advance)` (status, error, `IncrementSent`, `CurrentStep`, `NextSendAt`), `IncrementSoftBounce(ctx, tx, id int) (int, error)`, `UpsertSchedule/DisableSchedule` (non-RLS `outreach_schedules` mirror). Datetimes `timestamptz`; ISO output as Python.

- [ ] **Step 1: Write failing tests**: `TestSequenceCRUDMatchesPythonShape` (fixtures), `TestUpdateOnlyGivenKeys`, `TestDeleteCascadesLikePython`, `TestEnrollIdempotent`, `TestDueEnrollmentsOrderAndLimit`, `TestAdvanceEnrollmentVariants`, `TestSoftBounceCounter`, `TestScheduleMirrorUpsertDisable`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement** (`make sqlc`, stage only the three `.sql.go`). **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach sequence store (RED)`, `feat(go): outreach sequence, enrollment and schedule store`.

### Task 13: Store: sends and stats

**Files:**
- Create: `internal/db/queries/outreach_sends.sql`, `internal/outreachstore/sends.go`, `internal/outreachstore/sends_integration_test.go`

**Interfaces:**
- Produces: `GetSendByIdem(ctx, tx, wsID, idem string) (*Send, error)`; `InsertInFlight(ctx, tx, in SendIn) (created bool, err error)` (unique violation on `uq_outreach_send_idem` -> `created=false`, no error); `InsertTerminal(ctx, tx, in SendIn, status, skipReason, errText string) error` (ignores an existing idem like `_record_terminal`); `FinalizeSend(ctx, tx, wsID, idem string, f Finalize) error` (only set fields: status, skip_reason, message_id, error, sent_at, bounced_at, charged_usd); `GetSendByMessageID(ctx, tx, wsID, msgID string) (*SendRef, error)`; `LookupSendAnyWorkspace(ctx, pool *pgxpool.Pool, msgID string) (*GlobalRef, error)` (calls `outreach_send_by_message_id`, the only non-scoped query in the package, no tenant GUC set); `GetRecentSendTo(ctx, tx, wsID, email)`; `MarkSendBounced`; `ListSends(ctx, tx, seqID string, limit int)`; `SentCountLastHour`, `SentCountToday(seqID)` (day boundary = UTC midnight as Python `sent_count_today`); `SequenceStats(ctx, tx, seqID) (map, error)` (same keys and zero-fill as `get_sequence_stats`).

- [ ] **Step 1: Write failing tests**: `TestInsertInFlightUniqueIdem` (16 goroutines, one row), `TestFinalizeOnlySetsGivenFields`, `TestTerminalIgnoresExistingIdem`, `TestMessageIDLookupWorkspaceScoped`, `TestGlobalLookupReturnsOnlyRoutingTuple` (non-superuser pool, no GUC), `TestGlobalLookupUnknownIsNil`, `TestCountsWindowBoundaries`, `TestStatsMatchFixtures`, `TestListSendsOrderAndLimit`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach send store (RED)`, `feat(go): outreach send ledger store and stats`.

### Task 14: Store: suppressions, inbound ledger, bounce application, breaker

**Files:**
- Create: `internal/db/queries/{outreach_suppressions,outreach_inbound}.sql`, `internal/outreachstore/{suppressions,inbound,breaker}.go`, `internal/outreachstore/bounce_integration_test.go`

**Interfaces:**
- Consumes: `emailnorm`, Task 12/13 functions (same package).
- Produces: `IsSuppressed(ctx, tx, wsID, email string) (bool, error)` (any of `MatchKeys`), `AddSuppression(ctx, tx, wsID, email, reason, source string, locked bool) (added bool, err error)` (canonical email; existing row is not downgraded from locked), `ListSuppressions(ctx, tx, limit int)`, `RemoveSuppression(ctx, tx, wsID, email string) (Outcome, error)` (`ok|not_found|locked`), `RecordInboundProcessed(ctx, tx, in InboundIn) (created bool, err error)` (empty `source_message_id` -> `uid:<uidvalidity>:<uid>`; recipient cut to 320, diagnostic to 500; both unique constraints -> `created=false`), `InboundMessageCount`, inbound schedule mirror (`GetInboundSchedule`, `UpsertInboundSchedule`, `DisableInboundSchedule`, non-RLS, takes the pool not a tenant tx), `RecordBounceAndMaybePause(ctx, tx, wsID, seqID string, complaint bool, th Thresholds) (tripped bool, err error)` (counts `sent`+`bounced` sends, `auto_paused` sequence becomes `paused`), `ApplyBounce(ctx, tx, a BounceApply, th Thresholds) error` (the exact three-way logic of `apply_bounce`: complaint -> mark send bounced + locked `complaint` suppression + enrollment bounced + complaint breaker; soft -> counter, suppress and terminal only at `OUTREACH_SOFT_BOUNCE_MAX`; hard -> mark bounced + `bounce` suppression + enrollment bounced + bounce breaker).

- [ ] **Step 1: Write failing tests**: `TestSuppressionMatchesGmailVariants`, `TestLockedSuppressionNotRemovable`, `TestAddSuppressionDoesNotUnlock`, `TestRecordInboundIdempotentBothKeys`, `TestApplyBounceMatchesVectors` (hard, soft x cap, complaint, replay gate), `TestBreakerThresholdsMatchVectors`, `TestBreakerNeedsMinSends`, `TestAutoPausedSequenceStopsDue`, `TestScheduleMirrorsNonRLS`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): suppression, inbound ledger and bounce store (RED)`, `feat(go): suppression, inbound ledger, bounce application and breaker`.

### Task 15: IMAP client

**Files:**
- Create: `internal/imapclient/client.go`, `internal/imapclient/client_test.go`; `go.mod`/`go.sum` (`github.com/emersion/go-imap/v2`, pinned)

**Interfaces:**
- Produces: `type Client interface { Connect(ctx) error; Select(ctx, folder string) (uidValidity string, err error); SearchUnseen(ctx, since time.Time) ([]string, error); Fetch(ctx, uid string) ([]byte, error); MarkSeen(ctx, uid string) error; Logout() }` (the Python `ImapClient` protocol); `func New(cfg outreachcfg.IMAP, dial func(ctx, network, addr string) (net.Conn, error), timeout time.Duration) Client` (implicit TLS when `UseSSL` with verified certificate, STARTTLS refused-to-skip when `!UseSSL` outside allow-listed test hosts; UID `SEARCH UNSEEN SINCE <dd-Mon-yyyy>`; `FETCH (RFC822)` body capped at 5 MiB, larger -> empty like a failed fetch; `STORE +FLAGS (\Seen)`; every call bounded by `ctx`).

- [ ] **Step 1: Write failing tests** against `imapmemserver`: `TestSelectReturnsUIDValidity`, `TestSearchUnseenSince`, `TestFetchReturnsRFC822`, `TestFetchOversizedIsEmpty`, `TestMarkSeenSetsFlagOnly`, `TestLoginFailureError`, `TestCertificateVerificationRequired`, `TestDialGuardUsed` (blocked IP never connects), `TestContextCancelAborts`, `TestErrorNeverContainsPassword`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): IMAP client (RED)`, `feat(go): IMAP client over go-imap v2`.

### Task 16: `send` job handler

**Files:**
- Create: `internal/outreachsend/{handler,window,idem}.go`, `internal/outreachsend/handler_test.go`, `internal/outreachsend/handler_integration_test.go`

**Interfaces:**
- Consumes: Tasks 7-9, 11, 12-14; `billing.CheckAndDebit` (plan 06 Task 6); `Caps` (**assumes plan 08**, Task 5 port); `outreachcfg`.
- Produces: `func SequenceStepIdem(wsID, seqID string, enrollmentID, step int) string` (`seq:<ws>:<seq>:<enr>:<step>`); `func InSendWindow(seq Seq, now time.Time) bool` and `NextWindowSlot(seq Seq, now time.Time) time.Time` (IANA tz via `time.LoadLocation`, invalid tz -> UTC, `start == end` or nil -> always, wrap-midnight); `type Handler struct{...}`, `func (h *Handler) Handle(ctx context.Context, c *queue.Claim) error` implementing `_do_send` in order: master switch; replay/dedup on idem (terminal -> return; `in_flight` -> finalize `skipped/replay`); load enrollment (missing -> `skipped/lead_not_found`) and sequence; no email -> `skipped/no_email`; render step subject/body with `variables` defaulting to `{"email": to}` (Python quirk kept); outside window -> reschedule, not terminal; suppressed -> `skipped/suppressed` + enrollment `suppressed`; SMTP unconfigured -> `failed` `SMTP not configured`; hourly cap -> reschedule +15 min; daily cap -> reschedule +1 h; caps reservation when cost > 0 (`cap` skip when not OK; nil `Caps` -> `queue.HandBack`, D7); commit `in_flight` row in its own transaction; build message (no footer -> release reservation + `skipped/no_footer`); SMTP via `Mailer`; success -> debit `run:<idem>` reason `outreach_send` (replay -> charged 0), settle, finalize `sent` with `message_id`, advance enrollment (`completed` after the last step, else `scheduled` + `delay_hours`); hard bounce -> release, finalize `bounced`, `bounce` suppression, enrollment `bounced`, breaker; transient -> release, finalize `failed`, enrollment `failed`, return an error so the queue retries (retry then short-circuits to `skipped/replay`). Each numbered step is its own `InWorkspace` transaction; none spans the SMTP call. The handler checks `ctx` before the dial and passes it into `Mailer.Send` (timeout 300 s job cap).

- [ ] **Step 1: Write failing tests**: `TestSendFlowMatchesPythonTranscripts` (every transcript from Task 1), `TestReplayTerminalNoSend`, `TestReplayInFlightSkipsNeverSends`, `TestInFlightCommittedBeforeSMTP` (blocking fake mailer; a second connection sees `in_flight`), `TestNoTransactionHeldDuringSMTP` (`pg_stat_activity` shows no open tx while the mailer blocks), `TestConcurrentSameIdemSendsOnce` (16 goroutines), `TestSuppressedRechecked`, `TestWindowReschedules`, `TestHourlyAndDailyCaps`, `TestDebitOnSuccessOnly`, `TestDebitIdempotentOnRetry`, `TestTransientFailureNeverCharged`, `TestReservationReleasedOnEveryNonSuccess`, `TestCostWithoutCapsHandsBack`, `TestMasterSwitchOff`, `TestCancelledContextBeforeDialSendsNothing`, `TestWindowAndNextSlotMatchVectors`, `TestIdemMatchesPython`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach send handler (RED)`, `feat(go): send window and idempotency keys`, `feat(go): at-most-once send handler`.

### Task 17: Due-send enqueue and tick

**Files:**
- Create: `internal/outreachsend/{enqueue,tick}.go`, `internal/outreachsend/enqueue_integration_test.go`

**Interfaces:**
- Consumes: `queue.EnqueueOnce` (plan 06), Tasks 12/13.
- Produces: `func EnqueueDue(ctx, tx pgx.Tx, wsID, seqID string, limit int, executor string) (int, error)` (= `enqueue_due_sends`: for each due enrollment, idem from `current_step`, skipped when a pending/processing `send` job already carries that idem or a send row exists, payload keys `workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email` + `idempotency_key`; stops at `limit`); `func Tick(ctx, pool, wsID, seqID string, now time.Time, cfg TickConfig) (int, error)` (= `tick_sequence`: non-active or missing sequence disables its schedule and returns 0; else enqueue and upsert the schedule at `now + OUTREACH_TICK_INTERVAL`); `func EnqueueSend(ctx, tx pgx.Tx, wsID string, p SendPayload, executor string) (bool, error)` for plan 08 (automation `send_email`) and the MCP tool with the same single-flight rule; `executor` is `go` when `GO_EXECUTOR_OUTREACH_SEND`.

- [ ] **Step 1: Write failing tests**: `TestEnqueueDueMatchesPythonPayloads`, `TestEnqueueDedupsPendingJobAndExistingSend`, `TestExecuteAndTickCollapse` (same idem enqueued once), `TestEnqueueLimit`, `TestTickDisablesInactiveSequence`, `TestTickReschedules`, `TestEnqueueExecutorFollowsFlag`, `TestEnqueueRollsBackWithTx`, `TestConcurrentEnqueueSingleFlight` (16 goroutines).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach enqueue and tick (RED)`, `feat(go): due-send enqueue, tick and EnqueueSend`.

### Task 18: `outreach_inbound_poll` job

**Files:**
- Create: `internal/outreachinbound/{poll,process,schedule}.go`, `internal/outreachinbound/poll_test.go`, `internal/outreachinbound/poll_integration_test.go`

**Interfaces:**
- Consumes: Tasks 10, 14, 15, 8; `queue.EnqueueOnce`.
- Produces: `func Process(ctx, tx pgx.Tx, wsID, uidValidity, uid string, raw []byte, th Thresholds) error` (= `_process_one`: parse, hard `Message-ID` match against our sends only, else ledger row `unmatched`/`unknown` with the recipient-only fallback recorded but never acted on; matched -> ledger insert is the gate, `ApplyBounce` only when created); `func Handler{...}.Handle(ctx, c *queue.Claim) error` (= `handle_inbound_poll`: feature flag; missing `workspace_id` -> return; config incomplete -> disable schedule; poll = connect, select, `SearchUnseen(now - lookback days)` capped at `OUTREACH_INBOUND_MAX_FETCH`, fetch + process each in its own tenant transaction, best-effort `MarkSeen` after the ledger commit, always logout; any error -> failed tick); `func Reschedule(ctx, pool, wsID string, failed bool, uidValidity *string, now time.Time, cfg) error` (failure counter, backoff `min(interval * 2^(n-1), 86400)`, auto-disable at the cap, success resets, next poll enqueued once with `fire_key = inbound:<ws>:<iso>`, priority 1, `max_retries` 3, executor per flag); one workspace's failure never aborts another.

- [ ] **Step 1: Write failing tests** (IMAP via `imapmemserver`): `TestProcessMatchesPythonCorpusOutcomes`, `TestSpoofedDSNWithoutMessageIDNotActedOn`, `TestReprocessingNeverDoubleCounts`, `TestSoftBounceCapSuppresses`, `TestComplaintLocksSuppression`, `TestBackoffMatchesVectors`, `TestAutoDisableAtCap`, `TestSuccessResetsFailures`, `TestNextPollEnqueuedOnce` (16 goroutines), `TestMissingCredsDisablesSchedule`, `TestFeatureFlagOff`, `TestMarkSeenFailureIgnored`, `TestLogoutAlwaysCalled`, `TestMaxFetchCap`, `TestOnePoisonMessageDoesNotStopBatch`, `TestBlockedHostFailsTickNotWorker` (SSRF guard).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach inbound poll (RED)`, `feat(go): inbound message processing`, `feat(go): inbound poll handler, backoff and scheduling`.

### Task 19: Worker wiring

**Files:**
- Modify: `cmd/worker/main.go`, `internal/config/config.go` (only if a missing flag is found), `cmd/worker/main_test.go`, `cmd/worker/main_integration_test.go`

**Interfaces:** Consumes Tasks 16-18. Produces handler registration for job types `send` (timeout 300 s) and `outreach_inbound_poll` (600 s) on `executor='go'` rows only; `HandBack` with reason when a handler reports `ErrNeedsPython` (cost without caps); a transient send error finalizes the claim as retry with the queue's backoff; `outreach_inbound_poll` retry/finalize parity; the worker's `AssertSafeRole` and schema gate unchanged; the Python scheduler keeps ticking (documented in the runbook).

- [ ] **Step 1: Write failing tests**: `TestWorkerHandlesSendJob`, `TestWorkerIgnoresPythonSendRows`, `TestWorkerHandsBackPaidSendWithoutCaps`, `TestWorkerSendRetryBackoff`, `TestWorkerInboundRescheduleEnqueuesNext`, `TestWorkerTimeoutsPerType`, `TestWorkerCancelMidSend` (context cancelled before dial -> no send).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): worker outreach handlers (RED)`, `feat(go): worker runs send and inbound poll jobs`.

### Task 20: Sequence CRUD routes

**Files:**
- Modify: stub `internal/api/outreachsequences.go`; Create: `internal/api/outreachsequences_test.go`, `internal/api/outreachsequences_integration_test.go`, `testdata/contract/outreach/sequences_*.json` (from Task 1)

**Interfaces:** Produces `GET /api/outreach/sequences` -> `{"sequences":[...]}`; `POST` (admin) body `{name, description="", steps=[{step_number, subject, body_html, delay_hours=0}], daily_limit=50, send_window_start=9, send_window_end=18, send_window_tz="UTC", consent_basis=""}` -> `{"id","name","status"}`; `GET /{seq_id}` -> sequence + `stats` (404 `Sequence not found`); `PUT /{seq_id}` (admin, only given keys) -> `{"status":"ok","id"}`; `DELETE /{seq_id}` -> `{"status":"ok"}`. FastAPI 422 bodies for type errors verbatim. Hardening (D3): `name` 1-200 chars, `description` <= 5000, `send_window_tz` <= 40 chars and a loadable IANA name, `send_window_start/end` 0-23, `daily_limit` 1-100000, `consent_basis` <= 40, `status` in `draft|active|paused|completed` on PUT, steps <= 50 and `delay_hours` 0-8760 answer 422 `detail` with the standard list shape (`loc`, `msg`, `type`) instead of Postgres 500s; `PUT` with `status="active"` is refused with 422 (activation must go through `start`; Python lets an admin activate without SMTP/consent/footer checks, a compliance hole documented in the README).

- [ ] **Step 1: Write failing tests**: `TestSequenceRoutesMatchFixtures`, `TestCreateRequiresAdmin`, `TestViewerCanList`, `TestCreateDefaults`, `TestUpdateOnlyGivenFields`, `TestUpdateUnknown404`, `TestDeleteUnknown404`, `TestOverlongFieldsAre422NotPostgresErrors`, `TestPutActiveStatusRefused`, `TestForeignWorkspace404`, `TestInvalidTimezone422`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach sequence routes (RED)`, `feat(go): outreach sequence CRUD routes`.

### Task 21: Sequence action routes

**Files:**
- Modify: stub `internal/api/outreachactions.go`; Create: tests + `testdata/contract/outreach/actions_*.json`

**Interfaces:** Produces `POST /{seq_id}/start` (admin; order of checks: 404 `Sequence not found`, 400 `SMTP not configured`, 400 `consent_basis required before activating`, 400 `OUTREACH_FOOTER (physical address) required`; sets `active`, runs `Tick` once, tick errors logged and swallowed, answers `{"status":"active"}`); `POST /{seq_id}/pause` (`paused`, schedule disabled, 404); `POST /{seq_id}/enroll` (admin; body `{lead_ids:[int], consent_source=""}`; per lead: `LeadsByIDs` read, `emailnorm.Normalize`, `no_email`/`suppressed`/`already_enrolled` skips, default `consent_source` `manual_enroll`; response `{"enrolled","skipped":[{"lead_id","reason"}],"total_lead_ids"}`; hardening D3: at most 5000 ids, 422 above; one transaction per 500 ids so a huge list cannot hold locks); `POST /{seq_id}/execute` (`{"enqueued":N}`, uses `EnqueueDue` with `OUTREACH_TICK_MAX_ENQUEUE`); `GET /{seq_id}/stats` (404 when absent); `GET /{seq_id}/sends` (`{"sends":[...]}` newest first, limit 200, no 404 for unknown sequences, as Python).

- [ ] **Step 1: Write failing tests**: `TestStartChecksInOrder`, `TestStartActivatesAndTicks`, `TestStartTickFailureStillActive`, `TestPauseDisablesSchedule`, `TestEnrollSkipReasons`, `TestEnrollCountsMatchFixtures`, `TestEnrollTooManyIds422`, `TestEnrollForeignLeadIsNoEmailSkip`, `TestExecuteEnqueuesOnce`, `TestStatsAndSendsMatchFixtures`, `TestActionsRequireAdminExceptReads`, `TestForeignWorkspace404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach sequence action routes (RED)`, `feat(go): sequence start, pause, enroll, execute, stats and sends routes`.

### Task 22: SMTP routes

**Files:**
- Modify: stub `internal/api/outreachsmtp.go`; Create: tests + `testdata/contract/outreach/smtp_*.json`

**Interfaces:** Produces `GET /smtp/status` (`{"configured","host","email","from_name","max_per_hour"}`, empty host/email as `null`, never the password; reads = `current_workspace`); `PUT /smtp/config` (admin; fields as `SMTPConfigUpdate`; only given fields written; response `{"status":"ok"}`; hardening D2: `smtp_host` must be a bare host name or IP (no scheme, path, whitespace, `@`), `smtp_port` 1-65535, `smtp_max_per_hour` 1-100000, 422 otherwise; blank values ignored as in `set_secret`); `POST /smtp/test` (admin; body `{to_email}`; builds a fixed test message, sends through `smtpclient` with the egress dial guard; success `{"status":"ok","message":"Test email sent to <addr>"}`, failure 400 with the Python error text (`SMTP not configured`, `invalid recipient address`, server reply); hardening: 10 requests per minute per workspace via `ratelimit` (429 body as plan 02), recipient validated before any dial).

- [ ] **Step 1: Write failing tests**: `TestStatusNeverReturnsPassword`, `TestStatusShapeMatchesFixtures`, `TestConfigWritesOnlyGivenFields`, `TestConfigRequiresAdmin`, `TestConfigHostValidation422`, `TestConfigPortRange422`, `TestSMTPTestSendsViaFakeServer`, `TestSMTPTestUnconfigured400`, `TestSMTPTestInvalidRecipient400`, `TestSMTPTestBlockedHostFailsClosed` (host resolves to a private IP -> 400, no connection), `TestSMTPTestRateLimited`, `TestConfigTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach SMTP routes (RED)`, `feat(go): SMTP status, config and test routes`.

### Task 23: Suppression routes

**Files:**
- Modify: stub `internal/api/outreachsuppressions.go`; Create: tests + `testdata/contract/outreach/suppressions_*.json`

**Interfaces:** Produces `GET /suppressions` (`{"suppressions":[...]}`, limit 500); `POST /suppressions` (admin; body `{email}`; `reason="manual"`, `source="manual"`, unlocked; `{"status":"ok","added":bool}`; hardening: empty or unparseable email 422 `detail` list instead of storing an empty key; email <= 320 chars); `DELETE /suppressions/{email}` (admin; URL-decoded path value normalized before lookup; 404 `Suppression not found`; 403 `Cannot remove an unsubscribe/complaint suppression`; `{"status":"ok"}`).

- [ ] **Step 1: Write failing tests**: `TestSuppressionRoutesMatchFixtures`, `TestAddDuplicateReturnsAddedFalse`, `TestAddGmailVariantsShareKey`, `TestRemoveLocked403`, `TestRemoveUnknown404`, `TestRemoveNormalizesPathEmail` (percent-encoding, case, IDNA), `TestEmptyEmail422`, `TestRequiresAdminForMutations`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach suppression routes (RED)`, `feat(go): suppression routes`.

### Task 24: Grounded draft read routes

**Files:**
- Modify: stub `internal/api/outreachdrafts.go`; Create: `internal/db/queries/outreach_drafts.sql`, tests + `testdata/contract/outreach/drafts_*.json`

**Interfaces:** Produces `GET /drafts?limit=50&offset=0` (`limit` 1-200 else FastAPI 422, `offset >= 0`; newest first by `created_at`; body `{"drafts":[...],"limit","offset"}` with the `_draft_to_api` key order, `include_evidence=false` omitting `sentence_evidence`/`source_snapshot`; `sent_at`/`created_at`/`updated_at` ISO strings or `null`; `send_performed`) and `GET /drafts/{draft_id}` (with evidence; 404 `Draft not found`). Read-only; `current_workspace`.

- [ ] **Step 1: Write failing tests**: `TestDraftsListMatchesFixtures`, `TestDraftsPaginationBounds422`, `TestDraftGetWithEvidence`, `TestDraftForeignWorkspace404`, `TestDraftKeyOrderMatchesPython`, `TestViewerAllowed`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach draft routes (RED)`, `feat(go): grounded draft read routes`.

### Task 25: Public unsubscribe and bounce webhook

**Files:**
- Modify: stub `internal/api/outreachpublic.go`; Create: tests + `testdata/contract/outreach/{unsubscribe,webhook}_*.json` (raw HTML bodies preserved)

**Interfaces:** Produces `GET /unsubscribe?token=` (public; invalid -> 400 `<h2>Invalid unsubscribe link</h2>` HTML; valid or expired -> the Python HTML with the email and a form posting to `/api/outreach/unsubscribe?token=<token>`; hardening D5: email and token HTML/attribute-escaped, so the markup differs from Python only when the value contains `& < > " '`), `POST /unsubscribe?token=` (RFC 8058 one-click, no auth, no CSRF; rate limit 20 per 60 s per client IP and per token prefix (32 chars) with 429 `{"detail":"rate limited"}`; the client IP comes from the trusted proxy header configured for nginx, not from the proxy's own address, and the limiter map is bounded and expires entries (D5); invalid or expired -> 400 `invalid or expired token`; valid -> `InWorkspace(ws)` then locked suppression (`reason="unsubscribe"`, `source=seq or "unsubscribe"`), `{"status":"unsubscribed"}`, idempotent), `POST /webhooks/bounce` (platform secret first: unset -> 503, `X-Webhook-Secret` compared in constant time -> 403 `invalid signature`; body `{message_id, event="bounce", hard=true}`; global lookup via `LookupSendAnyWorkspace` -> 404 `message not found`; kind `complaint | hard | soft`; then in the owning workspace: ledger insert keyed `(imap_uid=message_id, uidvalidity="webhook", source_message_id=message_id)` -> replay `{"status":"duplicate"}`, else `ApplyBounce(source="webhook")` and `{"status":"ok"}`; a missing master key answers 503 `unsubscribe not configured` instead of crashing, D6).

- [ ] **Step 1: Write failing tests**: `TestUnsubscribeLandingMatchesFixtures`, `TestLandingEscapesEmailAndToken`, `TestExpiredTokenLandingRenders`, `TestUnsubscribePostAcceptsPythonToken`, `TestUnsubscribePostRejectsExpiredAndForged`, `TestUnsubscribeIdempotentAndLocked`, `TestUnsubscribeNoAuthNoCSRF`, `TestUnsubscribeRateLimitPerIPAndToken`, `TestRateLimiterBoundedMemory`, `TestUnsubscribeUsesRealClientIP`, `TestWebhookSecretOrder` (503, 403, 404), `TestWebhookConstantTimeCompare`, `TestWebhookKindsApplied`, `TestWebhookReplayDuplicate`, `TestWebhookCrossTenantIsolation` (workspace B id never reaches A), `TestWebhookNeverReadsOtherTenantRows`, `TestMissingMasterKey503NoPanic`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): public unsubscribe and bounce webhook (RED)`, `feat(go): public unsubscribe routes`, `feat(go): bounce webhook with definer lookup`.

### Task 26: Contract coverage, nginx split, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf` (one delimited `# BEGIN outreach` / `# END outreach` section), `scripts/nginx-routing-check.sh` (cases only), `testdata/contract/README.md` (deviations D2-D6, D8)
- Create: `testdata/contract/retained_outreach_routes.txt`, `docs/go-port/runbooks/outreach-cutover.md`, `internal/api/outreach_coverage_test.go`

**Interfaces:** Consumes everything. Produces per-route nginx blocks (retained path first, always `api_backend`: `POST /api/campaigns/generate`; ported `/api/outreach/*` follow the canary bucket; `/api/outreach/unsubscribe` and `/api/outreach/webhooks/bounce` are sticky to one stack per request source and forward `X-Forwarded-For`/`X-Real-IP` so the public limiter sees the client; unsubscribe links already sent must keep resolving at every canary percentage because tokens verify on both stacks); runbook: prerequisites (Task 2 migration applied, `SECRETS_MASTER_KEY` identical on both stacks, `OUTREACH_PUBLIC_BASE_URL`, `OUTREACH_BOUNCE_WEBHOOK_SECRET`, `AUTOMATIONS_ENABLED`), flag order (`GO_OUTREACH_ROUTES_ENABLED` first, `GO_EXECUTOR_OUTREACH_INBOUND`, then `GO_EXECUTOR_OUTREACH_SEND` last), why the Python scheduler stays, drain procedure (set flag off, wait for `send` rows with `executor='go'` to leave `pending/processing`; an `in_flight` row older than the job timeout is a replay-skip by design), rollback, bounce/complaint spot checks, SSRF allow-list for self-hosted SMTP (`EGRESS_ALLOW_HOSTS`, bench only) and the D2 plaintext refusal.

- [ ] **Step 1: Write failing tests** `TestEveryOutreachRouteIsPortedOrRetained` (parses `routers/{outreach,campaigns}.py`; every route is in the Go patterns or in `retained_outreach_routes.txt`; expected 22 ported + 1 retained), `TestRetainedRoutesNotRegisteredInGo`, `TestEveryOutreachJobTypeHandledOrPython` (`send` and `outreach_inbound_poll` dispatched by Go, others not), `TestNoOutreachSecretInFixturesOrLogs` (greps fixtures and a captured log for the seeded SMTP/IMAP passwords, master key and webhook secret).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3:** record missing fixtures, write the nginx blocks, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (campaigns always `py`; unsubscribe and webhook sticky; others follow the bucket) -> PASS.
- [ ] **Step 5: Commit** `test(go): outreach route coverage (RED)`, `feat(infra): nginx per-route split for outreach`, `docs(go-port): outreach cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row "send (SMTP), inbound (IMAP), sequences, unsubscribe, campaigns; outreach.db to Postgres": send (13, 16, 17), inbound (10, 15, 18), sequences (12, 20, 21), unsubscribe (6, 7, 25), campaigns (retained, D1), `outreach.db` (dead code, Task 1; Postgres store already exists, so nothing to migrate beyond plan 01's SQLite sweep), webhook (2, 25), drafts (24), SMTP config (8, 22), scheduler interplay (3, 17, 19, runbook). All 22 routes of `routers/outreach.py` are in Tasks 20-25.
- **Consistency:** `outreachstore` functions (12-14) are consumed by 16-18 and 20-25 with the same names; `outreachtoken.Signer` (7) in 25 and 16 (via `smtpmail.Build` inputs); `emailnorm.Normalize` (6) in 12, 14, 16, 21, 23; `smtpclient.Client` (11) behind `outreachsend.Mailer` (5) in 16 and 22; `egress.DialContext` (11) in 15 and 18; `queue.EnqueueOnce` (plan 06) in 17 and 18; `Caps` (5, assumes plan 08) only in 16.
- **Review Focus:** each line has its tests in Tasks 13, 16, 19 (1); 6, 7, 14, 16, 23, 25 (2); 8, 11, 15, 22 (3); 2, 14, 18, 25 (4); 17, 18 (5); 20-23, 25 (6).
- **Proportion:** interfaces, error strings, test names, ordering rules and file ownership carry the decisions; SQL bodies and handler glue are left to the implementer and the Python-recorded vectors.

## Open decisions (defaults apply unless overridden)

- **D1 Campaigns.** `POST /api/campaigns/generate` calls the LLM (`leadgen.llm.extract_json`) per lead. Default: retained on Python, listed in `retained_outreach_routes.txt`; it moves with the copilot/LLM plan, which can reuse `pyworker`.
- **D2 Customer SMTP/IMAP hosts and plaintext.** A workspace admin controls `SMTP_HOST`/`IMAP_HOST`; Python dials anything and with `SMTP_USE_TLS=0` sends the password in clear. Default (confirm): Go dials through the SSRF guard (blocked IPs refused at dial time, certificates verified) and refuses plaintext AUTH except for allow-listed test hosts; host/port validated at `PUT /smtp/config`. Documented deviation. Self-hosted installs with an internal relay use the egress allow-list.
- **D3 Input validation.** Values the Postgres column types reject currently 500 in Python (`send_window_tz` over 40, `name` over 200, etc.). Default: 422 with the standard FastAPI list shape; `PUT status="active"` is refused (activation only through `start`, which enforces SMTP, consent and footer); `enroll` capped at 5000 ids. Documented deviations.
- **D4 IMAP library.** Go has no IMAP in stdlib. Default (confirm): `github.com/emersion/go-imap/v2` pinned (client + `imapmemserver` for tests). Alternative: a hand-written minimal client (about 400 LOC, literal handling risk); not recommended.
- **D5 Public endpoint hardening.** Python escapes nothing on the unsubscribe landing and keys its in-process limiter on the proxy address (all users share one bucket behind nginx) in an unbounded dict. Default: HTML-escape, real client IP from the trusted proxy header, bounded expiring limiter. Documented deviation.
- **D6 Missing master key and parser bounds.** Python raises 500 when no master key resolves; Go answers 503 on public routes and refuses to start the outreach worker handlers with an error log. DSN parsing is bounded (1 MiB, depth 8). Documented deviations.
- **D7 Paid sends without caps.** When `OUTREACH_SEND_COST_USD > 0`, caps reservation depends on plan 08. Default: if no `Caps` is wired the Go worker hands the job back to Python (`queue.HandBack`), never sends unmetered; free sends (default 0) run fully in Go.
- **D8 Cross-tenant bounce lookup.** Python uses an owner connection with no GUC. Default: an Alembic `SECURITY DEFINER` function returning only the routing tuple, executable by the app role only (Task 2); Go never gets a BYPASSRLS connection.
- **D9 Scheduler.** The Python scheduler remains the only ticker for `bootstrap_outreach_schedules` and `bootstrap_inbound_schedules`; it enqueues with `executor` per flag. Go exposes `Tick`/`EnqueueSend` for a future Go scheduler. Default: no Go scheduler in this plan.
- **D10 Rollout.** Default: all new Go paths off (`GO_OUTREACH_ROUTES_ENABLED`, `GO_EXECUTOR_OUTREACH_SEND`, `GO_EXECUTOR_OUTREACH_INBOUND` false); routes by canary bucket, inbound before send; unsubscribe and webhook sticky.

## User decisions

(none yet; the defaults above apply until the user objects)

## User decisions (2026-10-26) — defaults applied under the standing rules

- D1: `POST /api/campaigns/generate` (LLM) stays on Python.
- D2: SMTP/IMAP dialed through the SSRF guard with certificate verification; plaintext AUTH refused except allow-listed test hosts; host/port validated (documented deviation).
- D3: out-of-range or over-long inputs answer 422 instead of 500; `PUT` cannot activate a sequence; `enroll` capped at 5000 ids (documented deviations).
- D4: `github.com/emersion/go-imap/v2` pinned for IMAP.
- D5: unsubscribe landing escaped; public limiter uses the real client IP and bounded memory (documented deviation).
- D6: missing master key is 503 and a startup error log, never a panic; DSN parsing bounded.
- D7: paid sends without plan 08 caps are handed back to Python; free sends run in Go.
- D8: cross-tenant bounce lookup via a `SECURITY DEFINER` function (Alembic), no BYPASSRLS connection in Go.
- D9: Python scheduler remains the only ticker; Go exposes `Tick` and `EnqueueSend`.
- D10: all new Go paths default off; inbound before send; unsubscribe and webhook sticky routing.
