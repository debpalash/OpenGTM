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
    """DuckDuckGo search."""
    from ddgs import DDGS
    from apps.api.services.leadgen.proxy_client import get_proxy

    proxy = get_proxy()
    def _do():
        ddgs = DDGS(proxy=proxy) if proxy else DDGS()
        with ddgs:
            return list(ddgs.text(query, max_results=max_results))

    try:
        return await asyncio.to_thread(_do)
    except Exception:
        try:
            def _direct():
                with DDGS() as ddgs:
                    return list(ddgs.text(query, max_results=max_results))
            return await asyncio.to_thread(_direct)
        except Exception:
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
    # Look for common address patterns
    addr_patterns = [
        # US-style: 123 Main St, City, ST 12345
        r'\d+\s+[\w\s]+(?:St|Ave|Rd|Dr|Blvd|Ln|Way|Ct|Pl|Terr)[.\s,]+[\w\s]+,\s*[A-Z]{2}\s*\d{5}',
        # Generic: text with city + state/country indicators
        r'(?:Address|Location|HQ|Office)[:\s]+([^|<\n]{10,120})',
    ]
    for pat in addr_patterns:
        m = re.search(pat, text, re.I)
        if m:
            addr = m.group(0).strip() if not m.groups() else m.group(1).strip()
            if 10 < len(addr) < 200:
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

        # ── Search 1: Google Maps / local business listings ──
        queries = [
            f'{search_query} phone address',
            f'{search_query} site:yellowpages.com OR site:yelp.com OR site:bbb.org',
        ]

        all_text = ""
        for query in queries:
            results = await _search_ddg(query, max_results=8)
            for r in results:
                text = f"{r.get('title', '')} {r.get('body', '')}"
                all_text += f" {text}"

                # Extract phone if not found yet
                if "phone" not in fields:
                    phone = _extract_phone_from_text(text)
                    if phone:
                        fields["phone"] = phone

                # Extract address
                if "address" not in fields:
                    addr = _extract_address_from_text(text)
                    if addr:
                        fields["address"] = addr

                # Extract rating
                if "google_rating" not in fields:
                    rating = _extract_rating_from_text(text)
                    if rating:
                        fields["google_rating"] = rating

            await asyncio.sleep(0.3)

        # ── Try to scrape a directory page for richer data ──
        for query in [f'site:yellowpages.com "{company}" {city}']:
            results = await _search_ddg(query, max_results=3)
            for r in results:
                href = r.get("href", "")
                if "yellowpages.com" in href or "yelp.com" in href:
                    import httpx
                    try:
                        async with httpx.AsyncClient(
                            timeout=10, follow_redirects=True, verify=False,
                            headers={"User-Agent": "Mozilla/5.0"},
                        ) as client:
                            resp = await client.get(href)
                            if resp.status_code == 200 and BeautifulSoup:
                                soup = BeautifulSoup(resp.text[:100_000], "html.parser")
                                page_text = soup.get_text(separator=" ", strip=True)

                                if "phone" not in fields:
                                    # Look for tel: links
                                    for a in soup.find_all("a", href=True):
                                        if a["href"].startswith("tel:"):
                                            fields["phone"] = a["href"].replace("tel:", "").strip()
                                            break

                                if "address" not in fields:
                                    addr_el = soup.find("address")
                                    if addr_el:
                                        fields["address"] = addr_el.get_text(strip=True)[:200]

                                # Industry/category
                                if "industry_tags" not in fields:
                                    cat_el = soup.find(class_=re.compile(r"categor|type|industry", re.I))
                                    if cat_el:
                                        fields["industry_tags"] = cat_el.get_text(strip=True)[:100]

                    except Exception:
                        pass
                    break  # Only scrape first directory result

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
