"""
Web Directory Scraper — JustDial, Sulekha, and general business directories.

Uses DuckDuckGo search to find listings from Indian business directories,
then extracts company data from the search results and/or pages.
"""

import asyncio
import re
import time
from typing import List
from urllib.parse import urlparse

from duckduckgo_search import DDGS
from leadgen.models import Lead

try:
    from patchright.async_api import async_playwright
    from bs4 import BeautifulSoup
except ImportError:
    async_playwright = None
    BeautifulSoup = None


def _extract_phone(text: str) -> str:
    """Extract an Indian phone number from text."""
    matches = re.findall(
        r'(\+?91[\-\s]?\d{10}|\b1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}\b|\b\d{3,4}[\-\s]?\d{6,8}\b|\b\d{10}\b)',
        text
    )
    return matches[0] if matches else ""


def _extract_email(text: str) -> str:
    """Extract email from text."""
    matches = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w+', text)
    return matches[0] if matches else ""


def scrape_via_search(
    queries: List[str],
    cities: List[str],
    max_results_per_query: int = 10,
    delay: float = 2.0,
) -> List[Lead]:
    """
    Use DuckDuckGo to find business directory listings.

    Targets: JustDial, Sulekha, IndiaMART, Glassdoor, AmbitionBox
    """
    leads = []
    seen_companies = set()

    directory_sites = [
        "site:justdial.com",
        "site:sulekha.com",
        "site:indiamart.com",
    ]

    with DDGS() as ddgs:
        for city in cities:
            for query in queries:
                for site_filter in directory_sites:
                    search_query = f"{query} {city} {site_filter}"
                    print(f"  🔍 Searching: {search_query}")

                    try:
                        results = list(ddgs.text(search_query, max_results=max_results_per_query))

                        for r in results:
                            title = r.get("title", "")
                            body = r.get("body", "")
                            href = r.get("href", "")
                            combined_text = f"{title} {body}"

                            # Extract company name from title
                            # JustDial: "Company Name - City | Justdial"
                            # Sulekha: "Company Name in City"
                            company_name = title.split(" - ")[0].split(" | ")[0].split(" in ")[0].strip()
                            # Clean up common suffixes
                            for suffix in ["Reviews", "Ratings", "Price", "Contact", "Address"]:
                                company_name = company_name.replace(suffix, "").strip()

                            if not company_name or company_name in seen_companies:
                                continue
                            if len(company_name) < 3 or len(company_name) > 100:
                                continue

                            seen_companies.add(company_name)

                            phone = _extract_phone(combined_text)
                            email = _extract_email(combined_text)

                            # Determine source domain
                            domain = urlparse(href).netloc if href else ""
                            source_tag = "web_directory"
                            if "justdial" in domain:
                                source_tag = "justdial"
                            elif "sulekha" in domain:
                                source_tag = "sulekha"
                            elif "indiamart" in domain:
                                source_tag = "indiamart"

                            lead = Lead(
                                company=company_name,
                                phone=phone,
                                email=email,
                                city=city,
                                specialization=query,
                                source=source_tag,
                                notes=f"Found via {domain}",
                            )
                            leads.append(lead)
                            print(f"    ✅ {company_name} ({city}) | {phone or 'no phone'}")

                    except Exception as e:
                        print(f"    ⚠ Search error: {e}")

                    time.sleep(delay)

    print(f"\n  📊 Collected {len(leads)} leads from web directories")
    return leads


async def scrape_justdial_page(url: str, city: str, headless: bool = True) -> List[Lead]:
    """
    Scrape a JustDial listing page directly for company details.
    Requires Patchright for stealth browsing.
    """
    if async_playwright is None or BeautifulSoup is None:
        print("  ⚠ patchright/beautifulsoup4 not installed")
        return []

    leads = []
    print(f"  🌐 Scraping JustDial page: {url}")

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=headless)
        context = await browser.new_context()
        page = await context.new_page()

        try:
            await page.goto(url, wait_until="domcontentloaded", timeout=15000)
            await asyncio.sleep(2)

            content = await page.content()
            soup = BeautifulSoup(content, "html.parser")

            # JustDial listing cards
            cards = soup.select(".store-details, .jsx-card, .resultbox_info")
            for card in cards:
                name_el = card.select_one(".store-name, .resultbox_title a, .lng_cont_name")
                phone_el = card.select_one(".contact-info, .resultbox_phone, .lng_cont_number")
                addr_el = card.select_one(".address-info, .resultbox_address, .cont_sw_addr")

                name = name_el.get_text(strip=True) if name_el else ""
                phone = _extract_phone(phone_el.get_text() if phone_el else "")
                address = addr_el.get_text(strip=True) if addr_el else ""

                if name:
                    lead = Lead(
                        company=name,
                        phone=phone,
                        city=city,
                        notes=f"Address: {address}" if address else "",
                        source="justdial",
                    )
                    leads.append(lead)

        except Exception as e:
            print(f"  ❌ JustDial scrape error: {e}")
        finally:
            await browser.close()

    print(f"  📊 Collected {len(leads)} leads from JustDial page")
    return leads
