"""Child process for tests/test_scheduler_lease_pg.py: one scheduler replica.

Competes for the leases ``scheduler:a`` and ``scheduler:b`` against a shared
database. While it leads a name it appends a row to ``lease_probe`` on every
tick through an ORM session fenced by the lease token, then sleeps. It never
exits on its own except at the deadline; the test kills it with SIGKILL to
simulate a crashed host.

    python -m tests.multihost_lease_worker <app_url> <ttl> <tick> <run_seconds> <label>
"""
from __future__ import annotations

import sys
import time

from sqlalchemy import create_engine, text
from sqlalchemy.orm import sessionmaker

from apps.api.services import scheduler_lease as sl


def main() -> None:
    url, ttl, tick, run_seconds, label = (
        sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4]), sys.argv[5],
    )
    engine = create_engine(url, pool_size=4)
    factory = sessionmaker(bind=engine, autoflush=False)
    sl.install_session_fence(factory)
    leadership = sl.Leadership(
        engine, ["a", "b"], holder=f"{label}:{time.time_ns()}", ttl_seconds=ttl,
        renew_every=ttl / 4,
    )
    leadership.poll()
    leadership.start()
    print("READY", flush=True)
    deadline = time.monotonic() + run_seconds
    try:
        while time.monotonic() < deadline:
            for name in ("a", "b"):
                try:
                    with leadership.lead(name) as lease:
                        if lease is None:
                            continue
                        with factory() as db:
                            db.execute(
                                text("INSERT INTO lease_probe (name, holder, token, at) "
                                     "VALUES (:n, :h, :t, clock_timestamp())"),
                                {"n": lease.name, "h": lease.holder, "t": lease.token},
                            )
                            db.commit()
                except sl.LeaseLost:
                    pass  # deposed mid-tick: the fence rolled the write back
            time.sleep(tick)
    finally:
        leadership.stop()


if __name__ == "__main__":
    main()
