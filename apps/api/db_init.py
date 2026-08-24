"""Schema initialisation entrypoint for the API.

Alembic is the source of truth for schema evolution. On startup we run
`alembic upgrade head`, which creates the schema on a fresh DB and applies any
pending migrations on an existing one (the thing `create_all()` could never do).

`create_all()` is kept ONLY as an explicit dev/test mode. Alembic failures are
never converted into a partially upgraded schema: startup fails and leaves the
operator with the original error.

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
    from apps.api.services.workbook import ingest as _ingest  # noqa: F401
    from apps.api.services.entities import models as _ent  # noqa: F401
    from apps.api.services.leadgen import orm_models as _leadgen_orm  # noqa: F401
    from apps.api.services.leadgen import source_stats as _source_stats  # noqa: F401
    from apps.api.services.leadgen import source_health as _source_health  # noqa: F401
    from apps.api.services.billing import models as _billing  # noqa: F401
    from apps.api.services.automations import models as _automations  # noqa: F401
    from apps.api.services.outreach import orm_models as _outreach  # noqa: F401
    from apps.api.services.poller import models as _poller  # noqa: F401
    from apps.api.services.mcp import models as _mcp  # noqa: F401


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
    """Bring the schema up to date using the explicitly selected strategy."""
    mode = (os.getenv("YUPCHA_DB_INIT") or "alembic").strip().lower()

    if mode not in {"alembic", "create_all", "skip"}:
        raise ValueError(
            "YUPCHA_DB_INIT must be one of: alembic, create_all, skip "
            f"(got {mode!r})"
        )

    if mode == "skip":
        logger.info("YUPCHA_DB_INIT=skip — leaving schema untouched.")
        return

    if mode == "create_all":
        app_env = (os.getenv("APP_ENV") or "dev").strip().lower()
        if app_env not in {"dev", "development", "test", "testing", "local"}:
            raise RuntimeError(
                "YUPCHA_DB_INIT=create_all is forbidden outside dev/test/local; "
                "use Alembic so existing schemas are upgraded safely"
            )
        _create_all()
        return

    _alembic_upgrade_head()
