"""
IPInfo — Company/organization data from IP address or domain.

BYOK provider: requires user's IPInfo token.
Free tier: 50,000 requests/month.
Docs: https://ipinfo.io/developers
"""

import os
import logging
import socket
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.ipinfo")


def _get_api_key() -> str:
    """Read IPInfo token from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("IPINFO_TOKEN", "") or os.getenv("IPINFO_TOKEN", "")
    except Exception:
        return os.getenv("IPINFO_TOKEN", "")


def _domain_to_ip(domain: str) -> str:
    """Resolve domain to IP address."""
    try:
        clean = domain.replace("https://", "").replace("http://", "").split("/")[0].strip()
        return socket.gethostbyname(clean)
    except Exception:
        return ""


class IPInfoProvider(EnrichmentProvider):
    """Enrich leads with organization/company data from their domain's IP."""

    name = "ipinfo"
    capabilities = ["company_data", "org", "location", "asn"]
    default_confidence = 0.7
    requires_api_key = True

    def can_provide(self, field: str) -> bool:
        return field in self.capabilities

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        token = _get_api_key()
        if not token:
            return EnrichmentResult(success=False, error="No IPInfo token configured")

        website = getattr(lead, "website", None) or ""
        if not website:
            return EnrichmentResult(success=False, error="No website to resolve")

        ip = _domain_to_ip(website)
        if not ip:
            return EnrichmentResult(success=False, error=f"Could not resolve {website}")

        try:
            async with httpx.AsyncClient(timeout=10) as client:
                resp = await client.get(
                    f"https://ipinfo.io/{ip}",
                    headers={"Authorization": f"Bearer {token}"},
                )
                data = resp.json()

                if data.get("org") or data.get("company"):
                    company_info = data.get("company", {})
                    result = {
                        "ip": ip,
                        "org": data.get("org", ""),
                        "hostname": data.get("hostname", ""),
                        "city": data.get("city", ""),
                        "region": data.get("region", ""),
                        "country": data.get("country", ""),
                        "company_name": company_info.get("name", data.get("org", "")),
                        "company_domain": company_info.get("domain", ""),
                        "company_type": company_info.get("type", ""),
                    }
                    return EnrichmentResult(
                        success=True,
                        data=result,
                        provider="ipinfo",
                        confidence=0.70,
                    )
                else:
                    return EnrichmentResult(success=False, error="No org data for IP")

        except httpx.TimeoutException:
            return EnrichmentResult(success=False, error="IPInfo API timeout")
        except Exception as e:
            logger.error(f"IPInfo error: {e}")
            return EnrichmentResult(success=False, error=str(e))
