"""Persist eval scores onto the ProviderStat ledger so the planner can read them.

Keeps the scorer itself DB-free (offline/deterministic, unit-testable without a
session). This thin layer is the one place the accuracy prior is written.
"""

from __future__ import annotations

from datetime import date, datetime, timezone
from typing import Dict, List, Optional

from sqlalchemy.orm import Session

from apps.api.services.workbook.planner_models import ProviderStat
from apps.api.services.leadgen.enrichment.eval.scorer import (
    ProviderScore,
    load_golden,
    load_recorded_outputs,
    score_outputs,
    rank_providers,
)


def persist_scores(db: Session, scores: Dict[str, ProviderScore]) -> int:
    """Write each provider's correctness score onto its ProviderStat row(s).

    Accuracy is learned per provider (golden set spans company fields), so we set
    the same score on every (provider, field) ledger row for that provider, and
    also create a provider-level row keyed on field='*' so the prior is available
    even for fields the provider hasn't been called on yet.
    Returns the number of provider rows touched.
    """
    now = datetime.now(timezone.utc)
    touched = 0
    for provider, ps in scores.items():
        rows = db.query(ProviderStat).filter(ProviderStat.provider == provider).all()
        has_wildcard = any(r.field == "*" for r in rows)
        if not has_wildcard:
            wildcard = ProviderStat(provider=provider, field="*")
            db.add(wildcard)
            db.flush()
            rows.append(wildcard)
        for r in rows:
            r.accuracy_score = ps.score
            r.accuracy_samples = ps.asserted
            r.accuracy_updated_at = now
            touched += 1
    db.flush()
    return touched


def run_and_persist(
    db: Session,
    golden_path: Optional[str] = None,
    recorded_path: Optional[str] = None,
    today: Optional[date] = None,
) -> List[ProviderScore]:
    """Load seeded data (or given paths), score offline, persist, return ranking."""
    golden = load_golden(golden_path) if golden_path else load_golden()
    outputs = load_recorded_outputs(recorded_path) if recorded_path else load_recorded_outputs()
    scores = score_outputs(golden, outputs, today=today)
    persist_scores(db, scores)
    return rank_providers(scores)
