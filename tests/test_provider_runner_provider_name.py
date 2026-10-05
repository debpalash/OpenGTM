"""A provider that leaves EnrichmentResult.provider empty must still settle.

Declarative (manifest v1) providers return ``EnrichmentResult(success=..., fields=...)``
without a provider name. ``accounting_envelope`` requires the response to name the
provider that was dispatched, so before the runner filled the name in, every paid
connector call ended as an uncertain spend attempt.
"""
import pytest

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.workbook import provider_runner, providers
from apps.api.services.workbook.provider_accounting import accounting_envelope


class _Unnamed(EnrichmentProvider):
    name = "unnamed_for_test"
    capabilities = ["email"]
    default_confidence = 0.5

    async def enrich(self, lead):
        return EnrichmentResult(success=True, fields={"email": "a@b.example"}, confidence=0.5)


@pytest.fixture
def registered():
    providers.register_provider(_Unnamed())
    yield
    providers._registry.pop(_Unnamed.name, None)


def test_runner_names_the_provider_so_a_paid_attempt_can_settle(registered):
    response = provider_runner._provider_job("unnamed_for_test", {"company": "Acme"})

    assert response["provider"] == "unnamed_for_test"
    envelope = accounting_envelope("unnamed_for_test", response, 50_000)
    assert envelope["charged_microusd"] == 50_000
    assert envelope["accounting_basis"] == "catalog_estimate"


def test_a_provider_that_names_itself_is_unchanged(registered):
    class Named(_Unnamed):
        async def enrich(self, lead):
            return EnrichmentResult(provider="unnamed_for_test", success=False, error="no_data")

    providers.register_provider(Named())
    assert provider_runner._provider_job("unnamed_for_test", {})["provider"] == "unnamed_for_test"
