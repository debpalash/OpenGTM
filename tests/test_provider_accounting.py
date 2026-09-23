import pytest
from apps.api.services.workbook.provider_accounting import accounting_envelope, UncertainProviderAccounting


@pytest.mark.parametrize("success,cost,expected", [(True, 20000, 20000), (True, 0, 0), (False, 0, 0)])
def test_catalog_cost_is_never_presented_as_vendor_confirmation(success, cost, expected):
    result = {"provider": "fixture", "success": success, "fields": {}}
    envelope = accounting_envelope("fixture", result, cost)
    assert envelope["charged_microusd"] == expected
    assert envelope["accounting_basis"] == "catalog_estimate"
    result["fields"]["changed"] = True
    assert envelope["result"]["fields"] == {}


@pytest.mark.parametrize("amount", [0, 30000])
def test_explicit_billing_evidence_applies_even_to_unsuccessful_result(amount):
    result = {"provider": "fixture", "success": False,
              "billing_evidence": {"charged_microusd": amount, "reference": "vendor-request-123"}}
    envelope = accounting_envelope("fixture", result, 20000)
    assert envelope["charged_microusd"] == amount
    assert envelope["accounting_basis"] == ("vendor_confirmed" if amount else "confirmed_nonbillable")


@pytest.mark.parametrize("response", [None, {}, {"provider": "other", "success": True},
    {"provider": "fixture", "success": False},
    {"provider": "fixture", "success": "true"},
    {"provider": "fixture", "success": True, "billing_evidence": {"charged_microusd": 0}},
    {"provider": "fixture", "success": True, "billing_evidence": {"charged_microusd": True, "reference": "id"}},
])
def test_missing_or_malformed_paid_accounting_stays_uncertain(response):
    with pytest.raises(UncertainProviderAccounting):
        accounting_envelope("fixture", response, 20000)
