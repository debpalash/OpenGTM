"""
Google Maps Scraper Provider — Business data from Google without API key.

Replaces: Google Maps API ($17/1K), Google Places API
Uses DuckDuckGo to find Google Maps listings and extracts
phone, address, rating data from the search results.

Also scrapes business directory listings from:
  - Yellow Pages
  - Yelp (public pages)
  - BBB (Better Business Bureau)

Free, unlimited, no API key needed.
"""

import asyncio
import logging
import re
import time
from typing import Dict, List, Optional

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

try:
    from bs4 import BeautifulSoup
except ImportError:
    BeautifulSoup = None

logger = logging.getLogger("leadgen.local_business")


async def _search_ddg(query: str, max_results: int = 8) -> List[Dict]:
    """DuckDuckGo search with timeout."""
    from apps.api.services.leadgen.enrichment.web_search import DDGS  # routed via SearXNG pool
    from apps.api.services.leadgen.proxy_client import get_proxy

    proxy = get_proxy()
    def _do():
        ddgs = DDGS(proxy=proxy, timeout=8) if proxy else DDGS(timeout=8)
        with ddgs:
            return list(ddgs.text(query, max_results=max_results))

    try:
        return await asyncio.wait_for(asyncio.to_thread(_do), timeout=10)
    except (asyncio.TimeoutError, Exception):
        return []


def _extract_phone_from_text(text: str) -> Optional[str]:
    """Extract a phone number from text."""
    patterns = [
        r'\+?\d{1,3}[\s\-.]?\(?\d{2,4}\)?[\s\-.]?\d{3,4}[\s\-.]?\d{3,4}',
        r'\b\d{3}[\s\-.]?\d{3}[\s\-.]?\d{4}\b',
        r'1800[\s\-.]?\d{2,3}[\s\-.]?\d{4,6}',
    ]
    for pat in patterns:
        m = re.search(pat, text)
        if m:
            phone = m.group().strip()
            digits = re.sub(r'\D', '', phone)
            if 7 <= len(digits) <= 15:
                return phone
    return None


def _extract_address_from_text(text: str) -> Optional[str]:
    """Extract a physical address from text."""
    addr_patterns = [
        # US-style: 123 Main St, City, ST 12345
        r'\d+\s+[\w\s]+(?:St|Ave|Rd|Dr|Blvd|Ln|Way|Ct|Pl|Terr)[.\s,]+[\w\s]+,\s*[A-Z]{2}\s*\d{5}',
        # Generic: Address: ...
        r'(?:Address|Location|HQ|Office)[:\s]+([^|<\n]{10,120})',
    ]
    for pat in addr_patterns:
        m = re.search(pat, text, re.I)
        if m:
            addr = m.group(0).strip() if not m.groups() else m.group(1).strip()
            # Validate: address should contain a digit (house/building number)
            if 10 < len(addr) < 200 and re.search(r'\d', addr):
                return addr
    return None


def _extract_rating_from_text(text: str) -> Optional[str]:
    """Extract star rating from text."""
    m = re.search(r'(\d\.?\d?)\s*(?:/5|stars?|out of 5|rating)', text, re.I)
    if m:
        try:
            rating = float(m.group(1))
            if 1 <= rating <= 5:
                return f"{rating}/5"
        except ValueError:
            pass
    return None


class LocalBusinessProvider(EnrichmentProvider):
    """Local business data from search engines + directories.

    Replaces Google Maps API by using DuckDuckGo to find business
    listings on Google Maps, Yellow Pages, Yelp, and BBB.

    Extracts: phone, address, rating, business category.
    Zero cost, no API key needed.
    """

    name = "local_business"
    capabilities = ["phone", "address", "industry_tags"]
    default_confidence = 0.65

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        company = lead.company or ""
        city = lead.city or ""

        if not company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No company name available",
                duration_ms=(time.time() - t0) * 1000,
            )

        fields = {}
        search_query = f'"{company}"'
        if city:
            search_query += f' "{city}"'

        # ── Single search query — extract from snippets only (no page scraping) ──
        results = await _search_ddg(f'{search_query} phone address contact', max_results=8)
        for r in results:
            text = f"{r.get('title', '')} {r.get('body', '')}"

            if "phone" not in fields:
                phone = _extract_phone_from_text(text)
                if phone:
                    # Validate phone: at least 10 digits for Indian/international numbers
                    digits = re.sub(r'\D', '', phone)
                    if len(digits) >= 10:
                        fields["phone"] = phone

            if "address" not in fields:
                addr = _extract_address_from_text(text)
                if addr:
                    fields["address"] = addr

            if "google_rating" not in fields:
                rating = _extract_rating_from_text(text)
                if rating:
                    fields["google_rating"] = rating

        if fields:
            return EnrichmentResult(
                provider=self.name, success=True,
                fields=fields,
                confidence=self.default_confidence,
                duration_ms=(time.time() - t0) * 1000,
            )

        return EnrichmentResult(
            provider=self.name, success=False,
            error="No local business data found",
            duration_ms=(time.time() - t0) * 1000,
        )
