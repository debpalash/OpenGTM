"""RSS/Atom fetcher built on the shared SSRF-pinned sender (spec §3 / §6 / §7).

Reuses ``automations.actions.pinned_get`` — the ONE audited sender — so the feed
URL is DNS-resolved, every A/AAAA validated as public (ANY private blocks the
whole fetch — rebinding), connected to the pinned IP with Host/SNI preserved and
``follow_redirects=False`` (a 30x to a private host is never chased). No second
hostname resolution, no TOCTOU. Conditional GET (If-None-Match / If-Modified-
Since from the cursor) makes steady-state no-op polls a cheap 304.

Pure stdlib XML parsing (no feedparser dep): handles both RSS 2.0 ``<item>`` and
Atom ``<entry>``.
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
from typing import List, Optional
from xml.etree import ElementTree as ET

from apps.api.core.config import settings
from apps.api.core.url_guard import BlockedUrlError

logger = logging.getLogger("poller.rss")

_UA = "Yupcha-Poller (+https://yupcha.com)"

# Per-host token-spacer (<= 1 req/s/host) — politeness, spec §7.
_HOST_LAST: dict = {}
_HOST_MIN_INTERVAL = 1.0


@dataclass
class FeedEntry:
    guid: str
    title: str = ""
    link: str = ""
    summary: str = ""
    published_epoch: float = 0.0


@dataclass
class FeedResult:
    status: int = 0           # HTTP status (304 = not modified short-circuit)
    not_modified: bool = False
    entries: List[FeedEntry] = field(default_factory=list)
    etag: Optional[str] = None
    last_modified: Optional[str] = None
    error: Optional[str] = None


def _localname(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]


def _parse_epoch(value: str) -> float:
    value = (value or "").strip()
    if not value:
        return 0.0
    # RFC 822 (RSS pubDate)
    try:
        dt = parsedate_to_datetime(value)
        if dt is not None:
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return dt.timestamp()
    except (TypeError, ValueError):
        pass
    # ISO 8601 (Atom updated/published)
    try:
        v = value.replace("Z", "+00:00")
        dt = datetime.fromisoformat(v)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except ValueError:
        return 0.0


def parse_feed(xml_text: str, max_entries: int) -> List[FeedEntry]:
    """Parse RSS 2.0 ``<item>`` and Atom ``<entry>`` into FeedEntry list."""
    try:
        root = ET.fromstring(xml_text)
    except ET.ParseError:
        return []
    entries: List[FeedEntry] = []
    for node in root.iter():
        ln = _localname(node.tag)
        if ln not in ("item", "entry"):
            continue
        title = guid = link = summary = ""
        published = ""
        for child in node:
            cln = _localname(child.tag)
            text = (child.text or "").strip()
            if cln == "title":
                title = text
            elif cln in ("guid", "id"):
                guid = text
            elif cln == "link":
                # RSS link text; Atom link href attribute.
                link = text or child.attrib.get("href", "") or link
            elif cln in ("description", "summary", "content"):
                summary = summary or text
            elif cln in ("pubDate", "published", "updated", "date"):
                published = published or text
        natural = guid or link or title
        if not natural:
            continue
        entries.append(FeedEntry(
            guid=natural, title=title, link=link, summary=summary[:2000],
            published_epoch=_parse_epoch(published),
        ))
        if len(entries) >= max_entries:
            break
    return entries


async def _afetch(url: str, etag: Optional[str], last_modified: Optional[str],
                  max_entries: int) -> FeedResult:
    from apps.api.services.automations.actions import pinned_get

    # per-host spacer
    from urllib.parse import urlparse
    host = (urlparse(url).hostname or "").lower()
    last = _HOST_LAST.get(host, 0.0)
    wait = _HOST_MIN_INTERVAL - (time.monotonic() - last)
    if wait > 0:
        await asyncio.sleep(wait)
    _HOST_LAST[host] = time.monotonic()

    headers = {"User-Agent": _UA, "Accept": "application/rss+xml, application/atom+xml, application/xml, text/xml"}
    if etag:
        headers["If-None-Match"] = etag
    if last_modified:
        headers["If-Modified-Since"] = last_modified

    try:
        resp = await pinned_get(url, headers, timeout=10.0)
    except BlockedUrlError as e:
        return FeedResult(error=f"blocked_url: {e}")
    except Exception as e:  # network/timeout
        return FeedResult(error=str(e)[:200])

    new_etag = resp.headers.get("ETag")
    new_lm = resp.headers.get("Last-Modified")
    if resp.status_code == 304:
        return FeedResult(status=304, not_modified=True, etag=etag or new_etag,
                          last_modified=last_modified or new_lm)
    if resp.status_code != 200:
        return FeedResult(status=resp.status_code, error=f"http_{resp.status_code}")
    entries = parse_feed(resp.text, max_entries)
    return FeedResult(status=200, entries=entries, etag=new_etag, last_modified=new_lm)


def fetch_feed(url: str, *, etag: Optional[str] = None,
               last_modified: Optional[str] = None,
               max_entries: Optional[int] = None) -> FeedResult:
    """Synchronous wrapper — the poller runs in the worker thread (asyncio.run).

    Returns a :class:`FeedResult`; on 304 ``not_modified`` is True (caller skips
    work). NEVER raises — a blocked URL / network error surfaces in ``.error``.
    """
    max_entries = max_entries or int(getattr(settings, "INTENT_POLLER_FEED_MAX_ENTRIES", 100))
    return asyncio.run(_afetch(url, etag, last_modified, max_entries))
