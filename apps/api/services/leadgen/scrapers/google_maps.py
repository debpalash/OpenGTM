"""
Google Maps Scraper — Discover companies via Google Maps.

Uses Patchright (stealth headless browser) to search Google Maps for
companies matching target queries in target cities. Extracts company name,
address, phone, website, rating, and review count.

Enhanced with multiple fallback selectors for reliability.
"""

import asyncio
import logging
import re
from typing import List, Optional
from dataclasses import dataclass

from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.scrapers.google_maps")

# Prefer patchright (stealth-patched Playwright). Fall back to vanilla
# playwright (a root dependency) when patchright isn't installed, so Google Maps
# scraping still works on a stock install instead of silently returning nothing.
try:
    from patchright.async_api import async_playwright
except ImportError:
    try:
        from playwright.async_api import async_playwright
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


async def _extract_text(locator, timeout: int = 3000) -> str:
    """Safely extract text content from a locator."""
    try:
        if await locator.count() > 0:
            return (await locator.first.text_content(timeout=timeout) or "").strip()
    except Exception:
        pass
    return ""


async def _extract_attr(locator, attr: str, timeout: int = 3000) -> str:
    """Safely extract an attribute from a locator."""
    try:
        if await locator.count() > 0:
            return (await locator.first.get_attribute(attr, timeout=timeout) or "").strip()
    except Exception:
        pass
    return ""


async def _extract_from_aria(page, keyword: str) -> str:
    """Extract text from an element with aria-label containing keyword."""
    try:
        el = page.locator(f'[aria-label*="{keyword}"]')
        if await el.count() > 0:
            label = await el.first.get_attribute("aria-label") or ""
            # For phone: extract digits
            if keyword.lower() in ("phone", "call"):
                match = re.search(r'[\d\-\+\(\)\s]{7,}', label)
                if match:
                    return match.group().strip()
            return label
    except Exception:
        pass
    return ""


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
        logger.warning(
            "No headless browser available (patchright/playwright not installed) — "
            "skipping Google Maps scrape. Install with: uv add patchright && "
            "uv run patchright install chromium"
        )
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

            # Accept cookies if prompted
            try:
                consent = page.locator('button:has-text("Accept all")')
                if await consent.count() > 0:
                    await consent.first.click()
                    await asyncio.sleep(1)
            except Exception:
                pass

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

                    # ── Name ──────────────────────────────────
                    name = await _extract_text(page.locator('h1'))
                    if not name:
                        name = await _extract_text(page.locator('[data-attrid="title"]'))
                    if not name or name in seen_names:
                        continue
                    seen_names.add(name)

                    # ── Phone (multiple strategies) ───────────
                    phone = ""
                    # Strategy 1: Copy button
                    phone = await _extract_text(page.locator('[data-tooltip="Copy phone number"]'))
                    # Strategy 2: aria-label
                    if not phone:
                        phone = await _extract_from_aria(page, "Phone")
                    # Strategy 3: tel: links
                    if not phone:
                        phone = await _extract_attr(page.locator('a[href^="tel:"]'), "href")
                        if phone:
                            phone = phone.replace("tel:", "").strip()
                    # Strategy 4: info button text containing digits
                    if not phone:
                        info_buttons = await page.locator('button[data-item-id*="phone"]').all()
                        for btn in info_buttons[:1]:
                            try:
                                phone = (await btn.text_content(timeout=2000) or "").strip()
                            except Exception:
                                pass

                    # ── Website (multiple strategies) ──────────
                    website = ""
                    website = await _extract_attr(page.locator('[data-tooltip="Open website"]'), "href")
                    if not website:
                        website = await _extract_attr(page.locator('a[aria-label*="Website"]'), "href")
                    if not website:
                        website = await _extract_attr(page.locator('a[data-item-id="authority"]'), "href")

                    # ── Address ────────────────────────────────
                    address = await _extract_text(page.locator('[data-tooltip="Copy address"]'))
                    if not address:
                        address = await _extract_from_aria(page, "Address")

                    # ── Category ───────────────────────────────
                    category = await _extract_text(page.locator('button[jsaction*="category"]'))
                    if not category:
                        category = await _extract_text(page.locator('[class*="fontBodyMedium"] button'))

                    # ── Rating ─────────────────────────────────
                    rating = ""
                    try:
                        rating_el = page.locator('[class*="fontDisplayLarge"]')
                        if await rating_el.count() > 0:
                            rating = (await rating_el.first.text_content(timeout=2000) or "").strip()
                    except Exception:
                        pass

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

