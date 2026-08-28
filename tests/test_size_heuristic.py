"""Company-size heuristic: normalize_band, infer_company_size, provider, scoring,
store filter, and feature-flag gating.

Covers docs/specs/sourcing-company-size-signals-spec.md acceptance criteria 1-7.
Runs on the default SQLite path (no live PG needed).
"""

import asyncio
import json
import os
import tempfile

import pytest

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.size_heuristic import (
    SIZE_BANDS,
    normalize_band,
    band_from_count,
    infer_company_size,
    revenue_band_from_funding,
)


# ── normalize_band (the latent-bug fix) ─────────────────────────────────────

@pytest.mark.parametrize("value,expected", [
    ("1-50", "1-50"),
    ("51-200", "51-200"),
    ("201-500", "201-500"),
    ("500+", "500+"),
    (120, "51-200"),          # raw int (LeadMagic/JSON-LD path) — previously scored 0
    ("120", "51-200"),
    ("10-50", "1-50"),        # ad-hoc range -> lower bound
    ("5,000", "500+"),        # thousands separator
    ("5000", "500+"),
    ("501-1000", "500+"),     # range above 500
    ("1000+", "500+"),        # open-ended
    ("200", "51-200"),
    ("201", "201-500"),
    ("50", "1-50"),
    ("", ""),
    (None, ""),
    ("unknown", ""),
    ("N/A", ""),
    (0, ""),
    (-5, ""),
])
def test_normalize_band_table(value, expected):
    assert normalize_band(value) == expected


def test_band_from_count_boundaries():
    assert band_from_count(50) == "1-50"
    assert band_from_count(51) == "51-200"
    assert band_from_count(200) == "51-200"
    assert band_from_count(201) == "201-500"
    assert band_from_count(500) == "201-500"
    assert band_from_count(501) == "500+"
    assert band_from_count(0) == ""


# ── infer_company_size ──────────────────────────────────────────────────────

def test_exact_count_wins():
    est = infer_company_size(Lead(company="A", employee_count_exact=300))
    assert est.band == "201-500"
    assert est.basis == "exact"
    assert est.confidence == 0.95
    assert not est.is_estimated


def test_hiring_volume_only():
    hs = json.dumps({"total_jobs": 12, "growth_signal": "hypergrowth"})
    est = infer_company_size(Lead(company="A", hiring_signals=hs))
    assert est.band in SIZE_BANDS
    assert est.band == "201-500"
    assert est.basis.startswith("estimated:")
    assert "jobs" in est.basis
    assert 0.3 <= est.confidence <= 0.6


def test_hiring_single_job_small():
    hs = json.dumps({"total_jobs": 1, "growth_signal": "hiring"})
    est = infer_company_size(Lead(company="A", hiring_signals=hs))
    assert est.band == "1-50"
    assert est.is_estimated


def test_funding_only():
    est = infer_company_size(Lead(company="A", funding_stage="Series C"))
    assert est.band == "500+"
    assert "funding" in est.basis


def test_funding_amount_attr():
    lead = Lead(company="A")
    # funding_amount isn't a dataclass field; the heuristic reads it defensively.
    setattr(lead, "funding_amount", 25_000_000)
    est = infer_company_size(lead)
    assert est.band == "201-500"
    assert "funding" in est.basis


def test_registry_only():
    dms = json.dumps([{"name": f"P{i}", "title": "Director"} for i in range(9)])
    est = infer_company_size(Lead(company="A", decision_makers=dms))
    assert est.band in SIZE_BANDS
    assert "registry" in est.basis


def test_combined_signals_raise_confidence():
    hs = json.dumps({"total_jobs": 6, "growth_signal": "rapid_growth"})
    dms = json.dumps([{"name": f"P{i}"} for i in range(5)])
    lead = Lead(company="A", hiring_signals=hs, funding_stage="Series B",
                decision_makers=dms, technologies="a,b,c,d,e,f,g,h,i,j,k")
    est = infer_company_size(lead)
    assert est.band in SIZE_BANDS
    # multiple distinct signals → confidence above the single-signal floor
    assert est.confidence > 0.4
    assert est.confidence <= 0.6


def test_empty_lead_returns_nothing():
    est = infer_company_size(Lead(company="A"))
    assert est.band == ""
    assert est.confidence == 0.0
    assert est.basis == ""


def test_never_raises_on_garbage_json():
    lead = Lead(company="A", hiring_signals="{not valid json",
                decision_makers="also not json")
    est = infer_company_size(lead)  # must not raise
    assert est.band == ""


# ── revenue helper (secondary; not scored) ─────────────────────────────────

def test_revenue_band_from_funding():
    assert revenue_band_from_funding(500_000) == "<$1M"
    assert revenue_band_from_funding(5_000_000) == "$1M-$10M"
    assert revenue_band_from_funding(25_000_000) == "$10M-$50M"
    assert revenue_band_from_funding(100_000_000) == "$50M+"
    assert revenue_band_from_funding("") == ""
    assert revenue_band_from_funding(0) == ""


# ── provider ────────────────────────────────────────────────────────────────

def test_provider_metadata_and_zero_network():
    from apps.api.services.leadgen.enrichment.providers.company_size_heuristic import (
        CompanySizeHeuristicProvider,
    )
    p = CompanySizeHeuristicProvider()
    assert p.requires_api_key is False
    assert p.cost_per_lookup == 0.0
    assert p.capabilities == ["company_size"]
    assert p.can_provide("company_size")


def test_provider_fills_when_signals_present():
    from apps.api.services.leadgen.enrichment.providers.company_size_heuristic import (
        CompanySizeHeuristicProvider,
    )
    hs = json.dumps({"total_jobs": 12, "growth_signal": "hypergrowth"})
    res = asyncio.run(CompanySizeHeuristicProvider().enrich(Lead(company="A", hiring_signals=hs)))
    assert res.success
    assert res.fields["company_size"] == "201-500"
    assert res.fields["company_size_basis"].startswith("estimated:")


def test_provider_no_signals_fails_gracefully():
    from apps.api.services.leadgen.enrichment.providers.company_size_heuristic import (
        CompanySizeHeuristicProvider,
    )
    res = asyncio.run(CompanySizeHeuristicProvider().enrich(Lead(company="A")))
    assert not res.success
    assert res.error == "insufficient_signals"


def test_waterfall_apply_results_never_overwrites_known_size():
    """apply_results fills empties only — a heuristic result can't clobber a real one."""
    from apps.api.services.leadgen.enrichment.provider import WaterfallEnricher
    wf = WaterfallEnricher()
    lead = Lead(company="A", company_size="51-200")  # already known
    wf.apply_results(lead, {"company_size": "500+", "company_size_basis": "estimated:jobs"}, [])
    assert lead.company_size == "51-200"  # preserved


def test_waterfall_apply_results_fills_empty_size():
    from apps.api.services.leadgen.enrichment.provider import WaterfallEnricher
    wf = WaterfallEnricher()
    lead = Lead(company="A")  # empty
    wf.apply_results(lead, {"company_size": "500+", "company_size_basis": "estimated:jobs"}, [])
    assert lead.company_size == "500+"
    assert lead.company_size_basis == "estimated:jobs"


# ── scoring regression (the bug fix) ───────────────────────────────────────

def test_scoring_credits_raw_int_size():
    from apps.api.services.leadgen.scoring import score_lead
    # company_size as a raw int string ("120") used to score 0; now credited.
    base = score_lead(Lead(company="A"))
    with_size = score_lead(Lead(company="A", company_size="120"))
    assert with_size - base == 15


def test_scoring_existing_band_unchanged():
    from apps.api.services.leadgen.scoring import score_lead
    from apps.api.services.leadgen.config import SCORING_WEIGHTS
    s = score_lead(Lead(company="A", company_size="201-500"))
    base = score_lead(Lead(company="A"))
    assert s - base == SCORING_WEIGHTS["company_size_large"]


def test_scoring_small_band_no_credit():
    from apps.api.services.leadgen.scoring import score_lead
    base = score_lead(Lead(company="A"))
    small = score_lead(Lead(company="A", company_size="1-50"))
    assert small == base  # 1-50 earns no large-company credit (unchanged)


# ── store filter (SQLite path; PG mirrors via RLS suite) ───────────────────

@pytest.fixture
def lead_db():
    from apps.api.services.leadgen.db import LeadDB
    fd, path = tempfile.mkstemp(suffix=".db")
    os.close(fd)
    d = LeadDB(path)
    yield d
    d.close()
    os.unlink(path)


def test_store_company_size_filter(lead_db):
    lead_db.upsert_lead(Lead(workspace_id="w1", company="Big", city="NYC", company_size="500+"))
    lead_db.upsert_lead(Lead(workspace_id="w1", company="Small", city="SF", company_size="1-50"))
    big = lead_db.get_leads(company_size="500+")
    assert {l.company for l in big} == {"Big"}
    small = lead_db.get_leads(company_size="1-50")
    assert {l.company for l in small} == {"Small"}


def test_store_basis_persists(lead_db):
    lead_db.upsert_lead(Lead(workspace_id="w1", company="A", city="NYC",
                             company_size="500+", company_size_basis="estimated:jobs+funding"))
    got = lead_db.get_leads(company_size="500+")
    assert got and got[0].company_size_basis == "estimated:jobs+funding"


# ── feature-flag gating ────────────────────────────────────────────────────

def test_provider_not_registered_when_flag_off():
    """With the flag off (default), the provider is absent from the registry."""
    from apps.api.core.config import settings
    from apps.api.services.workbook import providers as prov_mod
    # Default settings → flag off in the test env.
    if not getattr(settings, "COMPANY_SIZE_HEURISTIC_ENABLED", False):
        assert prov_mod.get_provider("company_size_heuristic") is None


def test_chain_unchanged_when_flag_off():
    from apps.api.core.config import settings
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
    if not getattr(settings, "COMPANY_SIZE_HEURISTIC_ENABLED", False):
        assert "company_size_heuristic" not in DEFAULT_WATERFALLS["company_size"]
