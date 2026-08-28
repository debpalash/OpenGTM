"""Source health-check (sourcing P2/P3) — OBSERVE-ONLY rolling per-source yield.

A **platform-global**, non-tenant ledger of how reliably each ``site:`` registry
source (``source_registry.SOURCES``) returns *usable* results when probed through
the live DDG path. It is the active-probe sibling of ``source_stats`` (the passive
per-run reliability ledger, ``services/leadgen/source_stats.py`` #98) and reuses
the same shape — one row per key, rolling counters + an EWMA, an atomic
read-or-create upsert — but keyed by source ``name`` only (no ``region``: a probe
runs a fixed set of canary geos, and the operator toggle it ANDs with is global).

It holds **no tenant PII** (only aggregate source health), so like
``source_stats`` / ``ProviderStat`` it lives OUTSIDE the RLS tenancy model and is
shared across worker replicas (it sits in the same non-RLS DB as the Job queue,
``SessionLocal``) — a strict improvement over the per-instance settings SQLite for
replica correctness.

Two independent flags, both DEFAULT OFF (see ``core/config.py``):

* ``SOURCE_HEALTH_ENABLED`` — master switch for the **probe job** (bootstrap +
  recording). OFF → no job is scheduled and nothing is recorded (no-op in prod).
* ``SOURCE_HEALTH_ENFORCE`` — the SEPARATE disable/exclude flag. OFF (the
  OBSERVE-ONLY default) → health is recorded and visible via ``/sources/health``
  but ``is_health_disabled`` returns ``False`` for everyone, so health **never
  changes which sources a live job runs** (a chronically-0 source keeps running).
  ON → an ``auto_disabled`` source is excluded from live jobs (and re-enabled on
  recovery). The state machine ALWAYS records full state (incl. ``auto_disabled``);
  this flag only controls whether that state filters live collection.

The whole design is built around the report's documented "empty != dead" failure
(``docs/research/data-source-test-report.md``): probes are multi-canary, slow to disable
(``DISABLE_THRESHOLD`` consecutive dead runs), and a **systemic-outage guard**
suppresses ALL zero transitions on a run where most sources came back empty (the
proxy-pool/DDG-rate-limit failure mode), so a transient infra hiccup can never
mass-disable healthy sources.
"""

from __future__ import annotations

import asyncio
import logging
from datetime import datetime, timedelta, timezone
from typing import List, Optional, Tuple

from sqlalchemy import Boolean, Column, DateTime, Float, Integer, String
from sqlalchemy.orm import Session
from sqlalchemy.sql import func

from apps.api.database import Base, SessionLocal

logger = logging.getLogger("leadgen.source_health")


# ── Tunables (read live from settings so tests/ops can override) ───────────
def _cfg(name: str, default):
    """Read a SOURCE_HEALTH_* knob from the cached settings object at call time
    (not import time) so monkeypatch/env overrides in tests + ops take effect."""
    from apps.api.core.config import settings
    return getattr(settings, name, default)


def probe_enabled() -> bool:
    """Master switch: is the probe job allowed to run / be scheduled?"""
    return bool(_cfg("SOURCE_HEALTH_ENABLED", False))


def enforce_enabled() -> bool:
    """SEPARATE disable/exclude flag (OBSERVE-ONLY default OFF). When OFF, health
    is recorded but NEVER excludes a source from live jobs."""
    return bool(_cfg("SOURCE_HEALTH_ENFORCE", False))


# Canary queries — (query, city) pairs run through the LIVE path per source. Geo
# diversity directly counters the report's query-sensitivity false negatives
# (forbes/quora 1/4, reddit 2/4): a source is "alive" if ANY canary returns
# usable results (yield = max over canaries). Keep this small — probe DDG volume
# is |SOURCES| x len(CANARY_QUERIES) per cycle.
CANARY_QUERIES: List[Tuple[str, str]] = [
    ("software companies", "Bangalore"),
    ("marketing agency", "New York"),
]

# EWMA smoothing for the observability yield signal (matches source_stats alpha).
EWMA_ALPHA = 0.3

# Probe DDG result cap per canary (mirrors the live registry path max_results=8).
PROBE_MAX_RESULTS = 8

_STATE_HEALTHY = "healthy"
_STATE_DEGRADED = "degraded"
_STATE_DISABLED = "auto_disabled"


def _now() -> datetime:
    return datetime.now(timezone.utc)


# ── ORM model (global, non-RLS, PK = source name) ──────────────────────────
class SourceHealth(Base):
    """Global per-source active-probe health. One row per ``SOURCES`` name."""

    __tablename__ = "source_health"

    # The registry source name is the natural, stable, global key (no surrogate
    # id, no workspace_id — sources are shared infra, exactly like the operator
    # toggle ``get_source_enabled``).
    name = Column(String(100), primary_key=True)

    # healthy | degraded | auto_disabled
    state = Column(String(20), nullable=False, default=_STATE_HEALTHY)

    consecutive_zero = Column(Integer, nullable=False, default=0)
    consecutive_nonzero = Column(Integer, nullable=False, default=0)

    last_yield = Column(Integer, nullable=True)        # usable results on last probe
    ewma_yield = Column(Float, nullable=True)          # rolling EWMA of yield
    probes = Column(Integer, nullable=False, default=0)  # total probe runs applied

    last_probe_at = Column(DateTime, nullable=True)
    last_ok_at = Column(DateTime, nullable=True)        # last nonzero probe
    last_outage_at = Column(DateTime, nullable=True)    # last run skipped as outage

    disabled_at = Column(DateTime, nullable=True)
    disabled_reason = Column(String(50), nullable=True)
    # Operator force-enabled: health may NEVER auto-disable this source again.
    manual_override = Column(Boolean, nullable=False, default=False)

    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {
            "name": self.name,
            "state": self.state,
            "consecutive_zero": self.consecutive_zero or 0,
            "consecutive_nonzero": self.consecutive_nonzero or 0,
            "last_yield": self.last_yield,
            "ewma_yield": round(self.ewma_yield, 3) if self.ewma_yield is not None else None,
            "probes": self.probes or 0,
            "last_probe_at": self.last_probe_at.isoformat() if self.last_probe_at else None,
            "last_ok_at": self.last_ok_at.isoformat() if self.last_ok_at else None,
            "last_outage_at": self.last_outage_at.isoformat() if self.last_outage_at else None,
            "disabled_at": self.disabled_at.isoformat() if self.disabled_at else None,
            "disabled_reason": self.disabled_reason,
            "manual_override": bool(self.manual_override),
            "updated_at": self.updated_at.isoformat() if self.updated_at else None,
        }


def _get_row(db: Session, name: str) -> Optional[SourceHealth]:
    return db.query(SourceHealth).filter(SourceHealth.name == name).first()


def _get_or_create(db: Session, name: str) -> SourceHealth:
    row = _get_row(db, name)
    if row is None:
        row = SourceHealth(
            name=name, state=_STATE_HEALTHY,
            consecutive_zero=0, consecutive_nonzero=0, probes=0,
            manual_override=False,
        )
        db.add(row)
        db.flush()
    return row


# ── Transition logic (pure; mutates the row) ───────────────────────────────
def _apply_transition(
    row: SourceHealth,
    probe_yield: int,
    outage: bool,
    now: Optional[datetime] = None,
    *,
    degrade_threshold: Optional[int] = None,
    disable_threshold: Optional[int] = None,
    recover_threshold: Optional[int] = None,
) -> None:
    """Fold one probe result into the row's rolling state.

    Always records observational fields (last_probe_at, last_yield, ewma, probes).
    On a run flagged as a **systemic outage**, ZERO results apply NO state/counter
    transitions (only a ``last_outage_at`` marker) — this is the single safeguard
    against the proxy-pool false-zero mass-disable. Nonzero results still advance
    recovery, even during an outage.
    """
    now = now or _now()
    degrade = degrade_threshold if degrade_threshold is not None else int(_cfg("SOURCE_HEALTH_DEGRADE_THRESHOLD", 3))
    disable = disable_threshold if disable_threshold is not None else int(_cfg("SOURCE_HEALTH_DISABLE_THRESHOLD", 6))
    recover = recover_threshold if recover_threshold is not None else int(_cfg("SOURCE_HEALTH_RECOVER_THRESHOLD", 2))

    # Observational fields — recorded every probe regardless of outage.
    row.last_probe_at = now
    row.last_yield = probe_yield
    row.probes = (row.probes or 0) + 1
    if row.ewma_yield is None:
        row.ewma_yield = float(probe_yield)
    else:
        row.ewma_yield = EWMA_ALPHA * float(probe_yield) + (1.0 - EWMA_ALPHA) * row.ewma_yield

    if probe_yield > 0:
        # Nonzero — advance recovery (applies even during an outage).
        row.consecutive_zero = 0
        row.consecutive_nonzero = (row.consecutive_nonzero or 0) + 1
        row.last_ok_at = now
        if row.state == _STATE_DISABLED:
            if (row.consecutive_nonzero or 0) >= recover:
                row.state = _STATE_HEALTHY
                row.disabled_at = None
                row.disabled_reason = None
        elif row.state == _STATE_DEGRADED:
            # A degraded source that returns anything is healthy again.
            row.state = _STATE_HEALTHY
        return

    # Zero yield.
    if outage:
        # Infra failure — record the marker, apply NO zero transitions.
        row.last_outage_at = now
        return

    row.consecutive_nonzero = 0
    row.consecutive_zero = (row.consecutive_zero or 0) + 1
    if (row.consecutive_zero or 0) >= disable and not row.manual_override:
        row.state = _STATE_DISABLED
        row.disabled_at = now
        row.disabled_reason = "zero_yield"
    elif (row.consecutive_zero or 0) >= degrade:
        # Don't downgrade a (manual-override) source that can't be disabled out of
        # healthy any harder than degraded; degrade is purely advisory.
        if row.state != _STATE_DISABLED:
            row.state = _STATE_DEGRADED


# ── Probe (live DDG path, junk-filtered, max-over-canary) ──────────────────
async def _run_probe(source: dict) -> int:
    """Probe one source via the LIVE path; return usable yield = max over canaries.

    Reuses ``build_queries`` (registry) and ``_ddg_search`` + ``_is_junk_url``
    (job_runner) so the probe signal matches exactly what a live job experiences.
    Lazy imports avoid a circular import with job_runner (which imports this gate).
    """
    from apps.api.services.leadgen.source_registry import build_queries
    from apps.api.services.leadgen.job_runner import _ddg_search, _is_junk_url

    best = 0
    for q_text, city in CANARY_QUERIES:
        queries = build_queries(source, q_text, city)
        if not queries:
            continue
        try:
            results = await _ddg_search(queries[0], max_results=PROBE_MAX_RESULTS)
        except Exception:
            results = []
        usable = sum(1 for r in results if not _is_junk_url((r or {}).get("href", "")))
        if usable > best:
            best = usable
    return best


# ── Gate (the additive, observe-only-by-default exclusion layer) ───────────
def health_disabled_names(db: Optional[Session] = None) -> set[str]:
    """Set of source names health says to exclude from live jobs.

    EMPTY whenever ``SOURCE_HEALTH_ENFORCE`` is OFF (the observe-only default) —
    a single short-circuit with no DB query, so health never changes which
    sources run. Fail-open: any error → empty set (a health bug can't blind the
    engine). One query (batched) so the live filter stays cheap.
    """
    if not enforce_enabled():
        return set()
    own = db is None
    if own:
        db = SessionLocal()
    try:
        rows = db.query(SourceHealth.name).filter(SourceHealth.state == _STATE_DISABLED).all()
        return {r[0] for r in rows}
    except Exception as e:  # fail-open
        logger.warning(f"source_health gate query failed (fail-open): {e}")
        return set()
    finally:
        if own:
            db.close()


def is_health_disabled(name: str, db: Optional[Session] = None) -> bool:
    """True only if ``SOURCE_HEALTH_ENFORCE`` is ON and the source row is
    ``auto_disabled``. Missing row → False (fail-open / treated healthy). Flag
    OFF → always False (observe-only)."""
    if not enforce_enabled():
        return False
    own = db is None
    if own:
        db = SessionLocal()
    try:
        row = _get_row(db, name)
        return bool(row and row.state == _STATE_DISABLED)
    except Exception as e:  # fail-open
        logger.warning(f"source_health is_health_disabled failed (fail-open): {e}")
        return False
    finally:
        if own:
            db.close()


# ── Operator visibility + manual override ──────────────────────────────────
def health_summary(db: Optional[Session] = None) -> dict:
    """Per-source health for the operator (`GET /sources/health`)."""
    own = db is None
    if own:
        db = SessionLocal()
    try:
        rows = db.query(SourceHealth).order_by(SourceHealth.name).all()
        by_state: dict[str, int] = {}
        for r in rows:
            by_state[r.state] = by_state.get(r.state, 0) + 1
        return {
            "enforce": enforce_enabled(),     # is the disable gate live?
            "probe_enabled": probe_enabled(),
            "total": len(rows),
            "by_state": by_state,
            "sources": [r.to_api() for r in rows],
        }
    finally:
        if own:
            db.close()


def health_map(db: Optional[Session] = None) -> dict[str, dict]:
    """Name -> health dict, for merging a badge into `GET /sources`."""
    own = db is None
    if own:
        db = SessionLocal()
    try:
        return {r.name: r.to_api() for r in db.query(SourceHealth).all()}
    finally:
        if own:
            db.close()


def manual_enable(name: str, db: Optional[Session] = None) -> Optional[dict]:
    """Operator force-enable: clear any auto_disabled state and PIN
    ``manual_override`` so health can never auto-disable this source again.
    Operator intent wins. No-op-safe if no row exists yet (creates a pinned row).
    """
    own = db is None
    if own:
        db = SessionLocal()
    try:
        row = _get_or_create(db, name)
        row.manual_override = True
        if row.state == _STATE_DISABLED:
            row.state = _STATE_HEALTHY
            row.disabled_at = None
            row.disabled_reason = None
            row.consecutive_zero = 0
        db.commit()
        return row.to_api()
    finally:
        if own:
            db.close()


def reset_health(name: str, db: Optional[Session] = None) -> Optional[dict]:
    """Operator reset a source's health row to a clean healthy baseline."""
    own = db is None
    if own:
        db = SessionLocal()
    try:
        row = _get_row(db, name)
        if row is None:
            return None
        row.state = _STATE_HEALTHY
        row.consecutive_zero = 0
        row.consecutive_nonzero = 0
        row.disabled_at = None
        row.disabled_reason = None
        row.manual_override = False
        db.commit()
        return row.to_api()
    finally:
        if own:
            db.close()


# ── Scheduled durable job: probe all sources, apply transitions ────────────
def _all_probe_sources() -> List[dict]:
    """Every registry source (regardless of enabled/region/health) — disabled
    sources must keep being probed so they can recover."""
    from apps.api.services.leadgen.source_registry import get_all_sources
    return get_all_sources(enabled_only=False)


async def handle_source_health_check(job_id: int, payload: dict) -> None:
    """Durable handler: probe every source, compute the systemic-outage guard,
    apply transitions, persist, then self-reschedule (single global cadence).

    Master-flag aware: if ``SOURCE_HEALTH_ENABLED`` is OFF the run is a no-op and
    does NOT self-reschedule (so toggling the flag off cleanly drains the job).
    """
    if not probe_enabled():
        logger.info(f"[job {job_id}] source_health_check: SOURCE_HEALTH_ENABLED off — skip + no reschedule")
        return

    sources = _all_probe_sources()
    concurrency = max(1, int(_cfg("SOURCE_HEALTH_PROBE_CONCURRENCY", 8)))
    outage_ratio = float(_cfg("SOURCE_HEALTH_OUTAGE_RATIO", 0.6))

    # Phase 1: probe all sources in bounded batches (mirror the live registry
    # batching: low concurrency, sleep between batches to avoid amplifying DDG
    # rate-limiting with the probe itself).
    yields: dict[str, int] = {}
    for i in range(0, len(sources), concurrency):
        batch = sources[i:i + concurrency]
        results = await asyncio.gather(*(_run_probe(s) for s in batch), return_exceptions=True)
        for s, res in zip(batch, results):
            yields[s["name"]] = 0 if isinstance(res, Exception) else int(res)
        if i + concurrency < len(sources):
            await asyncio.sleep(1)

    # Phase 2: systemic-outage guard — if most sources came back empty, treat the
    # whole run as infra failure and suppress ALL zero transitions.
    total = len(yields) or 1
    zeros = sum(1 for v in yields.values() if v == 0)
    zero_ratio = zeros / total
    outage = zero_ratio >= outage_ratio
    if outage:
        logger.warning(
            f"[job {job_id}] source_health_check: systemic outage guard TRIPPED "
            f"(zero_ratio={zero_ratio:.2f} >= {outage_ratio}) — no zero transitions applied"
        )

    # Phase 3: persist transitions atomically.
    now = _now()
    with SessionLocal() as db:
        for name, y in yields.items():
            row = _get_or_create(db, name)
            _apply_transition(row, y, outage, now=now)
        db.commit()

    disabled = [n for n, v in yields.items() if v == 0]
    logger.info(
        f"[job {job_id}] source_health_check: probed {total} sources, "
        f"{zeros} zero ({zero_ratio:.0%}), outage={outage}"
    )

    # Phase 4: self-reschedule the next cycle (single-flight).
    _reschedule_next()


# ── Scheduling (single global cadence, single-flight, restart-safe) ────────
def _next_run_at() -> datetime:
    """Wall-clock-aligned next run. Reuses the automations scheduler so cadence
    semantics match the rest of the platform."""
    from apps.api.services.automations.engine import compute_next_run
    interval = str(_cfg("SOURCE_HEALTH_INTERVAL", "daily"))
    # Fixed UTC-midnight anchor → a stable, replica-independent fire schedule.
    anchor = datetime(2026, 1, 1, tzinfo=timezone.utc)
    return compute_next_run(anchor, interval)


def _enqueue_health_if_absent(next_run_at: Optional[datetime] = None) -> bool:
    """Enqueue ONE ``source_health_check`` job, single-flight: no-op if any
    pending/processing health job already exists. Multi-replica safe (the atomic
    Job claim guarantees at-most-once execution of the claimed job)."""
    from apps.api.models import Job
    from apps.api.services.job_scheduling import enqueue_job_once
    next_run_at = next_run_at or _next_run_at()
    with SessionLocal() as db:
        pending = (
            db.query(Job)
            .filter(Job.type == "source_health_check", Job.status.in_(("pending", "processing")))
            .count()
        )
        if pending:
            return False
        fire_key = f"source_health:{next_run_at.replace(microsecond=0).isoformat()}"
        job = enqueue_job_once(
            db,
            job_type="source_health_check",
            payload={},
            fire_key=fire_key,
            next_run_at=next_run_at,
        )
        if job is None:
            return False
        db.commit()
    return True


def _reschedule_next() -> None:
    try:
        _enqueue_health_if_absent()
    except Exception as e:
        logger.warning(f"source_health reschedule skipped: {e}")


def bootstrap_source_health() -> bool:
    """Cold-start: if the probe is enabled and no health job is pending, enqueue
    one. No-op when ``SOURCE_HEALTH_ENABLED`` is off. Survives restarts;
    single-flight + atomic claim make it replica-safe."""
    if not probe_enabled():
        return False
    enqueued = _enqueue_health_if_absent()
    if enqueued:
        logger.info("bootstrapped recurring source_health_check")
    return enqueued
