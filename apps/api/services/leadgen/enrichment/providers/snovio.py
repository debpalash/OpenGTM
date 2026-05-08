"""
Snov.io — Email finder + verification API.

BYOK provider: requires Snov.io API credentials (client_id + client_secret).
Free tier: 50 credits/month.
Docs: https://snov.io/knowledgebase/api
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.snovio")

BASE_URL = "https://api.snov.io/v1"


def _get_credentials() -> tuple[str, str]:
    """Read Snov.io credentials from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        client_id = get_setting("SNOVIO_CLIENT_ID", "") or os.getenv("SNOVIO_CLIENT_ID", "")
        client_secret = get_setting("SNOVIO_CLIENT_SECRET", "") or os.getenv("SNOVIO_CLIENT_SECRET", "")
        return client_id, client_secret
    except Exception:
        return os.getenv("SNOVIO_CLIENT_ID", ""), os.getenv("SNOVIO_CLIENT_SECRET", "")


async def _get_access_token(client: httpx.AsyncClient) -> str:
    """OAuth2 client credentials flow for Snov.io."""
    client_id, client_secret = _get_credentials()
    if not client_id or not client_secret:
        return ""

    resp = await client.post(
        f"{BASE_URL}/oauth/access_token",
        json={"grant_type": "client_credentials", "client_id": client_id, "client_secret": client_secret},
    )
    if resp.status_code == 200:
        return resp.json().get("access_token", "")
    return ""


def _extract_domain(url_or_company: str) -> str:
    if not url_or_company:
        return ""
    url = url_or_company.strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0].lower()


class SnovioProvider(EnrichmentProvider):
    name = "snovio"
    capabilities = ["email", "contact_person", "contact_title"]
    default_confidence = 0.80

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        client_id, client_secret = _get_credentials()
        if not client_id or not client_secret:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No Snov.io API credentials configured",
            )

        domain = _extract_domain(lead.website or "")
        if not domain:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No domain available for lookup",
            )

        t0 = time.time()

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                token = await _get_access_token(client)
                if not token:
                    return EnrichmentResult(
                        provider=self.name, success=False,
                        error="Failed to authenticate with Snov.io",
                        duration_ms=(time.time() - t0) * 1000,
                    )

                fields = {}

                # Domain email search — finds emails at a company domain
                resp = await client.post(
                    f"{BASE_URL}/get-domain-emails-with-info",
                    json={"access_token": token, "domain": domain, "limit": 5},
                )

                if resp.status_code == 200:
                    data = resp.json()
                    emails = data.get("emails", [])

                    if emails:
                        # Prefer decision-maker emails (C-level, VP, Director)
                        priority_titles = ["ceo", "cto", "cfo", "founder", "director", "vp", "head", "owner", "manager"]
                        best = None

                        for e in emails:
                            title = (e.get("position") or "").lower()
                            if any(t in title for t in priority_titles):
                                best = e
                                break

                        if not best:
                            best = emails[0]

                        if best.get("email"):
                            fields["email"] = best["email"]
                        name_parts = []
                        if best.get("first_name"):
                            name_parts.append(best["first_name"])
                        if best.get("last_name"):
                            name_parts.append(best["last_name"])
                        if name_parts:
                            fields["contact_person"] = " ".join(name_parts)
                        if best.get("position"):
                            fields["contact_title"] = best["position"]

                if fields:
                    return EnrichmentResult(
                        provider=self.name, success=True,
                        fields=fields,
                        confidence=self.default_confidence,
                        duration_ms=(time.time() - t0) * 1000,
                    )

                return EnrichmentResult(
                    provider=self.name, success=False,
                    error="No emails found for domain",
                    duration_ms=(time.time() - t0) * 1000,
                )

        except Exception as e:
            logger.warning(f"Snov.io error: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
