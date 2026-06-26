"""
Apollo.io — People + company enrichment API.

BYOK provider: requires user's Apollo.io API key.
Free tier: 50 credits/month.
Docs: https://apolloio.github.io/apollo-api-docs/
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.apollo")

BASE_URL = "https://api.apollo.io/api/v1"


def _get_api_key() -> str:
    """Read Apollo API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("APOLLO_API_KEY", "") or os.getenv("APOLLO_API_KEY", "")
    except Exception:
        return os.getenv("APOLLO_API_KEY", "")


def _extract_domain(url_or_company: str) -> str:
    if not url_or_company:
        return ""
    url = url_or_company.strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0].lower()


class ApolloProvider(EnrichmentProvider):
    name = "apollo_io"
    capabilities = ["email", "phone", "contact_person", "contact_title", "linkedin_url", "company_size"]
    default_confidence = 0.85
    source_license = "proprietary-api"  # vendor ToS forbids resale (per-fact provenance)

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No Apollo.io API key configured",
            )

        domain = _extract_domain(lead.website or lead.company)
        t0 = time.time()

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                fields = {}

                # 1. Organization enrichment (if we have domain)
                if domain:
                    resp = await client.post(
                        f"{BASE_URL}/organizations/enrich",
                        headers={"X-Api-Key": api_key, "Content-Type": "application/json"},
                        json={"domain": domain},
                    )
                    if resp.status_code == 200:
                        org = resp.json().get("organization", {})
                        if org.get("estimated_num_employees"):
                            fields["employee_count_exact"] = org["estimated_num_employees"]
                        if org.get("industry"):
                            fields["industry_tags"] = org["industry"]
                        if org.get("linkedin_url"):
                            fields["linkedin_url"] = org["linkedin_url"]
                        if org.get("short_description"):
                            fields["description"] = org["short_description"][:500]

                # 2. People search (find decision maker)
                search_params = {}
                if domain:
                    search_params["q_organization_domains"] = domain
                elif lead.company:
                    search_params["q_organization_name"] = lead.company

                if search_params:
                    search_params["page"] = 1
                    search_params["per_page"] = 3
                    # Prefer C-level / VP
                    search_params["person_seniorities"] = ["owner", "c_suite", "vp", "director"]

                    resp = await client.post(
                        f"{BASE_URL}/mixed_people/search",
                        headers={"X-Api-Key": api_key, "Content-Type": "application/json"},
                        json=search_params,
                    )
                    if resp.status_code == 200:
                        people = resp.json().get("people", [])
                        if people:
                            person = people[0]
                            name = person.get("name", "")
                            if name:
                                fields["contact_person"] = name
                            if person.get("title"):
                                fields["contact_title"] = person["title"]
                            if person.get("email"):
                                fields["email"] = person["email"]
                                fields["email_provider"] = "apollo_io"
                            if person.get("phone_numbers"):
                                phones = person["phone_numbers"]
                                if phones:
                                    fields["phone"] = phones[0].get("sanitized_number", phones[0].get("raw_number", ""))
                                    fields["phone_provider"] = "apollo_io"
                            if person.get("linkedin_url"):
                                fields["linkedin_url"] = person["linkedin_url"]

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
                    error="No data found",
                    duration_ms=(time.time() - t0) * 1000,
                )

        except httpx.HTTPStatusError as e:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=f"Apollo API error: {e.response.status_code}",
                duration_ms=(time.time() - t0) * 1000,
            )
        except Exception as e:
            logger.warning(f"Apollo.io error: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
