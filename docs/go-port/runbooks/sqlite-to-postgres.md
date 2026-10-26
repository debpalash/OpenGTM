# Runbook: SQLite stores to PostgreSQL (cutover and rollback)

Moves the identity-plane, settings, chat, function and memory SQLite stores into PostgreSQL so
the Go API can serve them. Schema is owned by Alembic (head `e1f5a9b3d7c2`); Go never runs DDL.
Every Python command below is run from `legacy/python/` via `uv run` (`uv run python -m ...`) with
`DATABASE_URL` set to the **owner** PostgreSQL URL (the backfill needs the table owner, not the
runtime role). Every `docker compose` command is run from the **repo root**. Each code block states
its working directory in a leading `# cwd:` comment; do not rely on a `cd` carrying over between blocks.

## Variables used throughout

```bash
# The LIVE data dir: the repo-root ./data that docker-compose mounts at /app/data.
# This is NOT legacy/python/data (a stale dev copy). Use an absolute path.
export DATA_DIR=/abs/path/to/OpenGTM/data
export DATABASE_URL=postgresql+psycopg://<owner>:<pw>@<host>:5432/<db>   # SQLAlchemy form, read by the Python CLIs

# psql connection: password NEVER on argv (visible in `ps` / /proc/*/cmdline and shell history).
export PGHOST=<host> PGPORT=5432 PGUSER=<owner> PGDATABASE=<db>
# Provide the password via ~/.pgpass (chmod 600, line: host:5432:db:owner:password),
# or a PGSERVICEFILE, or export PGPASSWORD in an interactive subshell (read -rs PGPASSWORD).
# Never put it in a URL passed to psql, and never pass --pg-url: the stores CLI defaults to $DATABASE_URL.
```

> **Prerequisite: rewire Docker/compose to `legacy/python`: DONE.**
> The root `Dockerfile` now copies `apps/api/`, `migrations/` and `alembic.ini` from `legacy/python/`
> (build context stays the repo root). `docker compose build` for `migrate`/`api`/`worker`/`scheduler`/`seed`
> succeeds, and `migrate` was verified against a throwaway Postgres: it reaches head `e1f5a9b3d7c2`, so
> `docker compose run --rm migrate` and the in-image (`/app`) route work.
> **Remaining CI caveat:** `legacy/python/` is gitignored (untracked by design), so these builds only work
> from a working tree that contains it. CI or any remote build from a clean git checkout must provide
> `legacy/python/` first, or the `COPY legacy/python/...` steps fail.

SQLite files are never modified or deleted by any step of this runbook.

## Deploy order (important)

`migrate` -> Go worker (`worker-go`) -> Python API. The Go `queue.CheckSchema` is an exact match on
the required Alembic revision (`e1f5a9b3d7c2`), so a Go worker started before `migrate` (or against an
older schema) refuses to start, and the Python API must not serve the new backend before the schema
exists. During the cutover window everything is stopped, so this order is applied in step 6.

## 1. Pre-flight

```bash
# cwd: legacy/python
export SQLITE_STORES_BACKEND=sqlite          # must be sqlite until step 6

# integrity-checked PostgreSQL + $DATA_DIR backup (refuses to overwrite an existing archive)
BACKUP=backups/pre-stores-cutover-$(date +%Y%m%d%H%M).tar.gz
uv run python -m apps.api.cli backup --output "$BACKUP" --data-dir "$DATA_DIR"
uv run python -m apps.api.cli backup-verify "$BACKUP"

# record source counts (compare with the verify output in step 5)
sqlite3 "$DATA_DIR"/workspaces.db 'select count(*) from workspaces'
sqlite3 "$DATA_DIR"/workspaces.db 'select count(*) from workspace_members'
sqlite3 "$DATA_DIR"/workspaces.db 'select count(*) from workspace_settings'
sqlite3 "$DATA_DIR"/workspaces.db 'select count(*) from user_active_workspace'
sqlite3 "$DATA_DIR"/data.db       'select count(*) from settings'
sqlite3 "$DATA_DIR"/chat_history.db 'select count(*) from conversations'
sqlite3 "$DATA_DIR"/chat_history.db 'select count(*) from messages'
```

Do not proceed if the backup does not verify. (Archives made before the PostgreSQL dump digest was
added carry no `postgres` manifest key; they still verify against the per-file inventory and print a
warning.) The backfill dry-run needs the migrated schema, so it runs after step 3 (see 3b).

## 2. Stop services

```bash
docker compose stop api worker scheduler worker-go
```

(`worker-go` only runs with `--profile go`; stopping it when absent is harmless.) Nothing may write
to the SQLite files or PostgreSQL from here until step 6.

## 3. Migrate PostgreSQL to the new head

```bash
# cwd: repo root
docker compose run --rm migrate                 # requires the rewired image (see prerequisite)
```

```bash
# cwd: legacy/python   (host-side equivalent; DATABASE_URL = owner URL)
YUPCHA_DB_INIT=alembic uv run alembic upgrade head
uv run alembic current                          # expect: e1f5a9b3d7c2 (head)
```

### 3b. Optional dry-run (needs the migrated schema)

```bash
# cwd: legacy/python
uv run python -m apps.api.scripts.sqlite_stores.cli backfill --data-dir "$DATA_DIR" --dry-run   # writes nothing
```

## 4. Backfill

```bash
# cwd: legacy/python
uv run python -m apps.api.scripts.sqlite_stores.cli backfill --data-dir "$DATA_DIR"
```

Idempotent (`INSERT ... ON CONFLICT DO UPDATE`); safe to re-run. It reads SQLite through the `sqlite3`
backup API into a temp copy, sets `app.workspace_id` per tenant batch for FORCE-RLS tables, and
copies secret ciphertext (`enc:v1:` / `enc:v2:vault:`) verbatim; nothing is decrypted. It fails closed
(exit 1) on unparseable values. Orphans (members/active-workspace rows whose `user_id` has no row in
`users`) are reported, not fatal.

Guard rails:

- **Wrong/empty `DATA_DIR`.** If `workspaces.db` is absent from `--data-dir`, `backfill` and `verify`
  exit 1 with a clear message. Pass `--allow-missing workspaces` only if the install truly has no
  `workspaces.db`. Any other absent store file (`data.db`, `chat_history.db`, ...) prints a `warning:`
  line on stderr naming the missing file(s); read it, a typo in `$DATA_DIR` shows up here.
- **Dangling foreign keys.** SQLite never enforced foreign keys, PostgreSQL does. Before any write the
  backfill checks every enforced parent key (members/settings/SCIM -> `workspaces`, OIDC identities
  and member permissions -> `workspace_members`, SCIM group members -> `workspace_scim_groups`). A
  violation aborts with `BackfillError` naming the table and the offending key ids (workspace ids,
  user ids, group ids; never secret values) and `Nothing was written`. To resolve: in a **copy** of
  the source SQLite file (never edit the live one), delete the orphan child rows or insert the
  missing parent, point `--data-dir` at the copy (or fix in place after the stop in step 2), and re-run.
- **Cutover marker.** The PG-only `__stores_backend` row in `app_settings` is never copied to or from
  SQLite (backfill, verify and export-back all ignore it).
- **Chat message order.** SQLite chat timestamps have 1 s resolution. Messages sharing the same second
  within a conversation get ordinal microsecond offsets in SQLite rowid order, so the PostgreSQL order
  matches the old order. API output is unchanged.
- Conversations with a NULL or empty `workspace_id`/`user_id` are skipped (and counted under `skipped`)
  because they are invisible today.

Per-workspace leads/signals ledgers (`leads.db`, `signals.db`, `workspaces/<slug>/leads.db`) are copied
by the existing `apps/api/scripts/migrate_sqlite_to_pg.py` (run it only if the ledgers have not already
been migrated): `LEGACY_SQLITE="$DATA_DIR/data.db" uv run python -m apps.api.scripts.migrate_sqlite_to_pg`. It only
looks at `data.db` via `LEGACY_SQLITE`; per-workspace ledger paths are resolved by the script itself, so
check its output against the step 5 ledger counts. The stores CLI only verifies them.

## 5. Verify (gate)

```bash
# cwd: legacy/python   (--pg-url defaults to $DATABASE_URL; do not pass the URL on argv)
uv run python -m apps.api.scripts.sqlite_stores.cli verify --data-dir "$DATA_DIR"
echo $?     # must be 0
```

Exit 0 only if every table's SHA-256 digest matches and there is no ledger mismatch. The same command
also runs `verify_ledgers` (leads/signals counts per workspace, `main` -> `leads.db`/`signals.db`, other
workspaces -> `workspaces/<slug>/leads.db`); there is no separate ledger subcommand. A missing
`workspaces.db` in `--data-dir` while PostgreSQL has workspaces is reported as a mismatch (fail closed).
Check the printed counts against the pre-flight numbers. Review `orphans` (user ids not in `users`),
`skipped`, and `dead_stores` (row counts of `outreach.db`; expect all zero, see step 9).

**Stop and do not continue** on a non-zero exit. Fix, re-run backfill, re-verify. Nothing has been
cut over yet; roll back by simply restarting services (step 7a).

## 6. Cut over

```bash
# 6.1 flip the flag in .env (api, worker, scheduler all read it)
SQLITE_STORES_BACKEND=postgres

# 6.2 record the cutover marker (idempotent upsert). Every Python entrypoint (API, worker,
#     scheduler, job child, CLI) then refuses to start with SQLITE_STORES_BACKEND=sqlite, and the
#     guard fails closed if it cannot read the marker.
# cwd: legacy/python
SQLITE_STORES_BACKEND=postgres uv run python -m apps.api.cli mark-cutover

# 6.3 start in deploy order: migrate (already done) -> Go worker -> Python API
# cwd: repo root
docker compose --profile go up -d worker-go
docker compose up -d api worker scheduler
```

Smoke test (any failure -> rollback, step 7):

```bash
API=http://localhost:8000
TOKEN=$(curl -s -X POST $API/auth/token -d 'username=<user>&password=<pw>' | jq -r .access_token)
curl -s -H "Authorization: Bearer $TOKEN" $API/api/workspaces | jq '.[].name'     # same workspaces as pre-flight
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     $API/api/settings/workspace-integrations/<id> -d '{"values":{"<field>":"smoke-test-value"}}'
curl -s -H "Authorization: Bearer $TOKEN" $API/api/settings/workspace-integrations | jq   # field shows masked, never plaintext
```

Note: the smoke PUT is the first write to PostgreSQL. After it, use rollback 7b, not 7a. Use a
throwaway field or a value you are happy to keep.

## 7. Rollback

### 7a. Before the first write (nothing has written to PostgreSQL since cutover)

SQLite files are still authoritative and unchanged.

```bash
# cwd: repo root
docker compose stop api worker scheduler worker-go
# .env: SQLITE_STORES_BACKEND=sqlite
psql -c "DELETE FROM app_settings WHERE key = '__stores_backend'"    # uses PG* env + ~/.pgpass, no password on argv
docker compose up -d api worker scheduler
```

(The marker is only present if 6.2 ran; deleting it is a no-op otherwise. With the marker still present
and the flag at `sqlite`, the API refuses to start by design.)

### 7b. After writes (PostgreSQL has newer data than the SQLite files)

```bash
# cwd: repo root
docker compose stop api worker scheduler worker-go

# cwd: legacy/python
# rebuild SQLite files from PostgreSQL into a NEW directory (never writes into $DATA_DIR itself, refuses --out == --data-dir)
uv run python -m apps.api.scripts.sqlite_stores.cli export-back --out "$DATA_DIR/rollback" --data-dir "$DATA_DIR"

# verify the export against PostgreSQL: must exit 0. --skip-ledgers is required here: the rollback
# export never contains leads/signals ledgers (see below), so comparing them would always mismatch.
uv run python -m apps.api.scripts.sqlite_stores.cli verify --data-dir "$DATA_DIR/rollback" --skip-ledgers
```

`export-back` writes `workspaces.db`, `data.db`, `chat_history.db`, `functions.db`, `chat_memory.db`
into `$DATA_DIR/rollback/`. It snapshots the existing `$DATA_DIR/data.db` read-only and replaces **only** the
`settings` rows, so the exported `data.db` is already a full snapshot (all non-settings tables intact);
copying the whole file over `$DATA_DIR/data.db` is safe. Chat, functions and memory stores are repointed to
PostgreSQL by the same flag, so the same rollback covers them.

The export directory is created `0700` and its files `0600` (they contain settings values and secret
ciphertext); keep them that way.

Keep the originals before overwriting, then copy:

```bash
# cwd: any (paths are absolute via $DATA_DIR)
mkdir -p "$DATA_DIR"/pre-rollback && cd "$DATA_DIR"
cp -p workspaces.db data.db chat_history.db functions.db chat_memory.db pre-rollback/ 2>/dev/null || true
cp -p data.db-wal data.db-shm pre-rollback/ 2>/dev/null || true   # sidecars, if present
rm -f data.db-wal data.db-shm                    # stale sidecars must not be replayed onto the restored file
for f in workspaces.db data.db chat_history.db functions.db chat_memory.db; do cp -p rollback/$f $f; done

psql -c "DELETE FROM app_settings WHERE key = '__stores_backend'"    # PG* env + ~/.pgpass, no password on argv
# .env: SQLITE_STORES_BACKEND=sqlite
# cwd: repo root
docker compose up -d api worker scheduler        # worker-go: --profile go up -d worker-go
```

**Scope: leads/signals ledgers are out of this plan's rollback.** `export-back` does not write them
(they were already migrated to PostgreSQL by `migrate_sqlite_to_pg.py` and stay there either way), which
is why the 7b verify uses `--skip-ledgers`. The cutover gate in step 5 still compares them. Re-verify
after restart: `GET /api/workspaces` matches, and the workspace secrets saved during the window read
back masked.

Re-attempting the cutover later: re-run steps 2-6 (backfill is idempotent and will overwrite PostgreSQL
rows from the now-restored SQLite files, which is correct after a 7b restore).

## 8. Retention

Keep the SQLite files (`workspaces.db`, `data.db`, `chat_history.db`, plus `$DATA_DIR/pre-rollback/` and
`$DATA_DIR/rollback/` if created) and the pre-flight backup for **30 days** after cutover, then move them
to cold archive. Never auto-delete; deletion is a manual, deliberate operator action after the
retention window and a clean `verify`.

## 9. Stores NOT migrated by this procedure

| Store | Reason |
|---|---|
| `enrichment_cache.db` | TTL cache, rebuilt by plan 5. |
| `eval_*.db` | Dev only. |
| `outreach.db` | Dead: `services/outreach/sequence.py` has no importers and the `outreach_*` PostgreSQL tables already replace it. `verify` prints its row counts under `dead_stores`; confirm they are empty. |
| `openmemory.db` | The optional mem0 library's private format. |
| `signals.db`, per-workspace `leads.db` | Already copied by `apps/api/scripts/migrate_sqlite_to_pg.py`; this runbook only verifies them (ledger counts in step 5). |

## Rehearsal

Dry run of steps 3-5 and 7b on a scratch copy; no compose, no services, `legacy/python/data` untouched.

- Source: copy of `legacy/python/data/*.db`. The tree was almost empty (1 workspace, 1 `settings`
  row, 21 `tool_approvals`), so it was **seeded in the copy** with 1 workspace member, 1 active-workspace
  row, 1 `workspace_settings` row (fake `enc:v1:` ciphertext), 1 conversation and 1 message.
- Target: fresh database `opengtm_rehearsal` (docker postgres), `alembic upgrade head`; `alembic current`
  reported `e1f5a9b3d7c2 (head)`.
- `backfill --dry-run` and `backfill`: exit 0, identical row counts.
- `verify` after backfill: **exit 0, `ok: true`**, every table `digest_match: true`,
  `ledger_mismatches: []`, `skipped: {}`, `dead_stores: {}`.
- `export-back --out rollback --data-dir <copy>`: produced `workspaces.db`, `data.db`, `chat_history.db`,
  `functions.db`, `chat_memory.db`; `not_exported: []`.
- `verify --data-dir rollback` against the same PostgreSQL: **exit 0, `ok: true`**, all digests match.

Row counts (sqlite / postgres), identical for both verifies; tables not listed were 0 / 0:

```
workspaces=1/1  workspace_members=1/1  user_active_workspace=1/1  workspace_settings=1/1
app_settings=1/1  chat_conversations=1/1  chat_messages=1/1  chat_tool_approvals=21/21
orphans: workspace_members.user_id:1, user_active_workspace.user_id:1
```

The orphans are expected here: the rehearsal PostgreSQL has no `users` rows. They are reported, not
failures. Not exercised in the rehearsal: the service stop/start, flag flip, marker insert and HTTP smoke
tests (steps 2, 6, 7a), which need the compose stack.
