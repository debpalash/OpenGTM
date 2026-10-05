"""Go and Python scheduler replicas share one lease protocol (PG-gated).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_scheduler_lease_go_interop_pg.py

A Python holder leads; the Go implementation (apps/server/internal/lease, run
through ``go test``) must be refused while that lease is live, then must take it
over after it expires with the next fencing token, after which the Python
holder is refused and fenced out. Needs ``go`` on PATH.
"""
import re
import shutil
import subprocess
import threading
import time
from pathlib import Path

import pytest
from sqlalchemy import create_engine

from tests.multihost_support import TEST_DATABASE_URL, temp_database

pytestmark = [
    pytest.mark.skipif(not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set"),
    pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not on PATH"),
]

SERVER = Path(__file__).resolve().parents[1] / "apps" / "server"


def _go(owner_url: str, phase: str, lease_name: str) -> str:
    import os
    env = dict(os.environ,
               OPENGTM_TEST_DATABASE_URL=owner_url.replace("postgresql+psycopg://", "postgresql://"),
               OPENGTM_INTEROP_PHASE=phase, OPENGTM_INTEROP_LEASE=lease_name)
    res = subprocess.run(
        ["go", "test", "-count=1", "-run", "^TestInteropFromPython$", "-v", "./internal/lease/"],
        cwd=SERVER, env=env, capture_output=True, text=True, timeout=600)
    assert res.returncode == 0, res.stdout + res.stderr
    return res.stdout


def test_go_is_refused_then_takes_over_a_python_lease_with_the_next_token():
    from apps.api.services.scheduler_lease import SchedulerLease
    with temp_database() as d:
        engine = create_engine(d.app_url, pool_size=2)
        try:
            py = SchedulerLease(engine, "interop", holder="py-holder", ttl_seconds=4)
            assert py.acquire() and py.token == 1
            stop = threading.Event()

            def heartbeat():                                   # a healthy Python leader
                while not stop.wait(0.5):
                    py.acquire()

            t = threading.Thread(target=heartbeat)
            t.start()
            try:
                _go(d.owner_url, "blocked", "interop")        # live Python lease: Go refused
            finally:
                stop.set()
                t.join()
            time.sleep(4.3)                                    # Python stops renewing: "crash"
            out = _go(d.owner_url, "takeover", "interop")
            assert int(re.search(r"INTEROP_TOKEN=(\d+)", out).group(1)) == 2
            assert not py.acquire() and py.token is None       # Python is now the follower
        finally:
            engine.dispose()
