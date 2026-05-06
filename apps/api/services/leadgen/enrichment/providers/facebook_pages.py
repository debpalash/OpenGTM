"""
Facebook Page Scraper Provider.

Inspired by wael-sudo2/facebook-page-info-scraper.
Extracts business contact info from Facebook pages.

Many Indian SMBs have Facebook pages as their primary web presence,
even without a dedicated website. This provider catches those leads.
"""

import asyncio
import json
import logging
import re
from time import time
from typing import Optional

from apps.api.services.leadgen.enrichment.provider import (
    EnrichmentProvider, EnrichmentResult,
)
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.facebook")


class FacebookPageProvider(EnrichmentProvider):
    """Extract contact info from Facebook business pages via DDG search."""

    name = "facebook_pages"
    capabilities = ["phone", "email", "address"]
    default_confidence = 0.5

    def __init__(self, delay: float = 1.0):
        self.delay = delay

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        """Search for and extract data from a company's Facebook page."""
        start = time()

        if not lead.company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_company_name",
                duration_ms=(time() - start) * 1000,
            )

        fb_data = await self._search_facebook_page(lead.company, lead.city)

        if not fb_data:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_facebook_page",
                duration_ms=(time() - start) * 1000,
            )

        fields = {}
        if fb_data.get("phone"):
            fields["phone"] = fb_data["phone"]
        if fb_data.get("email"):
            fields["email"] = fb_data["email"]
        if fb_data.get("address"):
            fields["address"] = fb_data["address"]

        if not fields:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="no_contact_on_page",
                duration_ms=(time() - start) * 1000,
            )

        return EnrichmentResult(
            provider=self.name,
            success=True,
            confidence=0.5,
            fields=fields,
            duration_ms=(time() - start) * 1000,
        )

    async def _search_facebook_page(self, company: str, city: str = "") -> Optional[dict]:
        """Search DDG for Facebook page and extract contact info from snippet."""
        from ddgs import DDGS
        from apps.api.services.leadgen.proxy_client import get_ddgs

        query = f'site:facebook.com "{company}"'
        if city:
            query += f' "{city}"'
        query += " business"

        def _search():
            try:
                with get_ddgs() as ddgs:
                    return list(ddgs.text(query, max_results=5))
            except Exception:
                return []

        results = await asyncio.to_thread(_search)

        for r in results:
            href = r.get("href", "")
            body = r.get("body", "")
            title = r.get("title", "")

            # Must be a Facebook page (not group, profile, etc.)
            if "facebook.com/" not in href.lower():
                continue
            if any(x in href.lower() for x in ["/groups/", "/events/", "/marketplace/"]):
                continue

            # Extract contact info from the snippet
            combined = f"{title} {body}"
            data = {}

            # Extract phone
            phones = re.findall(
                r'(?:\+91[\s\-]?)?(?:\d{2,4}[\s\-]?\d{6,8}|\d{10,12})',
                combined
            )
            if phones:
                # Clean and take first valid phone
                phone = re.sub(r'[\s\-]', '', phones[0])
                if len(phone) >= 10:
                    data["phone"] = phone

            # Extract email
            emails = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', combined)
            if emails:
                email = emails[0].lower()
                if not any(x in email for x in ['facebook', 'example', 'domain']):
                    data["email"] = email

            # Extract address (look for common Indian address patterns)
            addr_match = re.search(
                r'(?:located\s+(?:at|in)|address|located)\s*[:.]?\s*(.{20,80})',
                combined, re.IGNORECASE,
            )
            if addr_match:
                data["address"] = addr_match.group(1).strip().rstrip('.')

            if data:
                data["facebook_url"] = href
                return data

        return None
