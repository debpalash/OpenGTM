"""Schema initialisation entrypoint for the API.

Alembic is the source of truth for schema evolution. On startup we run
`alembic upgrade head`, which creates the schema on a fresh DB and applies any
pending migrations on an existing one (the thing `create_all()` could never do).

`create_all()` is kept ONLY as a guarded dev/test fallback: it is used when
Alembic is unavailable or explicitly disabled, or when the env opts into it
(e.g. the SQLite test DB, where each run starts from an empty file and we don't
want to pay migration overhead). It NEVER ALTERs existing tables, so it must not
be relied on for evolving a real (Postgres) database.

Control knobs (env vars):
  * YUPCHA_DB_INIT=alembic   (default) → run `alembic upgrade head`
  * YUPCHA_DB_INIT=create_all          → use Base.metadata.create_all()
  * YUPCHA_DB_INIT=skip                → do nothing (caller manages schema)
"""
import logging
import os

logger = logging.getLogger(__name__)

# Repo root = three levels up from this file (apps/api/db_init.py → repo).
_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))


def _import_all_models() -> None:
    """Import every module that defines a model so Base.metadata is complete.

    Needed for the create_all() fallback (Alembic's env.py does its own imports).
    """
    import apps.api.models  # noqa: F401
    from apps.api.services.workbook import models as _wb  # noqa: F401
    from apps.api.services.workbook import planner_models as _pl  # noqa: F401
    from apps.api.services.workbook import activity_models as _act  # noqa: F401
    from apps.api.services.workbook import trace_models as _tr  # noqa: F401
    from apps.api.services.entities import models as _ent  # noqa: F401
    from apps.api.services.leadgen import orm_models as _leadgen_orm  # noqa: F401


def _create_all() -> None:
    from apps.api.database import Base, engine

    _import_all_models()
    Base.metadata.create_all(bind=engine)
    logger.info("Schema ensured via create_all() (dev/test fallback).")


def _alembic_upgrade_head() -> None:
    from alembic import command
    from alembic.config import Config

    ini_path = os.path.join(_REPO_ROOT, "alembic.ini")
    cfg = Config(ini_path)
    # env.py resolves the URL from DATABASE_URL/settings, so we don't set it here.
    command.upgrade(cfg, "head")
    logger.info("Schema ensured via 'alembic upgrade head'.")


def init_db() -> None:
    """Bring the schema up to date. Prefers Alembic; falls back to create_all."""
    mode = (os.getenv("YUPCHA_DB_INIT") or "alembic").strip().lower()

    if mode == "skip":
        logger.info("YUPCHA_DB_INIT=skip — leaving schema untouched.")
        return

    if mode == "create_all":
        _create_all()
        return

    # Default: Alembic. Fall back to create_all if Alembic is missing/misconfigured
    # so local/test boot is never broken by a migration-tooling problem.
    try:
        _alembic_upgrade_head()
    except Exception as exc:  # pragma: no cover - defensive boot path
        logger.warning(
            "Alembic upgrade failed (%s) — falling back to create_all(). "
            "This does NOT ALTER existing tables; investigate before relying on it.",
            exc,
        )
        _create_all()
