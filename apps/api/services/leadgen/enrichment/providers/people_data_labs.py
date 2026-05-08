"""
People Data Labs — Person + company enrichment API.

BYOK provider: requires PDL API key.
Free tier: 100 matches/month.
Docs: https://docs.peopledatalabs.com/
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.pdl")

BASE_URL = "https://api.peopledatalabs.com/v5"


def _get_api_key() -> str:
    """Read PDL API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("PDL_API_KEY", "") or os.getenv("PDL_API_KEY", "")
    except Exception:
        return os.getenv("PDL_API_KEY", "")


def _extract_domain(url_or_company: str) -> str:
    if not url_or_company:
        return ""
    url = url_or_company.strip()
    for prefix in ("https://", "http://", "www."):
        url = url.removeprefix(prefix)
    return url.split("/")[0].split("?")[0].lower()


class PeopleDataLabsProvider(EnrichmentProvider):
    name = "people_data_labs"
    capabilities = [
        "email", "phone", "contact_person", "contact_title",
        "linkedin_url", "company_size", "industry_tags", "description",
    ]
    default_confidence = 0.88

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No People Data Labs API key configured",
            )

        domain = _extract_domain(lead.website or "")
        company_name = lead.company or ""

        if not domain and not company_name:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No domain or company name available",
            )

        t0 = time.time()

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                fields = {}

                # 1. Company enrichment
                if domain:
                    resp = await client.get(
                        f"{BASE_URL}/company/enrich",
                        params={"website": domain},
                        headers={"X-Api-Key": api_key},
                    )
                    if resp.status_code == 200:
                        data = resp.json()
                        if data.get("size"):
                            fields["company_size"] = data["size"]
                        if data.get("industry"):
                            fields["industry_tags"] = data["industry"]
                        if data.get("summary"):
                            fields["description"] = data["summary"][:500]
                        if data.get("linkedin_url"):
                            fields["linkedin_url"] = data["linkedin_url"]
                        if data.get("employee_count"):
                            fields["employee_count_exact"] = data["employee_count"]

                # 2. Person search — find contacts at the company
                search_params = {
                    "size": 3,
                    "query": {
                        "bool": {
                            "must": [],
                        }
                    },
                }

                if domain:
                    search_params["query"]["bool"]["must"].append(
                        {"term": {"job_company_website": domain}}
                    )
                elif company_name:
                    search_params["query"]["bool"]["must"].append(
                        {"match": {"job_company_name": company_name}}
                    )

                # Prefer senior titles
                search_params["query"]["bool"]["should"] = [
                    {"match": {"job_title_levels": "cxo"}},
                    {"match": {"job_title_levels": "vp"}},
                    {"match": {"job_title_levels": "director"}},
                    {"match": {"job_title_levels": "owner"}},
                ]

                resp = await client.post(
                    f"{BASE_URL}/person/search",
                    headers={"X-Api-Key": api_key, "Content-Type": "application/json"},
                    json=search_params,
                )

                if resp.status_code == 200:
                    results = resp.json().get("data", [])
                    if results:
                        person = results[0]
                        name = person.get("full_name", "")
                        if name:
                            fields["contact_person"] = name
                        if person.get("job_title"):
                            fields["contact_title"] = person["job_title"]
                        # Work email preferred over personal
                        work_email = person.get("work_email")
                        personal_emails = person.get("emails", [])
                        if work_email:
                            fields["email"] = work_email
                        elif personal_emails:
                            # Pick first verified email
                            for em in personal_emails:
                                if isinstance(em, dict):
                                    fields["email"] = em.get("address", "")
                                    break
                                elif isinstance(em, str):
                                    fields["email"] = em
                                    break
                        if person.get("linkedin_url"):
                            fields["linkedin_url"] = person["linkedin_url"]
                        phones = person.get("phone_numbers", [])
                        if phones:
                            fields["phone"] = phones[0] if isinstance(phones[0], str) else str(phones[0])

                if fields:
                    return EnrichmentResult(
                        provider=self.name, success=True,
                        fields=fields,
                        confidence=self.default_confidence,
                        duration_ms=(time.time() - t0) * 1000,
                    )

                return EnrichmentResult(
                    provider=self.name, success=False,
                    error="No data found",
                    duration_ms=(time.time() - t0) * 1000,
                )

        except Exception as e:
            logger.warning(f"People Data Labs error: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
