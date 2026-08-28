"""Shared pytest environment.

Every invocation gets a fresh, migrated SQLite database. A persistent
``data/_pytest.db`` made the suite order-dependent: ORM ``create_all`` in one
test could add head columns while the stale Alembic stamp caused a later test
to try adding them again.
"""
import os
import tempfile

_owns_default_database = "DATABASE_URL" not in os.environ
if _owns_default_database:
    _pytest_dir = tempfile.mkdtemp(prefix="yupcha-pytest-")
    os.environ["DATABASE_URL"] = f"sqlite:///{_pytest_dir}/suite.db"

# Migrate before test modules import ORM classes or call create_all. The CI suite
# deliberately supplies its own disposable SQLite path, so an explicit database
# is still ours to initialize when APP_ENV=test and Alembic was requested. This
# prevents an early ``create_all()`` fixture from producing an unstamped schema
# that a later ``init_db()`` mistakes for a fresh database. Arbitrary explicit
# databases (especially developer Postgres instances) remain caller-owned.
_is_managed_test_database = (
    os.environ.get("APP_ENV", "").strip().lower() in {"test", "testing"}
    and os.environ.get("YUPCHA_DB_INIT", "").strip().lower() == "alembic"
    and os.environ.get("DATABASE_URL", "").startswith("sqlite:")
)
if _owns_default_database or _is_managed_test_database:
    from apps.api.db_init import init_db

    init_db()
