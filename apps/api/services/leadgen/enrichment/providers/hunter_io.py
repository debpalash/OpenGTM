"""
Hunter.io — Email finder via domain search API.

BYOK provider: requires user's Hunter.io API key.
Free tier: 25 lookups/month.
Docs: https://hunter.io/api-documentation/v2
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.hunter")


def _get_api_key() -> str:
    """Read Hunter API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("HUNTER_API_KEY", "") or os.getenv("HUNTER_API_KEY", "")
    except Exception:
        return os.getenv("HUNTER_API_KEY", "")


def _extract_domain(url_or_company: str) -> str:
    """Extract domain from URL or company name."""
    if not url_or_company:
        return ""
    url = url_or_company.strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0].lower()


class HunterProvider(EnrichmentProvider):
    name = "hunter_io"
    capabilities = ["email"]
    default_confidence = 0.8
    source_license = "proprietary-api"  # vendor ToS forbids resale (per-fact provenance)

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No Hunter.io API key configured",
            )

        domain = _extract_domain(lead.website or lead.company)
        if not domain:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No domain available",
            )

        t0 = time.time()
        try:
            async with httpx.AsyncClient(timeout=15) as client:
                # If we have a contact person, use email-finder (more precise)
                if lead.contact_person:
                    parts = lead.contact_person.strip().split(None, 1)
                    first_name = parts[0] if parts else ""
                    last_name = parts[1] if len(parts) > 1 else ""

                    resp = await client.get(
                        "https://api.hunter.io/v2/email-finder",
                        params={
                            "domain": domain,
                            "first_name": first_name,
                            "last_name": last_name,
                            "api_key": api_key,
                        },
                    )
                    data = resp.json()
                    email_data = data.get("data", {})
                    if email_data.get("email"):
                        return EnrichmentResult(
                            provider=self.name,
                            success=True,
                            fields={
                                "email": email_data["email"],
                                "email_confidence": str(email_data.get("confidence", 0)),
                                "email_provider": "hunter_io",
                                "email_match_method": "exact_name",
                            },
                            confidence=email_data.get("confidence", 0) / 100,
                            duration_ms=(time.time() - t0) * 1000,
                        )

                # Fall back to domain search
                resp = await client.get(
                    "https://api.hunter.io/v2/domain-search",
                    params={
                        "domain": domain,
                        "api_key": api_key,
                        "limit": 5,
                    },
                )
                data = resp.json()
                emails = data.get("data", {}).get("emails", [])

                if emails:
                    # Pick highest confidence email
                    best = max(emails, key=lambda e: e.get("confidence", 0))
                    fields = {
                        "email": best["value"],
                        "email_confidence": str(best.get("confidence", 0)),
                        "email_provider": "hunter_io",
                        "email_match_method": "domain_search",
                    }
                    # Bonus: extract contact person if available
                    if best.get("first_name") and best.get("last_name"):
                        fields["contact_person"] = f"{best['first_name']} {best['last_name']}"
                    if best.get("position"):
                        fields["contact_title"] = best["position"]

                    return EnrichmentResult(
                        provider=self.name,
                        success=True,
                        fields=fields,
                        confidence=best.get("confidence", 0) / 100,
                        duration_ms=(time.time() - t0) * 1000,
                    )

                return EnrichmentResult(
                    provider=self.name,
                    success=False,
                    error="No emails found for domain",
                    duration_ms=(time.time() - t0) * 1000,
                )

        except httpx.HTTPStatusError as e:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=f"Hunter API error: {e.response.status_code}",
                duration_ms=(time.time() - t0) * 1000,
            )
        except Exception as e:
            logger.warning(f"Hunter.io error: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
