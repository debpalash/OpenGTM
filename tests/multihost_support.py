"""Throwaway PostgreSQL databases for the M8 multi-host tests (PG-gated).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_scheduler_lease_pg.py tests/test_multihost_*.py

``TEST_DATABASE_URL`` only has to name a server on which the role may create
databases (a superuser). Each caller gets its own uniquely named database,
migrated to Alembic head, plus a URL for the NOSUPERUSER NOBYPASSRLS runtime
role, so forced row-level security really applies. Nothing else is touched.
"""
from __future__ import annotations

import os
import uuid
from contextlib import contextmanager
from dataclasses import dataclass

from sqlalchemy import create_engine, text
from sqlalchemy.engine import make_url

from tests.pg_rls_support import APP_LOGIN_PASSWORD, APP_LOGIN_ROLE, rls_app_session

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")


@dataclass
class TempDb:
    name: str
    owner_url: str  # superuser: schema/seed/inspection only
    app_url: str    # NOSUPERUSER NOBYPASSRLS runtime role


def _server_url() -> str:
    assert TEST_DATABASE_URL, "TEST_DATABASE_URL not set"
    return TEST_DATABASE_URL


@contextmanager
def temp_database(prefix: str = "opengtm_m8"):
    name = f"{prefix}_{uuid.uuid4().hex[:10]}"
    server = make_url(_server_url())
    admin = create_engine(server.set(database="postgres"), isolation_level="AUTOCOMMIT")
    with admin.connect() as c:
        c.execute(text(f'CREATE DATABASE "{name}"'))
    owner_url = server.set(database=name).render_as_string(hide_password=False)
    try:
        _, dispose = rls_app_session(owner_url, pool_size=1)  # migrate + runtime role
        dispose()
        app_url = (
            make_url(owner_url)
            .set(username=APP_LOGIN_ROLE, password=APP_LOGIN_PASSWORD)
            .render_as_string(hide_password=False)
        )
        yield TempDb(name=name, owner_url=owner_url, app_url=app_url)
    finally:
        with admin.connect() as c:
            c.execute(text(
                "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
                "WHERE datname = :n AND pid <> pg_backend_pid()"), {"n": name})
            c.execute(text(f'DROP DATABASE IF EXISTS "{name}"'))
        admin.dispose()
