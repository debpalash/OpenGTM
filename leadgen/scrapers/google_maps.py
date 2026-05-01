"""
Google Maps Scraper — Discover HR/staffing companies via Google Maps.

Uses Patchright (stealth headless browser) to search Google Maps for
companies matching target queries in target cities. Extracts company name,
address, phone, website, rating, and review count.
"""

import asyncio
import re
from typing import List, Optional
from dataclasses import dataclass

from leadgen.models import Lead

try:
    from patchright.async_api import async_playwright
except ImportError:
    async_playwright = None


@dataclass
class MapResult:
    name: str = ""
    address: str = ""
    phone: str = ""
    website: str = ""
    rating: str = ""
    reviews: str = ""
    category: str = ""


async def scrape_google_maps(
    query: str,
    city: str,
    max_results: int = 20,
    headless: bool = True,
) -> List[Lead]:
    """
    Search Google Maps for businesses matching query in city.

    Args:
        query: Search term, e.g. "HR staffing agency"
        city: City name, e.g. "Bangalore"
        max_results: Maximum number of results to collect
        headless: Run browser in headless mode

    Returns:
        List of Lead objects with data from Maps
    """
    if async_playwright is None:
        print("  ⚠ patchright not installed. Run: pip install patchright")
        return []

    search_term = f"{query} in {city}"
    print(f"  🗺️  Searching Google Maps: '{search_term}'")

    leads = []

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=headless)
        context = await browser.new_context(
            viewport={"width": 1280, "height": 900},
            locale="en-US",
        )
        page = await context.new_page()

        try:
            maps_url = f"https://www.google.com/maps/search/{query}+in+{city}"
            await page.goto(maps_url, wait_until="domcontentloaded", timeout=20000)
            await asyncio.sleep(3)

            # Scroll the results panel to load more listings
            results_panel = page.locator('[role="feed"]')
            if await results_panel.count() > 0:
                for _ in range(5):
                    await results_panel.evaluate("el => el.scrollTop = el.scrollHeight")
                    await asyncio.sleep(1.5)

            # Extract listing links
            listing_links = await page.locator('a[href*="/maps/place/"]').all()
            print(f"  📍 Found {len(listing_links)} map listings")

            seen_names = set()
            for i, link in enumerate(listing_links[:max_results]):
                try:
                    await link.click()
                    await asyncio.sleep(2)

                    # Extract details from the side panel
                    name = ""
                    name_el = page.locator('h1')
                    if await name_el.count() > 0:
                        name = (await name_el.first.text_content() or "").strip()

                    if not name or name in seen_names:
                        continue
                    seen_names.add(name)

                    # Extract info items (address, phone, website, etc.)
                    phone = ""
                    website = ""
                    address = ""
                    category = ""

                    # Phone: look for tel: links or phone button
                    phone_btn = page.locator('[data-tooltip="Copy phone number"]')
                    if await phone_btn.count() > 0:
                        phone_text = await phone_btn.first.text_content()
                        if phone_text:
                            phone = phone_text.strip()

                    # Phone fallback: aria-label containing phone
                    if not phone:
                        phone_el = page.locator('[aria-label*="Phone"]')
                        if await phone_el.count() > 0:
                            phone_label = await phone_el.first.get_attribute("aria-label") or ""
                            phone_match = re.search(r'[\d\-\+\(\)\s]{7,}', phone_label)
                            if phone_match:
                                phone = phone_match.group().strip()

                    # Website
                    website_btn = page.locator('[data-tooltip="Open website"]')
                    if await website_btn.count() > 0:
                        website = await website_btn.first.get_attribute("href") or ""

                    if not website:
                        website_el = page.locator('a[aria-label*="Website"]')
                        if await website_el.count() > 0:
                            website = await website_el.first.get_attribute("href") or ""

                    # Address
                    addr_btn = page.locator('[data-tooltip="Copy address"]')
                    if await addr_btn.count() > 0:
                        address = (await addr_btn.first.text_content() or "").strip()

                    # Category/type
                    cat_el = page.locator('button[jsaction*="category"]')
                    if await cat_el.count() > 0:
                        category = (await cat_el.first.text_content() or "").strip()

                    lead = Lead(
                        company=name,
                        website=website,
                        phone=phone,
                        city=city,
                        specialization=category or query,
                        notes=f"Address: {address}" if address else "",
                        source="google_maps",
                    )
                    leads.append(lead)
                    print(f"    ✅ {name} | {phone or 'no phone'} | {website or 'no website'}")

                except Exception as e:
                    print(f"    ⚠ Error extracting listing {i}: {e}")
                    continue

        except Exception as e:
            print(f"  ❌ Google Maps scrape error: {e}")
        finally:
            await browser.close()

    print(f"  📊 Collected {len(leads)} leads from Google Maps")
    return leads


async def scrape_maps_multi_city(
    queries: List[str],
    cities: List[str],
    max_per_query: int = 20,
    headless: bool = True,
) -> List[Lead]:
    """Run Google Maps scraper across multiple queries and cities."""
    all_leads = []
    for city in cities:
        for query in queries:
            leads = await scrape_google_maps(query, city, max_per_query, headless)
            all_leads.extend(leads)
            await asyncio.sleep(3)  # Rate limiting between searches
    return all_leads
