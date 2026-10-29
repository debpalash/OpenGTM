# SQLite Stores to PostgreSQL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move every tenant- or identity-bearing SQLite store (workspaces.db, the settings table in data.db, chat history, functions, chat memory) into PostgreSQL through Alembic, with an idempotent backfill, a verifier, a reverse exporter and RLS where the data is tenant-scoped, so the Go API (plans 02 and 03) never has to read SQLite.

**Architecture:** Alembic in `legacy/python` stays the only migration owner. One table-driven spec module (`TableSpec` list) drives the forward backfill, the verifier and the reverse export, so the three can never disagree. The Python modules that own each store switch backend behind `SQLITE_STORES_BACKEND=sqlite|postgres` (default `sqlite` until cutover), so rollback before the first PG-era write is a flag flip. Identity-plane tables (looked up before the tenant is known) have no RLS; everything tenant-scoped has `FORCE ROW LEVEL SECURITY` with the existing `app.workspace_id` policy shape.

**Tech Stack:** Python 3 / SQLAlchemy / Alembic / pytest / uv (in `legacy/python`), PostgreSQL 16 and 18, Go 1.27.3 + sqlc (schema sync only).

**Spec:** `docs/go-port/inventory.md` section 5 (SQLite stores) and section 8 (plan 1 and "SQLite precedes plans 1, 2, 4, 8"); GitHub issue #33.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/` (gitignored). Every Python path here (`apps/api/...`, `migrations/...`, `scripts/...`, `tests/...`) means `legacy/python/<path>`; run Python commands from `legacy/python/`. Python edits are not versioned by this repo: take a backup (or commit to a separate repo) before Task 1. Only the Go files in Task 9 and the runbook in Task 10 are committed here.

## Global Constraints

- Alembic is the single migration owner; new revision's `down_revision` is the current head `4a5162738495`. Go never runs DDL.
- No CI changes. Verify Go with `cd apps/backend-go && make test` and `make test-integration` (needs `TEST_DATABASE_URL`); Python with `uv run pytest <path> -q`.
- Runtime role stays non-superuser `NOBYPASSRLS`. Grants follow `migrations/versions/b8c9d0e1f2a3_mcp_tokens_audit.py` (`GRANT SELECT, INSERT, UPDATE, DELETE ... TO {APP_DB_ROLE}`); tenant tables get `ENABLE` + `FORCE ROW LEVEL SECURITY` and one policy `USING`/`WITH CHECK` `(workspace_id)::text = current_setting('app.workspace_id', true)`.
- Preserve value semantics, not just shape: timestamps that Python returns as epoch floats (`time.time()`) stay `double precision` epoch columns, because `/api/workspaces` returns them as JSON numbers.
- Workspace ids stay `varchar(64)` (matches `mcp_tokens.workspace_id`, `connector_runs.workspace_id`); user ids stay `integer` with NO foreign key to `users` (SQLite had none; the verifier reports orphans instead of failing the copy).
- Secrets: ciphertext in `workspace_settings.value` is copied verbatim (`enc:v1:` / `enc:v2:vault:` envelopes untouched, never decrypted or re-encrypted by the migration). Plaintext never reaches logs or verifier output; the verifier compares SHA-256 digests.
- Backfill is idempotent (`INSERT ... ON CONFLICT DO UPDATE`), runs as the table owner, sets `app.workspace_id` per tenant batch for FORCE-RLS tables, and reads SQLite through the `sqlite3` backup API into a temp copy (consistent snapshot of WAL files).
- Cutover is a maintenance window: stop `api`, `worker`, `scheduler`, `worker-go`; migrate; backfill; verify; flip the flag; start. SQLite files are never modified or deleted by this plan (they are the rollback source).
- Granular commits, DCO signed (`git commit -s`) as Palash Debnath for committed (Go/doc) files; Python changes are checkpointed in the separate backup repo.

## Review Focus

1. **Membership chicken-and-egg under RLS.** `workspace_members`, `workspaces`, `user_active_workspace`, `workspace_oidc_identities`, `workspace_scim_tokens` are read before a workspace is known (header resolution, SCIM bearer-by-slug, SSO status). Putting RLS on them returns zero rows and locks everyone out. They are deliberately identity-plane (no RLS); Task 1 asserts the RLS split in a catalog test.
2. **Orphan and duplicate data in the source.** SQLite never enforced users FK, uniqueness of slug under case, or `workspace_members` rows pointing at deleted workspaces. A copy that aborts half-way leaves a mixed store. Task 2 tests orphans (reported, copied) and a duplicate-slug fixture (aborts before writing, nothing partial).
3. **Legacy chat rows with NULL `workspace_id`/`user_id`.** Python filters on both columns, so these rows are invisible today. Importing them under `main` would expose old private chats. Task 5 skips and counts them; the verifier prints the skipped count.
4. **Epoch-float vs timestamptz drift.** Converting `created_at` to `timestamptz` changes `/api/workspaces` JSON from `1761465600.123` to a string and breaks the frontend. Task 1 pins `double precision` and a round-trip test with a fractional epoch.
5. **Concurrent writers during cutover and a dual-mode flag left on `sqlite` on one replica.** A replica still on the SQLite backend after cutover silently diverges. Task 6 makes the module refuse to start in `sqlite` mode when `alembic_version` is at or past the new revision and the marker row `app_settings('__stores_backend')='postgres'` exists.

---

## File Structure

```
migrations/versions/<rev>_identity_stores_pg.py          # workspaces*, app_settings, RLS
migrations/versions/<rev2>_chat_functions_memory_pg.py   # chat_*, workbook_functions, chat_memories
apps/api/scripts/sqlite_stores/__init__.py
apps/api/scripts/sqlite_stores/specs.py                  # TableSpec list (source of truth)
apps/api/scripts/sqlite_stores/backfill.py               # sqlite -> pg (idempotent)
apps/api/scripts/sqlite_stores/verify.py                 # counts + digests + orphans
apps/api/scripts/sqlite_stores/export_back.py            # pg -> sqlite (rollback)
apps/api/scripts/sqlite_stores/cli.py                    # python -m ...cli {backfill|verify|export-back}
apps/api/services/workspace/pg_store.py                  # PG implementation of manager/secrets storage
apps/api/services/workspace/manager.py                   # backend switch (modify)
apps/api/services/workspace/secrets.py                   # storage via pg_store (modify)
apps/api/routers/settings.py                             # _get_db/_db_get/_db_set backend switch (modify)
apps/api/services/leadgen/llm.py                         # settings read via _db_get (modify, :64-75)
apps/api/services/{chat_history,memory}.py, services/workbook/functions.py  # backend switch (modify)
apps/api/services/backup.py                              # add pg_dump to snapshot (modify)
tests/test_sqlite_stores_*.py                            # per task
apps/backend-go/internal/db/schema.sql                   # regenerated (committed)
docs/go-port/runbooks/sqlite-to-postgres.md              # cutover + rollback runbook
```

Out of scope (decided, see Task 10 runbook): `enrichment_cache.db` (TTL cache, rebuilt by plan 5), `eval_*.db` (dev only), `outreach.db` (dead: `services/outreach/sequence.py` has no importers and `outreach_*` PG tables already replace it; Task 8 prints its row counts so an operator can confirm it is empty), `openmemory.db` (the optional mem0 library's private format), `signals.db` and per-workspace `leads.db` (already copied by `scripts/migrate_sqlite_to_pg.py`; Task 4 only verifies them).

---

### Task 1: Alembic revision A, identity-plane tables and `app_settings`

**Files:**
- Create: `migrations/versions/<rev>_identity_stores_pg.py` (generate id with `uv run alembic revision -m identity_stores_pg`; `down_revision = "4a5162738495"`)
- Test: `tests/test_sqlite_stores_schema.py` (marker `postgres`, uses `tests/pg_rls_support.py`)

**Interfaces:**
- Produces tables (all columns copied from `services/workspace/manager.py:48-148`, types as below):
  - `workspaces(id varchar(64) PK, name text NOT NULL, slug text NOT NULL UNIQUE, description text NOT NULL DEFAULT '', icon text NOT NULL DEFAULT '🏢', owner_id integer NULL, created_at double precision, updated_at double precision)`
  - `workspace_members(workspace_id varchar(64) FK workspaces(id), user_id integer, role text NOT NULL DEFAULT 'member', created_at double precision, PK(workspace_id,user_id))`
  - `user_active_workspace(user_id integer PK, workspace_id varchar(64) NOT NULL)`
  - `workspace_oidc_identities(workspace_id, issuer, subject, user_id, email text DEFAULT '', created_at double precision NOT NULL; PK(workspace_id,issuer,subject); UNIQUE(workspace_id,user_id); FK (workspace_id,user_id) -> workspace_members)`
  - `workspace_scim_tokens(workspace_id PK FK workspaces, token_hash text, token_prefix text, created_at, expires_at, last_used_at double precision, created_by integer)`
  - RLS tables (policy `workspace_isolation`): `workspace_settings(workspace_id, key, value text DEFAULT '', PK(workspace_id,key))`, `workspace_member_permissions(workspace_id,user_id,permission,effect CHECK in ('allow','deny'),updated_at; PK(...))`, `workspace_scim_users(workspace_id,user_id,external_id,display_name,active boolean NOT NULL DEFAULT true,created_at,updated_at; PK(workspace_id,user_id))`, `workspace_scim_groups(id varchar(64) PK, workspace_id, external_id, display_name NOT NULL, created_at, updated_at, UNIQUE(workspace_id,display_name))`, `workspace_scim_group_members(workspace_id, group_id FK groups(id) ON DELETE CASCADE, user_id, created_at, PK(workspace_id,group_id,user_id))`.
  - `app_settings(key text PK, value text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT now())`, no RLS (global config, same trust as data.db today).
- Consumes: `APP_DB_ROLE` helper pattern from `b8c9d0e1f2a3_mcp_tokens_audit.py`.

- [ ] **Step 1: Write failing tests** in `tests/test_sqlite_stores_schema.py`: `test_identity_plane_tables_have_no_rls` (for the 5 identity tables `relrowsecurity` is false), `test_tenant_tables_force_rls` (the 5 RLS tables have `relrowsecurity` and `relforcerowsecurity`), `test_cross_tenant_settings_invisible` (insert `workspace_settings` rows for ws A and B as owner; as the runtime role with `app.workspace_id=A` only A's row is visible; with GUC unset zero rows), `test_epoch_float_roundtrip` (insert `created_at=1761465600.123456`, read back equals), `test_downgrade_drops_all` (`alembic downgrade -1` then upgrade).
- [ ] **Step 2: Run** `uv run pytest tests/test_sqlite_stores_schema.py -q` -> FAIL (tables missing).
- [ ] **Step 3: Implement** the revision with `op.create_table` plus raw `op.execute` for grants/RLS/policies; `downgrade()` drops in reverse FK order.
- [ ] **Step 4: Run** the same command -> PASS.
- [ ] **Step 5: Checkpoint** the Python backup; no repo commit.

### Task 2: Backfill engine and workspaces.db copy

**Files:**
- Create: `apps/api/scripts/sqlite_stores/{__init__,specs,backfill,cli}.py`
- Test: `tests/test_sqlite_stores_backfill_workspaces.py`

**Interfaces:**
- Produces in `specs.py`: `@dataclass(frozen=True) class TableSpec: name: str; sqlite_db: str; sqlite_table: str; columns: tuple[str, ...]; pk: tuple[str, ...]; tenant_col: str | None; transforms: Mapping[str, Callable[[Any], Any]]` and `IDENTITY_SPECS: list[TableSpec]` (copy order = FK order: `workspaces`, `workspace_members`, `user_active_workspace`, `workspace_oidc_identities`, `workspace_scim_tokens`, `workspace_settings`, `workspace_member_permissions`, `workspace_scim_users` (transform `active`: `int -> bool`), `workspace_scim_groups`, `workspace_scim_group_members`, `app_settings` (source `data.db` table `settings`)).
- Produces in `backfill.py`: `def snapshot_sqlite(path: str, dest_dir: str) -> str` (uses `sqlite3.Connection.backup`), `def backfill(specs: list[TableSpec], data_dir: str, pg_url: str, *, dry_run: bool = False) -> dict[str, int]` returning rows upserted per table. `cli.py`: `python -m apps.api.scripts.sqlite_stores.cli backfill --data-dir data --dry-run`.

- [ ] **Step 1: Write failing tests**: `test_backfill_copies_all_identity_tables` (fixture workspaces.db built with `manager._get_db()` plus 2 workspaces, 3 members, 1 permission, 1 scim token, 1 group; assert row counts equal), `test_backfill_idempotent` (run twice; counts unchanged, no duplicate), `test_fractional_epoch_preserved`, `test_ciphertext_copied_verbatim` (`workspace_settings.value == 'enc:v1:abc...'` byte-equal), `test_duplicate_slug_aborts_before_write` (two rows with slugs differing only by case are fine; exact duplicate impossible in SQLite, so craft `slug=''` twice via dropped UNIQUE in a fixture; expect `BackfillError` and zero rows in PG), `test_orphan_member_user_reported_and_copied` (user id not in `users` -> row copied, returned in `result.orphans`), `test_rls_tables_use_tenant_guc` (copy succeeds under FORCE RLS).
- [ ] **Step 2: Run** -> FAIL.
- [ ] **Step 3: Implement** `backfill`: one PG transaction for the whole run; for FORCE-RLS tables group rows by `tenant_col` and `SELECT set_config('app.workspace_id', :ws, true)` before each group's upsert; validate duplicate keys before the first write; `--dry-run` rolls back and still returns counts.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 3: Settings (`data.db`) edge cases

**Files:**
- Modify: `apps/api/scripts/sqlite_stores/specs.py` (the `app_settings` spec only)
- Test: `tests/test_sqlite_stores_backfill_settings.py`

**Interfaces:**
- Consumes: `TableSpec`, `backfill` (Task 2).
- Produces: `settings` rows migrate keyed by `key`; rows whose `value` is NULL or empty become `''` (Python `_db_get` treats empty as unset and falls back to env, `routers/settings.py:54-64`, so empty must stay empty, not be dropped).

- [ ] **Step 1: Failing tests**: `test_empty_value_preserved_as_empty`, `test_settings_unicode_and_long_values_roundtrip` (4 KB value, emoji), `test_missing_data_db_is_noop` (no file -> `{}` result, exit 0), `test_workspace_integration_keys_not_copied_to_app_settings` (only the `settings` table is copied; `workspace_settings` is separate).
- [ ] **Step 2: Run** -> FAIL on empty-value and missing-file cases.
- [ ] **Step 3: Implement** transforms `value: lambda v: v or ""` and the missing-file short-circuit in `backfill`.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 4: Verify the already-migrated ledgers (leads and signals)

**Files:**
- Create: `apps/api/scripts/sqlite_stores/verify.py` (ledger part)
- Test: `tests/test_sqlite_stores_verify_ledgers.py`

**Interfaces:**
- Consumes: `scripts/migrate_sqlite_to_pg.py::_iter_workspaces()` (yields `(ws_id, slug, leads_path, signals_path)`).
- Produces: `def verify_ledgers(data_dir: str, pg_url: str) -> list[Mismatch]` where `@dataclass Mismatch: store: str; workspace_id: str | None; detail: str`. Compares per-workspace `COUNT(*)` of SQLite `leads.db:leads` vs PG `leads` (GUC set per workspace), and `signals.db:signals` (attributed to `main`) vs PG `signals`.

- [ ] **Step 1: Failing tests**: `test_ledger_counts_match_after_migrate` (run `migrate_per_workspace_leads_signals()` then verify -> `[]`), `test_ledger_detects_missing_rows` (delete 1 PG lead -> one `Mismatch`), `test_ledger_missing_sqlite_file_counts_zero` (absent leads.db is not an error).
- [ ] **Step 2: Run** -> FAIL.
- [ ] **Step 3: Implement** `verify_ledgers`; never mutates either side.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 5: Alembic revision B (chat, functions, memories) and their backfill

**Files:**
- Create: `migrations/versions/<rev2>_chat_functions_memory_pg.py` (`down_revision = <rev>`)
- Modify: `apps/api/scripts/sqlite_stores/specs.py` (add `CHAT_SPECS`)
- Test: `tests/test_sqlite_stores_chat_functions_memory.py`

**Interfaces:**
- Produces tables, mirroring `services/chat_history.py:56-95`, `services/workbook/functions.py:28`, `services/memory.py:69`:
  - `chat_conversations(id text PK, title text NOT NULL DEFAULT 'New Chat', workspace_id varchar(64) NOT NULL, user_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())` + index `(workspace_id,user_id,updated_at)`; RLS.
  - `chat_messages(id text PK, conversation_id text FK chat_conversations ON DELETE CASCADE, workspace_id varchar(64) NOT NULL, role text CHECK in ('user','assistant','system','tool'), content text NOT NULL DEFAULT '', tool_data text NULL, created_at timestamptz NOT NULL DEFAULT now())` + index `(conversation_id,created_at)`; RLS (`workspace_id` denormalised from the parent so the policy needs no join).
  - `chat_tool_approvals(id text PK, workspace_id, user_id text, tool_call_json text, status text DEFAULT 'pending', created_at_epoch bigint)` + index `(workspace_id,user_id,created_at_epoch)`; RLS.
  - `workbook_functions(id text PK, name text NOT NULL, description text DEFAULT '', category text DEFAULT 'custom', columns_chain jsonb NOT NULL DEFAULT '[]', version integer DEFAULT 1, usage_count integer DEFAULT 0, created_at double precision, updated_at double precision)`, no RLS (functions are global today; routes are admin-gated).
  - `chat_memories(id bigserial PK, user_id text NOT NULL, text text NOT NULL, metadata jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL)` + index on `user_id`, no RLS (keyed by user only, like the SQLite store).
- Transforms: SQLite `datetime('now')` strings (`YYYY-MM-DD HH:MM:SS`, UTC) -> `timestamptz`; `columns_chain`/`metadata` TEXT -> parsed JSON (invalid JSON -> `[]`/`{}` and counted); `chat_messages.workspace_id` filled from the parent conversation.

- [ ] **Step 1: Failing tests**: `test_chat_rls_isolates_workspaces`, `test_null_workspace_conversations_skipped_and_counted` (3 rows, 1 with NULL `workspace_id` -> 2 imported, `result.skipped["chat_conversations"] == 1`, and their messages skipped too), `test_message_cascade_on_conversation_delete`, `test_datetime_string_parsed_as_utc`, `test_functions_columns_chain_json_roundtrip`, `test_invalid_json_columns_chain_becomes_empty_list`, `test_memories_ids_preserved_and_sequence_advanced` (next insert id = max+1).
- [ ] **Step 2: Run** -> FAIL.
- [ ] **Step 3: Implement** the revision and the specs; `backfill` resets `bigserial`/`chat_memories_id_seq` with `setval` after load.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 6: Repoint workspace manager, secrets and settings to PostgreSQL

**Files:**
- Create: `apps/api/services/workspace/pg_store.py`
- Modify: `apps/api/services/workspace/manager.py` (every function that calls `_get_db()`, lines 40-601), `apps/api/services/workspace/secrets.py:_db/set_secret/_raw_workspace_value/rotate_encrypted_secrets`, `apps/api/routers/settings.py:36-70`, `apps/api/services/leadgen/llm.py:64-80`
- Test: `tests/test_workspace_manager_pg.py`, existing `tests/test_workspace*.py` re-run under both backends

**Interfaces:**
- Produces: `def backend() -> Literal["sqlite", "postgres"]` reading `SQLITE_STORES_BACKEND`; `pg_store` exposes the same function names/signatures as the manager storage layer (`create_workspace`, `add_member`, `member_role`, `has_permission`, `get_workspace_setting`, `set_workspace_setting`, `app_setting_get(key)`, `app_setting_set(key, value)` ...). Each call opens a short transaction on the app engine and, for RLS tables, first runs `SELECT set_config('app.workspace_id', :ws, true)`.
- Behaviour that must not change: `create_workspace` slug rule (`lower`, spaces/underscores to `-`, keep alnum and `-`), `is_member` includes the owner (`UNION workspaces.owner_id`), `get_user_active_workspace` falls back to the first accessible workspace, default workspace `slug='main'` auto-seeded with `ON CONFLICT (slug) DO NOTHING`, `delete_workspace` refuses `main` and deletes children in the order of `manager.py:536-556`, `_count_workspace_leads` counts PG `leads` under the workspace GUC in postgres mode.
- Startup guard: in `sqlite` mode, refuse to start if `app_settings('__stores_backend') = 'postgres'` exists.

- [ ] **Step 1: Failing tests**: parametrise the existing manager tests over `SQLITE_STORES_BACKEND in {sqlite, postgres}`; add `test_slug_rule_matches_across_backends`, `test_is_member_true_for_owner_without_member_row`, `test_active_workspace_fallback`, `test_delete_main_refused`, `test_delete_workspace_removes_children_under_rls`, `test_secret_roundtrip_postgres` (encrypt with `encrypt_value`, `set_secret`, `get_secret` equal), `test_pg_backend_refuses_sqlite_replica` (marker row present + `sqlite` mode -> `RuntimeError`), `test_db_get_falls_back_to_env_when_empty`.
- [ ] **Step 2: Run** `uv run pytest tests/test_workspace_manager_pg.py -q` -> FAIL.
- [ ] **Step 3: Implement** `pg_store` and the dispatch in each module; keep the SQLite code path byte-for-byte unchanged.
- [ ] **Step 4: Run** the new tests plus `uv run pytest tests -k "workspace or secrets or settings or mcp_token or scim" -q` under both backends -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 7: Repoint chat history, functions and memory

**Files:**
- Modify: `apps/api/services/chat_history.py`, `apps/api/services/workbook/functions.py`, `apps/api/services/memory.py` (`_BuiltinMemory` only)
- Test: `tests/test_chat_history_tenancy.py` (existing, parametrise), `tests/test_functions_pg.py`, `tests/test_memory_pg.py`

**Interfaces:**
- Consumes: `backend()` (Task 6). Public function signatures are unchanged; the `(workspace_id, user_id)` filter stays in the query AND is enforced by RLS.
- Produces: all three modules read `SQLITE_STORES_BACKEND` independently of the identity stores but obey the same refusal guard.

- [ ] **Step 1: Failing tests**: existing `test_chat_history_tenancy.py` cases pass under `postgres`; `test_functions_crud_pg` (create/list/update/usage_count/delete), `test_memory_recall_pg_keyword_and_recency`, `test_chat_pg_unscoped_query_returns_nothing` (no GUC -> zero rows).
- [ ] **Step 2: Run** -> FAIL.
- [ ] **Step 3: Implement** the PG branches with the same SQL semantics; JSON columns via `jsonb`.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 8: Verifier, reverse exporter and dead-store audit

**Files:**
- Modify: `apps/api/scripts/sqlite_stores/verify.py` (add `verify_stores`), `cli.py`
- Create: `apps/api/scripts/sqlite_stores/export_back.py`
- Test: `tests/test_sqlite_stores_verify.py`, `tests/test_sqlite_stores_export_back.py`

**Interfaces:**
- Produces: `def verify_stores(specs: list[TableSpec], data_dir: str, pg_url: str) -> VerifyReport` with `VerifyReport(tables: dict[str, TableResult], orphans: list[str], skipped: dict[str, int], dead_stores: dict[str, int])`; `TableResult(sqlite_rows: int, pg_rows: int, digest_match: bool)`. Digest = SHA-256 over rows sorted by `pk`, each row rendered with `json.dumps(row, sort_keys=True, default=str)`; for `value` columns of `workspace_settings` and `app_settings` only the hash of the value enters the digest. `dead_stores` counts rows of `outreach.db` tables `sequences`, `sequence_leads`, `send_log`.
- Produces: `def export_back(specs, pg_url, out_dir) -> dict[str, int]` writing fresh `workspaces.db` / `data.db` files in `out_dir` with the original schemas (reuse `manager._get_db` DDL); never overwrites `data/`.
- CLI exit code 0 only if every `digest_match` is true and no ledger mismatch; non-zero otherwise.

- [ ] **Step 1: Failing tests**: `test_verify_clean_after_backfill`, `test_verify_detects_changed_row` (alter one `role` in PG), `test_verify_never_prints_secret_value` (capture stdout/stderr, assert the plaintext fixture secret and the ciphertext are absent), `test_export_back_roundtrip` (backfill -> export -> `verify_stores` against exported dir clean), `test_dead_store_counts_reported`, `test_cli_exit_codes`.
- [ ] **Step 2: Run** -> FAIL.
- [ ] **Step 3: Implement** the verifier, exporter and CLI subcommands `verify` and `export-back`.
- [ ] **Step 4: Run** -> PASS.
- [ ] **Step 5: Checkpoint.**

### Task 9: Backup coverage and Go schema sync

**Files:**
- Modify: `apps/api/services/backup.py` (add `pg_dump -Fc` of the app database to `create_backup`/`restore`, around lines 40-110, only when `DATABASE_URL` is PostgreSQL)
- Modify: `apps/backend-go/internal/db/schema.sql` (regenerated), `apps/backend-go/internal/db/gen/` (regenerated if queries reference it)
- Create: `apps/backend-go/internal/tenancy/identity_rls_test.go` (build tag `integration`)
- Test: `tests/test_backup_restore.py` (extend), Go test above

**Interfaces:**
- Produces: backup manifest gains `{"postgres": {"format": "custom", "sha256": "<hex>"}}`; restore verifies the digest before `pg_restore`.
- Produces (Go): `TestIdentityPlaneNoRLS_TenantTablesForceRLS` (queries `pg_class` for the table split from Task 1) and `TestWorkspaceSettingsIsolatedByTenant` using `testpg.NewAppPool(t, 1)` and `tenancy.InWorkspace`.

- [ ] **Step 1: Failing tests**: Python `test_backup_includes_pg_dump_digest`, `test_restore_rejects_tampered_pg_dump`; Go tests above.
- [ ] **Step 2: Run** `uv run pytest tests/test_backup_restore.py -q` and `cd apps/backend-go && make test-integration` -> FAIL.
- [ ] **Step 3: Implement** the backup change; run `cd apps/backend-go && make schema && make sqlc && make sqlc-check` against a DB migrated to the new head, then write the Go test.
- [ ] **Step 4: Run** `make vet test test-integration` and the pytest file -> PASS.
- [ ] **Step 5: Commit** (Go only)

```bash
git add apps/backend-go/internal/db apps/backend-go/internal/tenancy/identity_rls_test.go
git commit -s -m "feat(go): sync schema for identity-plane tables and RLS split test"
```

### Task 10: Cutover and rollback runbook

**Files:**
- Create: `docs/go-port/runbooks/sqlite-to-postgres.md`

**Interfaces:**
- Consumes: CLI from Tasks 2, 5, 8. Produces the ordered procedure below, committed as a doc.

- [ ] **Step 1: Write the runbook** with these exact sections and commands: (1) pre-flight: `SQLITE_STORES_BACKEND=sqlite`, take `create_backup`, record `sqlite3 data/workspaces.db 'select count(*) from workspaces'` and the other counts; (2) stop `api worker scheduler worker-go`; (3) `docker compose run --rm migrate` (alembic to new head); (4) `python -m apps.api.scripts.sqlite_stores.cli backfill --data-dir data`; (5) `... verify` must exit 0, also run `scripts/migrate_sqlite_to_pg.py` ledger verify; (6) set `SQLITE_STORES_BACKEND=postgres`, insert marker `app_settings('__stores_backend','postgres')`, start services, smoke `POST /auth/token`, `GET /api/workspaces`, `PUT /api/settings/workspace-integrations/<id>` then read back masked; (7) rollback before first write: flip flag back; rollback after writes: stop, `... export-back --out data/rollback`, `verify` against it, copy files into `data/`, delete the marker, flip flag; (8) retention: keep SQLite files 30 days, then archive (never auto-delete); (9) documented non-migrated stores from File Structure.
- [ ] **Step 2: Dry run** the whole sequence on a copy of a populated `data/` and paste the observed `verify` summary into the runbook's "Rehearsal" section.
- [ ] **Step 3: Commit**

```bash
git add docs/go-port/runbooks/sqlite-to-postgres.md
git commit -s -m "docs(go-port): sqlite to postgres cutover and rollback runbook"
```

---

## Self-Review

- **Spec coverage:** workspaces/members/active workspace/encrypted secrets (Tasks 1, 2, 6), settings DB (3, 6), per-workspace ledgers (4), outreach.db (dead-store audit, 8), functions/chat/memory (5, 7), backups (9), RLS (1, 5, 9), backfill + verification (2, 8), rollback (6, 8, 10). `enrichment_cache.db`, `eval_*.db`, `openmemory.db` are explicitly out of scope.
- **Type consistency:** `TableSpec`, `backfill`, `verify_stores`, `export_back`, `backend()` are defined once (Tasks 2, 6, 8) and reused by name.
- **Decisions for the user:** (a) legacy NULL-tenant chat rows are skipped (Review Focus 3); (b) `workbook_functions` and `chat_memories` stay global/user-keyed without RLS to preserve today's behaviour; confirm or ask for a `workspace_id` column.
- **Proportion:** tasks state schema, signatures and test names; bodies are left to the implementer.
