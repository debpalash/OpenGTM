"""
LinkedIn Company Discovery — Find company LinkedIn profiles via search.

Uses DuckDuckGo with site:linkedin.com queries to discover company
LinkedIn pages and extract basic company information.
"""

import re
import time
from typing import List
from urllib.parse import urlparse

from ddgs import DDGS
from apps.api.services.leadgen.models import Lead


def discover_linkedin_companies(
    queries: List[str],
    cities: List[str],
    max_results: int = 10,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Search DuckDuckGo for LinkedIn company pages matching queries.

    Args:
        queries: Industry search terms, e.g. ["staffing company", "HR agency"]
        cities: Cities to search in
        max_results: Max results per query
        delay: Seconds between queries to avoid rate limiting

    Returns:
        List of Lead objects with LinkedIn data
    """
    leads = []
    seen_urls = set()

    with DDGS() as ddgs:
        for city in cities:
            for query in queries:
                search_query = f'site:linkedin.com/company "{query}" "{city}" India'
                print(f"  🔗 LinkedIn search: {search_query}")

                try:
                    results = list(ddgs.text(search_query, max_results=max_results))

                    for r in results:
                        href = r.get("href", "")
                        title = r.get("title", "")
                        body = r.get("body", "")

                        # Only keep actual company pages
                        if "/company/" not in href:
                            continue

                        # Normalize URL
                        parsed = urlparse(href)
                        company_path = parsed.path.rstrip("/")
                        linkedin_url = f"https://www.linkedin.com{company_path}"

                        if linkedin_url in seen_urls:
                            continue
                        seen_urls.add(linkedin_url)

                        # Extract company name from title
                        # LinkedIn title format: "Company Name | LinkedIn"
                        company_name = title.split(" | ")[0].split(" - ")[0].strip()
                        if not company_name or company_name.lower() == "linkedin":
                            continue

                        # Extract employee count if mentioned
                        company_size = ""
                        size_match = re.search(
                            r'(\d[\d,]+)\s*(?:employees|followers)', body, re.IGNORECASE
                        )
                        if size_match:
                            count = int(size_match.group(1).replace(",", ""))
                            if count < 50:
                                company_size = "1-50"
                            elif count < 200:
                                company_size = "51-200"
                            elif count < 500:
                                company_size = "201-500"
                            else:
                                company_size = "500+"

                        # Extract description
                        description = body[:200] if body else ""

                        lead = Lead(
                            company=company_name,
                            city=city,
                            linkedin_url=linkedin_url,
                            company_size=company_size,
                            description=description,
                            specialization=query,
                            source="linkedin",
                        )
                        leads.append(lead)
                        print(f"    ✅ {company_name} | {linkedin_url} | {company_size or 'size unknown'}")

                except Exception as e:
                    print(f"    ⚠ LinkedIn search error: {e}")

                time.sleep(delay)

    print(f"\n  📊 Discovered {len(leads)} companies on LinkedIn")
    return leads


def find_decision_makers(
    company_name: str,
    titles: List[str] = None,
    max_results: int = 5,
) -> List[dict]:
    """
    Search for decision-makers at a company via LinkedIn search.

    Returns list of dicts with name, title, linkedin_url.
    """
    if titles is None:
        titles = ["CEO", "CTO", "CHRO", "HR Director", "Founder", "Managing Director"]

    people = []

    with DDGS() as ddgs:
        for title in titles:
            query = f'site:linkedin.com/in "{company_name}" "{title}" India'
            try:
                results = list(ddgs.text(query, max_results=2))
                for r in results:
                    href = r.get("href", "")
                    result_title = r.get("title", "")

                    if "/in/" not in href:
                        continue

                    person_name = result_title.split(" - ")[0].split(" | ")[0].strip()
                    if person_name and person_name.lower() != "linkedin":
                        people.append({
                            "name": person_name,
                            "title": title,
                            "linkedin_url": href,
                        })
                        print(f"    👤 {person_name} ({title}) at {company_name}")
                        break  # One person per title is enough

            except Exception:
                pass

            time.sleep(1.5)

    return people
