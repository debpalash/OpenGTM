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
from apps.api.services.billing import models as _billing_models  # noqa: E402,F401
from apps.api.services.leadgen import orm_models as _leadgen_orm_models  # noqa: E402,F401
from apps.api.services.leadgen import source_stats as _leadgen_source_stats  # noqa: E402,F401
from apps.api.services.automations import models as _automations_models  # noqa: E402,F401
from apps.api.services.outreach import orm_models as _outreach_orm_models  # noqa: E402,F401
from apps.api.services.poller import models as _poller_models  # noqa: E402,F401
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


# Postgres-only objects that are hand-authored in the tenancy migration and are
# NOT modelled on the ORM Base (so autogenerate would otherwise try to DROP
# them). Tell autogenerate/`alembic check` to ignore them so the check stays
# clean. The names match the DDL in the c42d0273d9bd migration.
_IGNORED_PG_OBJECTS = {
    ("column", "leads.search_tsv"),
    ("index", "ix_leads_search_tsv"),
    # intent-poller jobs.fire_key single-flight indexes are hand-authored in the
    # migration (a PARTIAL unique index on PG, a plain index on SQLite) and are
    # NOT modelled on the ORM Base, so autogenerate must ignore them.
    ("index", "uq_jobs_fire_key_active"),
    ("index", "ix_jobs_fire_key"),
}


def _include_object(obj, name, type_, reflected, compare_to):
    """Skip the hand-authored Postgres-only FTS objects during autogenerate."""
    if type_ == "column" and getattr(obj, "table", None) is not None:
        key = ("column", f"{obj.table.name}.{name}")
        if key in _IGNORED_PG_OBJECTS:
            return False
    if type_ == "index" and ("index", name) in _IGNORED_PG_OBJECTS:
        return False
    return True


def run_migrations_offline() -> None:
    """Run migrations in 'offline' mode (emit SQL, no DBAPI needed)."""
    context.configure(
        url=_db_url,
        target_metadata=target_metadata,
        literal_binds=True,
        dialect_opts={"paramstyle": "named"},
        render_as_batch=_render_as_batch,
        compare_type=True,
        include_object=_include_object,
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
            include_object=_include_object,
        )

        with context.begin_transaction():
            context.run_migrations()


if context.is_offline_mode():
    run_migrations_offline()
else:
    run_migrations_online()
