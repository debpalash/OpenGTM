# Schema migrations (Go runner)

The PostgreSQL schema is owned by `opengtm-migrate` (`apps/backend-go/cmd/migrate`, package
`internal/migrate`, [goose v3](https://github.com/pressly/goose) with the migrations embedded in the
binary). Deploys need neither `legacy/python` nor Alembic, except to upgrade a database that is
older than Alembic revision `e7b2c4d6f8a0` (see "Old Alembic databases").

## Where it runs

| Context | Command |
|---|---|
| docker compose | the one-shot `migrate` service (Dockerfile target `migrate`, image `opengtm-migrate-go:local`; the api image also carries `/opengtm-migrate`); every app service waits for it (`service_completed_successfully`) |
| bare metal | `make migrate` (builds `bin/opengtm-migrate`, runs `up`); `make build` also builds it |
| container | `docker run --rm -e DATABASE_URL=... opengtm-api-go:local` with `--entrypoint /opengtm-migrate` |
| CI (Go job) | `go run ./cmd/migrate up` against the service database |

```
DATABASE_URL=postgres://owner:...@host:5432/db opengtm-migrate [up]   # apply pending (default)
DATABASE_URL=...                               opengtm-migrate status # versions, applied or pending (read only)
                                               opengtm-migrate version
```

`DATABASE_URL` needs **owner** credentials: DDL, `CREATE EXTENSION pg_trgm` (trusted, so a database owner
can do it), and `CREATE ROLE` if the app role is missing. The migration role must bypass RLS (owner or
`rolbypassrls`); the `outreach_send_by_message_id` lookup is `SECURITY DEFINER` and owned by it. A
SQLAlchemy URL (`postgresql+psycopg://`) is accepted, so the old `DATABASE_URL` in `.env` keeps working.

| Variable | Default | Meaning |
|---|---|---|
| `YUPCHA_APP_DB_ROLE` | `yupcha_app` | NOLOGIN group role that gets the runtime grants (created if missing) |
| `YUPCHA_PROVISION_RUNTIME_ROLE` | off | `1/true/yes/on`: create or update the runtime login (same as the old Python job) |
| `YUPCHA_RUNTIME_DB_USER` | `yupcha_runtime` | runtime login (member of the app role, `CONNECT`, `USAGE` on `public`) |
| `YUPCHA_RUNTIME_DB_PASSWORD` | (required with provisioning) | its password; never logged |

## What `up` does

All of it runs under `pg_advisory_lock(0x4f47544d4d494752)` on a dedicated session, so two
`opengtm-migrate` processes (two replicas of a deploy job, compose plus a manual run) serialize: the
second waits, then finds nothing pending.

| Database state | Action |
|---|---|
| empty `public` schema | **baseline** (version 1): the schema at Alembic `e7b2c4d6f8a0`, `alembic_version` stamped `e7b2c4d6f8a0`, the app role created if missing and granted what Alembic granted (schema `USAGE`, DML on all tables, sequences, default privileges, `EXECUTE` on `outreach_send_by_message_id` with `PUBLIC` revoked) |
| `alembic_version` = `e7b2c4d6f8a0`, no `goose_db_version` | **adopt**: version 1 is recorded without running the baseline; the app role gets `SELECT` on `goose_db_version` |
| `goose_db_version` at version >= 1 | apply pending versions (2, 3, ...) |
| any other Alembic revision (older, empty, several rows) | **refused**, nothing written: "upgrade it with the legacy tool first" |
| tables but neither version table | **refused** |

Each version runs in its own transaction (goose); a failure rolls that version back and exits non-zero.

## Startup check

api, worker and mcp (Go) refuse to start unless `goose_db_version` is at least
`config.RequiredGoSchemaVersion` (currently `1`). They no longer read `alembic_version`. A database
newer than the binary is accepted (migrations are additive), so a rolling deploy may keep old replicas
running. Error texts: `goose_db_version is missing ... run opengtm-migrate` (never migrated by Go, e.g. an
Alembic database not yet adopted) and `schema version N is below required M; run opengtm-migrate`.

`config.RequiredGoSchemaRevision` (`e7b2c4d6f8a0`) is now only the Alembic revision the baseline
reproduces and stamps; it stays equal to Python's `REQUIRED_GO_SCHEMA_REVISION`.

## Python during the canary

The Python API, worker and scheduler still run against the same database. They read
`alembic_version` (readiness head check, `go_executor`), which the runner keeps at `e7b2c4d6f8a0` on
fresh and adopted databases; later Go migrations never change it. Rules while Python is still deployed:

- Go migrations after the baseline must be **additive and backward compatible** with the Python code at
  `86f41c6` (new tables, nullable columns, new indexes); no renames, drops or new NOT NULL columns
  without a default.
- Do not run `alembic upgrade` / `alembic downgrade` on a Go-managed database. (`upgrade head` is a no-op
  at `e7b2c4d6f8a0`; anything else breaks the stamp contract.)
- The Python `migrate` job (`python -m apps.api.scripts.migrate`) is retired; compose no longer runs it.

## Old Alembic databases

A database below `e7b2c4d6f8a0` is refused. Upgrade it once with the legacy tool, then run
`opengtm-migrate`:

```
cd legacy/python            # at commit 86f41c6 or later
DATABASE_URL=postgresql+psycopg://owner:...@host/db uv run alembic upgrade e7b2c4d6f8a0
DATABASE_URL=postgres://owner:...@host/db opengtm-migrate     # adopts
```

Migrating SQLite data to PostgreSQL (`sqlite-to-postgres.md`, plan 01) is a separate one-off Python tool
and still needs `legacy/python`; it runs Alembic itself, then `opengtm-migrate` adopts the result.

## Adding a migration

1. Add `apps/backend-go/internal/migrate/migrations/NNNNN_name.sql` with the next number and goose
   annotations (`-- +goose Up`; `-- +goose StatementBegin` / `StatementEnd` around `$$` bodies;
   `-- +goose NO TRANSACTION` for `CREATE INDEX CONCURRENTLY`). Up only. New tables owned by the
   migration role get the app-role grants through the default privileges the baseline set; tenant tables
   still need their RLS (`ENABLE` + `FORCE` + `workspace_isolation` policy) in the same file.
2. `make -C apps/backend-go schema` (= `scripts/go_schema_dump.sh`): runs the Go runner on a scratch
   database in the test Postgres container and rewrites `internal/db/schema.sql` from `pg_dump`.
   `scripts/go_schema_dump.sh --check` only diffs.
3. `make -C apps/backend-go sqlc`; bump `config.RequiredGoSchemaVersion` if the Go code needs the change.
4. CI: the Go job migrates its database with the runner, and the integration test
   `TestSchemaSQLMatchesMigrations` (internal/migrate) fails when `schema.sql` does not describe what the
   migrations build (catalog comparison, no `pg_dump` needed).

The baseline file (`internal/migrate/baseline/e7b2c4d6f8a0.sql`) is never edited.

## Verification

```
opengtm-migrate status        # 1  baseline (Alembic e7b2c4d6f8a0)  applied <time>
psql "$DATABASE_URL" -Atc "SELECT version_num FROM alembic_version"   # e7b2c4d6f8a0
```

Tests: `go test -tags integration ./internal/migrate/` (fresh, adopt, refuse, idempotent, concurrent,
startup check, runtime login, schema.sql parity). `MIGRATE_ALEMBIC_DATABASE_URL=<an Alembic-built
database at e7b2c4d6f8a0>` adds a read-only catalog and grant comparison against it.

Known, harmless difference: three expressions Alembic created as
`ANY ((ARRAY['a'::character varying, ...])::text[])` (`ck_workbook_spend_status`,
`uq_connector_runs_active`, `uq_jobs_fire_key_active`) are stored by the baseline in the equivalent form
Postgres prints after a dump reload, `ANY (ARRAY[('a'::character varying)::text, ...])`. Adopted databases
keep the original form.
