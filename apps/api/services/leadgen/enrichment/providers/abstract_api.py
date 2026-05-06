"""
AbstractAPI — Email validation + deliverability check.

BYOK provider: requires user's AbstractAPI key.
Free tier: 100 validations/month.
Docs: https://www.abstractapi.com/api/email-verification-validation-api
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.abstractapi")

BASE_URL = "https://emailvalidation.abstractapi.com/v1"


def _get_api_key() -> str:
    """Read AbstractAPI key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("ABSTRACT_API_KEY", "") or os.getenv("ABSTRACT_API_KEY", "")
    except Exception:
        return os.getenv("ABSTRACT_API_KEY", "")


class AbstractAPIProvider(EnrichmentProvider):
    name = "abstract_api"
    capabilities = ["email_verify"]
    default_confidence = 0.9

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No AbstractAPI key configured",
            )

        email = lead.email
        if not email:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No email to validate",
            )

        t0 = time.time()
        try:
            async with httpx.AsyncClient(timeout=15) as client:
                resp = await client.get(
                    BASE_URL,
                    params={"api_key": api_key, "email": email},
                )
                resp.raise_for_status()
                data = resp.json()

                deliverability = data.get("deliverability", "UNKNOWN")
                is_valid = data.get("is_valid_format", {}).get("value", False)
                is_smtp = data.get("is_smtp_valid", {}).get("value", False)
                is_disposable = data.get("is_disposable_email", {}).get("value", False)
                quality_score = data.get("quality_score", 0)

                # Map to our confidence levels
                if deliverability == "DELIVERABLE" and is_smtp and not is_disposable:
                    confidence_label = "smtp_verified"
                    confidence = 0.95
                elif deliverability == "DELIVERABLE":
                    confidence_label = "verified"
                    confidence = 0.8
                elif is_valid:
                    confidence_label = "pattern"
                    confidence = 0.5
                else:
                    confidence_label = "invalid"
                    confidence = 0.0

                fields = {
                    "email_confidence": confidence_label,
                }

                return EnrichmentResult(
                    provider=self.name,
                    success=True,
                    fields=fields,
                    confidence=confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )

        except httpx.HTTPStatusError as e:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=f"AbstractAPI error: {e.response.status_code}",
                duration_ms=(time.time() - t0) * 1000,
            )
        except Exception as e:
            logger.warning(f"AbstractAPI error: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
