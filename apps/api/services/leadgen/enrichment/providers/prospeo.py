"""
Prospeo — LinkedIn-to-email finder + email verifier.

BYOK provider: requires Prospeo API key.
Free tier: 75 credits/month.
Docs: https://prospeo.io/api
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.prospeo")

BASE_URL = "https://api.prospeo.io"


def _get_api_key() -> str:
    """Read Prospeo API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("PROSPEO_API_KEY", "") or os.getenv("PROSPEO_API_KEY", "")
    except Exception:
        return os.getenv("PROSPEO_API_KEY", "")


def _extract_domain(url_or_company: str) -> str:
    if not url_or_company:
        return ""
    url = url_or_company.strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0].lower()


class ProspeoProvider(EnrichmentProvider):
    name = "prospeo"
    capabilities = ["email", "email_verify"]
    default_confidence = 0.82

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No Prospeo API key configured",
            )

        t0 = time.time()

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
                fields = {}

                # Strategy 1: LinkedIn URL → email (highest value)
                linkedin_url = lead.linkedin_url or ""
                if linkedin_url and "linkedin.com" in linkedin_url:
                    resp = await client.post(
                        f"{BASE_URL}/linkedin-email-finder",
                        headers=headers,
                        json={"url": linkedin_url},
                    )
                    if resp.status_code == 200:
                        data = resp.json().get("response", {})
                        if data.get("email"):
                            fields["email"] = data["email"]
                            fields["email_match_method"] = "linkedin"
                            if data.get("email_verified"):
                                fields["email_verified"] = True
                            return EnrichmentResult(
                                provider=self.name, success=True,
                                fields=fields,
                                confidence=0.90 if data.get("email_verified") else 0.75,
                                duration_ms=(time.time() - t0) * 1000,
                            )

                # Strategy 2: Domain email finder
                domain = _extract_domain(lead.website or "")
                name = lead.contact_person or ""

                if domain and name:
                    # Split name into first/last
                    parts = name.strip().split()
                    first = parts[0] if parts else ""
                    last = parts[-1] if len(parts) > 1 else ""

                    if first and last:
                        resp = await client.post(
                            f"{BASE_URL}/email-finder",
                            headers=headers,
                            json={"domain": domain, "first_name": first, "last_name": last},
                        )
                        if resp.status_code == 200:
                            data = resp.json().get("response", {})
                            if data.get("email"):
                                fields["email"] = data["email"]
                                fields["email_match_method"] = "exact_name"
                                return EnrichmentResult(
                                    provider=self.name, success=True,
                                    fields=fields,
                                    confidence=0.80,
                                    duration_ms=(time.time() - t0) * 1000,
                                )

                # Strategy 3: Verify existing email
                existing_email = lead.email or ""
                if existing_email and "@" in existing_email:
                    resp = await client.post(
                        f"{BASE_URL}/email-verifier",
                        headers=headers,
                        json={"email": existing_email},
                    )
                    if resp.status_code == 200:
                        data = resp.json().get("response", {})
                        is_valid = data.get("is_valid", False)
                        fields["email"] = existing_email
                        fields["email_verify"] = "valid" if is_valid else "invalid"
                        return EnrichmentResult(
                            provider=self.name, success=True,
                            fields=fields,
                            confidence=0.95 if is_valid else 0.3,
                            duration_ms=(time.time() - t0) * 1000,
                        )

                return EnrichmentResult(
                    provider=self.name, success=False,
                    error="No LinkedIn URL, domain+name, or email available",
                    duration_ms=(time.time() - t0) * 1000,
                )

        except Exception as e:
            logger.warning(f"Prospeo error: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
