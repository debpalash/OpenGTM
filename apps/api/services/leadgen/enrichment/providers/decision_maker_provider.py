"""
Decision Maker Provider — Find C-suite / VP contacts at companies.

Wraps the existing decision_maker_finder module as a formal EnrichmentProvider.
Uses DuckDuckGo LinkedIn searches to discover key personnel.

Free, unlimited, no API key needed.
"""

import time
import json
import logging

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.decision_maker_provider")


class DecisionMakerProvider(EnrichmentProvider):
    name = "decision_maker"
    capabilities = ["contact_person", "contact_title", "decision_makers", "linkedin_url"]
    default_confidence = 0.5

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        if not lead.company or len(lead.company) < 3:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No company name available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            from apps.api.services.leadgen.enrichment.decision_maker_finder import (
                find_decision_makers_for_lead,
            )

            # Clone lead to avoid side effects on the original
            enriched = await find_decision_makers_for_lead(lead, max_contacts=3)

            fields = {}

            if enriched.contact_person:
                fields["contact_person"] = enriched.contact_person
            if enriched.contact_title:
                fields["contact_title"] = enriched.contact_title
            if enriched.linkedin_url and enriched.linkedin_url != lead.linkedin_url:
                fields["linkedin_url"] = enriched.linkedin_url
            if enriched.decision_makers:
                fields["decision_makers"] = enriched.decision_makers  # JSON string

            if fields:
                return EnrichmentResult(
                    provider=self.name,
                    success=True,
                    fields=fields,
                    confidence=self.default_confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )

            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No decision makers found",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"Decision maker error for {lead.company}: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
