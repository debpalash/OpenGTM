"""Alembic environment for Yupcha.

Wires Alembic to the application's SQLAlchemy metadata and the runtime
DATABASE_URL (resolved from app config / env), rather than a hardcoded URL in
alembic.ini. Works for both Postgres (default) and SQLite (tests).

Key points:
  * We import the app's `Base` AND every model module so `Base.metadata` is
    complete — autogenerate only sees tables whose modules have been imported.
  * The DB URL comes from `apps.api.core.config.settings.DATABASE_URL`, which
    itself honours the DATABASE_URL env var. So `DATABASE_URL=... alembic ...`
    targets that DB. The `sqlalchemy.url` in alembic.ini is left as a harmless
    placeholder and is overridden here.
  * SQLite needs `render_as_batch=True` so ALTER-heavy migrations work (SQLite
    cannot ALTER columns natively; batch mode rebuilds tables).
"""
import os
import sys
from logging.config import fileConfig

from sqlalchemy import engine_from_config, pool

from alembic import context

# ── Make the `apps.api...` package importable regardless of CWD ──────────────
# env.py lives at <repo>/migrations/env.py → repo root is one level up.
_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _REPO_ROOT not in sys.path:
    sys.path.insert(0, _REPO_ROOT)

# ── App metadata: import Base + EVERY model module so all tables register ─────
from apps.api.database import Base  # noqa: E402
import apps.api.models  # noqa: E402,F401
from apps.api.services.workbook import models as _wb_models  # noqa: E402,F401
from apps.api.services.workbook import planner_models as _planner_models  # noqa: E402,F401
from apps.api.services.workbook import activity_models as _activity_models  # noqa: E402,F401
from apps.api.services.workbook import trace_models as _trace_models  # noqa: E402,F401
from apps.api.services.entities import models as _entity_models  # noqa: E402,F401
from apps.api.core.config import settings  # noqa: E402

target_metadata = Base.metadata

# Alembic Config object (access to alembic.ini values).
config = context.config

# Resolve the DB URL from the app config (which reads the DATABASE_URL env var),
# overriding whatever placeholder sits in alembic.ini.
_db_url = os.getenv("DATABASE_URL") or settings.DATABASE_URL
config.set_main_option("sqlalchemy.url", _db_url)

# Set up Python logging from the ini, if present.
if config.config_file_name is not None:
    fileConfig(config.config_file_name)

# Batch mode is required for SQLite (no native ALTER COLUMN). Harmless elsewhere
# but we only enable it for SQLite to keep Postgres migrations clean.
_render_as_batch = _db_url.startswith("sqlite")


def run_migrations_offline() -> None:
    """Run migrations in 'offline' mode (emit SQL, no DBAPI needed)."""
    context.configure(
        url=_db_url,
        target_metadata=target_metadata,
        literal_binds=True,
        dialect_opts={"paramstyle": "named"},
        render_as_batch=_render_as_batch,
        compare_type=True,
    )

    with context.begin_transaction():
        context.run_migrations()


def run_migrations_online() -> None:
    """Run migrations in 'online' mode (real engine + connection)."""
    connectable = engine_from_config(
        config.get_section(config.config_ini_section, {}),
        prefix="sqlalchemy.",
        poolclass=pool.NullPool,
    )

    with connectable.connect() as connection:
        context.configure(
            connection=connection,
            target_metadata=target_metadata,
            render_as_batch=_render_as_batch,
            compare_type=True,
        )

        with context.begin_transaction():
            context.run_migrations()


if context.is_offline_mode():
    run_migrations_offline()
else:
    run_migrations_online()
