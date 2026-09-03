---
title: Database migrations
description: Alembic owns the main database schema. How startup applies it and how to add a migration.
sidebar:
  order: 4
---

Schema evolution for the main application database (the one `DATABASE_URL`
points at) is owned by [Alembic](https://alembic.sqlalchemy.org/). PostgreSQL
is the default backend; SQLite is used in tests and local development.

## How it is wired

- `alembic.ini` holds a placeholder URL; the real URL is injected at runtime
  from `DATABASE_URL`.
- `migrations/env.py` imports every model module so autogenerate sees all
  tables. SQLite gets `render_as_batch=True` because it has no native
  `ALTER COLUMN`.
- `migrations/versions/` contains the migration scripts, starting from a
  baseline that captures the whole schema.
- `apps/api/db_init.py` runs `alembic upgrade head` at startup. Migration
  errors are fatal and never masked by `create_all()`.
- `apps/api/scripts/migrate.py` is the one-shot deployment entrypoint. Docker
  Compose runs it as the `migrate` service and requires it to finish before the
  API and worker replicas start.

## Startup behaviour: `YUPCHA_DB_INIT`

| Value | Effect |
|---|---|
| `alembic` (default) | `alembic upgrade head`: creates a fresh schema and applies pending migrations |
| `create_all` | `Base.metadata.create_all()`: dev/test fallback, never alters existing tables, refused outside dev/test/local |
| `skip` | Do nothing; the caller manages the schema |

## Common commands

```bash
uv run alembic upgrade head      # apply all migrations
uv run alembic current           # current revision
uv run alembic history
uv run alembic downgrade -1      # roll back one step
uv run alembic check             # CI: non-zero exit if models drifted from migrations
```

Set `DATABASE_URL` to target a specific database.

## Writing a new migration

1. Edit the SQLAlchemy models.
2. Autogenerate against a database that is already at `head`, so the diff is
   only your change. A throwaway SQLite file works:

   ```bash
   export DATABASE_URL="sqlite:///./data/_scratch.db"
   uv run alembic upgrade head
   uv run alembic revision --autogenerate -m "describe the change"
   ```

3. Review the generated script. Autogenerate is not perfect: check column
   types, server defaults, indexes, RLS policies and data migrations, and make
   sure batch operations look sane for SQLite.
4. Confirm there is no leftover drift with `alembic upgrade head` followed by
   `alembic check`.
5. Commit the new file under `migrations/versions/`.

## Datastores outside Alembic

Alembic manages only tables in the main database's SQLAlchemy metadata. Two
control-plane stores are intentionally separate and single-node today:

- `data/workspaces.db`: workspaces, settings, members and the active-workspace
  pointer, created with plain SQL in the workspace manager.
- Per-workspace collection ledgers under `data/workspaces/<slug>/`.

Moving these into PostgreSQL is the first item on the
[hosted-operation roadmap](/reference/architecture/#remaining-path-to-a-hosted-multi-node-service).
