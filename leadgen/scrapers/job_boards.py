"""
Job Board Scraper — Find companies actively hiring on Naukri, Indeed, Foundit.

Companies posting jobs are ACTIVE companies with budget — high-quality leads.
Uses DuckDuckGo to discover employer pages and extract company data.
"""

import re
import time
from typing import List
from urllib.parse import urlparse

from ddgs import DDGS
from leadgen.models import Lead


def _extract_phone(text: str) -> str:
    """Extract phone from text."""
    match = re.search(r'(\+?91[\-\s]?\d{10}|\b\d{10}\b|0\d{2,4}[\-\s]?\d{6,8})', text)
    return match.group().strip() if match else ""


def _extract_email(text: str) -> str:
    """Extract email from text."""
    matches = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', text)
    for e in matches:
        if 'example' not in e.lower() and 'domain' not in e.lower():
            return e
    return ""


def scrape_naukri_employers(
    queries: List[str],
    cities: List[str],
    max_results: int = 15,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find companies actively hiring on Naukri.com via search.

    Targets employer profile pages, which indicate companies
    with active recruitment budgets — high value leads.
    """
    leads = []
    seen = set()

    with DDGS() as ddgs:
        for city in cities:
            for query in queries:
                search = f'site:naukri.com "{query}" "{city}" company reviews hiring'
                print(f"  💼 Naukri search: {search}")

                try:
                    results = list(ddgs.text(search, max_results=max_results))
                    for r in results:
                        title = r.get("title", "")
                        body = r.get("body", "")
                        href = r.get("href", "")

                        # Extract company name from Naukri title format
                        # "Company Name Reviews | AmbitionBox" or "Company Name Jobs"
                        name = title.split(" Reviews")[0].split(" Jobs")[0].split(" - ")[0].split(" | ")[0].strip()
                        name = re.sub(r'\s*(Pvt|Ltd|Private|Limited|India|Careers)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or name in seen or len(name) < 3:
                            continue
                        seen.add(name)

                        lead = Lead(
                            company=name,
                            city=city,
                            specialization=query,
                            source="naukri",
                            notes=f"Active on Naukri: {href}",
                        )
                        leads.append(lead)
                        print(f"    ✅ {name} ({city})")

                except Exception as e:
                    print(f"    ⚠ Error: {e}")

                time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from Naukri")
    return leads


def scrape_indeed_employers(
    queries: List[str],
    cities: List[str],
    max_results: int = 15,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find companies actively hiring on Indeed.co.in via search.
    """
    leads = []
    seen = set()

    with DDGS() as ddgs:
        for city in cities:
            for query in queries:
                search = f'site:indeed.co.in/cmp "{query}" "{city}"'
                print(f"  💼 Indeed search: {search}")

                try:
                    results = list(ddgs.text(search, max_results=max_results))
                    for r in results:
                        title = r.get("title", "")
                        href = r.get("href", "")

                        name = title.split(" - ")[0].split(" | ")[0].split(" Reviews")[0].strip()
                        name = re.sub(r'\s*(Pvt|Ltd|Private|Limited|India)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or name in seen or len(name) < 3:
                            continue
                        seen.add(name)

                        lead = Lead(
                            company=name,
                            city=city,
                            specialization=query,
                            source="indeed",
                            notes=f"Active on Indeed: {href}",
                        )
                        leads.append(lead)
                        print(f"    ✅ {name} ({city})")

                except Exception as e:
                    print(f"    ⚠ Error: {e}")

                time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from Indeed")
    return leads


def scrape_foundit_employers(
    queries: List[str],
    cities: List[str],
    max_results: int = 10,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Find companies on Foundit.in (formerly Monster India).
    """
    leads = []
    seen = set()

    with DDGS() as ddgs:
        for city in cities:
            for query in queries:
                search = f'site:foundit.in "{query}" "{city}" company'
                print(f"  💼 Foundit search: {search}")

                try:
                    results = list(ddgs.text(search, max_results=max_results))
                    for r in results:
                        title = r.get("title", "")
                        href = r.get("href", "")

                        name = title.split(" - ")[0].split(" | ")[0].split(" Jobs")[0].strip()
                        name = re.sub(r'\s*(Pvt|Ltd|Private|Limited|India)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or name in seen or len(name) < 3:
                            continue
                        seen.add(name)

                        lead = Lead(
                            company=name,
                            city=city,
                            specialization=query,
                            source="foundit",
                            notes=f"Active on Foundit: {href}",
                        )
                        leads.append(lead)
                        print(f"    ✅ {name} ({city})")

                except Exception as e:
                    print(f"    ⚠ Error: {e}")

                time.sleep(delay)

    print(f"  📊 Found {len(leads)} companies from Foundit")
    return leads
