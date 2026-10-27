"""Google Maps place discovery (ported from legacy leadgen/scrapers/google_maps.py).

Opens the Maps search for "<query> in <city>", scrolls the result feed, clicks
each listing and reads name, phone, website, address, category and rating with
the same fallback selectors as the legacy scraper. Returns raw places; the Go
side maps them to leads.

Deviation: the search URL is percent-encoded (legacy interpolated raw text).
"""

from __future__ import annotations

import logging
import re
from typing import Any
from urllib.parse import quote_plus

from ..browser import BrowserUnavailable
from ..engines import Deadline, Deps, _goto
from ..errors import ServiceError
from ..models import GoogleMapsRequest, GoogleMapsResult, MapPlace

logger = logging.getLogger("browser_service.scrapers.google_maps")


def search_url(query: str, city: str) -> str:
    return f"https://www.google.com/maps/search/{quote_plus(query)}+in+{quote_plus(city)}"


async def _text(locator: Any, wait_ms: int = 3000) -> str:
    try:
        if await locator.count() > 0:
            return (await locator.first.text_content(timeout=wait_ms) or "").strip()
    except Exception:  # noqa: S110 - a missing field is normal
        pass
    return ""


async def _attr(locator: Any, attr: str, wait_ms: int = 3000) -> str:
    try:
        if await locator.count() > 0:
            return (await locator.first.get_attribute(attr, timeout=wait_ms) or "").strip()
    except Exception:  # noqa: S110
        pass
    return ""


async def _aria(page: Any, keyword: str) -> str:
    try:
        el = page.locator(f'[aria-label*="{keyword}"]')
        if await el.count() > 0:
            label = await el.first.get_attribute("aria-label") or ""
            if keyword.lower() in ("phone", "call"):
                m = re.search(r"[\d\-\+\(\)\s]{7,}", label)
                if m:
                    return m.group().strip()
            return label
    except Exception:  # noqa: S110
        pass
    return ""


async def extract_place(page: Any) -> MapPlace:
    """Read the open place panel (legacy selector order)."""
    name = await _text(page.locator("h1")) or await _text(page.locator('[data-attrid="title"]'))

    phone = await _text(page.locator('[data-tooltip="Copy phone number"]'))
    if not phone:
        phone = await _aria(page, "Phone")
    if not phone:
        phone = (await _attr(page.locator('a[href^="tel:"]'), "href")).replace("tel:", "").strip()
    if not phone:
        for btn in (await page.locator('button[data-item-id*="phone"]').all())[:1]:
            try:
                phone = (await btn.text_content(timeout=2000) or "").strip()
            except Exception:  # noqa: S110
                pass

    website = (
        await _attr(page.locator('[data-tooltip="Open website"]'), "href")
        or await _attr(page.locator('a[aria-label*="Website"]'), "href")
        or await _attr(page.locator('a[data-item-id="authority"]'), "href")
    )
    address = await _text(page.locator('[data-tooltip="Copy address"]')) or await _aria(page, "Address")
    category = await _text(page.locator('button[jsaction*="category"]')) or await _text(
        page.locator('[class*="fontBodyMedium"] button')
    )
    rating = await _text(page.locator('[class*="fontDisplayLarge"]'), wait_ms=2000)
    return MapPlace(name=name, address=address, phone=phone, website=website, rating=rating, category=category)


async def scrape(deps: Deps, req: GoogleMapsRequest, deadline: Deadline) -> GoogleMapsResult:
    url = search_url(req.query, req.city)
    places: list[MapPlace] = []
    errors = 0
    partial = False
    found = 0
    session = deps.proxy.open_session(deps.settings.max_download_bytes, upstream=req.upstream())
    pool = deps.stealth_pool if deps.stealth_pool is not None else deps.pool
    try:
        async with pool.page(session, locale="en-US") as page:
            await _goto(page, session, url, "domcontentloaded", deadline)
            await page.wait_for_timeout(3000)
            try:
                consent = page.locator('button:has-text("Accept all")')
                if await consent.count() > 0:
                    await consent.first.click()
                    await page.wait_for_timeout(1000)
            except Exception:  # noqa: S110
                pass
            feed = page.locator('[role="feed"]')
            if await feed.count() > 0:
                for _ in range(5):
                    if deadline.left() < 5:
                        break
                    await feed.evaluate("el => el.scrollTop = el.scrollHeight")
                    await page.wait_for_timeout(1500)
            links = await page.locator('a[href*="/maps/place/"]').all()
            found = len(links)
            seen: set[str] = set()
            for link in links[: req.max_results]:
                if deadline.left() < 4:
                    partial = True
                    break
                try:
                    await link.click()
                    await page.wait_for_timeout(2000)
                    place = await extract_place(page)
                except Exception:
                    errors += 1
                    continue
                if not place.name or place.name in seen:
                    continue
                seen.add(place.name)
                places.append(place)
    except BrowserUnavailable as e:
        raise ServiceError("browser_unavailable") from e
    finally:
        deps.proxy.close_session(session)
    return GoogleMapsResult(
        source="google_maps",
        search_url=url,
        listings_found=found,
        places=places,
        errors=errors,
        partial=partial,
        elapsed_ms=deadline.elapsed_ms(),
    )
