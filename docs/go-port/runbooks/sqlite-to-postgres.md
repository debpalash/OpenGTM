# Runbook: SQLite stores to PostgreSQL (cutover and rollback)

Moves the identity-plane, settings, chat, function and memory SQLite stores into PostgreSQL so
the Go API can serve them. Schema is owned by Alembic (head `e1f5a9b3d7c2`); Go never runs DDL.
Run Python commands from the Python tree (`legacy/python/`, or `/app` inside the image) with
`DATABASE_URL` set to the **owner** PostgreSQL URL (the backfill needs the table owner, not the
runtime role). `data/` below is the mounted OpenGTM data directory.

SQLite files are never modified or deleted by any step of this runbook.

## Deploy order (important)

`migrate` -> Go worker (`worker-go`) -> Python API. The Go `queue.CheckSchema` is an exact match on
the required Alembic revision (`e1f5a9b3d7c2`), so a Go worker started before `migrate` (or against an
older schema) refuses to start, and the Python API must not serve the new backend before the schema
exists. During the cutover window everything is stopped, so this order is applied in step 6.

## 1. Pre-flight

```bash
export SQLITE_STORES_BACKEND=sqlite          # must be sqlite until step 6
export DATABASE_URL=postgresql+psycopg://<owner>:<pw>@<host>:5432/<db>

# integrity-checked PostgreSQL + data/ backup (refuses to overwrite an existing archive)
python -m apps.api.cli backup --output backups/pre-stores-cutover-$(date +%Y%m%d%H%M).tar.gz --data-dir data
python -m apps.api.cli backup-verify backups/pre-stores-cutover-<stamp>.tar.gz   # same file as above

# record source counts (compare with the verify output in step 5)
sqlite3 data/workspaces.db 'select count(*) from workspaces'
sqlite3 data/workspaces.db 'select count(*) from workspace_members'
sqlite3 data/workspaces.db 'select count(*) from workspace_settings'
sqlite3 data/workspaces.db 'select count(*) from user_active_workspace'
sqlite3 data/data.db       'select count(*) from settings'
sqlite3 data/chat_history.db 'select count(*) from conversations'
sqlite3 data/chat_history.db 'select count(*) from messages'

# optional: preview what backfill will copy, writes nothing
python -m apps.api.scripts.sqlite_stores.cli backfill --data-dir data --dry-run
```

Do not proceed if the backup does not verify.

## 2. Stop services

```bash
docker compose stop api worker scheduler worker-go
```

(`worker-go` only runs with `--profile go`; stopping it when absent is harmless.) Nothing may write
to the SQLite files or PostgreSQL from here until step 6.

## 3. Migrate PostgreSQL to the new head

```bash
docker compose run --rm migrate
# confirm
docker compose run --rm migrate alembic current     # expect: e1f5a9b3d7c2 (head)
```

## 4. Backfill

```bash
python -m apps.api.scripts.sqlite_stores.cli backfill --data-dir data
```

Idempotent (`INSERT ... ON CONFLICT DO UPDATE`); safe to re-run. It reads SQLite through the `sqlite3`
backup API into a temp copy, sets `app.workspace_id` per tenant batch for FORCE-RLS tables, and
copies secret ciphertext (`enc:v1:` / `enc:v2:vault:`) verbatim; nothing is decrypted. It fails closed
(exit 1) on unparseable values. Orphans (members/active-workspace rows whose `user_id` has no row in
`users`) are reported, not fatal.

Per-workspace leads/signals ledgers (`leads.db`, `signals.db`, `workspaces/<slug>/leads.db`) are copied
by the existing `apps/api/scripts/migrate_sqlite_to_pg.py` (set `LEGACY_SQLITE` to the source; run it only
if the ledgers have not already been migrated). The stores CLI only verifies them.

## 5. Verify (gate)

```bash
python -m apps.api.scripts.sqlite_stores.cli verify --data-dir data --pg-url "$DATABASE_URL"
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

# 6.2 insert the marker (owner role). The startup guard refuses sqlite mode once this exists.
psql "$OWNER_PG_URL" -c "INSERT INTO app_settings(key, value) VALUES ('__stores_backend','postgres')
                         ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value"

# 6.3 start in deploy order: migrate (already done) -> Go worker -> Python API
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
docker compose stop api worker scheduler worker-go
# .env: SQLITE_STORES_BACKEND=sqlite
psql "$OWNER_PG_URL" -c "DELETE FROM app_settings WHERE key = '__stores_backend'"
docker compose up -d api worker scheduler
```

(The marker is only present if 6.2 ran; deleting it is a no-op otherwise. With the marker still present
and the flag at `sqlite`, the API refuses to start by design.)

### 7b. After writes (PostgreSQL has newer data than the SQLite files)

```bash
docker compose stop api worker scheduler worker-go

# rebuild SQLite files from PostgreSQL into a NEW directory (never writes data/, refuses --out == --data-dir)
python -m apps.api.scripts.sqlite_stores.cli export-back --out data/rollback --data-dir data

# verify the export against PostgreSQL: must exit 0
python -m apps.api.scripts.sqlite_stores.cli verify --data-dir data/rollback --pg-url "$DATABASE_URL"
```

`export-back` writes `workspaces.db`, `data.db`, `chat_history.db`, `functions.db`, `chat_memory.db`
into `data/rollback/`. It snapshots the existing `data/data.db` read-only and replaces **only** the
`settings` rows, so the exported `data.db` is already a full snapshot (all non-settings tables intact);
copying the whole file over `data/data.db` is safe. Chat, functions and memory stores are repointed to
PostgreSQL by the same flag, so the same rollback covers them.

Keep the originals before overwriting, then copy:

```bash
mkdir -p data/pre-rollback && cp -p data/workspaces.db data/data.db data/chat_history.db data/pre-rollback/
for f in workspaces.db data.db chat_history.db functions.db chat_memory.db; do cp -p data/rollback/$f data/$f; done

psql "$OWNER_PG_URL" -c "DELETE FROM app_settings WHERE key = '__stores_backend'"
# .env: SQLITE_STORES_BACKEND=sqlite
docker compose up -d api worker scheduler        # worker-go: --profile go up -d worker-go
```

`export-back` does not touch leads/signals ledgers; those stay in PostgreSQL either way. Re-verify
after restart: `GET /api/workspaces` matches, and the workspace secrets saved during the window read
back masked.

Re-attempting the cutover later: re-run steps 2-6 (backfill is idempotent and will overwrite PostgreSQL
rows from the now-restored SQLite files, which is correct after a 7b restore).

## 8. Retention

Keep the SQLite files (`workspaces.db`, `data.db`, `chat_history.db`, plus `data/pre-rollback/` and
`data/rollback/` if created) and the pre-flight backup for **30 days** after cutover, then move them
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
