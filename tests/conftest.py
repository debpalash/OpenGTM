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

# Migrate before test modules import ORM classes or call create_all. Explicit
# DATABASE_URL callers own their database lifecycle (notably PG-gated suites).
if _owns_default_database:
    from apps.api.db_init import init_db

    init_db()
