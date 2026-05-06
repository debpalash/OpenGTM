"""
Crunchbase Scraper — Business intelligence and funding data.

Uses DuckDuckGo search to find Crunchbase organization profiles
and extracts company names, industries, and locations.
"""

import time
import re
from typing import List
from urllib.parse import urlparse

from ddgs import DDGS
from apps.api.services.leadgen.proxy_client import get_ddgs
from apps.api.services.leadgen.models import Lead


def scrape_via_search(
    queries: List[str],
    cities: List[str],
    max_results_per_query: int = 10,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Use DuckDuckGo to find Crunchbase company profiles.
    """
    leads = []
    seen_companies = set()

    with get_ddgs() as ddgs:
        for city in cities:
            for query in queries:
                search_query = f"{query} {city} site:crunchbase.com/organization"
                print(f"  🔍 Searching Crunchbase: {search_query}")

                try:
                    results = list(ddgs.text(search_query, max_results=max_results_per_query))

                    for r in results:
                        title = r.get("title", "")
                        body = r.get("body", "")
                        href = r.get("href", "")

                        # Extract company name from title
                        # Crunchbase titles often look like "Company Name - Funding, Financials..."
                        # or "Company Name - Crunchbase Company Profile & Funding"
                        company_name = title.split(" - ")[0].split(" | ")[0].strip()

                        if not company_name or company_name in seen_companies:
                            continue
                        if len(company_name) < 2 or len(company_name) > 100:
                            continue

                        seen_companies.add(company_name)

                        # Try to extract website from body or construct it
                        domain_match = re.search(r'([a-zA-Z0-9-]+\.[a-zA-Z]{2,})', body)
                        website = ""
                        if domain_match:
                            website = f"https://www.{domain_match.group(1)}"
                            
                        # Look for funding or employee hints in snippet
                        notes = "Source: Crunchbase"
                        if "funding" in body.lower() or "raised" in body.lower():
                            notes += " | Has Funding"

                        lead = Lead(
                            company=company_name,
                            website=website,
                            city=city,
                            specialization=query,
                            source="crunchbase",
                            notes=notes,
                        )
                        leads.append(lead)
                        print(f"    ✅ {company_name} ({city})")

                except Exception as e:
                    print(f"    ⚠ Search error: {e}")

                time.sleep(delay)

    print(f"\n  📊 Collected {len(leads)} leads from Crunchbase")
    return leads
