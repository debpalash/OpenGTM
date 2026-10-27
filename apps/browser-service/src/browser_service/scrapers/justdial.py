"""JustDial listing page (ported from legacy web_directories.scrape_justdial_page).

Renders the page in the browser and returns the listing cards' raw name,
phone text and address. Phone normalisation happens in Go.
Deviation: only justdial.com hosts are accepted (legacy took any URL).
"""

from __future__ import annotations

import asyncio
from urllib.parse import urlsplit

from bs4 import BeautifulSoup

from ..browser import BrowserUnavailable
from ..engines import Deadline, Deps, _check, _goto
from ..errors import ServiceError
from ..models import DirectoryListing, JustDialPageRequest, JustDialPageResult

MAX_LISTINGS = 200


def parse_listings(html: str) -> list[DirectoryListing]:
    soup = BeautifulSoup(html, "html.parser")
    out: list[DirectoryListing] = []
    for card in soup.select(".store-details, .jsx-card, .resultbox_info"):
        name_el = card.select_one(".store-name, .resultbox_title a, .lng_cont_name")
        phone_el = card.select_one(".contact-info, .resultbox_phone, .lng_cont_number")
        addr_el = card.select_one(".address-info, .resultbox_address, .cont_sw_addr")
        name = name_el.get_text(strip=True) if name_el else ""
        if not name:
            continue
        out.append(
            DirectoryListing(
                name=name[:300],
                phone_text=(phone_el.get_text() if phone_el else "").strip()[:300],
                address=(addr_el.get_text(strip=True) if addr_el else "")[:500],
            )
        )
        if len(out) >= MAX_LISTINGS:
            break
    return out


def _is_justdial(url: str) -> bool:
    host = (urlsplit(url).hostname or "").lower().rstrip(".")
    return host == "justdial.com" or host.endswith(".justdial.com")


async def scrape(deps: Deps, req: JustDialPageRequest, deadline: Deadline) -> JustDialPageResult:
    _check(deps, req.url)
    if not _is_justdial(req.url):
        raise ServiceError("bad_request")
    session = deps.proxy.open_session(deps.settings.max_download_bytes)
    try:
        async with deps.pool.page(session) as page:
            resp = await _goto(page, session, req.url, "domcontentloaded", deadline)
            await page.wait_for_timeout(min(2000, deadline.left() * 1000))
            html = await page.content()
            final_url = page.url
    except BrowserUnavailable as e:
        raise ServiceError("browser_unavailable") from e
    finally:
        deps.proxy.close_session(session)
    listings = await asyncio.to_thread(parse_listings, html[: deps.settings.max_response_bytes * 2])
    return JustDialPageResult(
        source="justdial_page",
        final_url=final_url,
        status=resp.status if resp is not None else 0,
        listings=listings,
        elapsed_ms=deadline.elapsed_ms(),
    )
