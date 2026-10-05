"""PostgreSQL scheduler leadership: leases with fencing tokens.

Periodic schedulers (``apps/api/scheduler.py``) are safe to run twice only
because every enqueue is idempotent, and three of them are not fully so (see
docs/plans/m8-multihost-state.md). For multi-host operation exactly one replica
must run each periodic scheduler, a crashed leader must be replaced, and a
leader that was merely paused must not be able to write after it was replaced.

Protocol (one row in ``scheduler_leases`` per scheduler, ``scheduler:<name>``;
the Go server speaks the same protocol in ``apps/server/internal/lease``)::

    acquire   INSERT ... ON CONFLICT DO UPDATE ... WHERE holder = me OR expired
              One statement. The database clock decides expiry, so host clock
              skew cannot create two leaders. The fencing token is incremented
              on every change of holder AND whenever the same holder re-acquires
              after its lease had lapsed; it is kept while the lease is live.
    renew     the same statement (a live re-acquire keeps the token).
    release   expires the lease now, keeping the row so the token never resets.
    fence     SELECT ... FOR SHARE on the row, matching holder AND token and
              not expired. Run in the transaction that writes, just before
              COMMIT. FOR SHARE makes a competing takeover wait for the commit,
              and after a takeover the old token matches nothing, so a deposed
              or paused leader's transaction is aborted instead of committed.

Why not ``pg_advisory_lock``: a session lock needs a dedicated connection that
transaction-pooling proxies break, it silently disappears on a connection
reset with no signal to the holder, and it carries no token to fence writes
with. A lease row survives pooling, is observable (``scheduler_leases``) and
fences.

A holder also keeps a conservative local deadline (monotonic clock, measured
from before the request was sent) and stops leading when it passes without a
successful renewal. That bounds how long a partitioned leader keeps working
before the database fence would reject it; the fence, not the local clock, is
what guarantees safety.
"""

from __future__ import annotations

import contextlib
import contextvars
import logging
import os
import socket
import threading
import time
import uuid
from typing import Callable, Dict, Iterable, Iterator, Optional

from sqlalchemy import event, text
from sqlalchemy.engine import Engine

logger = logging.getLogger("apps.api.scheduler_lease")

LEASE_PREFIX = "scheduler:"


class LeaseLost(RuntimeError):
    """The lease this work was fenced by is no longer held by this holder."""


_ACQUIRE = text(
    """
    INSERT INTO scheduler_leases AS l
        (name, holder, fencing_token, acquired_at, renewed_at, expires_at)
    VALUES (:name, :holder, 1, now(), now(), now() + make_interval(secs => :ttl))
    ON CONFLICT (name) DO UPDATE SET
        fencing_token = CASE WHEN l.holder = EXCLUDED.holder AND l.expires_at > now()
                             THEN l.fencing_token ELSE l.fencing_token + 1 END,
        acquired_at   = CASE WHEN l.holder = EXCLUDED.holder AND l.expires_at > now()
                             THEN l.acquired_at ELSE now() END,
        holder        = EXCLUDED.holder,
        renewed_at    = now(),
        expires_at    = EXCLUDED.expires_at
    WHERE l.holder = EXCLUDED.holder OR l.expires_at <= now()
    RETURNING fencing_token
    """
)

_RELEASE = text(
    """
    UPDATE scheduler_leases SET expires_at = now()
    WHERE name = :name AND holder = :holder AND fencing_token = :token
    """
)

# FOR SHARE: a takeover (UPDATE) blocks until this transaction ends. clock_
# timestamp() because now() is the start of a possibly long write transaction.
_FENCE = text(
    """
    SELECT 1 FROM scheduler_leases
    WHERE name = :name AND holder = :holder AND fencing_token = :token
      AND expires_at > clock_timestamp()
    FOR SHARE
    """
)

_SNAPSHOT = text(
    """
    SELECT name, holder, fencing_token, acquired_at, renewed_at, expires_at,
           expires_at > now() AS live
    FROM scheduler_leases ORDER BY name
    """
)


def new_holder_id() -> str:
    """Unique per process start, so a restarted process is a new holder (and
    therefore gets a new fencing token) even on the same host and pid."""
    return f"{socket.gethostname()}:{os.getpid()}:{uuid.uuid4().hex[:8]}"


class SchedulerLease:
    """One named lease held (or not) by this process."""

    def __init__(
        self,
        engine: Engine,
        name: str,
        *,
        holder: Optional[str] = None,
        ttl_seconds: float = 30.0,
    ):
        if ttl_seconds <= 0:
            raise ValueError("ttl_seconds must be positive")
        self.engine = engine
        self.name = name if name.startswith(LEASE_PREFIX) else LEASE_PREFIX + name
        self.holder = holder or new_holder_id()
        self.ttl = float(ttl_seconds)
        self._lock = threading.Lock()
        self._token: Optional[int] = None
        self._deadline = 0.0  # time.monotonic()

    # -- state ---------------------------------------------------------

    @property
    def token(self) -> Optional[int]:
        return self._token

    @property
    def held(self) -> bool:
        """True while a renewal succeeded recently enough to trust locally."""
        return self._token is not None and time.monotonic() < self._deadline

    # -- protocol ------------------------------------------------------

    def acquire(self) -> bool:
        """Acquire or renew. True when this holder leads; False when another
        holder's lease is still live. Database errors propagate."""
        started = time.monotonic()
        with self.engine.begin() as conn:
            row = conn.execute(
                _ACQUIRE, {"name": self.name, "holder": self.holder, "ttl": self.ttl}
            ).first()
        with self._lock:
            if row is None:
                if self._token is not None:
                    logger.warning("lost lease %s (holder %s)", self.name, self.holder)
                self._token = None
                self._deadline = 0.0
                return False
            token = int(row[0])
            if self._token is None:
                logger.info("acquired lease %s token=%s holder=%s", self.name, token, self.holder)
            elif token != self._token:
                # Lapsed and re-won: a new term. Work started under the old
                # token is fenced out; anything that cached it must refresh.
                logger.warning("re-acquired lease %s with new token %s (was %s)",
                               self.name, token, self._token)
            self._token = token
            # Trust the lease for strictly less than its remaining life: the
            # request may have been slow, and the database fence is the backstop.
            self._deadline = started + self.ttl * 0.8
            return True

    renew = acquire

    def release(self) -> None:
        """Hand the lease over now (graceful shutdown). Idempotent."""
        with self._lock:
            token, self._token, self._deadline = self._token, None, 0.0
        if token is None:
            return
        with self.engine.begin() as conn:
            conn.execute(_RELEASE, {"name": self.name, "holder": self.holder, "token": token})
        logger.info("released lease %s token=%s", self.name, token)

    def fence(self, session_or_conn, token: Optional[int] = None) -> None:
        """Raise :class:`LeaseLost` unless this holder still owns the lease at
        ``token`` (default: the current one). Call inside the transaction that
        must be fenced. Work that started under one term should pass the token
        it started with, so a lease re-won under a new token after a lapse
        does not retroactively bless it."""
        if token is None:
            token = self._token
        if token is None:
            raise LeaseLost(f"{self.name}: not held")
        ok = session_or_conn.execute(
            _FENCE, {"name": self.name, "holder": self.holder, "token": token}
        ).first()
        if ok is None:
            raise LeaseLost(f"{self.name}: token {token} is no longer current")


# ── commit fencing for ORM sessions ─────────────────────────────────────────

_active_fence: contextvars.ContextVar[Optional[tuple]] = contextvars.ContextVar(
    "scheduler_active_fence", default=None
)


@contextlib.contextmanager
def fenced(lease: SchedulerLease) -> Iterator[SchedulerLease]:
    """Within this block every ORM session commit first runs the lease fence
    in its own transaction, against the token held when the block was entered.
    Requires :func:`install_session_fence`."""
    if lease.token is None:
        raise LeaseLost(f"{lease.name}: not held")
    reset = _active_fence.set((lease, lease.token))
    try:
        yield lease
    finally:
        _active_fence.reset(reset)


def install_session_fence(session_factory) -> None:
    """Register the ``before_commit`` hook on a sessionmaker. Idempotent. The
    hook is inert unless :func:`fenced` is active in the current context, so
    installing it costs nothing for request handling."""
    if getattr(session_factory, "_scheduler_fence_installed", False):
        return

    @event.listens_for(session_factory, "before_commit")
    def _fence_before_commit(session):  # noqa: ANN001
        active = _active_fence.get()
        if active is not None:
            active[0].fence(session, active[1])

    session_factory._scheduler_fence_installed = True


# ── leadership of a set of schedulers ───────────────────────────────────────

class Leadership:
    """Leases for a set of schedulers plus the heartbeat that keeps them alive."""

    def __init__(
        self,
        engine: Engine,
        names: Iterable[str],
        *,
        holder: Optional[str] = None,
        ttl_seconds: float = 30.0,
        renew_every: Optional[float] = None,
    ):
        self.holder = holder or new_holder_id()
        self.ttl = float(ttl_seconds)
        self.renew_every = renew_every if renew_every is not None else self.ttl / 3.0
        self.leases: Dict[str, SchedulerLease] = {
            n: SchedulerLease(engine, n, holder=self.holder, ttl_seconds=self.ttl)
            for n in names
        }
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def poll(self) -> Dict[str, bool]:
        """Try to acquire or renew every lease once. A database error leaves the
        lease to expire locally; it never raises into the scheduler loop."""
        status: Dict[str, bool] = {}
        for name, lease in self.leases.items():
            try:
                status[name] = lease.acquire()
            except Exception:  # noqa: BLE001
                logger.exception("lease %s: heartbeat failed", lease.name)
                status[name] = lease.held
        return status

    def start(self) -> None:
        if self._thread is not None:
            return
        self._stop.clear()
        self._thread = threading.Thread(target=self._run, name="scheduler-lease", daemon=True)
        self._thread.start()

    def _run(self) -> None:
        while not self._stop.wait(self.renew_every):
            self.poll()

    def stop(self) -> None:
        """Stop heartbeating and hand every held lease over."""
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=5)
            self._thread = None
        for lease in self.leases.values():
            try:
                lease.release()
            except Exception:  # noqa: BLE001
                logger.exception("lease %s: release failed", lease.name)

    @contextlib.contextmanager
    def lead(self, name: str) -> Iterator[Optional[SchedulerLease]]:
        """Yield the fenced lease when this process leads ``name``, else None."""
        lease = self.leases[name]
        if not lease.held:
            try:
                lease.acquire()
            except Exception:  # noqa: BLE001
                logger.exception("lease %s: acquire failed", lease.name)
        if not lease.held:
            yield None
            return
        with fenced(lease):
            yield lease


def snapshot(engine: Engine) -> list[dict]:
    """Current lease rows (for operators and tests)."""
    with engine.begin() as conn:
        return [dict(r._mapping) for r in conn.execute(_SNAPSHOT)]
