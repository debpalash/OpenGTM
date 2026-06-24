# Database migrations (Alembic)

Schema evolution for the **main application database** (the one pointed at by
`DATABASE_URL`) is owned by [Alembic](https://alembic.sqlalchemy.org/). Postgres
is the default backend; SQLite is used in tests.

Previously the schema was built with `Base.metadata.create_all()`, which creates
missing tables but **never ALTERs existing ones** — so the first model change
after launch silently drifted from the live DB. Alembic fixes that: each schema
change is a versioned migration that is applied in order.

## How it's wired

- `alembic.ini` — standard config. Its `sqlalchemy.url` is a **placeholder**;
  the real URL is injected at runtime.
- `migrations/env.py` — imports the app's `Base` **and every model module** so
  autogenerate sees all tables, and resolves the DB URL from
  `DATABASE_URL` / `apps.api.core.config.settings.DATABASE_URL` (not the ini).
  SQLite gets `render_as_batch=True` (it has no native `ALTER COLUMN`).
- `migrations/versions/` — migration scripts. The first is the **baseline**
  capturing the entire current schema.
- `apps/api/db_init.py` — `init_db()` runs `alembic upgrade head` on startup
  (called from `apps/api/main.py`). It falls back to `create_all()` only if
  Alembic is unavailable/misconfigured, so local & test boot never break.

### Startup behaviour (`YUPCHA_DB_INIT` env var)

| Value | Effect |
|-------|--------|
| `alembic` (default) | `alembic upgrade head` — creates a fresh schema and applies pending migrations |
| `create_all` | `Base.metadata.create_all()` — dev/test fallback, never ALTERs existing tables |
| `skip` | do nothing (caller manages schema) |

## Common commands

All commands need `PYTHONPATH=.` and the repo `uv` env. Set `DATABASE_URL` to
target a specific DB (defaults to the app's configured Postgres URL).

```bash
# Apply all migrations (create/upgrade the schema)
PYTHONPATH=. uv run alembic upgrade head

# See current revision / history
PYTHONPATH=. uv run alembic current
PYTHONPATH=. uv run alembic history

# Roll back one migration
PYTHONPATH=. uv run alembic downgrade -1

# Verify the migrations match the models (CI-friendly: nonzero exit if drift)
PYTHONPATH=. uv run alembic check
```

## Making a new migration

1. Edit the SQLAlchemy models (e.g. `apps/api/models.py`,
   `apps/api/services/workbook/models.py`, …).
2. Autogenerate a migration:
   ```bash
   PYTHONPATH=. uv run alembic revision --autogenerate -m "describe the change"
   ```
   Run this against a DB already at `head` (so the diff is only your change). A
   throwaway SQLite DB works:
   ```bash
   DATABASE_URL="sqlite:///./data/_scratch.db" PYTHONPATH=. uv run alembic upgrade head
   DATABASE_URL="sqlite:///./data/_scratch.db" PYTHONPATH=. uv run alembic revision --autogenerate -m "..."
   ```
3. **Review the generated script** in `migrations/versions/`. Autogenerate is
   not perfect — check column types, server defaults, indexes, and data
   migrations. For SQLite-affecting changes, confirm batch operations look sane.
4. Verify there's no leftover drift:
   ```bash
   DATABASE_URL="sqlite:///./data/_scratch.db" PYTHONPATH=. uv run alembic upgrade head
   DATABASE_URL="sqlite:///./data/_scratch.db" PYTHONPATH=. uv run alembic check   # → "No new upgrade operations detected."
   ```
5. Commit the new file under `migrations/versions/`.

## Scope / out-of-scope datastores

Alembic only manages tables in `apps.api.database.Base.metadata` (the main DB).
The repo has **separate SQLite datastores** that are NOT managed here:

- **`data/workspaces.db`** — `workspaces`, `workspace_settings`,
  `workspace_members`, `user_active_workspace`. Created with raw `sqlite3` DDL
  (`CREATE TABLE IF NOT EXISTS`) + ad-hoc `ALTER` in
  `apps/api/services/workspace/manager.py`. These have no SQLAlchemy models and
  live in their own file, so they are intentionally excluded.
- **Per-workspace `data/workspaces/<slug>/leads.db`** — lead rows. The `Lead`
  type (`apps/api/services/leadgen/models.py`) is a plain dataclass, not an ORM
  model; these DBs are managed by the leadgen layer, not Alembic.
- **`apps/api/services/memory.py`** — a standalone SQLite `memories` table.

If any of these are migrated onto SQLAlchemy models on the main DB in the
future, add their modules to `migrations/env.py` imports and autogenerate a
migration.
