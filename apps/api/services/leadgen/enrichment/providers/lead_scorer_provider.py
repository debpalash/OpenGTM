"""
Lead Scorer Provider — wraps scoring.py as an EnrichmentProvider.

No API key needed. Calculates a 0-100 quality score + tier based on ICP fit,
data completeness, and signal strength.
"""

import logging

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.lead_scorer_provider")


class LeadScorerProvider(EnrichmentProvider):
    """Score leads against ICP using rule-based scoring engine."""

    name = "lead_scorer"
    capabilities = ["score", "score_tier"]
    default_confidence = 0.95
    requires_api_key = False

    def can_provide(self, field: str) -> bool:
        return field in self.capabilities

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        try:
            from apps.api.services.leadgen.scoring import score_lead, get_tier

            score = score_lead(lead)
            tier = get_tier(score)

            return EnrichmentResult(
                success=True,
                data={
                    "score": score,
                    "score_tier": tier,
                },
                provider="lead_scorer",
                confidence=0.95,
            )
        except Exception as e:
            logger.error(f"Lead scoring failed: {e}")
            return EnrichmentResult(success=False, error=str(e))
