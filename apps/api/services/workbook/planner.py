"""
Cost-aware waterfall planner (Pillar 2).

Replaces the static free-first/BYOK-last ordering with a learned one: order a
provider chain by expected yield ÷ cost using the ProviderStat ledger, skip
providers in cooldown, and never exceed a workbook's budget ceiling.

The provider ABC carries cost defaults (free OSS = $0). Known paid BYOK APIs are
priced centrally here so we don't edit 30 provider files.
"""

import logging
from datetime import datetime, timezone, timedelta
from typing import List, Optional

from sqlalchemy import func
from sqlalchemy.orm import Session

from apps.api.services.workbook.planner_models import ProviderStat

logger = logging.getLogger("workbook.planner")

# Provider economics now live in the vendor_catalog (single source of truth:
# built-in paid table + each provider's declared cost_per_lookup, incl.
# declarative YAML manifests). PROVIDER_COST kept as a back-compat alias.
from apps.api.services.workbook import vendor_catalog

PROVIDER_COST = {n: v.base_cost for n, v in vendor_catalog.VENDORS.items() if v.base_cost > 0}

COOLDOWN_SECONDS = 300  # how long a rate-limited provider is benched
# Circuit breaker: a provider that hard-times-out (dead host, down API, no key,
# unreachable source like a blocked DDG) is benched so we stop paying its full
# timeout on every subsequent cell. Shorter than the rate-limit cooldown because
# a timeout can be transient; the provider is retried once it expires.
TIMEOUT_COOLDOWN_SECONDS = 180


def provider_cost(name: str) -> float:
    return vendor_catalog.base_cost(name)


def is_paid(name: str) -> bool:
    return vendor_catalog.is_paid(name)


def _now() -> datetime:
    return datetime.now(timezone.utc)


def _stat(db: Session, provider: str, field: str) -> Optional[ProviderStat]:
    return (
        db.query(ProviderStat)
        .filter(ProviderStat.provider == provider, ProviderStat.field == field)
        .first()
    )


def in_cooldown(db: Session, provider: str, field: str) -> bool:
    st = _stat(db, provider, field)
    if not st or not st.cooldown_until:
        return False
    cd = st.cooldown_until
    if cd.tzinfo is None:
        cd = cd.replace(tzinfo=timezone.utc)
    return cd > _now()


# How strongly the correctness prior bends ordering. The prior is applied as a
# multiplier in [1-CORRECTNESS_WEIGHT, 1]: a provider scored fully accurate keeps
# its yield/cost score unchanged; a confidently-wrong provider (accuracy→0) is
# knocked down by up to this fraction. Tuned so accuracy can reorder providers of
# similar yield/cost without ever fully zeroing an otherwise-useful provider.
CORRECTNESS_WEIGHT = 0.6


def correctness_prior(db: Session, provider: str, field: str) -> float:
    """Per-provider correctness multiplier in (0, 1], from the accuracy eval
    harness (golden-dataset scoring persisted onto ProviderStat).

    Returns 1.0 (no effect) when the provider has never been evaluated — this is
    the graceful default that keeps existing ordering intact until accuracy data
    exists. Accuracy is learned per provider (golden set spans company fields),
    so the same prior applies across that provider's fields.
    """
    score = None
    st = _stat(db, provider, field)
    if st and st.accuracy_score is not None:
        score = st.accuracy_score
    else:
        # fall back to any field's accuracy for this provider (provider-level prior)
        other = (
            db.query(ProviderStat)
            .filter(ProviderStat.provider == provider, ProviderStat.accuracy_score.isnot(None))
            .first()
        )
        if other is not None:
            score = other.accuracy_score
    if score is None:
        return 1.0  # never evaluated → no effect (graceful default)
    return (1.0 - CORRECTNESS_WEIGHT) + CORRECTNESS_WEIGHT * max(0.0, min(1.0, score))


def _score(db: Session, provider: str, field: str) -> float:
    """Expected yield ÷ cost, bent by the correctness prior. Unseen providers get
    an optimistic hit-rate prior so they get tried; free providers are effectively
    divided by a tiny cost. The correctness prior multiplies the result so a
    provider that returns confident WRONG data (high hit-rate, low accuracy) no
    longer ranks above an accurate one — without it the planner ordered purely on
    hit-rate. Defaults to a no-op multiplier when no accuracy data exists."""
    st = _stat(db, provider, field)
    hit_rate = st.hit_rate if (st and st.attempts >= 3) else 0.5  # prior until we have data
    cost = provider_cost(provider)
    return (hit_rate / (cost + 0.001)) * correctness_prior(db, provider, field)


def order_chain(db: Session, field: str, chain: List[str], budget_remaining: Optional[float] = None) -> List[str]:
    """Return the chain reordered by yield/cost, with cooldown'd and (if over
    budget) unaffordable paid providers dropped. Free providers always kept."""
    usable = []
    for p in chain:
        if in_cooldown(db, p, field):
            logger.info(f"planner: skip {p} (cooldown) for {field}")
            continue
        if budget_remaining is not None and is_paid(p) and provider_cost(p) > budget_remaining:
            logger.info(f"planner: skip paid {p} (budget ${budget_remaining:.3f} < ${provider_cost(p)})")
            continue
        usable.append(p)
    # Highest yield/cost first; ties keep original order (stable sort).
    return sorted(usable, key=lambda p: _score(db, p, field), reverse=True)


def record_attempt(
    db: Session, provider: str, field: str, *,
    success: bool, confidence: float = 0.0, latency_ms: float = 0.0,
    rate_limited: bool = False, timed_out: bool = False,
):
    """Atomically upsert one provider attempt.

    Workbook rows run concurrently, so the old read-then-insert implementation
    raced on the unique ``(provider, field)`` key. More importantly on SQLite,
    its eager ``flush()`` acquired the single writer lock and the cell kept that
    transaction open while trying the rest of its network waterfall. One slow
    cell could therefore freeze login, job heartbeats, and every other cell.

    A single-statement upsert avoids the create race and makes every counter an
    additive database-side update. The caller still owns the transaction and
    must commit promptly; workbook enrichment records these attempts only after
    the cell result itself has committed.
    """
    table = ProviderStat.__table__
    now = _now()
    cooldown_until = None
    if rate_limited:
        cooldown_until = now + timedelta(seconds=COOLDOWN_SECONDS)
    elif timed_out:
        cooldown_until = now + timedelta(seconds=TIMEOUT_COOLDOWN_SECONDS)

    values = {
        "provider": provider,
        "field": field,
        "attempts": 1,
        "hits": 1 if success else 0,
        "total_confidence": confidence if success else 0.0,
        "total_latency_ms": latency_ms,
        "total_cost_usd": provider_cost(provider) if success else 0.0,
        "cooldown_until": cooldown_until,
        "updated_at": now,
        "accuracy_samples": 0,
    }
    updates = {
        "attempts": func.coalesce(table.c.attempts, 0) + 1,
        "hits": func.coalesce(table.c.hits, 0) + (1 if success else 0),
        "total_confidence": (
            func.coalesce(table.c.total_confidence, 0.0)
            + (confidence if success else 0.0)
        ),
        "total_latency_ms": func.coalesce(table.c.total_latency_ms, 0.0) + latency_ms,
        "total_cost_usd": (
            func.coalesce(table.c.total_cost_usd, 0.0)
            + (provider_cost(provider) if success else 0.0)
        ),
        "updated_at": now,
        "cooldown_until": (
            cooldown_until
            if cooldown_until is not None
            else table.c.cooldown_until
        ),
    }

    dialect = db.get_bind().dialect.name
    if dialect == "sqlite":
        from sqlalchemy.dialects.sqlite import insert
    elif dialect == "postgresql":
        from sqlalchemy.dialects.postgresql import insert
    else:  # pragma: no cover - supported deployments use SQLite or Postgres
        st = _stat(db, provider, field)
        if not st:
            st = ProviderStat(provider=provider, field=field)
            db.add(st)
            db.flush()
        st.attempts = (st.attempts or 0) + 1
        st.total_latency_ms = (st.total_latency_ms or 0.0) + latency_ms
        if success:
            st.hits = (st.hits or 0) + 1
            st.total_confidence = (st.total_confidence or 0.0) + confidence
            st.total_cost_usd = (st.total_cost_usd or 0.0) + provider_cost(provider)
        if cooldown_until is not None:
            st.cooldown_until = cooldown_until
        return

    statement = insert(table).values(**values).on_conflict_do_update(
        index_elements=[table.c.provider, table.c.field],
        set_=updates,
    )
    db.execute(statement)


def looks_rate_limited(error: str) -> bool:
    e = (error or "").lower()
    return any(s in e for s in ("429", "rate limit", "too many requests", "quota", "throttle"))
