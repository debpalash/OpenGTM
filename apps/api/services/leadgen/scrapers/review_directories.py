"""
B2B Review Directory Scraper — Clutch.co, GoodFirms, G2.

These directories list VERIFIED, REVIEWED companies — indicating they're
active, established, and investing in visibility. Extremely high-quality leads.
"""

import re
import time
from typing import List

from ddgs import DDGS
from apps.api.services.leadgen.proxy_client import get_ddgs
from apps.api.services.leadgen.models import Lead


def scrape_clutch(
    queries: List[str],
    max_results: int = 20,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find HR/staffing companies listed on Clutch.co.

    Clutch-listed companies are verified, reviewed, and actively seeking clients.
    """
    leads = []
    seen = set()

    with get_ddgs() as ddgs:
        for query in queries:
            search = f'site:clutch.co "{query}" India staffing HR'
            print(f"  ⭐ Clutch search: {search}")

            try:
                results = list(ddgs.text(search, max_results=max_results))
                for r in results:
                    title = r.get("title", "")
                    body = r.get("body", "")
                    href = r.get("href", "")

                    # Clutch company pages: "Company Name | Clutch.co"
                    name = title.split(" | ")[0].split(" - ")[0].strip()
                    name = re.sub(r'\s*Company Profile\s*$', '', name, flags=re.IGNORECASE).strip()
                    name = re.sub(r'\s*Reviews?\s*$', '', name, flags=re.IGNORECASE).strip()

                    if not name or name in seen or len(name) < 3:
                        continue
                    if name.lower() in ('clutch', 'clutch.co', 'top companies'):
                        continue
                    seen.add(name)

                    # Extract location from body
                    city = ""
                    for c in ["Bangalore", "Mumbai", "Delhi", "Hyderabad", "Pune",
                              "Chennai", "Noida", "Gurgaon", "Kolkata", "Ahmedabad"]:
                        if c.lower() in body.lower():
                            city = c
                            break

                    lead = Lead(
                        company=name,
                        city=city or "India",
                        specialization=query,
                        source="clutch",
                        description=body[:200] if body else "",
                        notes=f"Clutch profile: {href}",
                    )
                    leads.append(lead)
                    print(f"    ✅ {name} ({city or 'India'})")

            except Exception as e:
                print(f"    ⚠ Error: {e}")

            time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from Clutch")
    return leads


def scrape_goodfirms(
    queries: List[str],
    max_results: int = 20,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find companies listed on GoodFirms.co — another B2B review platform.
    """
    leads = []
    seen = set()

    with get_ddgs() as ddgs:
        for query in queries:
            search = f'site:goodfirms.co "{query}" India'
            print(f"  ⭐ GoodFirms search: {search}")

            try:
                results = list(ddgs.text(search, max_results=max_results))
                for r in results:
                    title = r.get("title", "")
                    body = r.get("body", "")
                    href = r.get("href", "")

                    name = title.split(" | ")[0].split(" - ")[0].split(" Reviews")[0].strip()

                    if not name or name in seen or len(name) < 3:
                        continue
                    if name.lower() in ('goodfirms', 'goodfirms.co'):
                        continue
                    seen.add(name)

                    city = ""
                    for c in ["Bangalore", "Mumbai", "Delhi", "Hyderabad", "Pune",
                              "Chennai", "Noida", "Gurgaon", "Kolkata", "Ahmedabad"]:
                        if c.lower() in body.lower():
                            city = c
                            break

                    lead = Lead(
                        company=name,
                        city=city or "India",
                        specialization=query,
                        source="goodfirms",
                        description=body[:200] if body else "",
                        notes=f"GoodFirms profile: {href}",
                    )
                    leads.append(lead)
                    print(f"    ✅ {name} ({city or 'India'})")

            except Exception as e:
                print(f"    ⚠ Error: {e}")

            time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from GoodFirms")
    return leads


def scrape_g2(
    queries: List[str],
    max_results: int = 15,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find companies on G2.com — enterprise software review platform.
    Good for finding HR tech / staffing companies using or competing with Yupcha.
    """
    leads = []
    seen = set()

    with get_ddgs() as ddgs:
        for query in queries:
            search = f'site:g2.com "{query}" India staffing recruitment'
            print(f"  ⭐ G2 search: {search}")

            try:
                results = list(ddgs.text(search, max_results=max_results))
                for r in results:
                    title = r.get("title", "")
                    body = r.get("body", "")
                    href = r.get("href", "")

                    name = title.split(" Reviews")[0].split(" | ")[0].split(" - ")[0].strip()

                    if not name or name in seen or len(name) < 3:
                        continue
                    if name.lower() in ('g2', 'g2.com'):
                        continue
                    seen.add(name)

                    lead = Lead(
                        company=name,
                        city="India",
                        specialization=query,
                        source="g2",
                        description=body[:200] if body else "",
                        notes=f"G2 profile: {href}",
                    )
                    leads.append(lead)
                    print(f"    ✅ {name}")

            except Exception as e:
                print(f"    ⚠ Error: {e}")

            time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from G2")
    return leads


def scrape_ambitionbox(
    queries: List[str],
    cities: List[str],
    max_results: int = 15,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find companies on AmbitionBox (Naukri's company review platform).

    Companies reviewed here are actively employing — strong signal.
    """
    leads = []
    seen = set()

    with get_ddgs() as ddgs:
        for city in cities:
            for query in queries:
                search = f'site:ambitionbox.com "{query}" "{city}" reviews'
                print(f"  ⭐ AmbitionBox search: {search}")

                try:
                    results = list(ddgs.text(search, max_results=max_results))
                    for r in results:
                        title = r.get("title", "")
                        body = r.get("body", "")
                        href = r.get("href", "")

                        name = title.split(" Reviews")[0].split(" | ")[0].split(" - ")[0].strip()
                        name = re.sub(r'\s*(Pvt|Ltd|Private|Limited|India)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or name in seen or len(name) < 3:
                            continue
                        if name.lower() in ('ambitionbox',):
                            continue
                        seen.add(name)

                        # Try to extract employee count
                        size = ""
                        size_match = re.search(r'(\d[\d,]+)\s*(?:employees|people)', body, re.IGNORECASE)
                        if size_match:
                            count = int(size_match.group(1).replace(",", ""))
                            if count < 50:
                                size = "1-50"
                            elif count < 200:
                                size = "51-200"
                            elif count < 500:
                                size = "201-500"
                            else:
                                size = "500+"

                        lead = Lead(
                            company=name,
                            city=city,
                            specialization=query,
                            company_size=size,
                            source="ambitionbox",
                            notes=f"AmbitionBox: {href}",
                        )
                        leads.append(lead)
                        print(f"    ✅ {name} ({city}) {size}")

                except Exception as e:
                    print(f"    ⚠ Error: {e}")

                time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from AmbitionBox")
    return leads
