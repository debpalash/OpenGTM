"""HTML helpers: size caps, html -> text, page metadata, challenge detection."""

from __future__ import annotations

import re

from bs4 import BeautifulSoup

# legacy leadgen/http.py challenge heuristics (CHALLENGE_MARKERS etc.)
CHALLENGE_MARKERS = (
    "Just a moment",
    "cf-challenge",
    "cf_clearance",
    "__cf_bm",
    "challenge-platform",
    "Checking your browser",
    "Attention Required",
    "Access Denied",
    "Please verify you are a human",
    "captcha-delivery.com",
    "geo.captcha-delivery.com",
)
JS_REQUIRED_MARKERS = (
    "enable javascript",
    "please enable javascript",
    "javascript is required",
    "javascript is disabled",
    "you need to enable javascript",
    "<noscript>",
)
BLOCKED_STATUS_CODES = frozenset({403, 429, 503, 520, 521, 522, 523, 524})
TINY_BODY_THRESHOLD = 500


def is_challenge(status: int, text: str) -> bool:
    if status in BLOCKED_STATUS_CODES:
        return True
    head = text[:5000]
    if any(m in head for m in CHALLENGE_MARKERS):
        return True
    lower = head.lower()
    if any(m in lower for m in JS_REQUIRED_MARKERS):
        return True
    return 200 <= status < 300 and 0 < len(text.strip()) < TINY_BODY_THRESHOLD


def truncate_utf8(s: str, max_bytes: int) -> tuple[str, bool]:
    """Cut ``s`` to at most ``max_bytes`` UTF-8 bytes without splitting a character."""
    b = s.encode("utf-8", errors="replace")
    if len(b) <= max_bytes:
        return s, False
    return b[:max_bytes].decode("utf-8", errors="ignore"), True


def page_meta(html: str) -> tuple[str, str]:
    """(title, meta description) — empty strings when absent."""
    soup = BeautifulSoup(html, "lxml")
    title = soup.title.get_text(strip=True) if soup.title else ""
    tag = soup.find("meta", attrs={"name": "description"}) or soup.find("meta", attrs={"property": "og:description"})
    desc = tag.get("content", "") if tag else ""
    return title[:1000], str(desc)[:2000]


def html_to_text(html: str) -> str:
    """Markdown-ish text (legacy services/scraper.py _parse_html): scripts,
    styles and embeds dropped, headings/lists kept, links and images stripped."""
    from markdownify import markdownify

    soup = BeautifulSoup(html, "lxml")
    for tag in soup(["script", "style", "noscript", "iframe", "svg"]):
        tag.decompose()
    body = str(soup.body) if soup.body else str(soup)
    text = markdownify(body, heading_style="ATX", strip=["a", "img"])
    return re.sub(r"\n{3,}", "\n\n", text).strip()
