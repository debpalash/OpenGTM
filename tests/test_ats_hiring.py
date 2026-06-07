"""
ATS hiring-signal provider (#9): slug generation + title→signal parsing.
Network calls are not exercised here (kept offline/deterministic).
"""
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.providers.ats_hiring import (
    _slug_candidates, _titles_to_signals, AtsHiringProvider,
)


def test_slug_candidates_from_domain_and_name():
    lead = Lead(company="Acme Robotics, Inc.", website="https://www.acmerobotics.com/careers")
    slugs = _slug_candidates(lead)
    assert "acmerobotics" in slugs           # domain root
    assert "acmeroboticsinc" in slugs        # compacted name
    assert any("-" in s for s in slugs)      # dashed variant

def test_slug_candidates_empty():
    assert _slug_candidates(Lead()) == []

def test_titles_to_signals_departments_and_tech():
    titles = [
        "Senior Backend Engineer (Python)",
        "Account Executive",
        "Staff Machine Learning Engineer",
        "Product Manager",
    ]
    s = _titles_to_signals(titles)
    assert s["open_roles"] == 4
    assert s["is_hiring"] is True
    assert s["eng_hiring"] is True
    assert s["departments"]["engineering"] >= 2
    assert s["departments"]["sales"] == 1
    assert "python" in s["tech_hiring"]
    assert "machine learning" not in s["tech_hiring"]  # not in tech keyword list
    assert s["sample_titles"][0].startswith("Senior Backend")

def test_titles_to_signals_empty():
    s = _titles_to_signals([])
    assert s["open_roles"] == 0 and s["is_hiring"] is False

def test_provider_metadata():
    p = AtsHiringProvider()
    assert p.name == "ats_hiring"
    assert "hiring_signals" in p.capabilities
    assert p.cost_per_lookup == 0.0
