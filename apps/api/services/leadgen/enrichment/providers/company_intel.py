"""
Company Intelligence Scraper — Funding, news, and company data from public sources.

Replaces: Crunchbase API ($49/mo), CB Insights, Dealroom, Owler

Scrapes publicly available data from:
  1. Crunchbase public profiles (no login needed for basic info)
  2. Google News (recent company mentions)
  3. OpenCorporates (company registration data)
  4. Wikipedia/Wikidata (company facts)

Uses DuckDuckGo to find and extract structured data from these sources.

Free, unlimited, no API key needed.
"""

import asyncio
import json
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

logger = logging.getLogger("leadgen.company_intel")


async def _search_ddg(query: str, max_results: int = 10) -> List[Dict]:
    """DuckDuckGo search with proxy rotation."""
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
        except Exception as e:
            logger.debug(f"DDG search failed: {e}")
            return []


async def _scrape_page(url: str, timeout: int = 12) -> Optional[str]:
    """Fetch a page via stealth HTTP."""
    import httpx
    try:
        async with httpx.AsyncClient(
            timeout=timeout,
            follow_redirects=True,
            verify=False,
            headers={
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
                "Accept": "text/html,application/xhtml+xml",
                "Accept-Language": "en-US,en;q=0.9",
            },
        ) as client:
            resp = await client.get(url)
            if resp.status_code == 200:
                return resp.text[:200_000]
    except Exception:
        pass
    return None


def _extract_funding_from_text(text: str) -> Dict:
    """Extract funding info from text (works on Crunchbase, news articles, etc.)."""
    info = {}

    # Funding amount patterns
    funding_patterns = [
        r'(?:raised|secured|closed|announced|received)\s*\$?([\d.]+)\s*(million|billion|M|B|mn|bn)',
        r'\$([\d.]+)\s*(million|billion|M|B|mn|bn)\s*(?:in\s+)?(?:funding|round|investment|capital)',
        r'(?:Series\s+[A-F]|Seed|Pre-Seed|Bridge)\s*(?:round)?[^$]*\$([\d.]+)\s*(million|billion|M|B|mn|bn)',
    ]

    for pattern in funding_patterns:
        m = re.search(pattern, text, re.I)
        if m:
            amount = float(m.group(1))
            unit = m.group(2).lower()
            if unit in ("billion", "b", "bn"):
                amount *= 1000
            info["last_funding_amount"] = f"${amount}M"
            break

    # Funding stage
    stage_patterns = [
        (r'(?:Series\s+([A-F]))', lambda m: f"Series {m.group(1)}"),
        (r'(?:Seed\s+round|seed\s+funding)', lambda m: "Seed"),
        (r'(?:Pre-Seed)', lambda m: "Pre-Seed"),
        (r'(?:IPO|went\s+public)', lambda m: "IPO"),
        (r'(?:Series\s+([A-F])\+?)', lambda m: f"Series {m.group(1)}"),
    ]
    for pattern, formatter in stage_patterns:
        m = re.search(pattern, text, re.I)
        if m:
            info["funding_stage"] = formatter(m)
            break

    # Investors
    investor_pattern = r'(?:backed by|led by|investors?(?:\s+include)?|participated)\s+([A-Z][\w\s,&]+?)(?:\.|,\s*and|\s*$)'
    m = re.search(investor_pattern, text, re.I)
    if m:
        investors = m.group(1).strip()
        if len(investors) < 200:
            info["investors"] = investors

    # Revenue mentions
    rev_pattern = r'(?:revenue|ARR|annual\s+recurring)\s*(?:of|reached|hit|crossed)?\s*\$?([\d.]+)\s*(million|billion|M|B|mn|bn)'
    m = re.search(rev_pattern, text, re.I)
    if m:
        amount = float(m.group(1))
        unit = m.group(2).lower()
        if unit in ("billion", "b", "bn"):
            amount *= 1000
        info["revenue_estimate"] = f"${amount}M"

    # Employee count
    emp_pattern = r'(\d[\d,]+)\+?\s*(?:employees|team\s+members|people|staff)'
    m = re.search(emp_pattern, text, re.I)
    if m:
        count = m.group(1).replace(",", "")
        if count.isdigit() and 1 <= int(count) <= 1_000_000:
            info["company_size"] = count

    return info


def _extract_news_from_results(results: List[Dict], company: str) -> List[str]:
    """Extract recent news headlines from search results."""
    news = []
    company_lower = company.lower()
    for r in results:
        title = r.get("title", "")
        body = r.get("body", "")
        if company_lower in title.lower() or company_lower in body.lower():
            headline = title[:120]
            if headline and headline not in news:
                news.append(headline)
    return news[:5]


class CompanyIntelProvider(EnrichmentProvider):
    """Company intelligence from public web sources.

    Replaces Crunchbase API + Owler + CB Insights by scraping
    publicly available data from:
    - Crunchbase public pages
    - Tech news (TechCrunch, Forbes, etc.)
    - Company registration databases
    - Wikipedia/Wikidata

    Extracts: funding amount, stage, investors, revenue,
    company size, recent news mentions.
    """

    name = "company_intel"
    capabilities = [
        "company_size", "industry_tags", "description",
        "founding_year",
    ]
    default_confidence = 0.68

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        company = lead.company or ""
        if not company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No company name available",
                duration_ms=(time.time() - t0) * 1000,
            )

        fields = {}
        all_text = ""

        # ── Search 1: Crunchbase public page ──
        cb_results = await _search_ddg(
            f'site:crunchbase.com "{company}"', max_results=3
        )
        for r in cb_results:
            body = r.get("body", "")
            title = r.get("title", "")
            all_text += f" {title} {body}"

            # Try to scrape the actual Crunchbase page for structured data
            href = r.get("href", "")
            if "crunchbase.com/organization" in href:
                page = await _scrape_page(href)
                if page and BeautifulSoup:
                    soup = BeautifulSoup(page, "html.parser")
                    page_text = soup.get_text(separator=" ", strip=True)
                    all_text += f" {page_text[:5000]}"

                    # Extract from structured data
                    for script in soup.find_all("script", type="application/ld+json"):
                        try:
                            ld = json.loads(script.string or "")
                            if isinstance(ld, dict):
                                if ld.get("description"):
                                    fields.setdefault("description", str(ld["description"])[:500])
                                if ld.get("foundingDate"):
                                    fields["founding_year"] = str(ld["foundingDate"])[:4]
                                if ld.get("numberOfEmployees"):
                                    emp = ld["numberOfEmployees"]
                                    if isinstance(emp, dict):
                                        fields["company_size"] = str(emp.get("value", ""))
                                    else:
                                        fields["company_size"] = str(emp)
                        except Exception:
                            continue
                break  # Only scrape first CB result

        await asyncio.sleep(0.3)

        # ── Search 2: Funding & news ──
        funding_results = await _search_ddg(
            f'"{company}" funding OR raised OR series OR investment',
            max_results=10,
        )
        for r in funding_results:
            all_text += f" {r.get('title', '')} {r.get('body', '')}"

        # ── Search 3: Company news ──
        news_results = await _search_ddg(
            f'"{company}" news announcement 2025 OR 2026',
            max_results=5,
        )
        news_headlines = _extract_news_from_results(news_results, company)

        # ── Extract structured data from all collected text ──
        funding_info = _extract_funding_from_text(all_text)
        fields.update({k: v for k, v in funding_info.items() if v})

        # Add news headlines
        if news_headlines:
            fields["recent_news"] = " | ".join(news_headlines[:3])

        if fields:
            return EnrichmentResult(
                provider=self.name, success=True,
                fields=fields,
                confidence=self.default_confidence,
                duration_ms=(time.time() - t0) * 1000,
            )

        return EnrichmentResult(
            provider=self.name, success=False,
            error="No company intelligence found",
            duration_ms=(time.time() - t0) * 1000,
        )
