"""Company-size heuristic provider.

A keyless, zero-network enrichment provider that derives an employee band from
signals ALREADY on the lead (hiring volume, funding, registry footprint, tech /
age) when ``company_size`` is unknown. It is registered LAST in the
``company_size`` waterfall and carries a low default confidence, so a real
scrape/paid provider always wins when one fires — this only fills the gaps they
leave (WaterfallEnricher.apply_results / the workbook waterfall break already
guarantee it never overwrites a real value).

No new paid APIs, no outbound calls: it is a pure derivation over the in-memory
:class:`~apps.api.services.leadgen.models.Lead`, so it inherits tenancy/RLS
automatically (it reads no DB).
"""

from time import time

from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider,
    EnrichmentResult,
)
from apps.api.services.leadgen.enrichment.size_heuristic import infer_company_size
from apps.api.services.leadgen.models import Lead


class CompanySizeHeuristicProvider(EnrichmentProvider):
    """Derive ``company_size`` (+ ``company_size_basis``) from existing signals."""

    name = "company_size_heuristic"
    capabilities = ["company_size"]
    # Below typical scrape/paid defaults (0.7+) so it loses the waterfall to any
    # real provider; the heuristic refines this per-lead via SizeEstimate.confidence.
    default_confidence = 0.4

    # Keyless, free, zero-cost — pure in-memory derivation.
    requires_api_key = False
    free_tier_limit = 0
    cost_per_lookup = 0.0

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        start = time()
        est = infer_company_size(lead)
        if not est.band:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="insufficient_signals",
                duration_ms=(time() - start) * 1000,
            )
        return EnrichmentResult(
            provider=self.name,
            success=True,
            confidence=est.confidence,
            fields={
                "company_size": est.band,
                "company_size_basis": est.basis,
            },
            duration_ms=(time() - start) * 1000,
        )
