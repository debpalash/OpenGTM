"""Shared database plumbing for the Python/Go job parity tests.

Every ``tests/test_*_go_parity_pg.py`` follows the same recipe (introduced by
the retention_enforce port):

1. migrate one template database with Alembic (shared by the whole pytest
   session through :func:`shared_template`), then clone it into two fresh
   databases per scenario, one for Python and one for Go;
2. seed both with the identical dataset;
3. run the unchanged Python handler against one and the Go handler (through
   ``go test``) against the other, both as NOSUPERUSER NOBYPASSRLS roles under
   forced row-level security;
4. dump the observable state of both and require equality.

``TEST_DATABASE_URL`` only has to name a server on which the role may create
databases and roles (the named database itself is never touched), e.g.

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres

Set ``KEEP_PARITY_DBS=1`` to keep the databases of a failing run.
"""
from __future__ import annotations

import atexit
import difflib
import json
import os
import shutil
import subprocess
import sys
import uuid
from pathlib import Path
from typing import Iterable, Sequence

import pytest

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")
REPO = Path(__file__).resolve().parents[2]
SERVER = REPO / "apps" / "server"
UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"

needs_go = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not on PATH")
needs_pg = pytest.mark.skipif(
    not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set (parity tests skipped)"
)

# (id, PostgreSQL session time zone, process time zone). Timestamps are written
# in the session zone and some Python code reads the process zone, so the mixed
# rows exercise the conversions where the two diverge.
ZONES = [
    ("utc", "UTC", "UTC"),
    ("los_angeles", "America/Los_Angeles", "America/Los_Angeles"),
    ("pg_utc_proc_kolkata", "UTC", "Asia/Kolkata"),
    ("pg_kolkata_proc_utc", "Asia/Kolkata", "UTC"),
]


def urls(database: str) -> tuple[str, str]:
    """(SQLAlchemy psycopg URL, plain libpq URL) for a database on the test server."""
    from sqlalchemy.engine import make_url

    url = make_url(TEST_DATABASE_URL).set(database=database, drivername="postgresql+psycopg")
    sa = url.render_as_string(hide_password=False)
    return sa, sa.replace("postgresql+psycopg://", "postgresql://", 1)


def admin():
    import psycopg

    return psycopg.connect(urls("postgres")[1], autocommit=True)


def drop(name: str) -> None:
    if os.getenv("KEEP_PARITY_DBS"):
        return
    with admin() as conn:
        conn.execute(f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)')


_template: str | None = None


def shared_template() -> str:
    """The migrated template database, created once per pytest process."""
    global _template
    if _template is None:
        name = f"opengtm_parity_tpl_{uuid.uuid4().hex[:8]}"
        with admin() as conn:
            conn.execute(f'CREATE DATABASE "{name}"')
        res = subprocess.run(
            ["uv", "run", "--frozen", "alembic", "upgrade", "head"],
            cwd=REPO, env=dict(os.environ, DATABASE_URL=urls(name)[0]),
            capture_output=True, text=True,
        )
        if res.returncode != 0:
            drop(name)
            raise AssertionError(f"alembic upgrade failed:\n{res.stdout}\n{res.stderr}")
        _template = name
        atexit.register(drop, name)
    return _template


def fresh_seeded_db(template: str, tz: str, sql_files: Iterable[Path], prefix: str = "opengtm_par") -> str:
    """Clone the template, set the session zone, then run the seed scripts."""
    import psycopg

    name = f"{prefix}_{uuid.uuid4().hex[:8]}"
    with admin() as conn:
        conn.execute(f'CREATE DATABASE "{name}" TEMPLATE "{template}"')
        conn.execute(f"ALTER DATABASE \"{name}\" SET timezone TO '{tz}'")
    sql = "\n".join(Path(f).read_text() for f in sql_files)
    with psycopg.connect(urls(name)[1]) as conn:  # new session: picks up the time zone
        conn.execute(sql)
    return name


class DatabasePair:
    """A Python database and a Go database seeded identically; dropped on exit."""

    def __init__(self, template: str, tz: str, sql_files: Sequence[Path], prefix: str):
        self.py = self.go = None
        self.py = fresh_seeded_db(template, tz, sql_files, prefix)
        self.go = fresh_seeded_db(template, tz, sql_files, prefix)

    def __enter__(self) -> "DatabasePair":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if exc_type is not None and os.getenv("KEEP_PARITY_DBS"):
            print(f"kept databases {self.py} (python) and {self.go} (go)")
        for name in (self.py, self.go):
            if name:
                drop(name)


def run_python(module: str, db: str, tz: str, out: Path, *extra: str, env: dict | None = None) -> None:
    """Run ``python -m <module>`` (a parity runner) against ``db`` in process zone ``tz``."""
    res = subprocess.run(
        [sys.executable, "-m", module, "--db-url", urls(db)[0], "--out", str(out), "--tz", tz, *extra],
        cwd=REPO, env={**os.environ, "TZ": tz, **(env or {})}, capture_output=True, text=True,
    )
    assert res.returncode == 0, f"python runner failed:\n{res.stdout}\n{res.stderr}"


def go_test(package: str, name: str, env: dict) -> None:
    """Run one Go test (``-run ^name$``) of ``./internal/...`` and refuse a skipped run."""
    res = subprocess.run(
        ["go", "test", "-count=1", "-run", f"^{name}$", f"./{package}/"],
        cwd=SERVER, env={**os.environ, **env}, capture_output=True, text=True,
    )
    assert res.returncode == 0, f"go test {name} failed:\n{res.stdout}\n{res.stderr}"
    assert "SKIP" not in res.stdout and "no tests to run" not in res.stdout, res.stdout


def run_go(package: str, db: str, tz: str, scenarios: Path, out: Path, test: str = "TestParityDriver",
           env: dict | None = None) -> None:
    go_test(package, test, {
        "OPENGTM_PARITY_DATABASE_URL": urls(db)[1],
        "OPENGTM_PARITY_SCENARIOS": str(scenarios),
        "OPENGTM_PARITY_OUT": str(out),
        "TZ": tz,
        **(env or {}),
    })


# ── dumping and comparing ────────────────────────────────────────────────────

def norm_uuid(expr: str) -> str:
    """SQL expression replacing every UUID in ``expr`` (text) with ``<uuid>``."""
    return f"regexp_replace({expr}, '{UUID}', '<uuid>', 'g')"


def rows(conn, sql: str, *args) -> list[list]:
    return [list(r) for r in conn.execute(sql, args or None).fetchall()]


def dump_with(db: str, fn) -> dict:
    """Run ``fn(conn, rows)`` on a fresh autocommit connection as the schema owner."""
    import psycopg

    with psycopg.connect(urls(db)[1], autocommit=True) as conn:
        return fn(conn, lambda sql, *a: rows(conn, sql, *a))


def group(pairs: list[list]) -> dict:
    grouped: dict = {}
    for ws, key in pairs:
        grouped.setdefault(ws, []).append(str(key))
    return grouped


def diff(name: str, py, go) -> list[str]:
    if py == go:
        return []
    if isinstance(py, dict) and isinstance(go, dict):
        out = []
        for key in sorted(set(py) | set(go)):
            out += diff(f"{name}.{key}", py.get(key), go.get(key))
        return out
    a = json.dumps(py, indent=1, default=str, sort_keys=True).splitlines()
    b = json.dumps(go, indent=1, default=str, sort_keys=True).splitlines()
    delta = list(difflib.unified_diff(a, b, "python", "go", lineterm="", n=1))
    return [f"{name} differs:\n" + "\n".join(delta[:60])]


def compare_steps(spec_steps: list, py_steps: list, go_steps: list) -> list[str]:
    """Problems found comparing the per-step outcomes (the error each step raised).

    A step with ``error_contains`` only requires both sides to fail with a
    message containing that text, and one with ``expect_error`` that both fail
    (driver wording of database errors differs); every other step must raise
    the identical error, or none.
    """
    assert len(py_steps) == len(go_steps) == len(spec_steps)
    problems = []
    for step, py, go in zip(spec_steps, py_steps, go_steps):
        label = f"{step['op']} job {step.get('job', '-')} ({step.get('note', '')})"
        needle = step.get("error_contains")
        if needle:
            if not (py["error"] and needle in py["error"] and go["error"] and needle in go["error"]):
                problems.append(f"{label}: python={py['error']!r} go={go['error']!r}, both must contain {needle!r}")
        elif step.get("expect_error"):
            if not (py["error"] and go["error"]):
                problems.append(f"{label}: python={py['error']!r} go={go['error']!r}, both must fail")
        elif py["error"] != go["error"]:
            problems.append(f"{label}: python raised {py['error']!r}, go {go['error']!r}")
    return problems


def assert_identical_state(py_db: str, go_db: str, dump) -> tuple[dict, dict]:
    """Dump both databases with ``dump(db)`` and require equality."""
    py_dump, go_dump = dump(py_db), dump(go_db)
    diffs = diff("state", py_dump, go_dump)
    assert not diffs, "\n\n".join(diffs)
    return py_dump, go_dump

