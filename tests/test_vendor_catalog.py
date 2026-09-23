"""
Phase: vendor/cost catalog — single source of truth for provider economics +
run-cost estimation. See docs/research/clay-alternatives-ingestion-catalog.md (#5).
"""
from apps.api.services.workbook import vendor_catalog as vc
from apps.api.services.workbook import planner
import pytest
from types import SimpleNamespace


def test_base_cost_known_and_unknown():
    assert vc.base_cost("hunter_io") == 0.04
    assert vc.base_cost("apollo_io") == 0.03
    assert vc.base_cost("website_scraper") == 0.0   # free OSS
    assert vc.base_cost("totally_unknown") == 0.0

def test_is_paid():
    assert vc.is_paid("hunter_io") is True
    assert vc.is_paid("website_scraper") is False

def test_calculate_cost_scaling():
    # single enrich
    assert vc.calculate_cost("hunter_io", "enrich") == 0.04
    # search scales per page of 25
    assert vc.calculate_cost("hunter_io", "search", {"limit": 50}) == 0.04 * 2
    assert vc.calculate_cost("hunter_io", "search", {"limit": 1}) == 0.04 * 1
    # bulk scales per record
    assert vc.calculate_cost("apollo_io", "bulk", {"count": 10}) == 0.03 * 10
    # free provider stays 0 regardless
    assert vc.calculate_cost("website_scraper", "bulk", {"count": 99}) == 0.0

def test_estimate_run_cost():
    est = vc.estimate_run_cost(
        100,
        {"email": ["website_scraper", "hunter_io", "apollo_io"],   # 2 paid
         "phone": ["numverify"]},                                   # 1 paid
    )
    assert est["rows"] == 100
    # worst = (0.04 + 0.03)*100 + 0.005*100
    assert abs(est["worst_usd"] - (0.07 * 100 + 0.005 * 100)) < 1e-6
    # best = cheapest paid per col: min(0.04,0.03)*100 + 0.005*100
    assert abs(est["best_usd"] - (0.03 * 100 + 0.005 * 100)) < 1e-6
    cols = {b["column"] for b in est["breakdown"]}
    assert cols == {"email", "phone"}

def test_planner_delegates_to_catalog():
    # planner is now a thin alias over the catalog
    assert planner.provider_cost("hunter_io") == vc.base_cost("hunter_io")
    assert planner.is_paid("hunter_io") is True
    assert planner.is_paid("website_scraper") is False
    # back-compat alias still populated
    assert "hunter_io" in planner.PROVIDER_COST


def test_estimate_distinguishes_unknown_from_registered_free_provider(monkeypatch):
    monkeypatch.setattr(vc, "_provider_declared_cost", lambda name: 0.0 if name == "fixture_free" else None)
    result = vc.estimate_run_cost(10, {"email": ["fixture_free", "unknown_lookup", "hunter_io"]})
    assert result["unknown_providers"] == ["unknown_lookup"]
    assert result["catalog_complete"] is False
    assert result["breakdown"][0]["unknown_providers"] == ["unknown_lookup"]
    assert result["worst_usd"] == 0.4
    assert "incomplete" in result["note"]
    free = vc.estimate_run_cost(10, {"email": ["fixture_free"]})
    assert free["catalog_complete"] is True
    assert free["unknown_providers"] == []
    assert free["worst_usd"] == 0


@pytest.mark.parametrize("price", [None, True, False, "", "invalid", -0.01, float("nan"), float("inf"), float("-inf")])
def test_invalid_declared_price_is_unknown_not_free(monkeypatch, price):
    from apps.api.services.workbook import providers
    monkeypatch.setattr(providers, "get_provider", lambda name: SimpleNamespace(cost_per_lookup=price))
    assert vc._provider_declared_cost("fixture") is None
    estimate = vc.estimate_run_cost(2, {"email": ["fixture"]})
    assert estimate["catalog_complete"] is False
    assert estimate["unknown_providers"] == ["fixture"]
    assert estimate["worst_usd"] == 0  # Excluded, explicitly not a free quote.
    assert vc.base_cost("hunter_io") == 0.04  # Known fallback remains usable.


@pytest.mark.parametrize("price,expected", [(0, 0.0), ("0", 0.0), (0.025, 0.025), ("0.025", 0.025)])
def test_valid_declared_price_remains_supported(monkeypatch, price, expected):
    from apps.api.services.workbook import providers
    monkeypatch.setattr(providers, "get_provider", lambda name: SimpleNamespace(cost_per_lookup=price))
    assert vc._provider_declared_cost("fixture") == expected
    estimate = vc.estimate_run_cost(2, {"email": ["fixture"]})
    assert estimate["catalog_complete"] is True
    assert estimate["worst_usd"] == 2 * expected
