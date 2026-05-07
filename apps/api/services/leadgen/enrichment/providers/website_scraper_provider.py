"""
Website Scraper Provider — Extract contact info from company websites.

Wraps the existing website_scraper module as a formal EnrichmentProvider.
Uses stealth HTTP to scrape homepage, /contact, /about pages for
emails, phones, social links, and descriptions.

Free, unlimited, no API key needed.
"""

import time
import logging

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.website_scraper_provider")


class WebsiteScraperProvider(EnrichmentProvider):
    name = "website_scraper"
    capabilities = ["email", "phone", "description", "linkedin_url", "twitter_url", "facebook_url"]
    default_confidence = 0.65

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        if not lead.website:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No website URL available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            from apps.api.services.leadgen.enrichment.website_scraper import (
                normalize_website_url,
                _scrape_via_http,
            )
            from apps.api.services.leadgen.http import StealthClient

            url = normalize_website_url(lead.website)
            client = StealthClient()
            data = await _scrape_via_http(client, url)

            fields = {}

            # Extract best email
            if data.get("emails"):
                fields["email"] = data["emails"][0]
                if len(data["emails"]) > 1:
                    fields["secondary_emails"] = "|".join(data["emails"][1:3])

            # Extract best phone
            if data.get("phones"):
                fields["phone"] = data["phones"][0]
                if len(data["phones"]) > 1:
                    fields["secondary_phones"] = "|".join(data["phones"][1:3])

            # Social links
            social = data.get("social", {})
            if social.get("linkedin"):
                fields["linkedin_url"] = social["linkedin"]
            if social.get("twitter"):
                fields["twitter_url"] = social["twitter"]
            if social.get("facebook"):
                fields["facebook_url"] = social["facebook"]

            # Description
            if data.get("description"):
                fields["description"] = data["description"]

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
                error="No contact info found on website",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"Website scraper error for {lead.website}: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
