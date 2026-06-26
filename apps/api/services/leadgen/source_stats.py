"""Source-reliability ledger (sourcing P2) — per-(source, region) health.

A **global**, non-tenant ledger of how reliably each lead-*source* (web_search,
linkedin, registry:indiamart, …) yields usable leads in a given region. It is the
sourcing-side mirror of ``ProviderStat`` (enrichment-provider ledger,
``services/workbook/planner_models.py`` + ``planner.py``): same shape — a unique
key, cumulative counters, an atomic upsert, and a learned EWMA score with an
optimistic prior until we have enough samples.

It holds **no tenant PII** (only aggregate source health), so like ``ProviderStat``
it sits outside the RLS tenancy model and is shared across workspaces (signal
accumulates fastest pooled). ``region`` is part of the key because region
dominates whether a source yields (e.g. ``naukri`` is India-only).

Reliability ``r ∈ [0,1]`` combines three per-run ratios:
    r = w_hit*hit_rate + w_dedup*dedup_survival + w_val*validation_pass
stored as a rolling EWMA so recent behaviour dominates (sources rot / recover).
Below ``MIN_SAMPLES`` runs the ledger returns ``None`` → NO score adjustment
(identical to today), exactly like the planner's "prior until we have data".

v1 is **score-only**: this never gates which sources execute (that would create a
starvation feedback loop where a down-ranked source is never sampled again).
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

from sqlalchemy import Column, String, Integer, Float, DateTime, UniqueConstraint
from sqlalchemy.orm import Session
from sqlalchemy.sql import func

from apps.api.database import Base


# ── Tunables (weights/alpha/min-samples) ──────────────────────────────────
# Component weights — hit-rate dominates (did the source return anything at all),
# with dedup-survival and validation-pass as quality refiners. Must sum to 1.0.
W_HIT = 0.5
W_DEDUP = 0.25
W_VAL = 0.25

# EWMA smoothing: r_new = alpha*r_batch + (1-alpha)*r_old. Lower = more memory.
EWMA_ALPHA = 0.3

# Runs required before reliability applies. Below this → None → no adjustment.
MIN_SAMPLES = 5


def normalize_source(raw: Optional[str]) -> str:
    """Canonicalize a ``lead.source`` channel string into a ledger key.

    The heterogeneous in-flight source strings (see ``job_runner``) are:
    ``web_search`` / ``directory`` / ``linkedin`` / ``job_board`` /
    ``review_site`` / ``google_maps`` (pass through, lower-cased), and
    ``registry:<name>`` for the 91-source registry (prefix stripped →
    ``<name>``). This is the single source of truth for that mapping so the
    tally side and the read side agree on keys.
    """
    s = (raw or "").strip().lower()
    if not s:
        return "unknown"
    if s.startswith("registry:"):
        s = s[len("registry:"):].strip()
    return s or "unknown"


@dataclass
class SourceRunCounters:
    """Per-source outcome tally for a single job run, accumulated in-flight."""

    emitted: int = 0          # leads the source produced after gather
    survived_dedup: int = 0   # how many survived dedup (in-batch + cross-job)
    validated: int = 0        # how many passed post-enrichment validation

    def batch_reliability(self) -> float:
        """The reliability of THIS run's outcome, in [0,1] — the EWMA input.

        hit_rate is binary per run (did the source return anything), while
        dedup-survival and validation-pass are ratios over what it emitted.
        """
        hit_rate = 1.0 if self.emitted > 0 else 0.0
        if self.emitted > 0:
            dedup_survival = self.survived_dedup / self.emitted
            validation_pass = self.validated / self.emitted
        else:
            dedup_survival = 0.0
            validation_pass = 0.0
        return W_HIT * hit_rate + W_DEDUP * dedup_survival + W_VAL * validation_pass


class SourceStat(Base):
    """Global per-(source, region) reliability ledger. Mirrors ProviderStat."""

    __tablename__ = "source_stats"
    __table_args__ = (
        UniqueConstraint("source", "region", name="uq_source_region"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    source = Column(String(100), nullable=False, index=True)
    region = Column(String(50), nullable=False, index=True)

    # Cumulative counters (transparency/debug; reliability ranks off the EWMA).
    runs = Column(Integer, default=0)                 # times this source ran
    runs_with_output = Column(Integer, default=0)     # runs that emitted >=1 lead
    emitted = Column(Integer, default=0)              # total leads emitted
    survived_dedup = Column(Integer, default=0)       # total survived dedup
    validated = Column(Integer, default=0)            # total passed validation

    reliability = Column(Float, nullable=True)        # rolling EWMA in [0,1]; None until first run
    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {
            "source": self.source,
            "region": self.region,
            "runs": self.runs or 0,
            "runs_with_output": self.runs_with_output or 0,
            "emitted": self.emitted or 0,
            "survived_dedup": self.survived_dedup or 0,
            "validated": self.validated or 0,
            "reliability": round(self.reliability, 4) if self.reliability is not None else None,
            "applies": (self.runs or 0) >= MIN_SAMPLES,
            "updated_at": self.updated_at.isoformat() if self.updated_at else None,
        }


def _stat(db: Session, source: str, region: str) -> Optional[SourceStat]:
    return (
        db.query(SourceStat)
        .filter(SourceStat.source == source, SourceStat.region == region)
        .first()
    )


def reliability(db: Session, source: str, region: str) -> Optional[float]:
    """Rolling EWMA reliability for a source+region, or ``None`` when there is
    not enough signal yet (``runs < MIN_SAMPLES``) → caller applies NO
    adjustment. ``source`` is normalized here so callers can pass raw channels.
    """
    st = _stat(db, normalize_source(source), region)
    if not st or (st.runs or 0) < MIN_SAMPLES or st.reliability is None:
        return None
    return st.reliability


def reliability_map(db: Session, region: str) -> dict[str, float]:
    """Bulk fetch all applicable reliabilities for a region (one query per job).

    Returns only sources at/above ``MIN_SAMPLES`` (others would be ``None`` =
    no adjustment anyway), keyed by normalized source.
    """
    rows = (
        db.query(SourceStat)
        .filter(SourceStat.region == region)
        .all()
    )
    out: dict[str, float] = {}
    for st in rows:
        if (st.runs or 0) >= MIN_SAMPLES and st.reliability is not None:
            out[st.source] = st.reliability
    return out


def record_run(
    db: Session,
    region: str,
    per_source_counters: dict[str, SourceRunCounters],
) -> None:
    """Atomic upsert of one job's per-source outcomes into the ledger.

    For each source: bump cumulative counters and fold this run's batch
    reliability into the rolling EWMA. Concurrency-safe in the same shape as
    ``planner.record_attempt`` — read-or-create the row, then apply additive
    increments (the unique constraint guards against duplicate rows).

    ``per_source_counters`` is keyed by the *raw* channel string; keys are
    normalized here so the tally side need not know the ledger key format.
    Sources with no emitted leads still count as a run (hit_rate contribution 0).
    """
    # Merge by normalized key first (e.g. two registry entries can't collide,
    # but this keeps the contract robust if a caller mixes raw forms).
    merged: dict[str, SourceRunCounters] = {}
    for raw, c in per_source_counters.items():
        key = normalize_source(raw)
        agg = merged.setdefault(key, SourceRunCounters())
        agg.emitted += c.emitted
        agg.survived_dedup += c.survived_dedup
        agg.validated += c.validated

    for source, c in merged.items():
        st = _stat(db, source, region)
        if not st:
            st = SourceStat(source=source, region=region)
            db.add(st)
            db.flush()
        st.runs = (st.runs or 0) + 1
        if c.emitted > 0:
            st.runs_with_output = (st.runs_with_output or 0) + 1
        st.emitted = (st.emitted or 0) + c.emitted
        st.survived_dedup = (st.survived_dedup or 0) + c.survived_dedup
        st.validated = (st.validated or 0) + c.validated

        r_batch = c.batch_reliability()
        if st.reliability is None:
            st.reliability = r_batch  # first observation seeds the EWMA
        else:
            st.reliability = EWMA_ALPHA * r_batch + (1.0 - EWMA_ALPHA) * st.reliability
