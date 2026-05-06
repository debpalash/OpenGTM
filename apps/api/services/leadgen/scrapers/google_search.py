"""
Google Search Scraper — Broad web search for lead discovery.

Uses DuckDuckGo to perform general web searches for HR/staffing companies,
extracting company data from search result snippets. Acts as a catch-all
source that covers websites, news articles, and directories not specifically
targeted by other scrapers.
"""

import re
import time
from typing import List
from urllib.parse import urlparse

from ddgs import DDGS
from apps.api.services.leadgen.proxy_client import get_ddgs
from apps.api.services.leadgen.models import Lead


# Domains to skip (these are covered by dedicated scrapers)
SKIP_DOMAINS = {
    "linkedin.com", "facebook.com", "twitter.com", "x.com",
    "youtube.com", "wikipedia.org", "instagram.com",
    "justdial.com", "sulekha.com", "indiamart.com",
    "naukri.com", "indeed.com", "foundit.in",
    "clutch.co", "goodfirms.co", "g2.com", "ambitionbox.com",
    "glassdoor.com", "glassdoor.co.in",
}


def _is_skip_domain(url: str) -> bool:
    """Check if URL is from a domain we already scrape separately."""
    try:
        domain = urlparse(url).netloc.lower()
        return any(sd in domain for sd in SKIP_DOMAINS)
    except Exception:
        return False


def scrape_google_search(
    queries: List[str],
    cities: List[str],
    max_results: int = 15,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Broad web search for HR/staffing companies.

    Searches for company websites directly (not via directories).
    Good for discovering smaller, local agencies that don't appear on
    major platforms.
    """
    leads = []
    seen = set()

    with get_ddgs() as ddgs:
        for city in cities:
            for query in queries:
                search = f'"{query}" "{city}" India contact email phone'
                print(f"  🔍 Web search: {search}")

                try:
                    results = list(ddgs.text(search, max_results=max_results))
                    for r in results:
                        title = r.get("title", "")
                        body = r.get("body", "")
                        href = r.get("href", "")

                        if _is_skip_domain(href):
                            continue

                        # Company name from title
                        name = title.split(" - ")[0].split(" | ")[0].strip()
                        name = re.sub(r'\s*(Home|Contact|About|Services|Welcome to)\s*', '', name, flags=re.IGNORECASE).strip()
                        name = re.sub(r'\s*(Pvt|Ltd|Private|Limited)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or name in seen or len(name) < 3 or len(name) > 80:
                            continue
                        seen.add(name)

                        # Extract data from snippet
                        combined = f"{title} {body}"
                        phone_match = re.search(r'(\+?91[\-\s]?\d{10}|\b\d{10}\b|0\d{2,4}[\-\s]?\d{6,8})', combined)
                        email_match = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', combined)

                        phone = phone_match.group().strip() if phone_match else ""
                        email = ""
                        for e in email_match:
                            if 'example' not in e.lower():
                                email = e
                                break

                        lead = Lead(
                            company=name,
                            website=href if not _is_skip_domain(href) else "",
                            phone=phone,
                            email=email,
                            city=city,
                            specialization=query,
                            source="google_search",
                        )
                        leads.append(lead)
                        status = f"{phone or 'no phone'} | {email or 'no email'}"
                        print(f"    ✅ {name} | {status}")

                except Exception as e:
                    print(f"    ⚠ Error: {e}")

                time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from web search")
    return leads


def scrape_news_mentions(
    queries: List[str],
    max_results: int = 20,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Search recent news for HR/staffing companies being mentioned.

    Companies in the news are actively growing / fundraising / expanding —
    excellent leads.
    """
    leads = []
    seen = set()

    with get_ddgs() as ddgs:
        for query in queries:
            search = f'"{query}" India funding OR expansion OR launched OR partnership 2025 2026'
            print(f"  📰 News search: {search}")

            try:
                results = list(ddgs.news(search, max_results=max_results))
                for r in results:
                    title = r.get("title", "")
                    body = r.get("body", "")
                    source_url = r.get("url", "")

                    # Try to find company names mentioned
                    # Look for capitalized multi-word names before keywords
                    name_patterns = re.findall(
                        r'([A-Z][a-zA-Z]+(?:\s+[A-Z][a-zA-Z]+){0,3})\s+(?:raises?|launches?|partners?|expands?|announces?|acquires?)',
                        title + " " + body
                    )

                    for name in name_patterns:
                        name = name.strip()
                        if name in seen or len(name) < 3:
                            continue
                        if name.lower() in ('the company', 'the firm', 'india'):
                            continue
                        seen.add(name)

                        lead = Lead(
                            company=name,
                            city="India",
                            specialization=query,
                            source="news",
                            notes=f"In news: {title[:100]}",
                            description=body[:200] if body else "",
                        )
                        leads.append(lead)
                        print(f"    📰 {name} — {title[:60]}")

            except Exception as e:
                print(f"    ⚠ News search error: {e}")

            time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from news")
    return leads
