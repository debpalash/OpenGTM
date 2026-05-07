"""
Social Finder Provider — Discover LinkedIn / Twitter / Facebook profiles.

Wraps the existing social_finder module as a formal EnrichmentProvider.
Uses DuckDuckGo site: searches to find company social profiles.

Free, unlimited, no API key needed.
"""

import time
import logging
import asyncio

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.social_finder_provider")


def _find_linkedin(company: str) -> str:
    """Search DDG for a company LinkedIn profile."""
    try:
        from apps.api.services.leadgen.proxy_client import get_ddgs
        query = f'site:linkedin.com/company "{company}"'
        with get_ddgs() as ddgs:
            results = list(ddgs.text(query, max_results=2))
        for r in results:
            href = r.get("href", "")
            if "linkedin.com/company" in href:
                return href
    except Exception:
        pass
    return ""


def _find_twitter(company: str) -> str:
    """Search DDG for a company Twitter/X profile."""
    try:
        from apps.api.services.leadgen.proxy_client import get_ddgs
        query = f'site:twitter.com OR site:x.com "{company}"'
        with get_ddgs() as ddgs:
            results = list(ddgs.text(query, max_results=2))
        for r in results:
            href = r.get("href", "")
            if "twitter.com/" in href or "x.com/" in href:
                return href
    except Exception:
        pass
    return ""


class SocialFinderProvider(EnrichmentProvider):
    name = "social_finder"
    capabilities = ["linkedin_url", "twitter_url"]
    default_confidence = 0.55

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
            fields = {}

            # LinkedIn
            if not lead.linkedin_url:
                linkedin = await asyncio.to_thread(_find_linkedin, lead.company)
                if linkedin:
                    fields["linkedin_url"] = linkedin

            # Twitter
            if not lead.twitter_url:
                twitter = await asyncio.to_thread(_find_twitter, lead.company)
                if twitter:
                    fields["twitter_url"] = twitter

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
                error="No social profiles found",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"Social finder error for {lead.company}: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
