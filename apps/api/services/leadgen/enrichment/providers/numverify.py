"""
NumVerify — Phone number validation and lookup.

BYOK provider: requires user's NumVerify API key.
Free tier: 100 validations/month.
Docs: https://numverify.com/documentation
"""

import os
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.numverify")


def _get_api_key() -> str:
    """Read NumVerify API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("NUMVERIFY_API_KEY", "") or os.getenv("NUMVERIFY_API_KEY", "")
    except Exception:
        return os.getenv("NUMVERIFY_API_KEY", "")


class NumVerifyProvider(EnrichmentProvider):
    """Validate and enrich phone numbers using NumVerify API."""

    name = "numverify"
    capabilities = ["phone_verify", "phone_carrier", "phone_line_type"]
    default_confidence = 0.85
    requires_api_key = True

    def can_provide(self, field: str) -> bool:
        return field in self.capabilities

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(success=False, error="No NumVerify API key configured")

        phone = getattr(lead, "phone", None) or ""
        if not phone:
            return EnrichmentResult(success=False, error="No phone number to validate")

        # Clean the phone number
        clean_phone = phone.strip().replace(" ", "").replace("-", "").replace("(", "").replace(")", "")

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                resp = await client.get(
                    "http://apilayer.net/api/validate",
                    params={
                        "access_key": api_key,
                        "number": clean_phone,
                        "format": 1,
                    },
                )
                data = resp.json()

                if data.get("valid"):
                    result = {
                        "valid": True,
                        "number": data.get("international_format", clean_phone),
                        "country": data.get("country_name", ""),
                        "carrier": data.get("carrier", ""),
                        "line_type": data.get("line_type", ""),
                        "location": data.get("location", ""),
                    }
                    return EnrichmentResult(
                        success=True,
                        fields={
                            "phone": result["number"],
                            "phone_carrier": result.get("carrier", ""),
                            "phone_line_type": result.get("line_type", ""),
                        },
                        provider="numverify",
                        confidence=0.90,
                    )
                else:
                    return EnrichmentResult(
                        success=False,
                        error="Number not valid",
                        provider="numverify",
                    )

        except httpx.TimeoutException:
            return EnrichmentResult(success=False, error="NumVerify API timeout")
        except Exception as e:
            logger.error(f"NumVerify error: {e}")
            return EnrichmentResult(success=False, error=str(e))
