"""Provider accuracy/freshness eval harness — offline, deterministic.

Covers:
  - the scorer ranks an accurate provider above a confidently-wrong one
  - freshness penalises stale-but-correct values
  - the planner score incorporates the persisted correctness prior
  - planner ordering is unchanged / graceful when NO accuracy data exists
"""

from concurrent.futures import ThreadPoolExecutor
from datetime import date

import pytest
from sqlalchemy import create_engine, event
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.services.workbook.planner_models import ProviderStat
from apps.api.services.workbook import planner
from apps.api.services.leadgen.enrichment.eval import scorer
from apps.api.services.leadgen.enrichment.eval.persist import persist_scores


# ── fixtures ──────────────────────────────────────────────────────

@pytest.fixture
def db():
    """Isolated in-memory SQLite session with only the provider_stats table."""
    engine = create_engine("sqlite:///:memory:")
    ProviderStat.__table__.create(bind=engine)
    Session = sessionmaker(bind=engine)
    s = Session()
    yield s
    s.close()


# A tiny golden set: one field with a known truth.
_GOLDEN = [
    {"id": "acme", "input": {"company": "Acme"},
     "expected": {"founding_year": "2002", "website": "acme.com"},
     "expected_asof": "2025-01-01"},
]


# ── scorer: accurate beats confidently-wrong ──────────────────────

def test_scorer_ranks_accurate_above_inaccurate():
    outputs = {
        "acme": {
            # accurate provider: both values correct
            "accurate": {"fields": {"founding_year": "2002", "website": "https://www.acme.com"}},
            # confidently-wrong provider: answers everything, all wrong
            "liar": {"fields": {"founding_year": "1999", "website": "wrong.com"}},
        }
    }
    scores = scorer.score_outputs(_GOLDEN, outputs, today=date(2025, 1, 1))
    ranking = scorer.rank_providers(scores)

    assert ranking[0].provider == "accurate"
    assert ranking[-1].provider == "liar"
    assert scores["accurate"].accuracy == 1.0
    assert scores["liar"].accuracy == 0.0
    # the liar has full COVERAGE (it answered everything) but bottom SCORE — the
    # exact failure mode the hit-rate-only planner couldn't see.
    assert scores["liar"].coverage == 1.0
    assert scores["liar"].score == 0.0
    assert scores["accurate"].score > scores["liar"].score


def test_freshness_penalises_stale_correct_values():
    outputs = {
        "acme": {
            "fresh": {"fields": {"founding_year": "2002"}, "asof": "2025-01-01"},
            "stale": {"fields": {"founding_year": "2002"}, "asof": "2015-01-01"},
        }
    }
    scores = scorer.score_outputs(_GOLDEN, outputs, today=date(2025, 1, 1))
    # both perfectly accurate, but the stale one scores lower via freshness
    assert scores["fresh"].accuracy == scores["stale"].accuracy == 1.0
    assert scores["fresh"].score > scores["stale"].score


def test_seeded_golden_set_ranks_wikidata_above_company_intel():
    """End-to-end over the committed seeded datasets (still offline)."""
    ranking = scorer.run_eval(today=date(2025, 6, 1))
    by_name = {s.provider: s for s in ranking}
    assert by_name["wikidata"].accuracy == 1.0
    assert by_name["company_intel"].accuracy == 0.0
    # wikidata (accurate) must outrank company_intel (confident-wrong)
    assert ranking.index(by_name["wikidata"]) < ranking.index(by_name["company_intel"])


# ── planner: prior feeds ordering ─────────────────────────────────

def _seed_stat(db, provider, field, attempts, hits):
    st = ProviderStat(provider=provider, field=field, attempts=attempts, hits=hits)
    db.add(st)
    db.flush()
    return st


def test_planner_score_incorporates_correctness_prior(db, monkeypatch):
    # two free providers, identical hit-rate → identical cost → identical base score
    monkeypatch.setattr(planner, "provider_cost", lambda name: 0.0)
    _seed_stat(db, "good", "founding_year", attempts=10, hits=10)
    _seed_stat(db, "liar", "founding_year", attempts=10, hits=10)

    base_good = planner._score(db, "good", "founding_year")
    base_liar = planner._score(db, "liar", "founding_year")
    assert base_good == pytest.approx(base_liar)  # hit-rate alone can't separate them

    # Now apply accuracy: good=1.0, liar=0.0
    persist_scores(db, {
        "good": scorer.ProviderScore(provider="good", asserted=4, correct=4, freshness_sum=4.0),
        "liar": scorer.ProviderScore(provider="liar", asserted=4, correct=0),
    })

    scored_good = planner._score(db, "good", "founding_year")
    scored_liar = planner._score(db, "liar", "founding_year")
    # the confidently-wrong provider is now knocked down below the accurate one
    assert scored_good > scored_liar
    # and ordering reflects it
    order = planner.order_chain(db, "founding_year", ["liar", "good"])
    assert order == ["good", "liar"]


def test_prior_applies_provider_level_to_uncalled_fields(db, monkeypatch):
    """Accuracy learned on one field bends ordering for a field the provider was
    never called on (provider-level prior via the field='*' wildcard row)."""
    monkeypatch.setattr(planner, "provider_cost", lambda name: 0.0)
    persist_scores(db, {
        "liar": scorer.ProviderScore(provider="liar", asserted=4, correct=0),
    })
    # 'liar' has no stat for 'email', but the wildcard accuracy still applies
    prior = planner.correctness_prior(db, "liar", "email")
    assert prior < 1.0


# ── graceful default: no accuracy data → ordering unchanged ────────

def test_ordering_unchanged_when_no_accuracy_data(db, monkeypatch):
    monkeypatch.setattr(planner, "provider_cost", lambda name: 0.0)
    # 'a' has a better hit-rate than 'b'; no accuracy data anywhere
    _seed_stat(db, "a", "email", attempts=10, hits=9)
    _seed_stat(db, "b", "email", attempts=10, hits=3)

    # prior is a no-op multiplier of exactly 1.0 for unevaluated providers
    assert planner.correctness_prior(db, "a", "email") == 1.0
    assert planner.correctness_prior(db, "b", "email") == 1.0

    order = planner.order_chain(db, "email", ["b", "a"])
    assert order == ["a", "b"]  # pure hit-rate ordering preserved


def test_record_attempt_is_atomic_under_sqlite_concurrency(tmp_path, monkeypatch):
    """Concurrent workbook cells must share one provider ledger row cleanly."""
    path = tmp_path / "provider-stats.db"
    engine = create_engine(
        f"sqlite:///{path}",
        connect_args={"check_same_thread": False, "timeout": 10},
    )

    @event.listens_for(engine, "connect")
    def _sqlite_wal(connection, _record):
        cursor = connection.cursor()
        cursor.execute("PRAGMA journal_mode=WAL")
        cursor.execute("PRAGMA busy_timeout=10000")
        cursor.close()

    ProviderStat.__table__.create(bind=engine)
    Session = sessionmaker(bind=engine)
    monkeypatch.setattr(planner, "provider_cost", lambda _name: 0.0)

    def _write_attempt(i):
        with Session() as session:
            planner.record_attempt(
                session,
                "shared_provider",
                "email",
                success=(i % 2 == 0),
                confidence=0.8,
                latency_ms=10.0,
            )
            session.commit()

    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(_write_attempt, range(80)))

    with Session() as session:
        stat = session.query(ProviderStat).one()
        assert stat.attempts == 80
        assert stat.hits == 40
        assert stat.total_confidence == pytest.approx(32.0)
        assert stat.total_latency_ms == pytest.approx(800.0)
