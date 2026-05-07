"""
DDG Company Provider — Company info enrichment via DuckDuckGo search.

Wraps the existing search_enricher module as a formal EnrichmentProvider.
Searches for missing company fields (website, phone, email, description)
using web search result snippets.

Free, unlimited, no API key needed.
"""

import re
import time
import logging
import asyncio

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.ddg_company_provider")


def _search_company_info(company: str, city: str = "") -> dict:
    """Search DDG for company contact info. Returns dict of found fields."""
    try:
        from apps.api.services.leadgen.proxy_client import get_ddgs

        parts = [company]
        if city:
            parts.append(city)
        parts.extend(["official website", "contact"])
        query = " ".join(parts)

        fields = {}

        with get_ddgs() as ddgs:
            results = list(ddgs.text(query, max_results=5))

        for r in results:
            text = f"{r.get('title', '')} {r.get('body', '')}"
            href = r.get("href", "")

            # Website (prefer non-directory links)
            if href and "http" in href and "website" not in fields:
                from urllib.parse import urlparse
                domain = urlparse(href).netloc.lower()
                skip_domains = [
                    "justdial", "sulekha", "indiamart", "linkedin",
                    "facebook", "twitter", "glassdoor", "ambitionbox",
                    "wikipedia", "youtube",
                ]
                if not any(sd in domain for sd in skip_domains):
                    fields["website"] = href

            # Phone
            if "phone" not in fields:
                patterns = [
                    r'\+?91[\-\s]?\d{5}[\-\s]?\d{5}',
                    r'\+?91[\-\s]?\d{10}',
                    r'1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}',
                    r'0\d{2,4}[\-\s]?\d{6,8}',
                ]
                for pat in patterns:
                    match = re.search(pat, text)
                    if match:
                        fields["phone"] = match.group().strip()
                        break

            # Email
            if "email" not in fields:
                emails = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', text)
                for email in emails:
                    if not any(x in email.lower() for x in ['example', 'domain.com', '.png']):
                        fields["email"] = email
                        break

        return fields

    except Exception:
        return {}


class DDGCompanyProvider(EnrichmentProvider):
    name = "ddg_company"
    capabilities = ["website", "phone", "email", "description"]
    default_confidence = 0.5

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        if not lead.company:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No company name available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            fields = await asyncio.to_thread(
                _search_company_info,
                lead.company,
                lead.city or "",
            )

            # Only return fields that the lead is missing
            filtered = {}
            if fields.get("website") and not lead.website:
                filtered["website"] = fields["website"]
            if fields.get("phone") and not lead.phone:
                filtered["phone"] = fields["phone"]
            if fields.get("email") and not lead.email:
                filtered["email"] = fields["email"]
            if fields.get("description") and not lead.description:
                filtered["description"] = fields["description"]

            if filtered:
                return EnrichmentResult(
                    provider=self.name,
                    success=True,
                    fields=filtered,
                    confidence=self.default_confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )

            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No new data found via search",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"DDG company search error for {lead.company}: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
