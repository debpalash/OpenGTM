"""
Phase: vendor/cost catalog — single source of truth for provider economics +
run-cost estimation. See docs/clay-alternatives-ingestion-catalog.md (#5).
"""
from apps.api.services.workbook import vendor_catalog as vc
from apps.api.services.workbook import planner


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
