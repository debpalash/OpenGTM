"""First-run demo seed verification against a LIVE Postgres (#42).

Substance check for the docker-compose first-run demo: a brand-new Postgres
must be brought up via Alembic and the seed must create the admin user, the
demo workbook (~25 rows) and enqueue the first enrichment run — with NO
DuplicateTable failure if Alembic is later re-run (the create_all-without-stamp
bug this guards against).

PG-GATED: skipped unless TEST_DATABASE_URL points at a reachable Postgres, so
the default (SQLite) CI suite still passes without a database. Run it here with:

    TEST_DATABASE_URL=postgresql+psycopg://user4@localhost:5432/postgres \\
        PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \\
        python -m pytest tests/test_firstrun_seed_pg.py -q

The test creates (and drops) its OWN throwaway database so it never touches the
DB named in TEST_DATABASE_URL. The seed runs in a SUBPROCESS because
`apps.api.database` binds its engine at import time — running it as the real
module entrypoint (exactly as compose does) is the honest test.
"""
import os
import subprocess
import sys
import uuid

import pytest

pytest.importorskip("psycopg")
import psycopg  # noqa: E402

_RAW = os.getenv("TEST_DATABASE_URL")
pytestmark = pytest.mark.skipif(
    not _RAW, reason="TEST_DATABASE_URL not set — PG-gated first-run seed test skipped"
)

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))


def _sqlalchemy_to_libpq(url: str) -> str:
    """psycopg.connect() wants a bare libpq URL (no +psycopg driver tag)."""
    return url.replace("postgresql+psycopg://", "postgresql://").replace(
        "postgres+psycopg://", "postgresql://"
    )


def _admin_url() -> str:
    """A libpq URL to the server (the 'postgres' maintenance db) for CREATE/DROP."""
    raw = _sqlalchemy_to_libpq(_RAW)
    # Repoint whatever dbname is in TEST_DATABASE_URL at 'postgres'.
    base, _, _ = raw.rpartition("/")
    return f"{base}/postgres"


@pytest.fixture()
def throwaway_db():
    name = f"yupcha_firstrun_test_{uuid.uuid4().hex[:8]}"
    admin = _admin_url()
    with psycopg.connect(admin, autocommit=True) as conn:
        conn.execute(f'CREATE DATABASE "{name}"')
    try:
        base, _, _ = _admin_url().rpartition("/")
        yield {
            "name": name,
            "sqlalchemy_url": f"postgresql+psycopg://{base.split('//', 1)[1]}/{name}",
            "libpq_url": f"{base}/{name}",
        }
    finally:
        with psycopg.connect(admin, autocommit=True) as conn:
            conn.execute(
                "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
                "WHERE datname = %s AND pid <> pg_backend_pid()",
                (name,),
            )
            conn.execute(f'DROP DATABASE IF EXISTS "{name}"')


def _run_seed(db_url: str) -> subprocess.CompletedProcess:
    env = dict(os.environ)
    env["DATABASE_URL"] = db_url
    env["YUPCHA_DB_INIT"] = "alembic"
    env["PYTHONPATH"] = _REPO_ROOT
    return subprocess.run(
        [sys.executable, "-m", "apps.api.scripts.seed_demo"],
        cwd=_REPO_ROOT,
        env=env,
        capture_output=True,
        text=True,
    )


def test_firstrun_seed_against_postgres(throwaway_db):
    db = throwaway_db

    # 1. Seed a FRESH database (no schema yet) exactly as compose's seed service
    #    does: `python -m apps.api.scripts.seed_demo`.
    proc = _run_seed(db["sqlalchemy_url"])
    assert proc.returncode == 0, f"seed failed:\nSTDOUT:\n{proc.stdout}\nSTDERR:\n{proc.stderr}"

    with psycopg.connect(db["libpq_url"]) as conn:
        # Schema came up via Alembic and is STAMPED (the create_all-without-stamp
        # bug would leave this table absent).
        ver = conn.execute("SELECT version_num FROM alembic_version").fetchone()
        assert ver and ver[0] == "f7274d0aacd9", f"unexpected alembic stamp: {ver}"

        # Admin user exists so the login screen is usable out of the box.
        admin = conn.execute(
            "SELECT username, is_admin FROM users WHERE is_admin = true ORDER BY id LIMIT 1"
        ).fetchone()
        assert admin is not None, "no admin user created"

        # Demo workbook exists, bound to a workspace, with the expected row count.
        wb = conn.execute(
            "SELECT name, status, workspace_id, total_rows FROM workbooks "
            "WHERE id = 'demo-zero-key-firstrun'"
        ).fetchone()
        assert wb is not None, "demo workbook not created"
        name, status, workspace_id, total_rows = wb
        assert workspace_id, "demo workbook has no workspace_id (tenancy)"
        assert total_rows == 25, f"expected 25 rows, got {total_rows}"

        # The rows themselves landed.
        (row_count,) = conn.execute(
            "SELECT count(*) FROM workbook_rows WHERE workbook_id = 'demo-zero-key-firstrun'"
        ).fetchone()
        assert row_count == 25, f"expected 25 workbook_rows, got {row_count}"

        # First enrichment run was enqueued for the demo workbook.
        (job_count,) = conn.execute(
            "SELECT count(*) FROM jobs WHERE type = 'run_workbook' "
            "AND payload->>'workbook_id' = 'demo-zero-key-firstrun'"
        ).fetchone()
        assert job_count == 1, f"expected 1 run_workbook job, got {job_count}"

    # 2. Re-running the seed is idempotent (no duplicate workbook/rows/jobs).
    proc2 = _run_seed(db["sqlalchemy_url"])
    assert proc2.returncode == 0, f"second seed failed:\n{proc2.stderr}"
    with psycopg.connect(db["libpq_url"]) as conn:
        (wb_count,) = conn.execute("SELECT count(*) FROM workbooks").fetchone()
        (row_count,) = conn.execute("SELECT count(*) FROM workbook_rows").fetchone()
        (job_count,) = conn.execute("SELECT count(*) FROM jobs").fetchone()
        assert (wb_count, row_count, job_count) == (1, 25, 1), (
            f"seed not idempotent: workbooks={wb_count} rows={row_count} jobs={job_count}"
        )

    # 3. A later `alembic upgrade head` (e.g. the API booting after a hand-run
    #    seed) must be a clean no-op — NOT a DuplicateTable error. This is the
    #    regression guard for the create_all()-without-stamp bug.
    env = dict(os.environ)
    env["DATABASE_URL"] = db["sqlalchemy_url"]
    env["PYTHONPATH"] = _REPO_ROOT
    up = subprocess.run(
        [sys.executable, "-m", "alembic", "upgrade", "head"],
        cwd=_REPO_ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    assert up.returncode == 0, f"alembic upgrade head after seed failed:\n{up.stderr}"
