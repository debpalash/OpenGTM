"""Source-reliability scoring tests (sourcing P2).

Covers (mirrors test_provider_accuracy_eval.py style — offline, deterministic):
  - normalize_source mapping across the heterogeneous channel strings
  - reliability math: component weights, batch reliability, EWMA, MIN_SAMPLES prior
  - _apply_source_reliability: bounded ±swing, clamp, r=0.5 no-op, None identity,
    never overturns ICP-fit dominance
  - flag-off regression: score_lead(lead) byte-identical with reliability=None
  - record_run atomic upsert / idempotency (concurrency-safe shape)
  - end-to-end: counters → record_run → reliability_map → directional nudge ≤ swing
"""

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen import scoring
from apps.api.services.leadgen.scoring import (
    score_lead, get_tier, _apply_source_reliability, RELIABILITY_SWING,
)
from apps.api.services.leadgen import source_stats
from apps.api.services.leadgen.source_stats import (
    SourceStat, SourceRunCounters, normalize_source,
    reliability, reliability_map, record_run,
    MIN_SAMPLES, EWMA_ALPHA, W_HIT, W_DEDUP, W_VAL,
)


# ── fixtures ──────────────────────────────────────────────────────

@pytest.fixture
def db():
    """Isolated in-memory SQLite session with only the source_stats table."""
    engine = create_engine("sqlite:///:memory:")
    SourceStat.__table__.create(bind=engine)
    Session = sessionmaker(bind=engine)
    s = Session()
    yield s
    s.close()


# ── normalize_source ──────────────────────────────────────────────

def test_normalize_source_registry_prefix_stripped():
    assert normalize_source("registry:indiamart") == "indiamart"
    assert normalize_source("registry:JustDial") == "justdial"


def test_normalize_source_channels_passthrough():
    for ch in ("web_search", "directory", "linkedin", "job_board",
               "review_site", "google_maps"):
        assert normalize_source(ch) == ch


def test_normalize_source_case_and_whitespace():
    assert normalize_source("  Web_Search ") == "web_search"
    assert normalize_source("registry:  IndiaMart ") == "indiamart"


def test_normalize_source_empty_is_unknown():
    assert normalize_source("") == "unknown"
    assert normalize_source(None) == "unknown"
    assert normalize_source("registry:") == "unknown"


# ── batch reliability math ────────────────────────────────────────

def test_batch_reliability_weighted_combine():
    c = SourceRunCounters(emitted=10, survived_dedup=8, validated=6)
    # hit_rate=1.0, dedup=0.8, val=0.6
    expected = W_HIT * 1.0 + W_DEDUP * 0.8 + W_VAL * 0.6
    assert c.batch_reliability() == pytest.approx(expected)
    assert c.batch_reliability() == pytest.approx(0.85)


def test_batch_reliability_no_output_is_zero():
    c = SourceRunCounters(emitted=0)
    assert c.batch_reliability() == 0.0


def test_weights_sum_to_one():
    assert W_HIT + W_DEDUP + W_VAL == pytest.approx(1.0)


# ── EWMA + MIN_SAMPLES prior ──────────────────────────────────────

def _run(db, source, region, emitted, survived, validated):
    record_run(db, region, {source: SourceRunCounters(emitted, survived, validated)})


def test_min_samples_prior_returns_none(db):
    # MIN_SAMPLES-1 runs → still None (no adjustment, identical to today).
    for _ in range(MIN_SAMPLES - 1):
        _run(db, "linkedin", "india", 5, 5, 5)
    assert reliability(db, "linkedin", "india") is None
    # one more run crosses the threshold → applies
    _run(db, "linkedin", "india", 5, 5, 5)
    assert reliability(db, "linkedin", "india") is not None


def test_ewma_update_math(db):
    # First run seeds the EWMA, second folds in via alpha.
    _run(db, "web_search", "us", 10, 8, 6)   # r1 = 0.85
    st = db.query(SourceStat).filter_by(source="web_search", region="us").one()
    assert st.reliability == pytest.approx(0.85)

    _run(db, "web_search", "us", 0, 0, 0)    # r2 = 0.0
    st = db.query(SourceStat).filter_by(source="web_search", region="us").one()
    expected = EWMA_ALPHA * 0.0 + (1 - EWMA_ALPHA) * 0.85
    assert st.reliability == pytest.approx(expected)


def test_runs_with_output_counter(db):
    _run(db, "naukri", "india", 3, 2, 1)   # output
    _run(db, "naukri", "india", 0, 0, 0)   # no output
    st = db.query(SourceStat).filter_by(source="naukri", region="india").one()
    assert st.runs == 2
    assert st.runs_with_output == 1
    assert st.emitted == 3


# ── _apply_source_reliability bounds ──────────────────────────────

def test_apply_none_is_identity():
    for base in (0, 1, 50, 73, 100):
        assert _apply_source_reliability(base, None) == base


def test_apply_r_half_is_noop():
    for base in (0, 25, 60, 100):
        assert _apply_source_reliability(base, 0.5) == base


def test_apply_bounded_swing():
    base = 50
    # max swing is +/- RELIABILITY_SWING/2
    hi = _apply_source_reliability(base, 1.0)
    lo = _apply_source_reliability(base, 0.0)
    assert hi - base == pytest.approx(RELIABILITY_SWING / 2)
    assert base - lo == pytest.approx(RELIABILITY_SWING / 2)
    assert hi - lo <= RELIABILITY_SWING


def test_apply_direction():
    assert _apply_source_reliability(50, 0.9) > 50   # reliable → up
    assert _apply_source_reliability(50, 0.1) < 50   # noisy → down


def test_apply_clamped_to_0_100():
    assert _apply_source_reliability(100, 1.0) == 100   # cannot exceed 100
    assert _apply_source_reliability(0, 0.0) == 0       # cannot go below 0
    assert _apply_source_reliability(2, 0.0) >= 0


def test_swing_never_overturns_icp_dominance():
    # A strong-ICP lead from a noisy source must still beat a weak-ICP lead
    # from a reliable source — reliability only breaks ties / nudges tiers.
    strong, weak = 80, 60
    strong_noisy = _apply_source_reliability(strong, 0.0)   # 80 - 4 = 76
    weak_reliable = _apply_source_reliability(weak, 1.0)    # 60 + 4 = 64
    assert strong_noisy > weak_reliable


# ── flag-off / score_lead regression ──────────────────────────────

def _fixture_leads():
    return [
        Lead(company="A", website="a.com", email="a@a.com", phone="1",
             company_size="201-500", specialization="IT Staffing", city="Mumbai",
             contact_person="X", source="web_search"),
        Lead(company="B", source="registry:indiamart"),
        Lead(company="C", website="c.com", email="c@c.com", linkedin_url="li",
             twitter_url="tw", source="linkedin"),
        Lead(company="D", founded_year="2001", revenue_range="$1M",
             glassdoor_rating="4.2", source="directory"),
    ]


def test_score_lead_default_unchanged_vs_explicit_none():
    for lead in _fixture_leads():
        assert score_lead(lead) == score_lead(lead, source_reliability=None)


def test_score_lead_reliability_is_bounded_nudge():
    for lead in _fixture_leads():
        base = score_lead(lead)
        up = score_lead(lead, source_reliability=1.0)
        down = score_lead(lead, source_reliability=0.0)
        assert abs(up - base) <= RELIABILITY_SWING / 2 + 0.5
        assert abs(down - base) <= RELIABILITY_SWING / 2 + 0.5
        assert 0 <= up <= 100 and 0 <= down <= 100


# ── record_run atomic upsert / idempotency ────────────────────────

def test_record_run_upserts_single_row_per_source_region(db):
    for _ in range(3):
        _run(db, "linkedin", "india", 4, 3, 2)
    rows = db.query(SourceStat).filter_by(source="linkedin", region="india").all()
    assert len(rows) == 1            # one row, not three
    assert rows[0].runs == 3
    assert rows[0].emitted == 12


def test_record_run_region_partitioned(db):
    _run(db, "naukri", "india", 5, 5, 5)
    _run(db, "naukri", "us", 5, 0, 0)
    assert (db.query(SourceStat).filter_by(source="naukri").count()) == 2


def test_record_run_normalizes_keys(db):
    # raw channel forms collapse onto one normalized key
    record_run(db, "india", {
        "registry:indiamart": SourceRunCounters(2, 2, 2),
        "  registry:IndiaMart ": SourceRunCounters(3, 3, 3),
    })
    rows = db.query(SourceStat).filter_by(region="india").all()
    assert len(rows) == 1
    assert rows[0].source == "indiamart"
    assert rows[0].emitted == 5
    assert rows[0].runs == 1   # merged into a single run


# ── reliability_map + end-to-end directional nudge ────────────────

def test_reliability_map_excludes_below_min_samples(db):
    for _ in range(MIN_SAMPLES):
        _run(db, "linkedin", "india", 5, 5, 5)   # high reliability, applies
    for _ in range(MIN_SAMPLES - 1):
        _run(db, "naukri", "india", 0, 0, 0)     # below threshold → excluded
    rmap = reliability_map(db, "india")
    assert "linkedin" in rmap
    assert "naukri" not in rmap


def test_end_to_end_low_reliability_scores_lower(db):
    # Seed a reliable source (good) and a noisy source (bad).
    for _ in range(MIN_SAMPLES):
        _run(db, "good", "india", 10, 10, 10)    # r ≈ 1.0
        _run(db, "bad", "india", 10, 0, 0)       # only hit_rate → r ≈ 0.5*1 = 0.5
    rmap = reliability_map(db, "india")
    assert rmap["good"] > rmap["bad"]

    # Two leads, identical ICP fit, different sources.
    base_lead_kwargs = dict(website="x.com", email="x@x.com", phone="1",
                            company_size="201-500", city="Mumbai")
    good_lead = Lead(company="G", source="good", **base_lead_kwargs)
    bad_lead = Lead(company="B", source="bad", **base_lead_kwargs)

    base = score_lead(good_lead)   # identical ICP base for both
    good_score = score_lead(good_lead, source_reliability=rmap["good"])
    bad_score = score_lead(bad_lead, source_reliability=rmap["bad"])

    assert good_score >= bad_score
    assert good_score - bad_score <= RELIABILITY_SWING   # bounded by swing
    # direction: reliable source nudged up relative to base, noisy not above it
    assert good_score >= base
