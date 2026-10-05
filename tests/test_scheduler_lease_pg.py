"""Scheduler leadership on real PostgreSQL (PG-gated).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_scheduler_lease_pg.py

Proves, against a throwaway database and the NOSUPERUSER NOBYPASSRLS runtime
role: exactly one holder at a time under contention (threads with separate
connection pools, and separate OS processes), failover after a crash (SIGKILL)
within the lease TTL, split-brain prevention by the fencing token (a deposed
holder's commit is aborted and a takeover waits for an in-flight fenced
commit), graceful hand-over, and the scheduler loop's gating.
"""
from __future__ import annotations

import os
import signal
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

import pytest
from sqlalchemy import create_engine, text
from sqlalchemy.orm import sessionmaker

from tests.multihost_support import TEST_DATABASE_URL, temp_database

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set (lease tests skipped)"
)

REPO = Path(__file__).resolve().parents[1]


@pytest.fixture(scope="module")
def db():
    with temp_database() as d:
        owner = create_engine(d.owner_url)
        with owner.begin() as c:
            c.execute(text(
                "CREATE TABLE lease_probe (id BIGSERIAL PRIMARY KEY, name TEXT, holder TEXT, "
                "token BIGINT, at TIMESTAMPTZ)"))
            c.execute(text("GRANT SELECT, INSERT ON lease_probe TO yupcha_app"))
            c.execute(text("GRANT USAGE, SELECT ON SEQUENCE lease_probe_id_seq TO yupcha_app"))
        owner.dispose()
        yield d


@pytest.fixture
def engine(db):
    e = create_engine(db.app_url, pool_size=8, max_overflow=0)
    with e.begin() as c:
        c.execute(text("DELETE FROM scheduler_leases"))
    yield e
    e.dispose()


def _lease(engine, name, holder, ttl=30.0):
    from apps.api.services.scheduler_lease import SchedulerLease
    return SchedulerLease(engine, name, holder=holder, ttl_seconds=ttl)


def _row(engine, name):
    with engine.begin() as c:
        return c.execute(text(
            "SELECT holder, fencing_token, expires_at > now() AS live "
            "FROM scheduler_leases WHERE name = :n"), {"n": "scheduler:" + name}).first()


def test_acquire_renew_keep_token_and_exclude_others(engine):
    a, b = _lease(engine, "x", "A"), _lease(engine, "x", "B")
    assert a.acquire() and a.token == 1 and a.held
    assert not b.acquire() and b.token is None and not b.held
    assert a.renew() and a.token == 1          # a live renewal keeps the term
    assert not b.acquire()


def test_release_hands_over_and_never_resets_the_token(engine):
    a, b = _lease(engine, "x", "A"), _lease(engine, "x", "B")
    assert a.acquire()
    a.release()
    assert not a.held and _row(engine, "x").live is False
    assert b.acquire() and b.token == 2        # new holder, new term
    b.release()
    assert a.acquire() and a.token == 3        # monotonic across holders
    a.release()
    a.release()                                 # idempotent


def test_same_holder_reacquiring_after_lapse_gets_a_new_token(engine):
    a = _lease(engine, "x", "A", ttl=0.3)
    assert a.acquire() and a.token == 1
    time.sleep(0.5)
    assert not a.held                           # local deadline passed
    assert a.acquire() and a.token == 2         # lapsed: old term is not blessed again


def test_expired_lease_is_taken_over_with_the_next_token(engine):
    a, b = _lease(engine, "x", "A", ttl=0.3), _lease(engine, "x", "B", ttl=30)
    assert a.acquire()
    assert not b.acquire()
    time.sleep(0.5)
    assert b.acquire() and b.token == 2
    assert not a.acquire() and a.token is None  # deposed holder learns on its next renewal


def test_contention_exactly_one_leader_at_a_time(engine):
    """Many replicas (each its own pool) hammer acquire/renew; at every instant
    at most one believes it holds, and the winner keeps the same token."""
    from apps.api.services.scheduler_lease import SchedulerLease
    n = 12
    engines = [create_engine(engine.url.render_as_string(hide_password=False), pool_size=1)
               for _ in range(n)]
    leases = [SchedulerLease(e, "contended", holder=f"H{i}", ttl_seconds=5) for i, e in enumerate(engines)]
    wins, errors = [], []
    barrier = threading.Barrier(n)

    def run(lease):
        try:
            barrier.wait(timeout=10)
            for _ in range(20):
                if lease.acquire():
                    wins.append((lease.holder, lease.token))
        except Exception as exc:  # noqa: BLE001
            errors.append(repr(exc))

    threads = [threading.Thread(target=run, args=(l,)) for l in leases]
    [t.start() for t in threads]
    [t.join(30) for t in threads]
    [e.dispose() for e in engines]
    assert not errors, errors
    assert len({h for h, _ in wins}) == 1, "two holders won"
    assert {t for _, t in wins} == {1}, "the leader's token changed while it was live"
    assert sum(1 for l in leases if l.held) == 1


def test_fence_aborts_a_deposed_holders_commit(engine):
    """Split brain: A is paused past its TTL, B takes over. A wakes up believing
    it still leads and commits; the fence aborts the transaction and the write
    never lands, while B's identical write does."""
    from apps.api.services import scheduler_lease as sl
    factory = sessionmaker(bind=engine, autoflush=False)
    sl.install_session_fence(factory)
    a, b = _lease(engine, "x", "A", ttl=0.4), _lease(engine, "x", "B", ttl=30)
    assert a.acquire()
    stale_token = a.token

    marker = uuid.uuid4().hex
    time.sleep(0.6)                       # A "pauses" (GC, VM freeze, partition)
    assert b.acquire() and b.token == stale_token + 1

    a._deadline = time.monotonic() + 100  # A's local clock still says "I lead"
    assert a.held
    with pytest.raises(sl.LeaseLost):
        with sl.fenced(a):
            with factory() as s:
                s.execute(text("INSERT INTO lease_probe (name, holder, token, at) "
                               "VALUES ('scheduler:x', :h, :t, now())"),
                          {"h": marker + "-A", "t": a.token})
                s.commit()
    with sl.fenced(b):
        with factory() as s:
            s.execute(text("INSERT INTO lease_probe (name, holder, token, at) "
                           "VALUES ('scheduler:x', :h, :t, now())"),
                      {"h": marker + "-B", "t": b.token})
            s.commit()
    with engine.begin() as c:
        holders = [r[0] for r in c.execute(
            text("SELECT holder FROM lease_probe WHERE holder LIKE :m"), {"m": marker + "%"})]
    assert holders == [marker + "-B"]


def test_fence_uses_the_token_of_the_term_the_work_started_in(engine):
    """A lease re-won after a lapse has a new token; work started under the old
    term must not be retroactively blessed by it."""
    from apps.api.services import scheduler_lease as sl
    factory = sessionmaker(bind=engine, autoflush=False)
    sl.install_session_fence(factory)
    a = _lease(engine, "x", "A", ttl=0.4)
    assert a.acquire()
    marker = uuid.uuid4().hex
    with pytest.raises(sl.LeaseLost):
        with sl.fenced(a):                      # captures token 1
            time.sleep(0.6)                      # lapses ...
            assert a.acquire() and a.token == 2  # ... and is re-won (new term)
            with factory() as s:
                s.execute(text("INSERT INTO lease_probe (name, holder, token, at) "
                               "VALUES ('scheduler:x', :h, 1, now())"), {"h": marker})
                s.commit()
    with engine.begin() as c:
        assert c.execute(text("SELECT count(*) FROM lease_probe WHERE holder = :m"),
                         {"m": marker}).scalar() == 0


def test_takeover_waits_for_an_in_flight_fenced_commit(engine):
    """FOR SHARE in the fence: a competitor cannot take the lease out from under
    a transaction that already passed its fence, so there is no window between
    'fence passed' and 'COMMIT' in which two leaders both commit."""
    a, b = _lease(engine, "x", "A", ttl=0.8), _lease(engine, "x", "B", ttl=30)
    assert a.acquire()
    done = threading.Event()
    result = {}

    def takeover():
        result["won"] = b.acquire()
        result["at"] = time.monotonic()
        done.set()

    with engine.connect() as conn:
        trans = conn.begin()
        a.fence(conn, a.token)                 # passes: lease live; row share-locked
        time.sleep(1.0)                        # lease is now expired, but A's txn is open
        t = threading.Thread(target=takeover)
        t.start()
        assert not done.wait(0.7), "takeover must block behind the fenced transaction"
        committed_at = time.monotonic()
        trans.commit()
    t.join(10)
    assert result["won"] and b.token == 2 and result["at"] >= committed_at


def test_scheduler_loop_runs_only_the_leaders_bootstraps(engine, monkeypatch):
    from apps.api import scheduler
    from apps.api.services import scheduler_lease as sl
    factory = sessionmaker(bind=engine, autoflush=False)
    sl.install_session_fence(factory)
    ran = []

    def make(name):
        def bootstrap():
            ran.append(name)
            with factory() as s:
                s.execute(text("INSERT INTO lease_probe (name, holder, token, at) "
                               "VALUES (:n, 'loop', 0, now())"), {"n": name})
                s.commit()
            return 1
        return bootstrap

    monkeypatch.setattr(scheduler, "_bootstrap_functions",
                        lambda: (("one", make("one")), ("two", make("two"))))
    la = sl.Leadership(engine, ["one", "two"], holder="A", ttl_seconds=30)
    lb = sl.Leadership(engine, ["one", "two"], holder="B", ttl_seconds=30)
    assert scheduler.reconcile_once(la) == {"one": 1, "two": 1}
    out = scheduler.reconcile_once(lb)
    assert out == {"one": "skipped: another replica leads", "two": "skipped: another replica leads"}
    assert ran == ["one", "two"]

    # A is deposed on "two" mid-flight: its commit is fenced, the error is
    # reported for that subsystem only, and nothing was written.
    def deposed():
        with engine.begin() as c:   # a peer takes the lease while A is working
            c.execute(text("UPDATE scheduler_leases SET holder='B', fencing_token=fencing_token+1 "
                           "WHERE name='scheduler:two'"))
        return make("two-late")()

    monkeypatch.setattr(scheduler, "_bootstrap_functions",
                        lambda: (("one", make("one")), ("two", deposed)))
    out = scheduler.reconcile_once(la)
    assert out["one"] == 1 and out["two"].startswith("error:") and "no longer current" in out["two"]
    with engine.begin() as c:
        assert c.execute(text("SELECT count(*) FROM lease_probe WHERE name = 'two-late'")).scalar() == 0
    la.stop()


def test_stop_releases_leases_for_instant_failover(engine):
    from apps.api.services import scheduler_lease as sl
    la = sl.Leadership(engine, ["x"], holder="A", ttl_seconds=30)
    lb = sl.Leadership(engine, ["x"], holder="B", ttl_seconds=30)
    la.poll(); la.start()
    assert lb.poll() == {"x": False}
    la.stop()
    assert lb.poll() == {"x": True}            # no wait for the 30 s TTL
    assert lb.leases["x"].token == 2


def test_heartbeat_keeps_a_busy_leader_alive_past_its_ttl(engine):
    from apps.api.services import scheduler_lease as sl
    la = sl.Leadership(engine, ["x"], holder="A", ttl_seconds=1.0, renew_every=0.2)
    lb = sl.Leadership(engine, ["x"], holder="B", ttl_seconds=1.0)
    la.poll(); la.start()
    deadline = time.monotonic() + 2.5          # > 2 TTLs of a "long bootstrap"
    while time.monotonic() < deadline:
        assert lb.poll() == {"x": False}
        time.sleep(0.15)
    assert la.leases["x"].held and la.leases["x"].token == 1
    la.stop()


# ── separate OS processes, SIGKILL failover ────────────────────────────────

def _spawn(db, label, ttl, tick, run_seconds):
    proc = subprocess.Popen(
        [sys.executable, "-m", "tests.multihost_lease_worker", db.app_url,
         str(ttl), str(tick), str(run_seconds), label],
        cwd=REPO, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        env=dict(os.environ, PYTHONPATH=str(REPO)),
    )
    assert proc.stdout.readline().strip() == "READY", proc.stderr.read()
    return proc


def test_three_processes_failover_after_sigkill_with_monotonic_tokens(db, engine):
    ttl, tick = 1.5, 0.1
    with engine.begin() as c:
        c.execute(text("DELETE FROM scheduler_leases"))
    with create_engine(db.owner_url).begin() as c:
        c.execute(text("TRUNCATE lease_probe"))
    procs = [_spawn(db, f"p{i}", ttl, tick, 30) for i in range(3)]
    try:
        time.sleep(2.5)

        def leaders():
            with engine.begin() as c:
                return {r[0]: (r[1], r[2]) for r in c.execute(text(
                    "SELECT name, holder, fencing_token FROM scheduler_leases"))}

        before = leaders()
        assert set(before) == {"scheduler:a", "scheduler:b"}
        victims = {h.split(":")[0] for h, _ in before.values()}   # label of the leader(s)
        assert victims, before
        victim = sorted(victims)[0]
        proc = procs[int(victim[1:])]
        killed_at = time.time()
        proc.send_signal(signal.SIGKILL)               # the host dies: no release()
        proc.wait(10)

        time.sleep(ttl + 2.0)
        after = leaders()
        for name, (holder, token) in after.items():
            if before[name][0].startswith(victim + ":"):
                assert not holder.startswith(victim + ":"), "dead leader still holds " + name
                assert token == before[name][1] + 1, (name, before[name], after[name])
            else:
                assert token == before[name][1], "a healthy leader's term must not change"
        with engine.begin() as c:
            rows = c.execute(text(
                "SELECT name, holder, token, extract(epoch FROM at)::float8 FROM lease_probe ORDER BY id")).all()
        # Takeover happened within TTL + a renewal period + scheduling slack.
        for name in after:
            if before[name][0].startswith(victim + ":"):
                first_new = min(r[3] for r in rows if r[0] == name and r[2] == after[name][1])
                assert first_new - killed_at <= ttl + ttl / 4 + 1.5, first_new - killed_at

        # Fencing invariants over every probe write ever made.
        by_name = {}
        for name, holder, token, at in rows:
            by_name.setdefault(name, []).append((token, holder, at))
        for name, items in by_name.items():
            tokens = [t for t, _, _ in items]
            assert tokens == sorted(tokens), f"{name}: a stale token wrote after a newer one: {tokens}"
            owners = {}
            for t, h, _ in items:
                owners.setdefault(t, set()).add(h)
            assert all(len(v) == 1 for v in owners.values()), f"{name}: one token, two holders {owners}"
    finally:
        for p in procs:
            if p.poll() is None:
                p.kill()
            p.wait(10)
            p.stdout.close(); p.stderr.close()
