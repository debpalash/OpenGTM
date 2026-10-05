# M8: multi-host state, secrets, collection ledgers and scheduler leadership

Updated: 2026-10-05
Status: implemented on `rewrite/m8-multihost`, **opt-in per flag**; the single-host
SQLite/local install is unchanged by default. Part of the
[hybrid platform RFC](hybrid-platform-rewrite.md) (milestone M8, "multi-host
readiness").

M8 is the milestone that makes a second host safe. This note records exactly what
state still lives outside PostgreSQL, where each item goes, what was built, how
to move a single-host install to multi-host, and what is still single-host only.
It is deliberately explicit about what was **not** done.

## What was built

| Piece | Flag (default) | Where |
| --- | --- | --- |
| Scheduler leadership: one lease per periodic scheduler with fencing tokens | `SCHEDULER_LEADER_ELECTION=false`, `SCHEDULER_LEASE_TTL_SECONDS=30` | `apps/api/services/scheduler_lease.py`, `apps/api/scheduler.py`; Go twin in `apps/server/internal/lease`, `opengtm leases` |
| Workspace directory, SSO/SCIM, per-workspace settings and encrypted secrets, global settings | `WORKSPACE_META_STORE=sqlite` | `apps/api/services/workspace/pg_meta.py` (backend), `manager.py`, `secrets.py`, `routers/settings.py` (hooks) |
| Collection job ledger (`jobs`, `job_stages` of the lead pipeline) | `COLLECTION_LEDGER_STORE=sqlite` | `apps/api/services/leadgen/ledger.py` |
| Idempotent backfill + verification | n/a (a command) | `python -m apps.api.scripts.multihost_backfill` |
| Migrations | n/a | `9b3d5f7a2c41` (leases, directory, settings), `b5d7f9a1c3e6` (ledger); both PostgreSQL-only, additive, reversible |

Every flag defaults to the existing behaviour, so merging this changes nothing on
a single-host install. `WORKSPACE_META_STORE`, `COLLECTION_LEDGER_STORE` and
`SCHEDULER_LEADER_ELECTION` must be identical on every replica of a deployment
(see [Rollback](#rollback)).

## Inventory: state outside PostgreSQL

"Status" is the state on this branch with the flags on. Hazard severity is for a
second host reading the same PostgreSQL.

| # | Item | Current location | Target | Migration / backfill | Multi-host hazard | Status |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | **Scheduler loop** (nine `bootstrap_*` reconcilers) | `apps/api/scheduler.py`, no election; every bootstrap runs in every scheduler process | `scheduler_leases` row per subsystem, fencing token | none (new table) | Two schedulers double-fire. Most enqueues are idempotent (`fire_key`), but `signal_scan` (`signal_scan:{now+interval}`), `outreach_inbound` (`inbound:{ws}:{now+interval}`) and `audiences` (`now+interval`) use non-deterministic keys: a race creates two self-perpetuating chains, duplicate IMAP sessions, or cancel-and-reschedule churn. Outreach send dedup is a non-atomic scan backed by a unique index. | **Done** (flagged) |
| 2 | **Workspace directory**: workspaces, members, permissions, OIDC identities, SCIM tokens/users/groups, active workspace | `data/workspaces.db` (SQLite) | 10 PostgreSQL tables, FORCE RLS (`workspace_id = app.workspace_id OR app.control_plane = on`) | `multihost_backfill --stores meta` | A second host has its own, divergent membership and sees an empty workspace list; `signal_scan` iterates `list_workspaces()` | **Done** (flagged) |
| 3 | **Per-workspace secrets** (envelope-encrypted `enc:v1:` / `enc:v2:vault:`) and workspace settings (OIDC config, ...) | `data/workspaces.db`, table `workspace_settings` | `workspace_settings` in PostgreSQL, **strict** FORCE RLS (only the bound workspace) | copied as opaque ciphertext; `--check-decrypt` proves the key | Secrets unreadable on every other host | **Done** (flagged) |
| 4 | **Global settings**, including provider API keys (plaintext) and `ACTIVE_WORKSPACE` | `data/data.db`, table `settings`; also read directly by `leadgen/llm.py` | `app_settings`, FORCE RLS (control-plane flag only); credential-named keys enveloped at rest | backfill envelopes keys named `*_KEY/_TOKEN/_SECRET/_PASSWORD/...` | Per-host API keys; plaintext at rest | **Done** (flagged) |
| 5 | **Collection ledger** `jobs`, `job_stages` | `data/leads.db` (main), `data/workspaces/<slug>/leads.db`; even when leads are in PostgreSQL | `collection_jobs`, `collection_job_stages`, FORCE RLS | `multihost_backfill --stores ledger` (tenant attribution below) | A collect job claimed on host B is invisible to the API on host A; `/api/jobs` polling returns 404 | **Done** (flagged) |
| 6 | `llm_usage` (daily counters) in the same files | `leads.db` | already `llm_usage_daily` for new usage (via the PostgreSQL lead store) | **not moved**: merging two counters needs a sum, which cannot be idempotent | History per host stays in SQLite; no correctness impact | Partly: new usage lands in PostgreSQL |
| 7 | `activity_log` | `leads.db` | n/a | has no writers | none | Not moved (dead) |
| 8 | Leads and signals | `PgLeadStore` (default on PostgreSQL); per-workspace SQLite only with `PG_LEAD_STORE=false` | already PostgreSQL | existing `scripts/migrate_sqlite_to_pg.py` | `PG_LEAD_STORE=false` is single-host only | Already PostgreSQL by default |
| 9 | Legacy `signals.db`, `outreach.db`, `reset_admin.py` | `data/*.db` | already migrated once by `migrate_sqlite_to_pg.py`; `sequence.py` is dead code | n/a | dead | n/a |
| 10 | **Workbook functions** | `data/functions.db` | PostgreSQL table (not workspace scoped today) | not done | functions created on one host are missing on another | **Not done** |
| 11 | **Chat history and tool approvals** | `data/chat_history.db` | PostgreSQL, FORCE RLS on `workspace_id` | not done | a conversation or a pending tool approval is only on one host; transcripts are unencrypted and tenant isolation there is a `WHERE` only | **Not done** |
| 12 | **Agent memory** | `data/openmemory.db` / `data/chat_memory.db` | PostgreSQL | not done | per-host memory | **Not done** |
| 13 | **Enrichment and email-verify caches** | `data/enrichment_cache.db` | per-host is acceptable (pure TTL cache, misses only); a shared table would raise hit rate | none needed | wasted provider calls, not wrong results | Not done, tolerable |
| 14 | **Autopilot plan store** (single-use approval nonce) | process memory, `services/agent/autopilot_plan_store.py` | PostgreSQL table with a single-use `DELETE ... RETURNING` consume | not done | plan and approve landing on different replicas fail with an unknown `plan_id` (fails closed, but the feature breaks) | **Not done** |
| 15 | **API rate limiter** (`slowapi`, ten routes) and the public unsubscribe limiter | process memory (`core/ratelimit.py`, `routers/outreach.py`) | Redis (`storage_uri`) or a PostgreSQL counter | not done | effective limit is N times the configured one | **Not done** |
| 16 | **Per-domain provider rate limiters and circuit breakers** | per `StealthClient` / per job subprocess (`leadgen/rate_limiter.py`, SEC/GLEIF spacers) | a shared limiter (Redis token bucket or PostgreSQL) | not done | per-IP provider caps (SEC 10 req/s) can be exceeded; already true across concurrent jobs on one host | **Not done** |
| 17 | **Live progress** (SSE, workbook WebSocket) | Redis pub/sub if `REDIS_URL` is set, else per-process; Go uses PostgreSQL `LISTEN/NOTIFY` | Redis required for multi-host; the two transports are not bridged | config | clients on another replica silently miss events; `workbook/enrichment.py` and `routers/workbooks.py` default `REDIS_URL` to `redis://localhost:6379` | **Not done** (operational requirement) |
| 18 | **Uploaded files** | none: no `UploadFile` endpoint exists | n/a | n/a | n/a | Nothing to move |
| 19 | **Exports and backups on disk** | HTTP exports stream; CLI exports and `create_backup` tarballs write under `data/`; `routers/system.py` browses and deletes under `data/` | object storage or a shared volume | not done | per-host files | **Not done** |
| 20 | **Plugin and connector installs** | local directories (`OPENGTM_PLUGIN_DIRS`, connector install dir with a host-local `flock`) | identical directories per replica, or a shared store | not done | installing on one host does not install on another; the Go catalog loads once at start | **Not done** |
| 21 | **Go in-process state** | egress per-domain limiter and robots cache; 30 s authz cache | per process | none | per-host politeness limits (fine when each host has its own egress IP); a revoked membership can stay valid up to 30 s | By design |
| 22 | Durable queue, heartbeats, claims, retention schedule mirrors, intent-poller state, billing ledger, ingest idempotency keys, outreach send limits | PostgreSQL already | n/a | n/a | already multi-replica safe (`FOR UPDATE SKIP LOCKED`, fire keys, unique indexes) | Already PostgreSQL |

Two observations that are not M8 work but matter to it:

* `bootstrap_retention_schedules` enqueues today's `retention:<ws>:<date>` job and
  then calls `schedule_policy`, which cancels every pending `retention:<ws>:%`
  job, including the one just enqueued, before enqueueing tomorrow's. Read the
  code before relying on scheduled retention runs; the Go port copies the same
  behaviour. This was found while inventorying and is unrelated to the leases.
* The Alembic migration `e5f6a7b8c9d0` reads `manager.list_workspaces()` to attribute
  legacy NULL-workspace workbooks. With `WORKSPACE_META_STORE=postgres` that lookup
  hits tables a database at that revision does not have yet. It only runs on data
  that already needed that rare backfill; run it on the single-host install first.

## Design

### Scheduler leadership

One row per scheduler in `scheduler_leases` (`name = scheduler:<subsystem>`,
`holder`, `fencing_token`, `expires_at`). A holder owns it until `expires_at`
**on the database clock**, so host clock skew cannot create two leaders.

* **Acquire / renew** is one `INSERT ... ON CONFLICT DO UPDATE ... WHERE holder = me
  OR expired`. The token increases on every change of holder and when the same
  holder re-acquires after a lapse; a live renewal keeps it.
* A scheduler process polls every lease each tick and a heartbeat thread renews
  every `TTL/3`, so a long bootstrap does not lose the lease. A leader that cannot
  renew stops leading when its conservative local deadline passes
  (0.8 x TTL from before the request was sent).
* **Fencing.** Inside a leader's bootstrap every ORM `commit` first runs
  `SELECT 1 FROM scheduler_leases WHERE name=.. AND holder=.. AND fencing_token=..
  AND expires_at > clock_timestamp() FOR SHARE`. A takeover (`UPDATE`) waits for
  in-flight fenced commits, and afterwards the old token matches nothing, so a
  deposed or paused leader's transaction is aborted (`LeaseLost`) instead of
  committed. The token captured when the bootstrap started is the one checked, so
  a lease re-won after a lapse does not bless old work.
* **Hand-over.** A graceful stop releases the leases (a peer leads at once); a
  crash is replaced after at most the TTL; `opengtm leases release <name>` forces
  it when the holder is known to be gone.
* Why not `pg_advisory_lock`: a session lock needs a dedicated connection that
  transaction-pooling proxies break, vanishes on a connection reset without telling
  the holder, and carries no token. A lease row survives pooling, is observable
  (`opengtm leases list`) and fences.
* Enabling it on SQLite is a startup error, not a silent no-op.

What it does and does not do: it makes each bootstrap run in exactly one scheduler
at a time and prevents a deposed leader from committing. It does not make the
three non-deterministic fire keys deterministic; they are simply no longer raced.

### Control plane: workspace directory, secrets, settings

`WORKSPACE_META_STORE=postgres` makes `manager._get_db()` return a
`PgMetaConnection`, a small implementation of the sqlite3 connection API used by
`manager.py`, `scim.py`, `oidc.py` and `secrets.py`, over SQLAlchemy. Their SQL and
logic are untouched; the dialect differences it bridges are `?` placeholders,
`INSERT OR REPLACE/IGNORE` and `BEGIN IMMEDIATE`, listed in `pg_meta.translate`.

Row-level security, everything FORCEd, role must be `NOSUPERUSER NOBYPASSRLS`
(refused otherwise, like `PgLeadStore`):

| Table | Policy |
| --- | --- |
| `workspace_settings` (secrets) | `workspace_id = app.workspace_id`, nothing else; not even the control-plane flag opens it |
| `workspaces`, members, permissions, OIDC, SCIM, `user_active_workspace` | `workspace_id = app.workspace_id` **or** `app.control_plane = on` |
| `app_settings` | `app.control_plane = on` only |
| `collection_jobs`, `collection_job_stages` | `workspace_id = app.workspace_id` |
| `scheduler_leases` | no workspace dimension (global control table, like `jobs`) |

Membership lookups are cross-tenant by nature ("which workspaces is this user
in?"), so the directory code path opts in with `app.control_plane`, set with
`set_config(..., true)` per transaction. A tenant-bound ORM session never sets it,
so a missing `WHERE` elsewhere cannot read another tenant's members, SCIM tokens or
identities. This is defence in depth, not a boundary against code that can run
arbitrary SQL, which can set the flag too, the same as `app.workspace_id`.

Secrets keep the existing mechanism (`enc:v1:` Fernet from `SECRETS_MASTER_KEY`,
`enc:v2:vault:` Transit): the same envelopes are written and copied as opaque
strings, `rotate_encrypted_secrets` rotates each workspace in its own tenant-bound
transaction, global credential settings get the envelope on write, and nothing
logs a value or a SQL parameter. The master key must be identical on every host
(and is not stored in PostgreSQL).

Cost: each manager call is a pooled connection and about three round trips (bind,
query, end). Measured on loopback: p50 0.3 to 0.4 ms per call. The request path
makes three to four such calls, so a cross-host deployment pays that times its
network RTT; there is no cache, by design (authorization must not go stale), and a
cache is a later, measured decision.

### Collection ledger

`COLLECTION_LEDGER_STORE=postgres` swaps `LeadDB(workspace_leads_db_path(slug))` for
`PgCollectionLedger(workspace_id)` at the six places that opened it for
bookkeeping (`open_job_ledger`). It implements the job/stage surface with the same
semantics (re-queue until `max_attempts`, cancel only active jobs, retry only
failed/cancelled), claims with `FOR UPDATE SKIP LOCKED`, accepts the raw
`jobs` / `job_stages` SQL a few call sites still issue, ends read transactions so a
run does not hold an idle-in-transaction connection, and requires the PostgreSQL
lead store so leads and ledger live together. Times stay ISO-8601 text so API
responses are identical.

## Backfill and verification

```bash
export DATABASE_URL=postgresql+psycopg://yupcha_app:...@pg:5432/yupcha   # runtime role
uv run python -m apps.api.scripts.multihost_backfill plan   --data-dir ./data
uv run python -m apps.api.scripts.multihost_backfill run    --data-dir ./data --check-decrypt
uv run python -m apps.api.scripts.multihost_backfill verify --data-dir ./data --check-decrypt
```

* **Read-only sources.** The SQLite files are opened `mode=ro`; nothing is created,
  altered or deleted.
* **Idempotent and resumable.** Rows are upserted by primary key in per-batch
  transactions. Re-run after any interruption; running N times equals once.
  Resuming costs a read per already-copied row, not a rewrite.
* **Non-destructive.** The default skips rows that already exist in PostgreSQL, so
  a re-run after cut-over cannot overwrite newer writes. `--overwrite` refreshes
  them and is only for the window before cut-over while the source is the truth.
* **Verified.** `verify` (also run by `run`) compares per table the row count and
  an order-independent checksum (sum of per-row SHA-256 mod 2^256) over the same
  normalised columns on both sides, and lists missing, extra and changed **keys**
  (capped). Ciphertext is compared as the opaque string it is; the global-settings
  digest hashes the decrypted value, so the enveloping step is checked as well.
  Exit code 1 on any difference. `--check-decrypt` proves every envelope decrypts
  with the current key and reports counts and `workspace/key` names only.
* **`main` workspace.** The target auto-creates a `main` workspace with a fresh id on
  first use; the source has its own. An untouched target `main` is replaced by the
  source's, one that already has dependent rows is refused for the operator to
  settle.
* **Ledger attribution.** A per-workspace `leads.db` belongs to that workspace. The
  main `leads.db` belongs to `main`, except a job whose own `workspace_id` names
  another known workspace follows that stamp (it was written by that tenant); a blank
  or unknown stamp falls back to `main`. Stages follow their job; stages whose job is
  not in the file are skipped and counted, never attached to a guessed tenant. The
  report's `_attribution` row counts both.
* **Restore proof.** `pg_dump`/`pg_restore` of the backfilled database into a fresh
  database re-verifies against the original SQLite sources, the secrets still
  decrypt with the same key, forced RLS and every policy came back, and the dump
  contains no plaintext secret (all three are asserted in
  `tests/test_multihost_backfill_pg.py`).

## Operator procedure: single host to multi-host

Do this in a maintenance window; the directory has no writer-side merge.

1. **Prerequisites.** PostgreSQL (runtime role `NOSUPERUSER NOBYPASSRLS`, as
   `opengtm doctor` checks). The **same** `SECRETS_MASTER_KEY` (or `SECRET_KEY` if the
   key is derived from it, and `SECRETS_PROVIDER` settings) on every host, kept
   outside the database. A reachable Redis (`REDIS_URL`, no `localhost` default) for
   progress and live updates. `OPENGTM_LEGACY_API_URL` pointing at a Python API load
   balancer. Identical plugin and connector directories on every host.
2. **Back up.** `uv run python -m apps.api.cli backup --output backup.tar.gz --database-url <owner URL>`
   and `backup-verify`; keep it until step 9. After the flags are on, the same
   `pg_dump` covers the directory, secrets and ledger (still no plaintext secret).
3. **Migrate.** `alembic upgrade head` (migrations `9b3d5f7a2c41`, `b5d7f9a1c3e6`;
   additive).
4. **Quiesce writers.** Stop every Python API, worker and scheduler process. Do not
   start any process with a `postgres` flag yet. Confirm the jobs table has no
   `processing` collect jobs (the queue reaper recovers stuck ones).
5. **Backfill and verify.** Run `plan`, then `run --check-decrypt` (above). Proceed only
   when it prints `"ok": true` and `all_decrypt` is true. If the source changed
   while it ran, run `run --overwrite` once more, still with writers stopped.
6. **Flip the flags on every replica at once:** `WORKSPACE_META_STORE=postgres`,
   `COLLECTION_LEDGER_STORE=postgres`, `SCHEDULER_LEADER_ELECTION=true`. Mixing values
   across replicas splits the directory and the ledger between two stores.
7. **Start** the API replicas, workers, and as many schedulers as you like on any
   hosts. `opengtm leases list` shows one live holder per `scheduler:<name>`.
8. **Smoke test.** Log in on each host, switch workspaces, read a workspace secret
   through an integration, start a collection on host A and poll it on host B,
   `opengtm leases list`, stop the leader and watch another take over within the TTL.
9. **Keep** `data/workspaces.db`, `data/data.db` and `leads.db` files for the rollback
   window; they are no longer written.

## Rollback

Flags first, data second.

* Stop all replicas; set `WORKSPACE_META_STORE`, `COLLECTION_LEDGER_STORE` back to
  `sqlite` and `SCHEDULER_LEADER_ELECTION` to `false`; start. The untouched SQLite
  files are the truth again. **Anything written to PostgreSQL after step 6 (new
  workspaces, members, secrets, collection jobs) is not in the SQLite files** and is
  lost to the single-host install unless exported first; take a `pg_dump` and decide
  before flipping back. There is no PostgreSQL to SQLite reverse backfill.
* Schema: `alembic downgrade 7c1e5a9d3b20` drops the new tables (reversible; data in
  them is dropped, so back up first).
* Scheduler leases alone: set `SCHEDULER_LEADER_ELECTION=false` and run a single
  scheduler; nothing else depends on the flag.

## What is proven, and what is not

Exit gate: "backfill/restore proof, reconciliation, and replicas on independent hosts
pass."

Proven here, against real PostgreSQL 18 as a `NOSUPERUSER NOBYPASSRLS` role (commands in
each test file's docstring; all PG-gated on `TEST_DATABASE_URL`, throwaway databases):

* Backfill: idempotent re-run, crash resume, divergence detection, no clobber,
  sources untouched (`tests/test_multihost_backfill_pg.py`, `tests/test_multihost_ledger_pg.py`).
* Restore: `pg_dump`/`pg_restore` into a fresh database re-verifies, secrets decrypt,
  RLS intact, dump has no plaintext secret.
* Reconciliation: `verify` (counts and checksums per table, divergence by key) and the
  existing SCIM, OIDC, RBAC and workspace-context test suites re-run unchanged against
  PostgreSQL (`tests/test_multihost_meta_parity_pg.py`), and the ledger runs the same
  operation sequence as `LeadDB` with identical results.
* Leadership: 12 contending replicas yield one leader; three OS processes with
  SIGKILL of the leader fail over within TTL with monotonic tokens and no stale-token
  write; a deposed holder's commit is aborted; a takeover waits for an in-flight fenced
  commit; Go and Python share the protocol (`tests/test_scheduler_lease_pg.py`,
  `..._go_interop_pg.py`, `apps/server/internal/lease`, race detector on).
* Tenant isolation: every new tenant table is FORCE RLS and tested for cross-tenant
  read, write, update, delete and unbound access.

**Not proven: replicas on independent hosts.** Every test runs on one machine: the
"hosts" are separate OS processes or separate connection pools against one PostgreSQL
over loopback. That exercises the database protocol (separate sessions, SIGKILL,
contention, locks), which is where the correctness is decided, but not network
partitions, real clock skew between hosts (the design uses only the database clock),
host loss with in-flight TCP, or latency-dependent behaviour. Closing the gate needs a
run with the API, workers and two schedulers on at least two machines against a shared
PostgreSQL and Redis, including a partition of the leader from PostgreSQL. No
production-sized backfill was run either; the tables are small by nature, but the
ledger can be large and its timings are unmeasured.

## Still single-host only

With every M8 flag on, these remain single-host and must not be relied on across hosts
until done (rows refer to the inventory):

* Workbook functions (`functions.db`), chat history and tool approvals
  (`chat_history.db`), agent memory (`openmemory.db`/`chat_memory.db`): #10 to #12.
* The autopilot plan store (plan and approve must reach the same replica): #14.
* The API rate limiter and the unsubscribe limiter (limits multiply by N): #15, and
  per-domain provider limiters and breakers: #16.
* Live progress without a shared Redis: #17. The Go `LISTEN/NOTIFY` and Python Redis
  transports are not bridged.
* Exports, backups and any file written under `data/`, and the `data/` browser in
  `routers/system.py`: #19.
* Plugin and connector installs, and the Go plugin catalog (loaded at start): #20.
* Per-host enrichment and email-verify caches (correct, just not shared): #13.
* `PG_LEAD_STORE=false` (per-workspace SQLite leads), and historical `llm_usage`
  counters in `leads.db`: #8, #6.
* The `leads_count` badge in `list_workspaces`, which counts a local `leads.db` and is
  wrong on any host that does not have it.
* The three non-deterministic scheduler fire keys are serialized by the lease but not
  made deterministic.
* One flag value per deployment: a mixed fleet is unsupported.
